package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// ---- an upload server that is slow, counts, and can be made to fail ---------------------

type slowUploads struct {
	mu       sync.Mutex
	size     int64
	received int64
	acked    map[int64]bool
	puts     int
	delay    time.Duration
	chunk    int64
	state    string
}

func (s *slowUploads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	view := func() map[string]any {
		return map[string]any{"data": map[string]any{"upload_id": "upl_x", "session_id": "s", "kind": "conversation", "size": s.size, "received": s.received, "state": s.state, "chunk_bytes": s.chunk}}
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == "PUT":
		n, _ := io.Copy(io.Discard, r.Body)
		time.Sleep(s.delay)
		s.mu.Lock()
		defer s.mu.Unlock()
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if off != s.received || n != r.ContentLength {
			w.WriteHeader(409)
			reply(map[string]any{"error": map[string]any{"code": "ks_upload_offset", "type": "ks_upload_offset", "message": "different count"}})
			return
		}
		s.received += n
		s.puts++
		s.acked[s.received] = true
		reply(view())
	case strings.HasSuffix(r.URL.Path, "/complete"):
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state = "verified"
		reply(view())
	default:
		s.mu.Lock()
		defer s.mu.Unlock()
		reply(view())
	}
}

// uploadSparse sends a file of the given size (a sparse file: nothing is held
// in memory or on disk) through the real uploader and answers what the bar
// showed and what the server was told.
func uploadSparse(t *testing.T, size int64, delay time.Duration, quiet bool) ([]int64, *slowUploads, time.Duration) {
	t.Helper()
	srv := &slowUploads{size: size, acked: map[int64]bool{}, delay: delay, chunk: 16 << 20, state: "receiving"}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	h := sha256.New()
	rf, _ := os.Open(p)
	if _, err := io.Copy(h, rf); err != nil {
		t.Fatal(err)
	}
	rf.Close()
	save := out
	out = outputSettings{quiet: quiet}
	defer func() { out = save }()
	ev := &[]int64{}
	u := &uploader{ctx: context.Background(), cr: hostedCreds{CTL: ts.URL, Token: "t"}, inv: &Invocation{set: map[string]bool{}, strs: map[string]string{}},
		sess: inventoryRow{ID: "s", RecordID: "s"}, agent: "main", ackBar: true, barSeen: ev,
		plan: &uploadPlan{Kind: "file", Wire: "conversation", Name: "the conversation", Local: p, To: "claude-code/7c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e5f", Mode: "0644", Content: size, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}}
	start := time.Now()
	u.declare(false)
	u.send()
	took := time.Since(start)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return *ev, srv, took
}

func checkBarRun(t *testing.T, name string, size int64, seen []int64, srv *slowUploads) {
	t.Helper()
	if srv.received != size {
		t.Fatalf("%s: the server holds %d of %d bytes", name, srv.received, size)
	}
	if len(seen) == 0 {
		t.Fatalf("%s: the bar was told nothing", name)
	}
	var prev int64
	lastPct := -1
	for i, d := range seen {
		pct := int(d * 100 / size)
		if d < prev || d > size || pct < lastPct || pct > 100 {
			t.Fatalf("%s: step %d (%d bytes) is not monotonic or is over 100%%", name, i, d)
		}
		if !srv.acked[d] {
			t.Fatalf("%s: the bar was told %d bytes, which the server never acknowledged", name, d)
		}
		if pct == 100 && (d != size || !srv.acked[size]) {
			t.Fatalf("%s: 100%% before the server acknowledged the last byte (%d)", name, d)
		}
		prev, lastPct = d, pct
	}
	if seen[len(seen)-1] != size {
		t.Fatalf("%s: the last count is %d, the file is %d", name, seen[len(seen)-1], size)
	}
}

func TestUploadBarAtTheBoundaries(t *testing.T) {
	const chunk = 16 << 20
	for _, size := range []int64{1, chunk - 1, chunk, chunk + 1, 2*chunk - 1, 2 * chunk, 2*chunk + 1} {
		ev, srv, _ := uploadSparse(t, size, 0, true)
		checkBarRun(t, fmt.Sprintf("%d bytes", size), size, ev, srv)
		want := int((size + chunk - 1) / chunk)
		if srv.puts != want {
			t.Fatalf("%d bytes took %d chunks, want %d", size, srv.puts, want)
		}
	}
}

func TestUploadBarThrottled300MB(t *testing.T) {
	const size = 300_000_000
	var screen bytes.Buffer
	save1, save2 := uiOut, uiErr
	defer func() { uiOut, uiErr = save1, save2 }()
	th := detectTheme(func(string) string { return "" }, true, true)
	uiOut, uiErr = newUI(io.Discard, th, 100, true), newUI(&screen, th, 100, true)
	ev, srv, took := uploadSparse(t, size, 150*time.Millisecond, false)
	checkBarRun(t, "300 MB", size, ev, srv)
	text := ansiRe.ReplaceAllString(screen.String(), "")
	if !strings.Contains(text, "Uploaded the conversation") && !strings.Contains(text, "Uploading") {
		t.Fatalf("nothing drawn:\n%s", text)
	}
	// the rate and the time left on screen are near the true ones
	truth := float64(size) / took.Seconds()
	rates := regexp.MustCompile(`([0-9.]+) (KB|MB|GB)/s`).FindAllStringSubmatch(text, -1)
	if len(rates) == 0 {
		t.Fatalf("no rate was shown on a three second transfer:\n%s", text)
	}
	for _, m := range rates {
		v, _ := strconv.ParseFloat(m[1], 64)
		v *= map[string]float64{"KB": 1e3, "MB": 1e6, "GB": 1e9}[m[2]]
		if v < truth/4 || v > truth*4 {
			t.Fatalf("the screen says %s %s/s, the transfer ran at %.0f B/s", m[1], m[2], truth)
		}
	}
	if !strings.Contains(text, "300 MB of 300 MB") && !strings.Contains(text, "300 MB in") {
		t.Fatalf("the bytes shown are not the file's size:\n%s", text)
	}
}

// The 2 GB limit: a sparse file, so the disk is not used and memory stays at a chunk.
func TestUploadBarAtTheTwoGBLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(os.TempDir(), &fs); err != nil || int64(fs.Bavail)*int64(fs.Bsize) < 15<<30 {
		t.Skip("less than 15 GiB free")
	}
	ev, srv, _ := uploadSparse(t, importMaxBytes, 0, true)
	checkBarRun(t, "2 GB", importMaxBytes, ev, srv)
	if srv.puts != 120 { // 2,000,000,000 / 16 MiB, rounded up
		t.Fatalf("%d chunks", srv.puts)
	}
}

// ---- the whole command, with a slow or failing service ---------------------------------

// bigSession writes a Claude Code session of about mb megabytes of messages.
func (e *importEnv) bigSession(mb int) string {
	e.t.Helper()
	var b strings.Builder
	pad := strings.Repeat("x", 100_000)
	for i := 0; i < mb*10; i++ {
		fmt.Fprintf(&b, `{"type":"user","uuid":"u%d","parentUuid":null,"cwd":"/p","sessionId":%q,"timestamp":"2026-11-13T10:00:00.000Z","version":"2.1.312","message":{"role":"user","content":"%s"}}`+"\n", i, claudeSID, pad)
	}
	p := filepath.Join(e.home, ".claude", "projects", "-p", claudeSID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	return p
}

var pctLine = regexp.MustCompile(`(?m)^\s*(cleaning|uploading)[a-z' ]*: (\d+)% \(([^)]*)\)\s*$`)

func percents(errs, noun string) []int {
	var out []int
	for _, m := range pctLine.FindAllStringSubmatch(errs, -1) {
		if m[1] == noun {
			n, _ := strconv.Atoi(m[2])
			out = append(out, n)
		}
	}
	return out
}

func TestImportShowsTrueProgressInPlainLines(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(3)
	out, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || !strings.Contains(out, "Imported") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	for _, noun := range []string{"cleaning", "uploading"} {
		ps := percents(errs, noun)
		// a line for each tenth crossed (a chunk may cross two at once), the last at 100
		if len(ps) < 5 || ps[len(ps)-1] != 100 || ps[0] > 30 {
			t.Fatalf("%s: lines at the tenths, got %v\n%s", noun, ps, errs)
		}
		for i := 1; i < len(ps); i++ {
			if ps[i] <= ps[i-1] {
				t.Fatalf("%s is not monotonic: %v", noun, ps)
			}
		}
	}
	// the bytes in the last uploading line are the bytes the service holds
	u := e.c.upload("conversation")
	want := kitBytes(int64(len(u.Got)))
	if !strings.Contains(errs, "100% ("+want+" of "+want+")") {
		t.Fatalf("the last line does not say %s, the bytes the service holds:\n%s", want, errs)
	}
	if !strings.Contains(errs, "Placing the conversation on KeepState") || !strings.Contains(errs, "Placed the conversation") {
		t.Fatalf("placing is not said:\n%s", errs)
	}
}

func TestImportJSONShowsNoProgress(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(1)
	_, errs, code := e.run("import", "claude-code", "--latest", "--yes", "--json")
	if code != 0 || strings.Contains(errs, "%") || strings.Contains(errs, "cleaning") || strings.Contains(errs, "uploading") {
		t.Fatalf("--json prints progress: %d\n%s", code, errs)
	}
}

func TestImportResumeRestartsTheBarAtTheAcknowledgedOffset(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(3)
	e.c.failAfter = 4 // the service takes four chunks (1 MiB), then is unavailable
	_, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code == 0 || !strings.Contains(errs, "had arrived") || !strings.Contains(errs, "run the same command again") {
		t.Fatalf("the failed upload: %d\n%s", code, errs)
	}
	u := e.c.upload("conversation")
	acked := int64(len(u.Got))
	if acked != 4*256<<10 {
		t.Fatalf("the service holds %d bytes", acked)
	}
	first := percents(errs, "uploading")
	if len(first) == 0 || first[len(first)-1] >= 100 {
		t.Fatalf("lines before the failure: %v", first)
	}
	if !strings.Contains(errs, bytesText(acked)+" of ") {
		t.Fatalf("the stop does not say what arrived (%s):\n%s", bytesText(acked), errs)
	}
	e.c.failAfter = 0
	before := e.c.count("PUT ")
	out, errs2, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || !strings.Contains(out, "Imported") {
		t.Fatalf("the second run: %d\n%s\n%s", code, out, errs2)
	}
	size := int64(len(e.c.upload("conversation").Got))
	startPct := int(acked * 100 / size)
	second := percents(errs2, "uploading")
	if len(second) == 0 || second[0] <= startPct/10*10 || second[len(second)-1] != 100 {
		t.Fatalf("the resumed bar starts at %d%% and shows %v", startPct, second)
	}
	if want := int((size - acked + 256<<10 - 1) / (256 << 10)); e.c.count("PUT ")-before != want {
		t.Fatalf("%d chunks after the resume, want only the %d missing", e.c.count("PUT ")-before, want)
	}
}

func TestImportCtrlCLeavesAResumableUpload(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(3)
	e.c.putDelay = 250 * time.Millisecond
	cmd := exec.Command(e.bin, "import", "claude-code", "--latest", "--yes")
	cmd.Dir = e.dir
	cmd.Env = append(append(os.Environ(), "XDG_CONFIG_HOME="+e.cfg), e.env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for e.c.count("PUT ") < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Signal(os.Interrupt)
	err := cmd.Wait()
	ee, _ := err.(*exec.ExitError)
	if ee == nil || ee.ExitCode() != exitInterrupt {
		t.Fatalf("exit: %v\n%s", err, se.String())
	}
	got := se.String()
	held := int64(len(e.c.upload("conversation").Got))
	if !strings.Contains(got, "the upload was stopped") || !strings.Contains(got, bytesText(held)+" of ") || !strings.Contains(got, "run the same command again") || !strings.Contains(got, "Next: ks import") {
		t.Fatalf("the message (the service holds %d bytes):\n%s", held, got)
	}
	e.c.putDelay = 0
	out, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || !strings.Contains(out, "Imported") || !strings.Contains(errs, "100% (") {
		t.Fatalf("after Ctrl-C: %d\n%s\n%s", code, out, errs)
	}
}

func TestImportOfNothingIsRefusedInPlainWords(t *testing.T) {
	e := newImportEnv(t)
	line := `{"type":"queue-operation","operation":"enqueue","sessionId":"` + claudeSID + `","cwd":"/p","timestamp":"2026-11-13T10:00:00.000Z"}` + "\n"
	p := filepath.Join(e.home, ".claude", "projects", "-p", claudeSID+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(line), 0o600)
	_, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != exitFailed || !strings.Contains(errs, "holds no messages") || e.c.index("POST /api/v2/sessions") >= 0 {
		t.Fatalf("%d\n%s", code, errs)
	}
}
