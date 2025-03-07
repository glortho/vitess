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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"vitess.io/vitess/go/exit"
	"vitess.io/vitess/go/vt/grpccommon"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/helpers"

	// Include deprecation warnings for soon-to-be-unsupported flag invocations.
	_flag "vitess.io/vitess/go/internal/flag"
)

var (
	fromImplementation = flag.String("from_implementation", "", "topology implementation to copy data from")
	fromServerAddress  = flag.String("from_server", "", "topology server address to copy data from")
	fromRoot           = flag.String("from_root", "", "topology server root to copy data from")

	toImplementation = flag.String("to_implementation", "", "topology implementation to copy data to")
	toServerAddress  = flag.String("to_server", "", "topology server address to copy data to")
	toRoot           = flag.String("to_root", "", "topology server root to copy data to")

	compare             = flag.Bool("compare", false, "compares data between topologies")
	doKeyspaces         = flag.Bool("do-keyspaces", false, "copies the keyspace information")
	doShards            = flag.Bool("do-shards", false, "copies the shard information")
	doShardReplications = flag.Bool("do-shard-replications", false, "copies the shard replication information")
	doTablets           = flag.Bool("do-tablets", false, "copies the tablet information")
	doRoutingRules      = flag.Bool("do-routing-rules", false, "copies the routing rules")
)

func main() {
	defer exit.RecoverAll()
	defer logutil.Flush()

	fs := pflag.NewFlagSet("topo2topo", pflag.ExitOnError)
	grpccommon.RegisterFlags(fs)
	log.RegisterFlags(fs)
	logutil.RegisterFlags(fs)
	_flag.Parse(fs)
	args := _flag.Args()
	if len(args) != 0 {
		flag.Usage()
		log.Exitf("topo2topo doesn't take any parameter.")
	}

	fromTS, err := topo.OpenServer(*fromImplementation, *fromServerAddress, *fromRoot)
	if err != nil {
		log.Exitf("Cannot open 'from' topo %v: %v", *fromImplementation, err)
	}
	toTS, err := topo.OpenServer(*toImplementation, *toServerAddress, *toRoot)
	if err != nil {
		log.Exitf("Cannot open 'to' topo %v: %v", *toImplementation, err)
	}

	ctx := context.Background()

	if *compare {
		compareTopos(ctx, fromTS, toTS)
		return
	}
	copyTopos(ctx, fromTS, toTS)
}

func copyTopos(ctx context.Context, fromTS, toTS *topo.Server) {
	if *doKeyspaces {
		helpers.CopyKeyspaces(ctx, fromTS, toTS)
	}
	if *doShards {
		helpers.CopyShards(ctx, fromTS, toTS)
	}
	if *doShardReplications {
		helpers.CopyShardReplications(ctx, fromTS, toTS)
	}
	if *doTablets {
		helpers.CopyTablets(ctx, fromTS, toTS)
	}
	if *doRoutingRules {
		helpers.CopyRoutingRules(ctx, fromTS, toTS)
	}
}

func compareTopos(ctx context.Context, fromTS, toTS *topo.Server) {
	var err error
	if *doKeyspaces {
		err = helpers.CompareKeyspaces(ctx, fromTS, toTS)
		if err != nil {
			log.Exitf("Compare keyspaces failed: %v", err)
		}
	}
	if *doShards {
		err = helpers.CompareShards(ctx, fromTS, toTS)
		if err != nil {
			log.Exitf("Compare shards failed: %v", err)
		}
	}
	if *doShardReplications {
		err = helpers.CompareShardReplications(ctx, fromTS, toTS)
		if err != nil {
			log.Exitf("Compare shard replications failed: %v", err)
		}
	}
	if *doTablets {
		err = helpers.CompareTablets(ctx, fromTS, toTS)
		if err != nil {
			log.Exitf("Compare tablets failed: %v", err)
		}
	}
	if err == nil {
		fmt.Println("Topologies are in sync")
		os.Exit(0)
	}
}
