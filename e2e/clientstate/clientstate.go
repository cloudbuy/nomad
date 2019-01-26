package clientstate

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/e2e/e2eutil"
	"github.com/hashicorp/nomad/e2e/execagent"
	"github.com/hashicorp/nomad/e2e/framework"
	"github.com/hashicorp/nomad/helper/discover"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/testutil"
)

func init() {
	framework.AddSuites(&framework.TestSuite{
		Component:   "clientstate",
		CanRunLocal: true,
		Cases: []framework.TestCase{
			&ClientStateTC{},
		},
	})
}

type ClientStateTC struct {
	framework.TC

	// bin is the path to Nomad binary
	bin string
}

func (tc *ClientStateTC) BeforeAll(f *framework.F) {
	bin, err := discover.NomadExecutable()
	f.NoError(err)
	tc.bin = bin
}

func getPID(client *api.Client, alloc *api.Allocation, path string) (int, error) {
	allocfs := client.AllocFS()
	r, err := allocfs.Cat(alloc, path, nil)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	out, err := ioutil.ReadAll(r)
	if err != nil {
		return 0, err
	}

	lines := bytes.SplitN(out, []byte{'\n'}, 2)
	if len(lines) != 2 {
		return 0, fmt.Errorf("expected 1 line not %q", string(out))
	}

	// Capture pid
	pid, err := strconv.Atoi(string(lines[0]))
	if err != nil {
		return 0, err
	}

	return pid, fmt.Errorf("invalid pid: %d", pid)
}

// TestClientState_Kill force kills Nomad agents and restarts them in a tight
// loop to assert Nomad is crash safe.
func (tc *ClientStateTC) TestClientState_Kill(f *framework.F) {
	t := f.T()
	t.Parallel()

	serverAgent, clientAgent, err := execagent.NewClientServerPair(tc.bin)
	f.NoError(err)

	serverOut := testlog.NewPrefixWriter(t, "SERVER: ")
	serverAgent.Cmd.Stdout = serverOut
	serverAgent.Cmd.Stderr = serverOut
	f.NoError(serverAgent.Start())
	defer serverAgent.Destroy()

	clientOut := testlog.NewPrefixWriter(t, "CLIENT: ")
	clientAgent.Cmd.Stdout = clientOut
	clientAgent.Cmd.Stderr = clientOut
	f.NoError(clientAgent.Start())
	defer clientAgent.Destroy()

	// Get a client for the server agent to use even while the client is
	// down.
	client, err := serverAgent.Client()
	f.NoError(err)

	jobID := "sleeper-" + uuid.Generate()[:8]
	allocs := e2eutil.RegisterAndWaitForAllocs(t, client, "clientstate/sleeper.nomad", jobID)
	f.Len(allocs, 1)

	alloc, _, err := client.Allocations().Info(allocs[0].ID, nil)
	f.NoError(err)

	defer func() {
		if _, _, err := client.Jobs().Deregister(jobID, false, nil); err != nil {
			t.Logf("error stopping job: %v", err)
		}

		testutil.WaitForResult(func() (bool, error) {
			sum, _, err := client.Jobs().Summary(jobID, nil)
			if err != nil {
				return false, err
			}
			if r := sum.Summary["sleeper"].Running; r > 0 {
				return false, fmt.Errorf("still running: %d", r)
			}
			return true, nil
		}, func(err error) {
			f.NoError(err)
		})

		//XXX Must use client agent for gc'ing allocs?
		clientAPI, err := clientAgent.Client()
		f.NoError(err)
		if err := clientAPI.Allocations().GC(alloc, nil); err != nil {
			t.Logf("error garbage collecting alloc: %v", err)
		}

		if err := client.System().GarbageCollect(); err != nil {
			t.Logf("error doing full gc: %v", err)
		}

		//HACK to wait until things have GC'd
		time.Sleep(time.Second)
	}()

	assertHealthy := func() {
		t.Helper()
		testutil.WaitForResult(func() (bool, error) {
			alloc, _, err = client.Allocations().Info(alloc.ID, nil)
			f.NoError(err) // should never error

			if len(alloc.TaskStates) == 0 {
				return false, fmt.Errorf("waiting for tasks to start")
			}

			if s := alloc.TaskStates["sleeper"].State; s != "running" {
				return false, fmt.Errorf("task should be running: %q", s)
			}

			// Restarts should never happen
			f.Zero(alloc.TaskStates["sleeper"].Restarts)
			return true, nil
		}, func(err error) {
			f.NoError(err)
		})
	}
	assertHealthy()

	// Find pid
	pid := 0
	testutil.WaitForResult(func() (bool, error) {
		pid, err = getPID(client, alloc, "sleeper/pid")
		return pid > 0, err
	}, func(err error) {
		f.NoError(err)
	})

	// Kill and restart a few times
	tries := 10
	for i := 0; i < tries; i++ {
		t.Logf("TEST RUN %d/%d", i+1, tries)

		// Kill -9 the Agent
		agentPid := clientAgent.Cmd.Process.Pid
		f.NoError(clientAgent.Cmd.Process.Signal(os.Kill))

		state, err := clientAgent.Cmd.Process.Wait()
		f.NoError(err)
		f.False(state.Exited()) // kill signal != exited
		f.False(state.Success())

		// Assert sleeper is still running
		f.NoError(syscall.Kill(pid, 0))
		assertHealthy()

		// Should not be able to reach its filesystem
		_, err = getPID(client, alloc, "sleeper/pid")
		f.Error(err)

		// Restart the agent (have to create a new Cmd)
		clientAgent.Cmd = exec.Command(clientAgent.BinPath, "agent",
			"-config", clientAgent.ConfFile,
			"-data-dir", clientAgent.DataDir,
			"-servers", fmt.Sprintf("127.0.0.1:%d", serverAgent.Vars.RPC),
		)
		clientAgent.Cmd.Stdout = clientOut
		clientAgent.Cmd.Stderr = clientOut
		f.NoError(clientAgent.Start())

		// Assert a new process did start
		f.NotEqual(clientAgent.Cmd.Process.Pid, agentPid)

		// Retrieving the pid should work once it restarts
		testutil.WaitForResult(func() (bool, error) {
			newPid, err := getPID(client, alloc, "sleeper/pid")
			return newPid == pid, err
		}, func(err error) {
			f.NoError(err)
		})

		// Alloc should still be running
		assertHealthy()
	}
}
