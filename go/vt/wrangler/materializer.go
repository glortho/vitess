/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package wrangler

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/json2"
	"vitess.io/vitess/go/sqlescape"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/binlog/binlogplayer"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/key"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/mysqlctl/tmutils"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topotools"
	"vitess.io/vitess/go/vt/vtctl/schematools"
	"vitess.io/vitess/go/vt/vtctl/workflow"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/evalengine"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
	"vitess.io/vitess/go/vt/vttablet/tabletmanager/vreplication"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	querypb "vitess.io/vitess/go/vt/proto/query"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
	vtctldatapb "vitess.io/vitess/go/vt/proto/vtctldata"
)

type materializer struct {
	wr            *Wrangler
	ms            *vtctldatapb.MaterializeSettings
	targetVSchema *vindexes.KeyspaceSchema
	sourceShards  []*topo.ShardInfo
	targetShards  []*topo.ShardInfo
	isPartial     bool
}

const (
	createDDLAsCopy                = "copy"
	createDDLAsCopyDropConstraint  = "copy:drop_constraint"
	createDDLAsCopyDropForeignKeys = "copy:drop_foreign_keys"
)

// addTablesToVSchema adds tables to an (unsharded) vschema. Depending on copyAttributes It will also add any sequence info
// that is associated with a table by copying it from the vschema of the source keyspace.
// For a migrate workflow we do not copy attributes since the source keyspace is just a proxy to import data into Vitess
// Todo: For now we only copy sequence but later we may also want to copy other attributes like authoritative column flag and list of columns
func (wr *Wrangler) addTablesToVSchema(ctx context.Context, sourceKeyspace string, targetVSchema *vschemapb.Keyspace, tables []string, copyAttributes bool) error {
	if targetVSchema.Tables == nil {
		targetVSchema.Tables = make(map[string]*vschemapb.Table)
	}
	for _, table := range tables {
		targetVSchema.Tables[table] = &vschemapb.Table{}
	}

	if copyAttributes { // if source keyspace is provided, copy over the sequence info.
		srcVSchema, err := wr.ts.GetVSchema(ctx, sourceKeyspace)
		if err != nil {
			return err
		}
		for _, table := range tables {
			srcTable, ok := srcVSchema.Tables[table]
			if ok {
				targetVSchema.Tables[table].AutoIncrement = srcTable.AutoIncrement
			}
		}

	}
	return nil
}

func shouldInclude(table string, excludes []string) bool {
	// We filter out internal tables elsewhere when processing SchemaDefinition
	// structures built from the GetSchema database related API calls. In this
	// case, however, the table list comes from the user via the -tables flag
	// so we need to filter out internal table names here in case a user has
	// explicitly specified some.
	// This could happen if there's some automated tooling that creates the list of
	// tables to explicitly specify.
	// But given that this should never be done in practice, we ignore the request.
	if schema.IsInternalOperationTableName(table) {
		return false
	}
	for _, t := range excludes {
		if t == table {
			return false
		}
	}
	return true
}

// MoveTables initiates moving table(s) over to another keyspace
func (wr *Wrangler) MoveTables(ctx context.Context, workflow, sourceKeyspace, targetKeyspace, tableSpecs,
	cell, tabletTypes string, allTables bool, excludeTables string, autoStart, stopAfterCopy bool,
	externalCluster string, dropForeignKeys bool, sourceTimeZone string, sourceShards []string) error {
	//FIXME validate tableSpecs, allTables, excludeTables
	var tables []string
	var externalTopo *topo.Server
	var err error

	if externalCluster != "" { // when the source is an external mysql cluster mounted using the Mount command
		externalTopo, err = wr.ts.OpenExternalVitessClusterServer(ctx, externalCluster)
		if err != nil {
			return err
		}
		wr.sourceTs = externalTopo
		log.Infof("Successfully opened external topo: %+v", externalTopo)
	}

	var vschema *vschemapb.Keyspace
	vschema, err = wr.ts.GetVSchema(ctx, targetKeyspace)
	if err != nil {
		return err
	}
	if vschema == nil {
		return fmt.Errorf("no vschema found for target keyspace %s", targetKeyspace)
	}
	if strings.HasPrefix(tableSpecs, "{") {
		if vschema.Tables == nil {
			vschema.Tables = make(map[string]*vschemapb.Table)
		}
		wrap := fmt.Sprintf(`{"tables": %s}`, tableSpecs)
		ks := &vschemapb.Keyspace{}
		if err := json2.Unmarshal([]byte(wrap), ks); err != nil {
			return err
		}
		for table, vtab := range ks.Tables {
			vschema.Tables[table] = vtab
			tables = append(tables, table)
		}
	} else {
		if len(strings.TrimSpace(tableSpecs)) > 0 {
			tables = strings.Split(tableSpecs, ",")
		}
		ksTables, err := wr.getKeyspaceTables(ctx, sourceKeyspace, wr.sourceTs)
		if err != nil {
			return err
		}
		if len(tables) > 0 {
			err = wr.validateSourceTablesExist(ctx, sourceKeyspace, ksTables, tables)
			if err != nil {
				return err
			}
		} else {
			if allTables {
				tables = ksTables
			} else {
				return fmt.Errorf("no tables to move")
			}
		}
		var excludeTablesList []string
		excludeTables = strings.TrimSpace(excludeTables)
		if excludeTables != "" {
			excludeTablesList = strings.Split(excludeTables, ",")
			err = wr.validateSourceTablesExist(ctx, sourceKeyspace, ksTables, excludeTablesList)
			if err != nil {
				return err
			}
		}
		var tables2 []string
		for _, t := range tables {
			if shouldInclude(t, excludeTablesList) {
				tables2 = append(tables2, t)
			}
		}
		tables = tables2
		if len(tables) == 0 {
			return fmt.Errorf("no tables to move")
		}
		log.Infof("Found tables to move: %s", strings.Join(tables, ","))

		if !vschema.Sharded {
			if err := wr.addTablesToVSchema(ctx, sourceKeyspace, vschema, tables, externalTopo == nil); err != nil {
				return err
			}
		}
	}
	if externalTopo == nil {
		// Save routing rules before vschema. If we save vschema first, and routing rules
		// fails to save, we may generate duplicate table errors.
		rules, err := topotools.GetRoutingRules(ctx, wr.ts)
		if err != nil {
			return err
		}
		for _, table := range tables {
			toSource := []string{sourceKeyspace + "." + table}
			rules[table] = toSource
			rules[table+"@replica"] = toSource
			rules[table+"@rdonly"] = toSource
			rules[targetKeyspace+"."+table] = toSource
			rules[targetKeyspace+"."+table+"@replica"] = toSource
			rules[targetKeyspace+"."+table+"@rdonly"] = toSource
			rules[targetKeyspace+"."+table] = toSource
			rules[sourceKeyspace+"."+table+"@replica"] = toSource
			rules[sourceKeyspace+"."+table+"@rdonly"] = toSource
		}
		if err := topotools.SaveRoutingRules(ctx, wr.ts, rules); err != nil {
			return err
		}
		if vschema != nil {
			// We added to the vschema.
			if err := wr.ts.SaveVSchema(ctx, targetKeyspace, vschema); err != nil {
				return err
			}
		}
	}
	if err := wr.ts.RebuildSrvVSchema(ctx, nil); err != nil {
		return err
	}
	ms := &vtctldatapb.MaterializeSettings{
		Workflow:              workflow,
		MaterializationIntent: vtctldatapb.MaterializationIntent_MOVETABLES,
		SourceKeyspace:        sourceKeyspace,
		TargetKeyspace:        targetKeyspace,
		Cell:                  cell,
		TabletTypes:           tabletTypes,
		StopAfterCopy:         stopAfterCopy,
		ExternalCluster:       externalCluster,
		SourceShards:          sourceShards,
	}
	if sourceTimeZone != "" {
		ms.SourceTimeZone = sourceTimeZone
		ms.TargetTimeZone = "UTC"
	}
	createDDLMode := createDDLAsCopy
	if dropForeignKeys {
		createDDLMode = createDDLAsCopyDropForeignKeys
	}

	for _, table := range tables {
		buf := sqlparser.NewTrackedBuffer(nil)
		buf.Myprintf("select * from %v", sqlparser.NewIdentifierCS(table))
		ms.TableSettings = append(ms.TableSettings, &vtctldatapb.TableMaterializeSettings{
			TargetTable:      table,
			SourceExpression: buf.String(),
			CreateDdl:        createDDLMode,
		})
	}
	mz, err := wr.prepareMaterializerStreams(ctx, ms)
	if err != nil {
		return err
	}

	if sourceTimeZone != "" {
		if err := mz.checkTZConversion(ctx, sourceTimeZone); err != nil {
			return err
		}
	}

	tabletShards, err := wr.collectTargetStreams(ctx, mz)
	if err != nil {
		return err
	}

	migrationID, err := getMigrationID(targetKeyspace, tabletShards)
	if err != nil {
		return err
	}

	if externalCluster == "" {
		exists, tablets, err := wr.checkIfPreviousJournalExists(ctx, mz, migrationID)
		if err != nil {
			return err
		}
		if exists {
			wr.Logger().Errorf("Found a previous journal entry for %d", migrationID)
			msg := fmt.Sprintf("found an entry from a previous run for migration id %d in _vt.resharding_journal of tablets %s,",
				migrationID, strings.Join(tablets, ","))
			msg += fmt.Sprintf("please review and delete it before proceeding and restart the workflow using the Workflow %s.%s start",
				workflow, targetKeyspace)
			return fmt.Errorf(msg)
		}
	}
	if autoStart {
		return mz.startStreams(ctx)
	}
	wr.Logger().Infof("Streams will not be started since -auto_start is set to false")

	return nil
}

func (wr *Wrangler) validateSourceTablesExist(ctx context.Context, sourceKeyspace string, ksTables, tables []string) error {
	// validate that tables provided are present in the source keyspace
	var missingTables []string
	for _, table := range tables {
		if schema.IsInternalOperationTableName(table) {
			continue
		}
		found := false

		for _, ksTable := range ksTables {
			if table == ksTable {
				found = true
				break
			}
		}
		if !found {
			missingTables = append(missingTables, table)
		}
	}
	if len(missingTables) > 0 {
		return fmt.Errorf("table(s) not found in source keyspace %s: %s", sourceKeyspace, strings.Join(missingTables, ","))
	}
	return nil
}

func (wr *Wrangler) getKeyspaceTables(ctx context.Context, ks string, ts *topo.Server) ([]string, error) {
	shards, err := ts.GetServingShards(ctx, ks)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return nil, fmt.Errorf("keyspace %s has no shards", ks)
	}
	primary := shards[0].PrimaryAlias
	if primary == nil {
		return nil, fmt.Errorf("shard does not have a primary: %v", shards[0].ShardName())
	}
	allTables := []string{"/.*/"}

	ti, err := ts.GetTablet(ctx, primary)
	if err != nil {
		return nil, err
	}
	req := &tabletmanagerdatapb.GetSchemaRequest{Tables: allTables}
	schema, err := wr.tmc.GetSchema(ctx, ti.Tablet, req)
	if err != nil {
		return nil, err
	}
	log.Infof("got table schemas from source primary %v.", primary)

	var sourceTables []string
	for _, td := range schema.TableDefinitions {
		sourceTables = append(sourceTables, td.Name)
	}
	return sourceTables, nil
}

func (wr *Wrangler) checkIfPreviousJournalExists(ctx context.Context, mz *materializer, migrationID int64) (bool, []string, error) {
	forAllSources := func(f func(*topo.ShardInfo) error) error {
		var wg sync.WaitGroup
		allErrors := &concurrency.AllErrorRecorder{}
		for _, sourceShard := range mz.sourceShards {
			wg.Add(1)
			go func(sourceShard *topo.ShardInfo) {
				defer wg.Done()

				if err := f(sourceShard); err != nil {
					allErrors.RecordError(err)
				}
			}(sourceShard)
		}
		wg.Wait()
		return allErrors.AggrError(vterrors.Aggregate)
	}

	var (
		mu      sync.Mutex
		exists  bool
		tablets []string
		ws      = workflow.NewServer(wr.ts, wr.tmc)
	)

	err := forAllSources(func(si *topo.ShardInfo) error {
		tablet, err := wr.ts.GetTablet(ctx, si.PrimaryAlias)
		if err != nil {
			return err
		}
		if tablet == nil {
			return nil
		}
		_, exists, err = ws.CheckReshardingJournalExistsOnTablet(ctx, tablet.Tablet, migrationID)
		if err != nil {
			return err
		}
		if exists {
			mu.Lock()
			defer mu.Unlock()
			tablets = append(tablets, tablet.AliasString())
		}
		return nil
	})
	return exists, tablets, err
}

// CreateLookupVindex creates a lookup vindex and sets up the backfill.
func (wr *Wrangler) CreateLookupVindex(ctx context.Context, keyspace string, specs *vschemapb.Keyspace, cell, tabletTypes string, continueAfterCopyWithOwner bool) error {
	ms, sourceVSchema, targetVSchema, err := wr.prepareCreateLookup(ctx, keyspace, specs, continueAfterCopyWithOwner)
	if err != nil {
		return err
	}
	if err := wr.ts.SaveVSchema(ctx, ms.TargetKeyspace, targetVSchema); err != nil {
		return err
	}
	ms.Cell = cell
	ms.TabletTypes = tabletTypes
	if err := wr.Materialize(ctx, ms); err != nil {
		return err
	}
	if err := wr.ts.SaveVSchema(ctx, keyspace, sourceVSchema); err != nil {
		return err
	}

	return wr.ts.RebuildSrvVSchema(ctx, nil)
}

// prepareCreateLookup performs the preparatory steps for creating a lookup vindex.
func (wr *Wrangler) prepareCreateLookup(ctx context.Context, keyspace string, specs *vschemapb.Keyspace, continueAfterCopyWithOwner bool) (ms *vtctldatapb.MaterializeSettings, sourceVSchema, targetVSchema *vschemapb.Keyspace, err error) {
	// Important variables are pulled out here.
	var (
		// lookup vindex info
		vindexName      string
		vindex          *vschemapb.Vindex
		targetKeyspace  string
		targetTableName string
		vindexFromCols  []string
		vindexToCol     string

		// source table info
		sourceTableName string
		// sourceTable is the supplied table info
		sourceTable *vschemapb.Table
		// sourceVSchemaTable is the table info present in the vschema
		sourceVSchemaTable *vschemapb.Table
		// sourceVindexColumns are computed from the input sourceTable
		sourceVindexColumns []string

		// target table info
		createDDL        string
		materializeQuery string
	)

	// Validate input vindex
	if len(specs.Vindexes) != 1 {
		return nil, nil, nil, fmt.Errorf("only one vindex must be specified in the specs: %v", specs.Vindexes)
	}
	for name, vi := range specs.Vindexes {
		vindexName = name
		vindex = vi
	}
	if !strings.Contains(vindex.Type, "lookup") {
		return nil, nil, nil, fmt.Errorf("vindex %s is not a lookup type", vindex.Type)
	}

	targetKeyspace, targetTableName, err = sqlparser.ParseTable(vindex.Params["table"])
	if err != nil || targetKeyspace == "" {
		return nil, nil, nil, fmt.Errorf("vindex table name must be in the form <keyspace>.<table>. Got: %v", vindex.Params["table"])
	}

	vindexFromCols = strings.Split(vindex.Params["from"], ",")
	if strings.Contains(vindex.Type, "unique") {
		if len(vindexFromCols) != 1 {
			return nil, nil, nil, fmt.Errorf("unique vindex 'from' should have only one column: %v", vindex)
		}
	} else {
		if len(vindexFromCols) < 2 {
			return nil, nil, nil, fmt.Errorf("non-unique vindex 'from' should have more than one column: %v", vindex)
		}
	}
	vindexToCol = vindex.Params["to"]
	// Make the vindex write_only. If one exists already in the vschema,
	// it will need to match this vindex exactly, including the write_only setting.
	vindex.Params["write_only"] = "true"
	// See if we can create the vindex without errors.
	if _, err := vindexes.CreateVindex(vindex.Type, vindexName, vindex.Params); err != nil {
		return nil, nil, nil, err
	}

	// Validate input table
	if len(specs.Tables) != 1 {
		return nil, nil, nil, fmt.Errorf("exactly one table must be specified in the specs: %v", specs.Tables)
	}
	// Loop executes once.
	for k, ti := range specs.Tables {
		if len(ti.ColumnVindexes) != 1 {
			return nil, nil, nil, fmt.Errorf("exactly one ColumnVindex must be specified for the table: %v", specs.Tables)
		}
		sourceTableName = k
		sourceTable = ti
	}

	// Validate input table and vindex consistency
	if sourceTable.ColumnVindexes[0].Name != vindexName {
		return nil, nil, nil, fmt.Errorf("ColumnVindex name must match vindex name: %s vs %s", sourceTable.ColumnVindexes[0].Name, vindexName)
	}
	if vindex.Owner != "" && vindex.Owner != sourceTableName {
		return nil, nil, nil, fmt.Errorf("vindex owner must match table name: %v vs %v", vindex.Owner, sourceTableName)
	}
	if len(sourceTable.ColumnVindexes[0].Columns) != 0 {
		sourceVindexColumns = sourceTable.ColumnVindexes[0].Columns
	} else {
		if sourceTable.ColumnVindexes[0].Column == "" {
			return nil, nil, nil, fmt.Errorf("at least one column must be specified in ColumnVindexes: %v", sourceTable.ColumnVindexes)
		}
		sourceVindexColumns = []string{sourceTable.ColumnVindexes[0].Column}
	}
	if len(sourceVindexColumns) != len(vindexFromCols) {
		return nil, nil, nil, fmt.Errorf("length of table columns differes from length of vindex columns: %v vs %v", sourceVindexColumns, vindexFromCols)
	}

	// Validate against source vschema
	sourceVSchema, err = wr.ts.GetVSchema(ctx, keyspace)
	if err != nil {
		return nil, nil, nil, err
	}
	if sourceVSchema.Vindexes == nil {
		sourceVSchema.Vindexes = make(map[string]*vschemapb.Vindex)
	}
	// If source and target keyspaces are same, Make vschemas point to the same object.
	if keyspace == targetKeyspace {
		targetVSchema = sourceVSchema
	} else {
		targetVSchema, err = wr.ts.GetVSchema(ctx, targetKeyspace)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if targetVSchema.Vindexes == nil {
		targetVSchema.Vindexes = make(map[string]*vschemapb.Vindex)
	}
	if targetVSchema.Tables == nil {
		targetVSchema.Tables = make(map[string]*vschemapb.Table)
	}
	if existing, ok := sourceVSchema.Vindexes[vindexName]; ok {
		if !proto.Equal(existing, vindex) {
			return nil, nil, nil, fmt.Errorf("a conflicting vindex named %s already exists in the source vschema", vindexName)
		}
	}
	sourceVSchemaTable = sourceVSchema.Tables[sourceTableName]
	if sourceVSchemaTable == nil {
		if !schema.IsInternalOperationTableName(sourceTableName) {
			return nil, nil, nil, fmt.Errorf("source table %s not found in vschema", sourceTableName)
		}
	}
	for _, colVindex := range sourceVSchemaTable.ColumnVindexes {
		// For a conflict, the vindex name and column should match.
		if colVindex.Name != vindexName {
			continue
		}
		colName := colVindex.Column
		if len(colVindex.Columns) != 0 {
			colName = colVindex.Columns[0]
		}
		if colName == sourceVindexColumns[0] {
			return nil, nil, nil, fmt.Errorf("ColumnVindex for table %v already exists: %v, please remove it and try again", sourceTableName, colName)
		}
	}

	// Validate against source schema
	sourceShards, err := wr.ts.GetServingShards(ctx, keyspace)
	if err != nil {
		return nil, nil, nil, err
	}
	onesource := sourceShards[0]
	if onesource.PrimaryAlias == nil {
		return nil, nil, nil, fmt.Errorf("source shard has no primary: %v", onesource.ShardName())
	}
	req := &tabletmanagerdatapb.GetSchemaRequest{Tables: []string{sourceTableName}}
	tableSchema, err := schematools.GetSchema(ctx, wr.ts, wr.tmc, onesource.PrimaryAlias, req)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(tableSchema.TableDefinitions) != 1 {
		return nil, nil, nil, fmt.Errorf("unexpected number of tables returned from schema: %v", tableSchema.TableDefinitions)
	}

	// Generate "create table" statement
	lines := strings.Split(tableSchema.TableDefinitions[0].Schema, "\n")
	if len(lines) < 3 {
		// Unreachable
		return nil, nil, nil, fmt.Errorf("schema looks incorrect: %s, expecting at least four lines", tableSchema.TableDefinitions[0].Schema)
	}
	var modified []string
	modified = append(modified, strings.Replace(lines[0], sourceTableName, targetTableName, 1))
	for i := range sourceVindexColumns {
		line, err := generateColDef(lines, sourceVindexColumns[i], vindexFromCols[i])
		if err != nil {
			return nil, nil, nil, err
		}
		modified = append(modified, line)
	}

	if vindex.Params["data_type"] == "" || strings.EqualFold(vindex.Type, "consistent_lookup_unique") || strings.EqualFold(vindex.Type, "consistent_lookup") {
		modified = append(modified, fmt.Sprintf("  %s varbinary(128),", sqlescape.EscapeID(vindexToCol)))
	} else {
		modified = append(modified, fmt.Sprintf("  %s %s,", sqlescape.EscapeID(vindexToCol), sqlescape.EscapeID(vindex.Params["data_type"])))
	}
	buf := sqlparser.NewTrackedBuffer(nil)
	fmt.Fprintf(buf, "  PRIMARY KEY (")
	prefix := ""
	for _, col := range vindexFromCols {
		fmt.Fprintf(buf, "%s%s", prefix, sqlescape.EscapeID(col))
		prefix = ", "
	}
	fmt.Fprintf(buf, ")")
	modified = append(modified, buf.String())
	modified = append(modified, ")")
	createDDL = strings.Join(modified, "\n")

	// Generate vreplication query
	buf = sqlparser.NewTrackedBuffer(nil)
	buf.Myprintf("select ")
	for i := range vindexFromCols {
		buf.Myprintf("%v as %v, ", sqlparser.NewIdentifierCI(sourceVindexColumns[i]), sqlparser.NewIdentifierCI(vindexFromCols[i]))
	}
	if strings.EqualFold(vindexToCol, "keyspace_id") || strings.EqualFold(vindex.Type, "consistent_lookup_unique") || strings.EqualFold(vindex.Type, "consistent_lookup") {
		buf.Myprintf("keyspace_id() as %v ", sqlparser.NewIdentifierCI(vindexToCol))
	} else {
		buf.Myprintf("%v as %v ", sqlparser.NewIdentifierCI(vindexToCol), sqlparser.NewIdentifierCI(vindexToCol))
	}
	buf.Myprintf("from %v", sqlparser.NewIdentifierCS(sourceTableName))
	if vindex.Owner != "" {
		// Only backfill
		buf.Myprintf(" group by ")
		for i := range vindexFromCols {
			buf.Myprintf("%v, ", sqlparser.NewIdentifierCI(vindexFromCols[i]))
		}
		buf.Myprintf("%v", sqlparser.NewIdentifierCI(vindexToCol))
	}
	materializeQuery = buf.String()

	// Update targetVSchema
	var targetTable *vschemapb.Table
	if targetVSchema.Sharded {
		// Choose a primary vindex type for target table based on source specs
		var targetVindexType string
		var targetVindex *vschemapb.Vindex
		for _, field := range tableSchema.TableDefinitions[0].Fields {
			if sourceVindexColumns[0] == field.Name {
				targetVindexType, err = vindexes.ChooseVindexForType(field.Type)
				if err != nil {
					return nil, nil, nil, err
				}
				targetVindex = &vschemapb.Vindex{
					Type: targetVindexType,
				}
				break
			}
		}
		if targetVindex == nil {
			// Unreachable. We validated column names when generating the DDL.
			return nil, nil, nil, fmt.Errorf("column %s not found in schema %v", sourceVindexColumns[0], tableSchema.TableDefinitions[0])
		}
		if existing, ok := targetVSchema.Vindexes[targetVindexType]; ok {
			if !proto.Equal(existing, targetVindex) {
				return nil, nil, nil, fmt.Errorf("a conflicting vindex named %v already exists in the target vschema", targetVindexType)
			}
		} else {
			targetVSchema.Vindexes[targetVindexType] = targetVindex
		}

		targetTable = &vschemapb.Table{
			ColumnVindexes: []*vschemapb.ColumnVindex{{
				Column: vindexFromCols[0],
				Name:   targetVindexType,
			}},
		}
	} else {
		targetTable = &vschemapb.Table{}
	}
	if existing, ok := targetVSchema.Tables[targetTableName]; ok {
		if !proto.Equal(existing, targetTable) {
			return nil, nil, nil, fmt.Errorf("a conflicting table named %v already exists in the target vschema", targetTableName)
		}
	} else {
		targetVSchema.Tables[targetTableName] = targetTable
	}

	ms = &vtctldatapb.MaterializeSettings{
		Workflow:              targetTableName + "_vdx",
		MaterializationIntent: vtctldatapb.MaterializationIntent_CREATELOOKUPINDEX,
		SourceKeyspace:        keyspace,
		TargetKeyspace:        targetKeyspace,
		StopAfterCopy:         vindex.Owner != "" && !continueAfterCopyWithOwner,
		TableSettings: []*vtctldatapb.TableMaterializeSettings{{
			TargetTable:      targetTableName,
			SourceExpression: materializeQuery,
			CreateDdl:        createDDL,
		}},
	}

	// Update sourceVSchema
	sourceVSchema.Vindexes[vindexName] = vindex
	sourceVSchemaTable.ColumnVindexes = append(sourceVSchemaTable.ColumnVindexes, sourceTable.ColumnVindexes[0])

	return ms, sourceVSchema, targetVSchema, nil
}

func generateColDef(lines []string, sourceVindexCol, vindexFromCol string) (string, error) {
	source := sqlescape.EscapeID(sourceVindexCol)
	target := sqlescape.EscapeID(vindexFromCol)

	for _, line := range lines[1:] {
		if strings.Contains(line, source) {
			line = strings.Replace(line, source, target, 1)
			line = strings.Replace(line, " AUTO_INCREMENT", "", 1)
			line = strings.Replace(line, " DEFAULT NULL", "", 1)
			return line, nil
		}
	}
	return "", fmt.Errorf("column %s not found in schema %v", sourceVindexCol, lines)
}

// ExternalizeVindex externalizes a lookup vindex that's finished backfilling or has caught up.
func (wr *Wrangler) ExternalizeVindex(ctx context.Context, qualifiedVindexName string) error {
	splits := strings.Split(qualifiedVindexName, ".")
	if len(splits) != 2 {
		return fmt.Errorf("vindex name should be of the form keyspace.vindex: %s", qualifiedVindexName)
	}
	sourceKeyspace, vindexName := splits[0], splits[1]
	sourceVSchema, err := wr.ts.GetVSchema(ctx, sourceKeyspace)
	if err != nil {
		return err
	}
	sourceVindex := sourceVSchema.Vindexes[vindexName]
	if sourceVindex == nil {
		return fmt.Errorf("vindex %s not found in vschema", qualifiedVindexName)
	}

	targetKeyspace, targetTableName, err := sqlparser.ParseTable(sourceVindex.Params["table"])
	if err != nil || targetKeyspace == "" {
		return fmt.Errorf("vindex table name must be in the form <keyspace>.<table>. Got: %v", sourceVindex.Params["table"])
	}
	workflow := targetTableName + "_vdx"
	targetShards, err := wr.ts.GetServingShards(ctx, targetKeyspace)
	if err != nil {
		return err
	}

	// Create a parallelizer function.
	forAllTargets := func(f func(*topo.ShardInfo) error) error {
		var wg sync.WaitGroup
		allErrors := &concurrency.AllErrorRecorder{}
		for _, targetShard := range targetShards {
			wg.Add(1)
			go func(targetShard *topo.ShardInfo) {
				defer wg.Done()

				if err := f(targetShard); err != nil {
					allErrors.RecordError(err)
				}
			}(targetShard)
		}
		wg.Wait()
		return allErrors.AggrError(vterrors.Aggregate)
	}

	err = forAllTargets(func(targetShard *topo.ShardInfo) error {
		targetPrimary, err := wr.ts.GetTablet(ctx, targetShard.PrimaryAlias)
		if err != nil {
			return err
		}
		p3qr, err := wr.tmc.VReplicationExec(ctx, targetPrimary.Tablet, fmt.Sprintf("select id, state, message, source from _vt.vreplication where workflow=%s and db_name=%s", encodeString(workflow), encodeString(targetPrimary.DbName())))
		if err != nil {
			return err
		}
		qr := sqltypes.Proto3ToResult(p3qr)
		for _, row := range qr.Rows {
			id, err := evalengine.ToInt64(row[0])
			if err != nil {
				return err
			}
			state := row[1].ToString()
			message := row[2].ToString()
			var bls binlogdatapb.BinlogSource
			sourceBytes, err := row[3].ToBytes()
			if err != nil {
				return err
			}
			if err := prototext.Unmarshal(sourceBytes, &bls); err != nil {
				return err
			}
			if sourceVindex.Owner == "" || !bls.StopAfterCopy {
				// If there's no owner or we've requested that the workflow NOT be stopped
				// after the copy phase completes, then all streams need to be running.
				if state != binlogplayer.BlpRunning {
					return fmt.Errorf("stream %d for %v.%v is not in Running state: %v", id, targetShard.Keyspace(), targetShard.ShardName(), state)
				}
			} else {
				// If there is an owner, all streams need to be stopped after copy.
				if state != binlogplayer.BlpStopped || !strings.Contains(message, "Stopped after copy") {
					return fmt.Errorf("stream %d for %v.%v is not in Stopped after copy state: %v, %v", id, targetShard.Keyspace(), targetShard.ShardName(), state, message)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if sourceVindex.Owner != "" {
		// If there is an owner, we have to delete the streams.
		err := forAllTargets(func(targetShard *topo.ShardInfo) error {
			targetPrimary, err := wr.ts.GetTablet(ctx, targetShard.PrimaryAlias)
			if err != nil {
				return err
			}
			query := fmt.Sprintf("delete from _vt.vreplication where db_name=%s and workflow=%s", encodeString(targetPrimary.DbName()), encodeString(workflow))
			_, err = wr.tmc.VReplicationExec(ctx, targetPrimary.Tablet, query)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	// Remove the write_only param and save the source vschema.
	delete(sourceVindex.Params, "write_only")
	if err := wr.ts.SaveVSchema(ctx, sourceKeyspace, sourceVSchema); err != nil {
		return err
	}
	return wr.ts.RebuildSrvVSchema(ctx, nil)
}

func (wr *Wrangler) collectTargetStreams(ctx context.Context, mz *materializer) ([]string, error) {
	var shardTablets []string
	var mu sync.Mutex
	err := mz.forAllTargets(func(target *topo.ShardInfo) error {
		var qrproto *querypb.QueryResult
		var id int64
		var err error
		targetPrimary, err := mz.wr.ts.GetTablet(ctx, target.PrimaryAlias)
		if err != nil {
			return vterrors.Wrapf(err, "GetTablet(%v) failed", target.PrimaryAlias)
		}
		query := fmt.Sprintf("select id from _vt.vreplication where db_name=%s and workflow=%s", encodeString(targetPrimary.DbName()), encodeString(mz.ms.Workflow))
		if qrproto, err = mz.wr.tmc.VReplicationExec(ctx, targetPrimary.Tablet, query); err != nil {
			return vterrors.Wrapf(err, "VReplicationExec(%v, %s)", targetPrimary.Tablet, query)
		}
		qr := sqltypes.Proto3ToResult(qrproto)
		for i := 0; i < len(qr.Rows); i++ {
			id, err = evalengine.ToInt64(qr.Rows[i][0])
			if err != nil {
				return err
			}
			mu.Lock()
			shardTablets = append(shardTablets, fmt.Sprintf("%s:%d", target.ShardName(), id))
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return shardTablets, nil
}

// getMigrationID produces a reproducible hash based on the input parameters.
func getMigrationID(targetKeyspace string, shardTablets []string) (int64, error) {
	sort.Strings(shardTablets)
	hasher := fnv.New64()
	hasher.Write([]byte(targetKeyspace))
	for _, str := range shardTablets {
		hasher.Write([]byte(str))
	}
	// Convert to int64 after dropping the highest bit.
	return int64(hasher.Sum64() & math.MaxInt64), nil
}

func (wr *Wrangler) prepareMaterializerStreams(ctx context.Context, ms *vtctldatapb.MaterializeSettings) (*materializer, error) {
	if err := wr.validateNewWorkflow(ctx, ms.TargetKeyspace, ms.Workflow); err != nil {
		return nil, err
	}
	mz, err := wr.buildMaterializer(ctx, ms)
	if err != nil {
		return nil, err
	}
	if err := mz.deploySchema(ctx); err != nil {
		return nil, err
	}
	insertMap := make(map[string]string, len(mz.targetShards))
	for _, targetShard := range mz.targetShards {
		inserts, err := mz.generateInserts(ctx, targetShard)
		if err != nil {
			return nil, err
		}
		insertMap[targetShard.ShardName()] = inserts
	}
	if err := mz.createStreams(ctx, insertMap); err != nil {
		return nil, err
	}
	return mz, nil
}

// Materialize performs the steps needed to materialize a list of tables based on the materialization specs.
func (wr *Wrangler) Materialize(ctx context.Context, ms *vtctldatapb.MaterializeSettings) error {
	mz, err := wr.prepareMaterializerStreams(ctx, ms)
	if err != nil {
		return err
	}
	return mz.startStreams(ctx)
}

func (wr *Wrangler) buildMaterializer(ctx context.Context, ms *vtctldatapb.MaterializeSettings) (*materializer, error) {
	vschema, err := wr.ts.GetVSchema(ctx, ms.TargetKeyspace)
	if err != nil {
		return nil, err
	}
	targetVSchema, err := vindexes.BuildKeyspaceSchema(vschema, ms.TargetKeyspace)
	if err != nil {
		return nil, err
	}
	if targetVSchema.Keyspace.Sharded {
		for _, ts := range ms.TableSettings {
			if targetVSchema.Tables[ts.TargetTable] == nil {
				return nil, fmt.Errorf("table %s not found in vschema for keyspace %s", ts.TargetTable, ms.TargetKeyspace)
			}
		}
	}
	isPartial := false
	sourceShards, err := wr.sourceTs.GetServingShards(ctx, ms.SourceKeyspace)
	if err != nil {
		return nil, err
	}
	if len(ms.SourceShards) > 0 {
		isPartial = true
		var sourceShards2 []*topo.ShardInfo
		for _, shard := range sourceShards {
			for _, shard2 := range ms.SourceShards {
				if shard.ShardName() == shard2 {
					sourceShards2 = append(sourceShards2, shard)
					break
				}
			}
		}
		sourceShards = sourceShards2
	}
	if len(sourceShards) == 0 {
		return nil, fmt.Errorf("no source shards specified for workflow %s ", ms.Workflow)
	}

	targetShards, err := wr.ts.GetServingShards(ctx, ms.TargetKeyspace)
	if err != nil {
		return nil, err
	}
	if len(ms.SourceShards) > 0 {
		var targetShards2 []*topo.ShardInfo
		for _, shard := range targetShards {
			for _, shard2 := range ms.SourceShards {
				if shard.ShardName() == shard2 {
					targetShards2 = append(targetShards2, shard)
					break
				}
			}
		}
		targetShards = targetShards2
	}
	if len(targetShards) == 0 {
		return nil, fmt.Errorf("no target shards specified for workflow %s ", ms.Workflow)
	}

	return &materializer{
		wr:            wr,
		ms:            ms,
		targetVSchema: targetVSchema,
		sourceShards:  sourceShards,
		targetShards:  targetShards,
		isPartial:     isPartial,
	}, nil
}

func (mz *materializer) getSourceTableDDLs(ctx context.Context) (map[string]string, error) {
	sourceDDLs := make(map[string]string)
	allTables := []string{"/.*/"}

	sourcePrimary := mz.sourceShards[0].PrimaryAlias
	if sourcePrimary == nil {
		return nil, fmt.Errorf("source shard must have a primary for copying schema: %v", mz.sourceShards[0].ShardName())
	}

	ti, err := mz.wr.sourceTs.GetTablet(ctx, sourcePrimary)
	if err != nil {
		return nil, err
	}
	req := &tabletmanagerdatapb.GetSchemaRequest{Tables: allTables}
	sourceSchema, err := mz.wr.tmc.GetSchema(ctx, ti.Tablet, req)
	if err != nil {
		return nil, err
	}

	for _, td := range sourceSchema.TableDefinitions {
		sourceDDLs[td.Name] = td.Schema
	}
	return sourceDDLs, nil
}

func (mz *materializer) deploySchema(ctx context.Context) error {
	var sourceDDLs map[string]string
	var mu sync.Mutex

	return mz.forAllTargets(func(target *topo.ShardInfo) error {
		allTables := []string{"/.*/"}

		hasTargetTable := map[string]bool{}
		req := &tabletmanagerdatapb.GetSchemaRequest{Tables: allTables}
		targetSchema, err := schematools.GetSchema(ctx, mz.wr.ts, mz.wr.tmc, target.PrimaryAlias, req)
		if err != nil {
			return err
		}

		for _, td := range targetSchema.TableDefinitions {
			hasTargetTable[td.Name] = true
		}

		targetTablet, err := mz.wr.ts.GetTablet(ctx, target.PrimaryAlias)
		if err != nil {
			return err
		}

		var applyDDLs []string
		for _, ts := range mz.ms.TableSettings {
			if hasTargetTable[ts.TargetTable] {
				// Table already exists.
				continue
			}
			if ts.CreateDdl == "" {
				return fmt.Errorf("target table %v does not exist and there is no create ddl defined", ts.TargetTable)
			}

			var err error
			mu.Lock()
			if len(sourceDDLs) == 0 {
				//only get ddls for tables, once and lazily: if we need to copy the schema from source to target
				//we copy schemas from primaries on the source keyspace
				//and we have found use cases where user just has a replica (no primary) in the source keyspace
				sourceDDLs, err = mz.getSourceTableDDLs(ctx)
			}
			mu.Unlock()
			if err != nil {
				log.Errorf("Error getting DDLs of source tables: %s", err.Error())
				return err
			}

			createDDL := ts.CreateDdl
			if createDDL == createDDLAsCopy || createDDL == createDDLAsCopyDropConstraint || createDDL == createDDLAsCopyDropForeignKeys {
				if ts.SourceExpression != "" {
					// Check for table if non-empty SourceExpression.
					sourceTableName, err := sqlparser.TableFromStatement(ts.SourceExpression)
					if err != nil {
						return err
					}
					if sourceTableName.Name.String() != ts.TargetTable {
						return fmt.Errorf("source and target table names must match for copying schema: %v vs %v", sqlparser.String(sourceTableName), ts.TargetTable)

					}
				}

				ddl, ok := sourceDDLs[ts.TargetTable]
				if !ok {
					return fmt.Errorf("source table %v does not exist", ts.TargetTable)
				}

				if createDDL == createDDLAsCopyDropConstraint {
					strippedDDL, err := stripTableConstraints(ddl)
					if err != nil {
						return err
					}

					ddl = strippedDDL
				}

				if createDDL == createDDLAsCopyDropForeignKeys {
					strippedDDL, err := stripTableForeignKeys(ddl)
					if err != nil {
						return err
					}

					ddl = strippedDDL
				}
				createDDL = ddl
			}

			applyDDLs = append(applyDDLs, createDDL)
		}

		if len(applyDDLs) > 0 {
			sql := strings.Join(applyDDLs, ";\n")

			_, err = mz.wr.tmc.ApplySchema(ctx, targetTablet.Tablet, &tmutils.SchemaChange{
				SQL:              sql,
				Force:            false,
				AllowReplication: true,
				SQLMode:          vreplication.SQLMode,
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func stripTableForeignKeys(ddl string) (string, error) {

	ast, err := sqlparser.ParseStrictDDL(ddl)
	if err != nil {
		return "", err
	}

	stripFKConstraints := func(cursor *sqlparser.Cursor) bool {
		switch node := cursor.Node().(type) {
		case sqlparser.DDLStatement:
			if node.GetTableSpec() != nil {
				var noFKConstraints []*sqlparser.ConstraintDefinition
				for _, constraint := range node.GetTableSpec().Constraints {
					if constraint.Details != nil {
						if _, ok := constraint.Details.(*sqlparser.ForeignKeyDefinition); !ok {
							noFKConstraints = append(noFKConstraints, constraint)
						}
					}
				}
				node.GetTableSpec().Constraints = noFKConstraints
			}
		}
		return true
	}

	noFKConstraintAST := sqlparser.Rewrite(ast, stripFKConstraints, nil)
	newDDL := sqlparser.String(noFKConstraintAST)
	return newDDL, nil
}

func stripTableConstraints(ddl string) (string, error) {
	ast, err := sqlparser.ParseStrictDDL(ddl)
	if err != nil {
		return "", err
	}

	stripConstraints := func(cursor *sqlparser.Cursor) bool {
		switch node := cursor.Node().(type) {
		case sqlparser.DDLStatement:
			if node.GetTableSpec() != nil {
				node.GetTableSpec().Constraints = nil
			}
		}
		return true
	}

	noConstraintAST := sqlparser.Rewrite(ast, stripConstraints, nil)
	newDDL := sqlparser.String(noConstraintAST)

	return newDDL, nil
}

func (mz *materializer) generateInserts(ctx context.Context, targetShard *topo.ShardInfo) (string, error) {
	ig := vreplication.NewInsertGenerator(binlogplayer.BlpStopped, "{{.dbname}}")

	for _, sourceShard := range mz.sourceShards {
		// Don't create streams from sources which won't contain data for the target shard.
		// We only do it for MoveTables for now since this doesn't hold for materialize flows
		// where the target's sharding key might differ from that of the source
		if mz.ms.MaterializationIntent == vtctldatapb.MaterializationIntent_MOVETABLES &&
			!key.KeyRangesIntersect(sourceShard.KeyRange, targetShard.KeyRange) {
			continue
		}
		bls := &binlogdatapb.BinlogSource{
			Keyspace:        mz.ms.SourceKeyspace,
			Shard:           sourceShard.ShardName(),
			Filter:          &binlogdatapb.Filter{},
			StopAfterCopy:   mz.ms.StopAfterCopy,
			ExternalCluster: mz.ms.ExternalCluster,
			SourceTimeZone:  mz.ms.SourceTimeZone,
			TargetTimeZone:  mz.ms.TargetTimeZone,
		}
		for _, ts := range mz.ms.TableSettings {
			rule := &binlogdatapb.Rule{
				Match: ts.TargetTable,
			}

			if ts.SourceExpression == "" {
				bls.Filter.Rules = append(bls.Filter.Rules, rule)
				continue
			}

			// Validate non-empty query.
			stmt, err := sqlparser.Parse(ts.SourceExpression)
			if err != nil {
				return "", err
			}
			sel, ok := stmt.(*sqlparser.Select)
			if !ok {
				return "", fmt.Errorf("unrecognized statement: %s", ts.SourceExpression)
			}
			filter := ts.SourceExpression
			if mz.targetVSchema.Keyspace.Sharded && mz.targetVSchema.Tables[ts.TargetTable].Type != vindexes.TypeReference {
				cv, err := vindexes.FindBestColVindex(mz.targetVSchema.Tables[ts.TargetTable])
				if err != nil {
					return "", err
				}
				mappedCols := make([]*sqlparser.ColName, 0, len(cv.Columns))
				for _, col := range cv.Columns {
					colName, err := matchColInSelect(col, sel)
					if err != nil {
						return "", err
					}
					mappedCols = append(mappedCols, colName)
				}
				subExprs := make(sqlparser.SelectExprs, 0, len(mappedCols)+2)
				for _, mappedCol := range mappedCols {
					subExprs = append(subExprs, &sqlparser.AliasedExpr{Expr: mappedCol})
				}
				vindexName := fmt.Sprintf("%s.%s", mz.ms.TargetKeyspace, cv.Name)
				subExprs = append(subExprs, &sqlparser.AliasedExpr{Expr: sqlparser.NewStrLiteral(vindexName)})
				subExprs = append(subExprs, &sqlparser.AliasedExpr{Expr: sqlparser.NewStrLiteral("{{.keyrange}}")})
				inKeyRange := &sqlparser.FuncExpr{
					Name:  sqlparser.NewIdentifierCI("in_keyrange"),
					Exprs: subExprs,
				}
				if sel.Where != nil {
					sel.Where = &sqlparser.Where{
						Type: sqlparser.WhereClause,
						Expr: &sqlparser.AndExpr{
							Left:  inKeyRange,
							Right: sel.Where.Expr,
						},
					}
				} else {
					sel.Where = &sqlparser.Where{
						Type: sqlparser.WhereClause,
						Expr: inKeyRange,
					}
				}

				filter = sqlparser.String(sel)
			}

			rule.Filter = filter

			bls.Filter.Rules = append(bls.Filter.Rules, rule)
		}
		workflowSubType := binlogdatapb.VReplicationWorkflowSubType_None
		if mz.isPartial {
			workflowSubType = binlogdatapb.VReplicationWorkflowSubType_Partial
		}
		ig.AddRow(mz.ms.Workflow, bls, "", mz.ms.Cell, mz.ms.TabletTypes,
			int64(mz.ms.MaterializationIntent),
			int64(workflowSubType))
	}
	return ig.String(), nil
}

func matchColInSelect(col sqlparser.IdentifierCI, sel *sqlparser.Select) (*sqlparser.ColName, error) {
	for _, selExpr := range sel.SelectExprs {
		switch selExpr := selExpr.(type) {
		case *sqlparser.StarExpr:
			return &sqlparser.ColName{Name: col}, nil
		case *sqlparser.AliasedExpr:
			match := selExpr.As
			if match.IsEmpty() {
				if colExpr, ok := selExpr.Expr.(*sqlparser.ColName); ok {
					match = colExpr.Name
				} else {
					// Cannot match against a complex expression.
					continue
				}
			}
			if match.Equal(col) {
				colExpr, ok := selExpr.Expr.(*sqlparser.ColName)
				if !ok {
					return nil, fmt.Errorf("vindex column cannot be a complex expression: %v", sqlparser.String(selExpr))
				}
				return colExpr, nil
			}
		default:
			return nil, fmt.Errorf("unsupported select expression: %v", sqlparser.String(selExpr))
		}
	}
	return nil, fmt.Errorf("could not find vindex column %v", sqlparser.String(col))
}

func (mz *materializer) createStreams(ctx context.Context, insertsMap map[string]string) error {
	return mz.forAllTargets(func(target *topo.ShardInfo) error {
		inserts := insertsMap[target.ShardName()]
		targetPrimary, err := mz.wr.ts.GetTablet(ctx, target.PrimaryAlias)
		if err != nil {
			return vterrors.Wrapf(err, "GetTablet(%v) failed", target.PrimaryAlias)
		}
		buf := &strings.Builder{}
		t := template.Must(template.New("").Parse(inserts))
		input := map[string]string{
			"keyrange": key.KeyRangeString(target.KeyRange),
			"dbname":   targetPrimary.DbName(),
		}
		if err := t.Execute(buf, input); err != nil {
			return err
		}
		if _, err := mz.wr.TabletManagerClient().VReplicationExec(ctx, targetPrimary.Tablet, buf.String()); err != nil {
			return err
		}
		return nil
	})
}

func (mz *materializer) startStreams(ctx context.Context) error {
	return mz.forAllTargets(func(target *topo.ShardInfo) error {
		targetPrimary, err := mz.wr.ts.GetTablet(ctx, target.PrimaryAlias)
		if err != nil {
			return vterrors.Wrapf(err, "GetTablet(%v) failed", target.PrimaryAlias)
		}
		query := fmt.Sprintf("update _vt.vreplication set state='Running' where db_name=%s and workflow=%s", encodeString(targetPrimary.DbName()), encodeString(mz.ms.Workflow))
		if _, err := mz.wr.tmc.VReplicationExec(ctx, targetPrimary.Tablet, query); err != nil {
			return vterrors.Wrapf(err, "VReplicationExec(%v, %s)", targetPrimary.Tablet, query)
		}
		return nil
	})
}

func (mz *materializer) forAllTargets(f func(*topo.ShardInfo) error) error {
	var wg sync.WaitGroup
	allErrors := &concurrency.AllErrorRecorder{}
	for _, target := range mz.targetShards {
		wg.Add(1)
		go func(target *topo.ShardInfo) {
			defer wg.Done()

			if err := f(target); err != nil {
				allErrors.RecordError(err)
			}
		}(target)
	}
	wg.Wait()
	return allErrors.AggrError(vterrors.Aggregate)
}

// checkTZConversion is a light-weight consistency check to validate that, if a source time zone is specified to MoveTables,
// that the current primary has the time zone loaded in order to run the convert_tz() function used by VReplication to do the
// datetime conversions. We only check the current primaries on each shard and note here that it is possible a new primary
// gets elected: in this case user will either see errors during vreplication or vdiff will report mismatches.
func (mz *materializer) checkTZConversion(ctx context.Context, tz string) error {
	err := mz.forAllTargets(func(target *topo.ShardInfo) error {
		targetPrimary, err := mz.wr.ts.GetTablet(ctx, target.PrimaryAlias)
		if err != nil {
			return vterrors.Wrapf(err, "GetTablet(%v) failed", target.PrimaryAlias)
		}
		testDateTime := "2006-01-02 15:04:05"
		query := fmt.Sprintf("select convert_tz(%s, %s, 'UTC')", encodeString(testDateTime), encodeString(tz))
		qrproto, err := mz.wr.tmc.ExecuteFetchAsApp(ctx, targetPrimary.Tablet, false, &tabletmanagerdatapb.ExecuteFetchAsAppRequest{
			Query:   []byte(query),
			MaxRows: 1,
		})
		if err != nil {
			return vterrors.Wrapf(err, "ExecuteFetchAsApp(%v, %s)", targetPrimary.Tablet, query)
		}
		qr := sqltypes.Proto3ToResult(qrproto)
		if gotDate, err := time.Parse(testDateTime, qr.Rows[0][0].ToString()); err != nil {
			return fmt.Errorf("unable to perform time_zone conversions from %s to UTC — result of the attempt was: %s. Either the specified source time zone is invalid or the time zone tables have not been loaded on the %s tablet",
				tz, gotDate, targetPrimary.Alias)
		}
		return nil
	})
	return err
}
