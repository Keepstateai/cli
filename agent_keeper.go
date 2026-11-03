// agent_keeper.go: the keeper, so that a child ends with its parent.
//
// ks agent run starts one keeper per caller, detached from the terminal
// (its own session, no input or output). It is not a command for people.
// Every 2 seconds it checks that the caller's process still runs with the
// same start time. When the caller has exited, it asks the service once to
// end the caller (POST /api/v2/callers/{id}/end): the service saves and
// pauses each running child that would end with it. Nothing is deleted, and
// a child started with --outlive-parent is not touched.
//
// Only an exit the keeper observed ends anything. If the keeper itself dies,
// the laptop sleeps or the network is gone, nothing ends: the children keep
// running and their parent can still be ended by hand. The keeper exits on
// its own once no child that would end remains.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

var callerIDShape = regexp.MustCompile(`^cl_[0-9a-f]{24}$`)

func keeperPoll() time.Duration {
	if v := os.Getenv("KS_KEEPER_POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 2 * time.Second
}

func keeperLockPath(id string) string { return filepath.Join(callersDir(), id+".keeper.lock") }

// keeperRunning answers whether a keeper for this caller holds its lock.
func keeperRunning(id string) bool {
	if err := os.MkdirAll(callersDir(), 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(keeperLockPath(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// ensureKeeper starts the caller's keeper unless one already runs. Two runs
// at the same moment may both start one; the second finds the lock held
// and exits at once.
func ensureKeeper(id string) error {
	if keeperRunning(id) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "agent", "keeper", "--caller", id)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func keeperLog(id, format string, a ...any) {
	f, err := os.OpenFile(filepath.Join(callersDir(), id+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", nowStamp(), fmt.Sprintf(format, a...))
}

func runAgentKeeper(inv *Invocation) {
	id := inv.Str("caller")
	if !callerIDShape.MatchString(id) {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "the keeper is started by ks agent run with --caller cl_<24 hex>; it is not a command for people"})
	}
	cr, ok := hostedToken()
	if !ok {
		fail(&cliError{Code: exitAuth, Kind: "not_signed_in", Message: "Not signed in. Run: ks login", NextAction: "ks login"})
	}
	if err := os.MkdirAll(callersDir(), 0o700); err != nil {
		die(err)
	}
	lf, err := os.OpenFile(keeperLockPath(id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		die(err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return // another keeper watches this caller
	}
	rec, err := readCallerRecord(id)
	if err != nil {
		keeperLog(id, "the record could not be read (%v); nothing is watched", err)
		return
	}
	if rec.Caller.PID <= 0 {
		keeperLog(id, "no process was identified for %s; nothing ends its children on its own", rec.Caller.Label)
		return
	}
	keeperLog(id, "watching %s: pid %d started %s", rec.Caller.Label, rec.Caller.PID, rec.Caller.Started)
	poll := keeperPoll()
	pruneEvery := 30 * poll
	lastPrune := time.Now()
	for {
		if !callerAlive(rec.Caller) {
			keeperLog(id, "%s exited; asking the service to end it", rec.Caller.Label)
			if err := endCaller(cr, id, poll); err != nil {
				keeperLog(id, "the end was not recorded (%s); the children keep running", errText(err))
			} else {
				keeperLog(id, "ended: its children that would end are saved and paused, never deleted")
			}
			return
		}
		if time.Since(lastPrune) >= pruneEvery {
			lastPrune = time.Now()
			if remaining := pruneChildren(cr, id); remaining == 0 {
				keeperLog(id, "no child that would end remains; the keeper stops")
				return
			}
		}
		time.Sleep(poll)
		if r, rerr := readCallerRecord(id); rerr == nil && r.Caller.ID != "" {
			rec = r
		}
	}
}

// pruneChildren drops from the record the children the service says are
// gone, and answers how many children that would end remain.
func pruneChildren(cr hostedCreds, id string) int {
	remaining := 0
	_ = withCallerRecord(id, func(r *callerRecord) error {
		kept := r.Children[:0]
		for _, ch := range r.Children {
			st, err := readAgentStatus(cr, ch.SessionID, ch.AgentID)
			if err == nil && st.State == "clear" {
				continue
			}
			kept = append(kept, ch)
			if !ch.Outlive {
				remaining++
			}
		}
		r.Children = kept
		return nil
	})
	return remaining
}

// endCaller asks the service once to end the caller. A lost answer is asked
// again under the same idempotency key, so the end is recorded once.
func endCaller(cr hostedCreds, id string, poll time.Duration) error {
	path := "/api/v2/callers/" + url.PathEscape(id) + "/end"
	body := []byte(`{"reason":"caller_exited"}`)
	key := newIdempotencyKey()
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(poll * time.Duration(1<<attempt))
		}
		resp, raw, err := doBounded(cr, "POST", path, map[string]string{"Idempotency-Key": key}, body)
		switch {
		case err != nil:
			last = err
			continue
		case resp.StatusCode/100 == 2:
			return nil
		case resp.StatusCode >= 500:
			last = hostedError("POST", path, resp, raw)
			continue
		}
		return hostedError("POST", path, resp, bytes.TrimSpace(raw))
	}
	if last == nil {
		last = errors.New("no answer")
	}
	return last
}
