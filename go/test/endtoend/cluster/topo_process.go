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

package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"

	"vitess.io/vitess/go/vt/log"
)

// TopoProcess is a generic handle for a running Topo service .
// It can be spawned manually
type TopoProcess struct {
	Name               string
	Binary             string
	DataDirectory      string
	LogDirectory       string
	ListenClientURL    string
	AdvertiseClientURL string
	Port               int
	Host               string
	VerifyURL          string
	PeerURL            string
	ZKPorts            string

	proc *exec.Cmd
	exit chan error
}

// Setup starts a new topo service
func (topo *TopoProcess) Setup(topoFlavor string, cluster *LocalProcessCluster) (err error) {
	switch topoFlavor {
	case "zk2":
		return topo.SetupZookeeper(cluster)
	case "consul":
		return topo.SetupConsul(cluster)
	default:
		// We still rely on the etcd v2 API for things like mkdir.
		// If this ENV var is not set then some tests may fail with etcd 3.4+
		// where the v2 API is disabled by default in both the client and server.
		os.Setenv("ETCDCTL_API", "2")
		return topo.SetupEtcd()
	}
}

// SetupEtcd spawns a new etcd service and initializes it with the defaults.
// The service is kept running in the background until TearDown() is called.
func (topo *TopoProcess) SetupEtcd() (err error) {
	topo.proc = exec.Command(
		topo.Binary,
		"--name", topo.Name,
		"--data-dir", topo.DataDirectory,
		"--listen-client-urls", topo.ListenClientURL,
		"--advertise-client-urls", topo.AdvertiseClientURL,
		"--initial-advertise-peer-urls", topo.PeerURL,
		"--listen-peer-urls", topo.PeerURL,
		"--initial-cluster", fmt.Sprintf("%s=%s", topo.Name, topo.PeerURL),
		"--enable-v2=true",
	)

	err = createDirectory(topo.DataDirectory, 0700)
	if err != nil && !os.IsExist(err) {
		return err
	}
	errFile, err := os.Create(path.Join(topo.DataDirectory, "topo-stderr.txt"))
	if err != nil {
		return err
	}

	topo.proc.Stderr = errFile

	topo.proc.Env = append(topo.proc.Env, os.Environ()...)

	log.Infof("Starting etcd with command: %v", strings.Join(topo.proc.Args, " "))

	err = topo.proc.Start()
	if err != nil {
		return
	}

	topo.exit = make(chan error)
	go func() {
		topo.exit <- topo.proc.Wait()
	}()

	timeout := time.Now().Add(60 * time.Second)
	for time.Now().Before(timeout) {
		if topo.IsHealthy() {
			return
		}
		select {
		case err := <-topo.exit:
			return fmt.Errorf("process '%s' exited prematurely (err: %s)", topo.Binary, err)
		default:
			time.Sleep(300 * time.Millisecond)
		}
	}

	return fmt.Errorf("process '%s' timed out after 60s (err: %s)", topo.Binary, <-topo.exit)
}

// SetupZookeeper spawns a new zookeeper topo service and initializes it with the defaults.
// The service is kept running in the background until TearDown() is called.
func (topo *TopoProcess) SetupZookeeper(cluster *LocalProcessCluster) (err error) {

	host, err := os.Hostname()
	if err != nil {
		return
	}

	topo.ZKPorts = fmt.Sprintf("%d:%d:%d", cluster.GetAndReservePort(), cluster.GetAndReservePort(), topo.Port)

	topo.proc = exec.Command(
		topo.Binary,
		"--log_dir", topo.LogDirectory,
		"--zk.cfg", fmt.Sprintf("1@%v:%s", host, topo.ZKPorts),
		"init",
	)

	errFile, _ := os.Create(path.Join(topo.DataDirectory, "topo-stderr.txt"))
	topo.proc.Stderr = errFile
	topo.proc.Env = append(topo.proc.Env, os.Environ()...)

	log.Infof("Starting zookeeper with args %v", strings.Join(topo.proc.Args, " "))
	err = topo.proc.Run()
	if err != nil {
		return
	}
	return
}

// ConsulConfigs are the configurations that are added the config files which are used by consul
type ConsulConfigs struct {
	Ports   PortsInfo `json:"ports"`
	DataDir string    `json:"data_dir"`
	LogFile string    `json:"log_file"`
}

// PortsInfo is the different ports used by consul
type PortsInfo struct {
	DNS     int `json:"dns"`
	HTTP    int `json:"http"`
	SerfLan int `json:"serf_lan"`
	SerfWan int `json:"serf_wan"`
	Server  int `json:"server"`
}

// SetupConsul spawns a new consul service and initializes it with the defaults.
// The service is kept running in the background until TearDown() is called.
func (topo *TopoProcess) SetupConsul(cluster *LocalProcessCluster) (err error) {

	topo.VerifyURL = fmt.Sprintf("http://%s:%d/v1/kv/?keys", topo.Host, topo.Port)

	_ = os.MkdirAll(topo.LogDirectory, os.ModePerm)
	_ = os.MkdirAll(topo.DataDirectory, os.ModePerm)

	configFile := path.Join(os.Getenv("VTDATAROOT"), "consul.json")

	logFile := path.Join(topo.LogDirectory, "/consul.log")
	_, _ = os.Create(logFile)

	var config []byte
	configs := ConsulConfigs{
		Ports: PortsInfo{
			DNS:     cluster.GetAndReservePort(),
			HTTP:    topo.Port,
			SerfLan: cluster.GetAndReservePort(),
			SerfWan: cluster.GetAndReservePort(),
			Server:  cluster.GetAndReservePort(),
		},
		DataDir: topo.DataDirectory,
		LogFile: logFile,
	}
	config, err = json.Marshal(configs)
	if err != nil {
		log.Error(err.Error())
		return
	}

	err = os.WriteFile(configFile, config, 0666)
	if err != nil {
		return
	}

	topo.proc = exec.Command(
		topo.Binary, "agent",
		"-server",
		"-ui",
		"-bootstrap-expect", "1",
		"-bind", "127.0.0.1",
		"-config-file", configFile,
	)

	errFile, _ := os.Create(path.Join(topo.DataDirectory, "topo-stderr.txt"))
	topo.proc.Stderr = errFile

	topo.proc.Env = append(topo.proc.Env, os.Environ()...)

	log.Errorf("Starting consul with args %v", strings.Join(topo.proc.Args, " "))
	err = topo.proc.Start()
	if err != nil {
		return
	}

	topo.exit = make(chan error)
	go func() {
		topo.exit <- topo.proc.Wait()
	}()

	timeout := time.Now().Add(60 * time.Second)
	for time.Now().Before(timeout) {
		if topo.IsHealthy() {
			return
		}
		select {
		case err := <-topo.exit:
			return fmt.Errorf("process '%s' exited prematurely (err: %s)", topo.Binary, err)
		default:
			time.Sleep(300 * time.Millisecond)
		}
	}

	return fmt.Errorf("process '%s' timed out after 60s (err: %s)", topo.Binary, <-topo.exit)
}

// TearDown shutdowns the running topo service
func (topo *TopoProcess) TearDown(Cell string, originalVtRoot string, currentRoot string, keepdata bool, topoFlavor string) error {

	if topoFlavor == "zk2" {
		cmd := "shutdown"
		if keepdata {
			cmd = "teardown"
		}
		topo.proc = exec.Command(
			topo.Binary,
			"--log_dir", topo.LogDirectory,
			"--zk.cfg", fmt.Sprintf("1@%v:%s", topo.Host, topo.ZKPorts),
			cmd,
		)

		err := topo.proc.Run()
		if err != nil {
			return err
		}
	} else {
		if topo.proc == nil || topo.exit == nil {
			return nil
		}

		if !(*keepData || keepdata) {
			topo.removeTopoDirectories(Cell)
		}

		// Attempt graceful shutdown with SIGTERM first
		_ = topo.proc.Process.Signal(syscall.SIGTERM)

		if !(*keepData || keepdata) {
			_ = os.RemoveAll(topo.DataDirectory)
			_ = os.RemoveAll(currentRoot)
			_ = os.Setenv("VTDATAROOT", originalVtRoot)
		}

		select {
		case <-topo.exit:
			topo.proc = nil
			return nil

		case <-time.After(10 * time.Second):
			topo.proc.Process.Kill()
			topo.proc = nil
			return <-topo.exit
		}
	}

	return nil
}

// IsHealthy function checks if topo server is up and running
func (topo *TopoProcess) IsHealthy() bool {
	resp, err := http.Get(topo.VerifyURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func (topo *TopoProcess) removeTopoDirectories(Cell string) {
	if err := topo.ManageTopoDir("rmdir", "/vitess/global"); err != nil {
		log.Errorf("Failed to remove global topo directory: %v", err)
	}
	if err := topo.ManageTopoDir("rmdir", "/vitess/"+Cell); err != nil {
		log.Errorf("Failed to remove local topo directory: %v", err)
	}
}

// ManageTopoDir creates global and zone in etcd2
func (topo *TopoProcess) ManageTopoDir(command string, directory string) (err error) {
	url := topo.VerifyURL + directory
	payload := strings.NewReader(`{"dir":"true"}`)
	if command == "mkdir" {
		req, _ := http.NewRequest("PUT", url, payload)
		req.Header.Add("content-type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
		}
		return err
	} else if command == "rmdir" {
		req, _ := http.NewRequest("DELETE", url+"?dir=true", payload)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
		}
		return err
	} else {
		return nil
	}
}

// TopoProcessInstance returns a TopoProcess handle for a etcd sevice,
// configured with the given Config.
// The process must be manually started by calling setup()
func TopoProcessInstance(port int, peerPort int, hostname string, flavor string, name string) *TopoProcess {
	binary := "etcd"
	if flavor == "zk2" {
		binary = "zkctl"
	}
	if flavor == "consul" {
		binary = "consul"
	}

	topo := &TopoProcess{
		Name:   name,
		Binary: binary,
		Port:   port,
		Host:   hostname,
	}

	topo.AdvertiseClientURL = fmt.Sprintf("http://%s:%d", topo.Host, topo.Port)
	topo.ListenClientURL = fmt.Sprintf("http://%s:%d", topo.Host, topo.Port)
	topo.DataDirectory = path.Join(os.Getenv("VTDATAROOT"), fmt.Sprintf("%s_%d", "topo", port))
	topo.LogDirectory = path.Join(os.Getenv("VTDATAROOT"), fmt.Sprintf("%s_%d", "topo", port), "logs")
	topo.VerifyURL = fmt.Sprintf("http://%s:%d/v2/keys", topo.Host, topo.Port)
	topo.PeerURL = fmt.Sprintf("http://%s:%d", hostname, peerPort)
	return topo
}
