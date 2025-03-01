/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreedto in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"vitess.io/vitess/go/exit"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vtctld"
)

func init() {
	servenv.RegisterDefaultFlags()
	servenv.RegisterFlags()
	servenv.RegisterGRPCServerFlags()
	servenv.RegisterGRPCServerAuthFlags()
	servenv.RegisterServiceMapFlag()
}

// used at runtime by plug-ins
var (
	ts *topo.Server
)

func main() {
	servenv.ParseFlags("vtctld")
	servenv.Init()
	defer servenv.Close()

	ts = topo.Open()
	defer ts.Close()

	// Init the vtctld core
	err := vtctld.InitVtctld(ts)
	if err != nil {
		exit.Return(1)
	}

	// Register http debug/health
	vtctld.RegisterDebugHealthHandler(ts)

	// Start schema manager service.
	initSchema()

	// And run the server.
	servenv.RunDefault()
}
