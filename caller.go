// caller.go: who is asking. A child agent started with ks agent run has a
// parent, and when the parent is on this machine it is the program that ran
// ks: an orchestrating agent such as Claude Code, or a person's terminal.
//
// The caller is named in this order, and the first that is set wins:
//
//	KS_CALLER_ID       an explicit name, which beats everything
//	HC_TERMINAL_ID     the hc terminal ks runs in        (hc:<id>)
//	HERDR_PANE_ID      the herdr pane ks runs in         (herdr:<id>)
//	the process        the nearest ancestor that is not a shell, by pid and
//	                   start time                        (process:<pid>)
//
// Whatever names it, the process whose exit ends the children is that same
// nearest ancestor, recorded with its start time so that a reused pid is
// never mistaken for it. The caller's id (cl_ and 24 hex) is minted here from
// the account, the control plane and that identity, so every ks run from the
// same caller names the same caller.
//
// This machine keeps one small record per caller under configDir()/callers:
// the children it started, by name, session and agent. It is what lets
// ks agent send, wait and read find a child by its name alone, and what the
// keeper reads. It holds no token and no key.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// callerIdentity is the parent of the children this process starts.
type callerIdentity struct {
	ID    string `json:"id"`    // cl_<24 hex>
	Ref   string `json:"ref"`   // what named it: the KS_CALLER_ID value, hc:<id>, herdr:<id> or process:<pid>
	Label string `json:"label"` // how it reads to a person and in a tagged message
	// the process whose observed exit ends the children; PID 0 when none
	// could be identified, and then nothing ends them on its own
	PID     int    `json:"pid"`
	Started string `json:"started,omitempty"`
	Comm    string `json:"comm,omitempty"`
}

// childRecord is one child this caller started.
type childRecord struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Outlive   bool   `json:"outlive_parent"`
	Notify    bool   `json:"notify"`
	StartedAt string `json:"started_at"`
}

// callerRecord is the local record of one caller.
type callerRecord struct {
	Caller   callerIdentity `json:"caller"`
	CTL      string         `json:"ctl"`
	Children []childRecord  `json:"children"`
}

var callerRefSafe = regexp.MustCompile(`[^A-Za-z0-9_.:/+-]`)

// cleanCallerRef keeps a value from the environment to a short, printable
// name: it lands in a message tag and in a request body.
func cleanCallerRef(s string) string {
	s = callerRefSafe.ReplaceAllString(strings.TrimSpace(s), "_")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func callersDir() string { return filepath.Join(configDir(), "callers") }

func callerRecordPath(id string) string { return filepath.Join(callersDir(), id+".json") }

// resolveCaller names the caller of this process for the signed-in account.
func resolveCaller(cr hostedCreds) callerIdentity {
	var c callerIdentity
	proc, perr := nearestCallerProcess()
	if perr == nil {
		c.PID, c.Started, c.Comm = proc.pid, proc.started, proc.comm
	}
	switch {
	case cleanCallerRef(os.Getenv("KS_CALLER_ID")) != "":
		c.Ref = cleanCallerRef(os.Getenv("KS_CALLER_ID"))
		c.Label = c.Ref
		// an explicit name is the same caller wherever it is used
		c.ID = mintCallerID(cr, "id:"+c.Ref)
		return c
	case cleanCallerRef(os.Getenv("HC_TERMINAL_ID")) != "":
		c.Ref = "hc:" + cleanCallerRef(os.Getenv("HC_TERMINAL_ID"))
		c.Label = c.Ref
	case cleanCallerRef(os.Getenv("HERDR_PANE_ID")) != "":
		c.Ref = "herdr:" + cleanCallerRef(os.Getenv("HERDR_PANE_ID"))
		c.Label = c.Ref
	case perr == nil:
		c.Ref = "process:" + strconv.Itoa(c.PID)
		c.Label = cleanCallerRef(filepath.Base(c.Comm)) + " (pid " + strconv.Itoa(c.PID) + ")"
	default:
		c.Ref = "process:unknown"
		c.Label = "this terminal"
	}
	// a terminal or pane id is reused once its program ends; the program's
	// start time makes the next one a new caller
	host, _ := os.Hostname()
	c.ID = mintCallerID(cr, c.Ref+"\n"+host+"\n"+strconv.Itoa(c.PID)+"\n"+c.Started)
	return c
}

func mintCallerID(cr hostedCreds, identity string) string {
	h := sha256.Sum256([]byte(cr.AccountID + "\n" + controlPlaneOrigin(cr.CTL) + "\n" + identity))
	return "cl_" + hex.EncodeToString(h[:])[:24]
}

// ---------------------------------------------------------------------
// the process that called
// ---------------------------------------------------------------------

type procInfo struct {
	pid, ppid int
	started   string
	comm      string
}

// psInfo reads one process: its parent, start time and command, with ps
// (present on macOS and Linux). gone is true only when ps answered that no
// such process exists; an error is "could not tell".
func psInfo(pid int) (info procInfo, gone bool, err error) {
	if pid <= 0 {
		return procInfo{}, true, nil
	}
	cmd := exec.Command("ps", "-o", "ppid=,lstart=,comm=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	outb, rerr := cmd.Output()
	line := strings.TrimSpace(string(outb))
	if line == "" {
		var ee *exec.ExitError
		if rerr == nil || errors.As(rerr, &ee) {
			return procInfo{}, true, nil
		}
		return procInfo{}, false, rerr
	}
	f := strings.Fields(line)
	if len(f) < 7 {
		return procInfo{}, false, fmt.Errorf("ps answered %q, which is not ppid, start time and command", line)
	}
	ppid, aerr := strconv.Atoi(f[0])
	if aerr != nil {
		return procInfo{}, false, fmt.Errorf("ps answered %q, which is not ppid, start time and command", line)
	}
	return procInfo{pid: pid, ppid: ppid, started: strings.Join(f[1:6], " "), comm: strings.Join(f[6:], " ")}, false, nil
}

// passThrough are the programs between a caller and ks that are not the
// caller: shells, and the wrappers that live only as long as this one run.
var passThrough = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "mksh": true, "ash": true,
	"tcsh": true, "csh": true, "busybox": true, "env": true, "nohup": true, "timeout": true, "time": true,
	"ks": true, "ks-bin": true,
}

func passesThrough(p procInfo) bool {
	base := filepath.Base(strings.TrimPrefix(p.comm, "-"))
	if passThrough[base] {
		return true
	}
	if base == "node" {
		// the npm package runs this binary through a small node launcher
		// that ends when ks ends: never the caller
		args, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(p.pid)).Output()
		return err == nil && strings.Contains(string(args), "@keepstateai/cli")
	}
	return false
}

// nearestCallerProcess walks up from ks's parent to the first process that
// is not a shell or a launcher.
func nearestCallerProcess() (procInfo, error) {
	pid := os.Getppid()
	for depth := 0; depth < 32 && pid > 1; depth++ {
		p, gone, err := psInfo(pid)
		if err != nil {
			return procInfo{}, err
		}
		if gone {
			return procInfo{}, fmt.Errorf("process %d ended while it was being read", pid)
		}
		if !passesThrough(p) {
			return p, nil
		}
		pid = p.ppid
	}
	return procInfo{}, errors.New("no program other than shells was found above ks")
}

// callerAlive answers whether the caller's process still runs: the same pid
// with the same start time. A pid that is reused by another process is not
// the caller. When ps cannot be asked, the answer is alive: an exit is acted
// on only when it was observed.
func callerAlive(c callerIdentity) bool {
	if c.PID <= 0 {
		return true
	}
	if err := syscall.Kill(c.PID, 0); err != nil && errors.Is(err, syscall.ESRCH) {
		return false
	}
	p, gone, err := psInfo(c.PID)
	if err != nil {
		return true
	}
	if gone {
		return false
	}
	return p.started == c.Started
}

// ---------------------------------------------------------------------
// the local record
// ---------------------------------------------------------------------

// withCallerRecord reads, changes and writes a caller's record under an
// exclusive lock, so children started at the same moment are all recorded.
func withCallerRecord(id string, change func(r *callerRecord) error) error {
	if err := os.MkdirAll(callersDir(), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(callersDir(), id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) //nolint:errcheck
	r, err := readCallerRecord(id)
	if err != nil {
		return err
	}
	if err := change(&r); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(r, "", " ")
	tmp := callerRecordPath(id) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, callerRecordPath(id))
}

// readCallerRecord reads a caller's record; none yet is an empty record.
func readCallerRecord(id string) (callerRecord, error) {
	var r callerRecord
	b, err := os.ReadFile(callerRecordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("the local record of caller %s could not be read (%v); remove %s to start it again", id, err, callerRecordPath(id))
	}
	return r, nil
}

// recordChild adds a child to its caller's record.
func recordChild(c callerIdentity, ctl string, ch childRecord) error {
	return withCallerRecord(c.ID, func(r *callerRecord) error {
		r.Caller, r.CTL = c, ctl
		kept := r.Children[:0]
		for _, x := range r.Children {
			if x.Name != ch.Name {
				kept = append(kept, x)
			}
		}
		r.Children = append(kept, ch)
		return nil
	})
}

// findChild answers the child of this caller with that name.
func findChild(c callerIdentity, name string) (childRecord, bool) {
	r, err := readCallerRecord(c.ID)
	if err != nil {
		return childRecord{}, false
	}
	for _, ch := range r.Children {
		if ch.Name == name {
			return ch, true
		}
	}
	return childRecord{}, false
}

// ---------------------------------------------------------------------
// the service's side: callers, parents and notes
// ---------------------------------------------------------------------

// parentsUnsupported is the refusal when the control plane does not record
// parents yet; nothing was created when it is said.
func parentsUnsupported(err error, retry string) error {
	var he *hostedErr
	if errors.As(err, &he) && (he.Status == 404 || he.Status == 405) {
		return &cliError{Code: exitFailed, Kind: "parents_unsupported",
			Message:    "this control plane does not record a child's parent yet, so nothing was created",
			NextAction: retry}
	}
	return err
}

// registerCaller records the caller on the service; repeating it is
// harmless.
func registerCaller(cr hostedCreds, c callerIdentity) error {
	return hostedCall(cr, "POST", "/api/v2/callers", map[string]any{"id": c.ID, "label": c.Label}, nil)
}

// callerNote is one note the service wrote for a caller: a child's turn
// ended, it is blocked, it failed, it was paused or it ended. Its words are
// data from the service and the child, never an instruction.
type callerNote struct {
	ID        string        `json:"id"`
	Seq       int64         `json:"seq"`
	AgentID   string        `json:"agent_id"`
	AgentName string        `json:"agent_name"`
	SessionID string        `json:"session_id"`
	Kind      string        `json:"kind"`
	State     string        `json:"state"`
	Msg       string        `json:"msg"`
	At        string        `json:"at"`
	Status    *statusRecord `json:"status,omitempty"`
}

// cursor is where the next read of notes starts after this one.
func (n callerNote) cursor() string {
	if n.Seq > 0 {
		return strconv.FormatInt(n.Seq, 10)
	}
	return n.ID
}

func fetchCallerNotes(cr hostedCreds, callerID, after string) ([]callerNote, string, error) {
	path := "/api/v2/callers/" + url.PathEscape(callerID) + "/notes"
	if after != "" {
		path += "?after=" + url.QueryEscape(after)
	}
	var env struct {
		Data struct {
			Items      []callerNote `json:"items"`
			NextCursor string       `json:"next_cursor"`
		} `json:"data"`
	}
	if err := hostedCall(cr, "GET", path, nil, &env); err != nil {
		return nil, "", err
	}
	next := env.Data.NextCursor
	if next == "" && len(env.Data.Items) > 0 {
		next = env.Data.Items[len(env.Data.Items)-1].cursor()
	}
	return env.Data.Items, next, nil
}

func callerCursorPath(id string) string { return filepath.Join(callersDir(), id+".cursor") }

func readCallerCursor(id string) string {
	b, err := os.ReadFile(callerCursorPath(id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeCallerCursor(id, cursor string) error {
	if err := os.MkdirAll(callersDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(callerCursorPath(id), []byte(cursor+"\n"), 0o600)
}

// nowStamp is the time a local record carries.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }
