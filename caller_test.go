// Who the caller is: the order of the names, the id minted from them, the
// process found above the shells, a reused pid told from the caller, and a
// record that keeps every child started at the same moment.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"testing"
)

func TestCallerNamesInOrder(t *testing.T) {
	cr := hostedCreds{AccountID: "acct_t", CTL: "https://ctl.example"}
	shape := regexp.MustCompile(`^cl_[0-9a-f]{24}$`)
	t.Setenv("KS_CALLER_ID", "planner-1")
	t.Setenv("HC_TERMINAL_ID", "t7")
	t.Setenv("HERDR_PANE_ID", "p3")
	a := resolveCaller(cr)
	if a.Ref != "planner-1" || a.Label != "planner-1" || !shape.MatchString(a.ID) || resolveCaller(cr).ID != a.ID {
		t.Fatalf("KS_CALLER_ID first, and the same id each time: %+v", a)
	}
	if other := resolveCaller(hostedCreds{AccountID: "acct_other", CTL: cr.CTL}); other.ID == a.ID {
		t.Fatal("another account's caller has the same id")
	}
	t.Setenv("KS_CALLER_ID", "")
	if h := resolveCaller(cr); h.Ref != "hc:t7" || h.ID == a.ID || !shape.MatchString(h.ID) {
		t.Fatalf("then hc: %+v", h)
	}
	t.Setenv("HC_TERMINAL_ID", "")
	if h := resolveCaller(cr); h.Ref != "herdr:p3" {
		t.Fatalf("then herdr: %+v", h)
	}
	t.Setenv("HERDR_PANE_ID", "")
	p := resolveCaller(cr)
	if p.Ref != fmt.Sprintf("process:%d", p.PID) || p.PID <= 1 || p.Started == "" {
		t.Fatalf("then the process: %+v", p)
	}
	t.Setenv("KS_CALLER_ID", `a"b<c> d`)
	if got := resolveCaller(cr).Label; got != "a_b_c__d" {
		t.Fatalf("an unsafe caller name is kept as %q", got)
	}
}

func TestShellsPassThrough(t *testing.T) {
	for comm, want := range map[string]bool{"zsh": true, "-bash": true, "/bin/sh": true, "/usr/bin/env": true, "ks": true, "claude": false, "Terminal": false, "codex": false} {
		if got := passesThrough(procInfo{pid: 1, comm: comm}); got != want {
			t.Errorf("%s passes through: %v, want %v", comm, got, want)
		}
	}
}

func TestCallerAliveIsThePidAndItsStartTime(t *testing.T) {
	me, gone, err := psInfo(os.Getpid())
	if err != nil || gone || me.started == "" {
		t.Fatalf("ps on this process: %+v %v %v", me, gone, err)
	}
	if !callerAlive(callerIdentity{PID: os.Getpid(), Started: me.started}) {
		t.Fatal("this process is not alive")
	}
	if callerAlive(callerIdentity{PID: os.Getpid(), Started: "Mon Jan 1 00:00:00 2001"}) {
		t.Fatal("a pid with another start time was taken for the caller")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p, _, _ := psInfo(cmd.Process.Pid)
	c := callerIdentity{PID: cmd.Process.Pid, Started: p.started}
	if !callerAlive(c) {
		t.Fatal("a running process reads as gone")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if callerAlive(c) {
		t.Fatal("an exited process reads as alive")
	}
	if !callerAlive(callerIdentity{}) {
		t.Fatal("no process to watch must read alive: nothing ends unless an exit is observed")
	}
}

func TestCallerRecordKeepsEveryChild(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := callerIdentity{ID: "cl_000000000000000000000001", Label: "x"}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := recordChild(c, "https://ctl.example", childRecord{Name: fmt.Sprintf("kid%d", i), SessionID: fmt.Sprint("s", i), AgentID: fmt.Sprint("a", i)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	r, err := readCallerRecord(c.ID)
	if err != nil || len(r.Children) != 12 {
		t.Fatalf("children recorded at the same moment: %d %v", len(r.Children), err)
	}
	if ch, ok := findChild(c, "kid7"); !ok || ch.SessionID != "s7" {
		t.Fatalf("findChild: %+v %v", ch, ok)
	}
}
