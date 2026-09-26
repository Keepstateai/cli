// inspect.go: looking inside a session without a shell (KS-058).
//
//	ks file list [path]   one directory of the session's workspace
//	ks file show <path>   one workspace file, bounded
//	ks agent logs         the session's setup log and its agent's log
//
// Every one is a READ through the control plane, which asks the session's
// machine as the record's owner. None takes a command: the caller's words
// are a relative path, a byte bound and an opaque cursor. The client refuses
// an absolute path or one that climbs (..) before it asks anything, and the
// service and the guest refuse them again, with links, host paths and
// secret files.
//
// Nothing here wakes a session: a session that is not running is reported
// as such (files) or read from what the service kept (logs), and following
// the logs ends, saying why, when the service says it ends.
//
// What reaches the terminal is safe to print: control and escape sequences
// in file content or log text are made VISIBLE (\x1b), never emitted, and
// log text is passed through the client's own redaction as well as the
// service's.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type fileEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	MTime    int64  `json:"mtime"`
	Redacted bool   `json:"redacted"`
}

type fileList struct {
	SessionID string      `json:"session_id"`
	Root      string      `json:"root"`
	Path      string      `json:"path"`
	IsDir     bool        `json:"is_dir"`
	Entries   []fileEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
	Total     int         `json:"total"`
}

type fileContent struct {
	SessionID      string `json:"session_id"`
	Path           string `json:"path"`
	Size           int64  `json:"size"`
	Returned       int64  `json:"returned"`
	Truncated      bool   `json:"truncated"`
	MTime          int64  `json:"mtime"`
	SHA256Returned string `json:"sha256_returned"`
	ContentB64     string `json:"content_b64"`
}

type logEntry struct {
	Source string `json:"source"`
	Time   string `json:"time,omitempty"`
	Level  string `json:"level,omitempty"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
}

type logsPage struct {
	SessionID    string     `json:"session_id"`
	RuntimeState string     `json:"runtime_state"`
	Entries      []logEntry `json:"entries"`
	NextCursor   string     `json:"next_cursor"`
	AgentLog     struct {
		Status      string `json:"status"`
		Detail      string `json:"detail,omitempty"`
		Size        int64  `json:"size,omitempty"`
		LastWritten string `json:"last_written,omitempty"`
		Stale       string `json:"stale,omitempty"`
	} `json:"agent_log"`
	Gaps   []string `json:"gaps"`
	Follow struct {
		Ends bool   `json:"ends"`
		Why  string `json:"why"`
	} `json:"follow"`
}

// fileShowMax is the C04 bound for one read: 1 MiB.
const fileShowMax = 1 << 20

// workspacePath refuses, before any request, what can never be a workspace
// path: an absolute path, a drive, a backslash, a NUL, a climb.
func workspacePath(p string) string {
	if p == "" || p == "." {
		return "."
	}
	bad := strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.ContainsAny(p, "\\\x00") || (len(p) > 1 && p[1] == ':')
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			bad = true
		}
	}
	if bad {
		fail(&cliError{Code: exitIntegrity, Kind: "path_refused",
			Message: fmt.Sprintf("%q is not a path inside the workspace: paths are relative, without .., and never a host path; nothing was read", sanitize(p))})
	}
	return p
}

// visible makes control characters and escape sequences visible instead of
// letting the terminal act on them; newlines and tabs stay.
func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// inspectRefusal names what an inspection refusal means, with the exit the
// C03 table gives it.
func inspectRefusal(err error, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "ks_path_refused":
		ce.Code, ce.NextAction = exitIntegrity, ""
	case "ks_path_redacted":
		ce.Code, ce.NextAction = exitIntegrity, ""
		ce.Message += " (a secret file's contents are never returned)"
	case "ks_path_absent":
		ce.Code, ce.NextAction = exitFailed, "ks file list --session "+sess.ShortID
	case "ks_session_not_running":
		ce.Code = exitConflict
		ce.NextAction = "ks agent resume main --session " + sess.ShortID + " (inspection never wakes a session)"
	case "ks_no_runtime":
		ce.Code, ce.NextAction = exitFailed, "ks session show "+sess.ShortID
	}
	return ce
}

func inspectBase(sess inventoryRow) string {
	return "/api/v2/sessions/" + url.PathEscape(agentSessionID(sess))
}

func hostedFileList(cr hostedCreds, inv *Invocation) {
	p := workspacePath(inv.Arg(0))
	sess := agentSession(cr, inv)
	var env struct {
		Data fileList `json:"data"`
	}
	if err := hostedCall(cr, "GET", inspectBase(sess)+"/files?"+url.Values{"path": {p}}.Encode(), nil, &env); err != nil {
		die(inspectRefusal(err, sess))
	}
	l := env.Data
	emit(l, func() {
		fmt.Printf("%s (session %s)\n", visible(l.Path), sess.ShortID)
		for _, e := range l.Entries {
			name := visible(e.Name)
			if e.Type == "dir" {
				name += "/"
			}
			note := ""
			if e.Redacted {
				note = "  [redacted: its contents are never shown]"
			}
			fmt.Printf("  %-5s %-6s %12s  %s%s\n", visible(e.Type), visible(e.Mode), commas(e.Size), name, note)
		}
		if l.Truncated {
			fmt.Printf("showing %d of %d entries; the list was cut at the service's bound\n", len(l.Entries), l.Total)
		}
	})
}

func hostedFileShow(cr hostedCreds, inv *Invocation) {
	p := workspacePath(inv.Arg(0))
	if p == "." {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "name a file; ks file list shows a directory"})
	}
	max := int64(fileShowMax)
	if inv.Set("max-bytes") {
		max = inv.Int("max-bytes")
		if max > fileShowMax {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--max-bytes is at most %s (1 MiB)", commas(fileShowMax))})
		}
	}
	if o := inv.Str("out"); o != "" {
		if err := checkTarget(o, false); err != nil {
			die(err)
		}
	}
	sess := agentSession(cr, inv)
	var env struct {
		Data fileContent `json:"data"`
	}
	q := url.Values{"path": {p}, "max_bytes": {fmt.Sprint(max)}}
	if err := hostedCall(cr, "GET", inspectBase(sess)+"/files/content?"+q.Encode(), nil, &env); err != nil {
		die(inspectRefusal(err, sess))
	}
	c := env.Data
	raw, err := base64.StdEncoding.DecodeString(c.ContentB64)
	if err != nil {
		fail(integrity("content_unreadable", "the file's bytes did not arrive as base64; nothing is shown"))
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != c.Returned || (c.SHA256Returned != "" && hex.EncodeToString(sum[:]) != strings.ToLower(c.SHA256Returned)) {
		fail(integrity("digest_mismatch", "the bytes received do not match the size and sha256 the service stated for them; nothing is shown"))
	}
	if o := inv.Str("out"); o != "" {
		f, err := os.OpenFile(o, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			die(&cliError{Code: exitConflict, Kind: "target_exists", Message: fmt.Sprintf("%s could not be created without replacing anything (%v); nothing was written", o, err)})
		}
		if _, err := f.Write(raw); err != nil {
			f.Close()
			os.Remove(o)
			die(err)
		}
		if err := f.Close(); err != nil {
			die(err)
		}
	}
	emit(c, func() {
		more := ""
		if c.Truncated {
			more = fmt.Sprintf("; the file is %s bytes and continues past this bound", commas(c.Size))
		}
		progress("%s (session %s): %s bytes shown, sha256 %s%s", visible(c.Path), sess.ShortID, commas(c.Returned), short(c.SHA256Returned), more)
		if o := inv.Str("out"); o != "" {
			fmt.Printf("wrote %s bytes to %s\n", commas(c.Returned), o)
			return
		}
		if !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
			fmt.Printf("%s is not text (%s bytes); it is not printed. Write it to a file: ks file show %s --session %s --out FILE\n", visible(c.Path), commas(c.Returned), p, sess.ShortID)
			return
		}
		fmt.Print(visible(string(raw)))
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			fmt.Println()
		}
		if c.Truncated {
			fmt.Printf("[cut at %s of %s bytes]\n", commas(c.Returned), commas(c.Size))
		}
	})
}

func logLine(e logEntry) string {
	parts := []string{}
	if e.Time != "" {
		parts = append(parts, e.Time)
	}
	parts = append(parts, e.Source)
	if e.Level != "" {
		parts = append(parts, e.Level)
	}
	if e.Kind != "" {
		parts = append(parts, e.Kind+":")
	}
	return visible(sanitize(strings.Join(parts, " ") + " " + e.Text))
}

func logStatusLines(p logsPage) []string {
	var out []string
	if p.AgentLog.Status != "" && p.AgentLog.Status != "read" {
		s := "agent log " + p.AgentLog.Status
		if p.AgentLog.Detail != "" {
			s += ": " + p.AgentLog.Detail
		}
		out = append(out, visible(sanitize(s)))
	}
	if p.AgentLog.Stale != "" {
		out = append(out, visible(sanitize("agent log stale: "+p.AgentLog.Stale)))
	}
	for _, g := range p.Gaps {
		out = append(out, visible(sanitize("gap: "+g)))
	}
	return out
}

func hostedAgentLogs(cr hostedCreds, inv *Invocation) {
	sess := agentSession(cr, inv)
	cursor := inv.Str("cursor")
	fetch := func(cursor string) (logsPage, error) {
		q := url.Values{}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if inv.Set("limit") {
			q.Set("limit", fmt.Sprint(inv.Int("limit")))
		}
		var env struct {
			Data logsPage `json:"data"`
		}
		path := inspectBase(sess) + "/logs"
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		err := hostedCall(cr, "GET", path, nil, &env)
		return env.Data, err
	}
	p, err := fetch(cursor)
	if err != nil {
		die(inspectRefusal(err, sess))
	}
	if !inv.Bool("follow") {
		emit(p, func() {
			fmt.Printf("logs of session %s (%s)\n", sess.ShortID, p.RuntimeState)
			for _, e := range p.Entries {
				fmt.Println(logLine(e))
			}
			for _, l := range logStatusLines(p) {
				fmt.Println(l)
			}
			if len(p.Entries) == 0 {
				fmt.Println("no log lines from this cursor")
			}
			if p.NextCursor != "" {
				fmt.Printf("continue: ks agent logs --session %s --cursor %s\n", sess.ShortID, p.NextCursor)
			}
		})
		return
	}
	// following: one line per entry (one JSON object per line under --json),
	// polling from the service's cursor; it ends when the service says so,
	// or locally on Ctrl-C, and never asks the session to wake
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	cadence := pollCadence(boundedWait())
	lastStatus := ""
	for {
		for _, e := range p.Entries {
			emitLine(map[string]any{"type": "log", "entry": e}, logLine(e))
		}
		if st := strings.Join(logStatusLines(p), "\n"); st != "" && st != lastStatus {
			emitLine(map[string]any{"type": "status", "agent_log": p.AgentLog, "gaps": p.Gaps}, st)
			lastStatus = st
		}
		if p.NextCursor != "" {
			cursor = p.NextCursor
		}
		if p.Follow.Ends {
			why := p.Follow.Why
			if why == "" {
				why = "the service says there is nothing more to follow"
			}
			emitLine(map[string]any{"type": "end", "why": why, "runtime_state": p.RuntimeState, "cursor": cursor},
				visible(sanitize(fmt.Sprintf("following ended (session %s): %s", p.RuntimeState, why))))
			return
		}
		select {
		case <-stop:
			emitLine(map[string]any{"type": "detached", "cursor": cursor}, "stopped following; nothing on the session changed. Continue: ks agent logs --follow --session "+sess.ShortID+" --cursor "+cursor)
			return
		case <-time.After(cadence):
		}
		next, err := fetch(cursor)
		if err != nil {
			ce := classify(inspectRefusal(err, sess))
			ce.Message = "following stopped: " + ce.Message
			ce.NextAction = "ks agent logs --follow --session " + sess.ShortID + " --cursor " + cursor
			die(ce)
		}
		p = next
	}
}
