// The keeper: an observed exit of the caller ends it once; a reused pid
// with another start time is not the caller; with no child left that would
// end, it stops without ending anything; and ks agent run starts it so a
// child ends when the program that started it exits.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCallerRecordT(t *testing.T, cfg string, r callerRecord) {
	t.Helper()
	dir := filepath.Join(cfg, "keepstate", "callers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	writeFileT(t, filepath.Join(dir, r.Caller.ID+".json"), string(b))
}

func startKeeper(t *testing.T, bin, cfg, id string) *exec.Cmd {
	t.Helper()
	k := exec.Command(bin, "agent", "keeper", "--caller", id)
	k.Env = append(os.Environ(), fastEnv(cfg)...)
	k.Env = append(k.Env, "KS_KEEPER_POLL_MS=50")
	if err := k.Start(); err != nil {
		t.Fatal(err)
	}
	return k
}

func waitExit(t *testing.T, k *exec.Cmd, within time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = k.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		_ = k.Process.Kill()
		<-done
		return false
	}
}

func keeperLogT(cfg, id string) string {
	b, _ := os.ReadFile(filepath.Join(cfg, "keepstate", "callers", id+".log"))
	return string(b)
}

func TestKeeperEndsTheCallerOnceWhenItExits(t *testing.T) {
	c := newInteropCtl()
	bin, cfg, _ := interopEnv(t, c)
	caller := exec.Command("sleep", "30")
	if err := caller.Start(); err != nil {
		t.Fatal(err)
	}
	p, _, _ := psInfo(caller.Process.Pid)
	id := "cl_00000000000000000000000a"
	writeCallerRecordT(t, cfg, callerRecord{Caller: callerIdentity{ID: id, Label: "sleep", PID: caller.Process.Pid, Started: p.started},
		Children: []childRecord{{Name: "kid", SessionID: "session_9", AgentID: "agent_9"}}})
	k := startKeeper(t, bin, cfg, id)
	time.Sleep(300 * time.Millisecond)
	if c.count("POST /api/v2/callers/"+id+"/end") != 0 {
		t.Fatal("the keeper ended a caller that is alive")
	}
	_ = caller.Process.Kill()
	_ = caller.Wait()
	if !waitExit(t, k, 10*time.Second) {
		t.Fatalf("the keeper did not stop after the caller exited\n%s", keeperLogT(cfg, id))
	}
	fixtureBarrier()
	if n := c.count("POST /api/v2/callers/" + id + "/end"); n != 1 {
		t.Fatalf("end was called %d times\n%s", n, keeperLogT(cfg, id))
	}
	if body := c.first("POST /api/v2/callers/" + id + "/end"); !strings.Contains(body, `"reason":"caller_exited"`) {
		t.Fatalf("end body: %s", body)
	}
}

func TestKeeperDoesNotTakeAReusedPidForTheCaller(t *testing.T) {
	c := newInteropCtl()
	bin, cfg, _ := interopEnv(t, c)
	id := "cl_00000000000000000000000b"
	// this test's own pid is alive, but its start time is not the caller's
	writeCallerRecordT(t, cfg, callerRecord{Caller: callerIdentity{ID: id, Label: "gone", PID: os.Getpid(), Started: "Mon Jan 1 00:00:00 2001"},
		Children: []childRecord{{Name: "kid", SessionID: "session_9", AgentID: "agent_9"}}})
	k := startKeeper(t, bin, cfg, id)
	if !waitExit(t, k, 10*time.Second) {
		t.Fatal("the keeper kept watching a pid that belongs to another process")
	}
	fixtureBarrier()
	if n := c.count("POST /api/v2/callers/" + id + "/end"); n != 1 {
		t.Fatalf("end was called %d times", n)
	}
}

func TestKeeperStopsWhenNoChildWouldEnd(t *testing.T) {
	c := newInteropCtl()
	c.status["agent_9"] = []string{"clear"}
	bin, cfg, _ := interopEnv(t, c)
	me, _, _ := psInfo(os.Getpid())
	id := "cl_00000000000000000000000c"
	writeCallerRecordT(t, cfg, callerRecord{Caller: callerIdentity{ID: id, Label: "me", PID: os.Getpid(), Started: me.started},
		Children: []childRecord{{Name: "kid", SessionID: "session_9", AgentID: "agent_9"}, {Name: "free", SessionID: "session_8", AgentID: "agent_8", Outlive: true}}})
	k := startKeeper(t, bin, cfg, id)
	if !waitExit(t, k, 15*time.Second) {
		t.Fatalf("the keeper did not stop with no child left that would end\n%s", keeperLogT(cfg, id))
	}
	fixtureBarrier()
	if n := c.count("POST /api/v2/callers/" + id + "/end"); n != 0 {
		t.Fatalf("end was called %d times for a caller that is alive", n)
	}
	// a second keeper for the same caller: one holds the lock, the other leaves
	k1 := startKeeper(t, bin, cfg, id)
	k2 := startKeeper(t, bin, cfg, id)
	defer func() { _ = k1.Process.Kill(); _ = k2.Process.Kill() }()
	_ = waitExit(t, k2, 3*time.Second)
}

func TestAgentRunStartsTheKeeperSoTheChildEndsWithItsCaller(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to stand in for a calling program")
	}
	c := newInteropCtl()
	bin, cfg, _ := interopEnv(t, c)
	// python3 stands in for the calling program: the nearest process above
	// ks that is not a shell, which exits once ks returns
	cmd := exec.Command(py, "-c", "import subprocess,sys; sys.exit(subprocess.call(sys.argv[1:]))", bin, "agent", "run", "--bg", "--name", "kid", "do it", "--json")
	cmd.Env = append(os.Environ(), fastEnv(cfg)...)
	cmd.Env = append(cmd.Env, "KS_KEEPER_POLL_MS=50", "KS_CALLER_ID=", "HC_TERMINAL_ID=", "HERDR_PANE_ID=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	d := jsonData(t, string(out))
	parent, _ := d["parent"].(map[string]any)
	id, _ := parent["ref"].(string)
	if !strings.HasPrefix(parent["label"].(string), "python") && !strings.HasPrefix(strings.ToLower(parent["label"].(string)), "python") {
		t.Fatalf("the caller is not the program above ks: %v", parent)
	}
	deadline := time.Now().Add(10 * time.Second)
	for c.count("POST /api/v2/callers/"+id+"/end") == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := c.count("POST /api/v2/callers/" + id + "/end"); n != 1 {
		t.Fatalf("the child's caller exited and end was called %d times\n%s", n, keeperLogT(cfg, id))
	}
}
