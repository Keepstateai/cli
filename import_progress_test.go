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

// ---- the bar itself, on a clock the test holds -------------------------------------

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func testBar(total, already int64, mode string) (*xferBar, *bytes.Buffer, *fakeClock, *[]barEvent) {
	var buf bytes.Buffer
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	ev := &[]barEvent{}
	b := &xferBar{label: "Uploading", noun: "uploading", total: total, mode: mode, w: &buf, now: clk.now, lastDec: -1, scale: 1, events: ev}
	b.begin(already)
	return b, &buf, clk, ev
}

func TestBarLineHasTheSpecsShape(t *testing.T) {
	b, _, clk, _ := testBar(1_370_000_000, 0, "tty")
	for i := 0; i < 10; i++ { // 12 MB a second for ten seconds, then a look
		clk.t = clk.t.Add(time.Second)
		b.update(int64(i+1) * 12_000_000)
	}
	b.done = 880_000_000 // the count the line is asked about
	b.window = []barSample{{clk.t.Add(-10 * time.Second), 760_000_000}, {clk.t, 880_000_000}}
	line := b.line(false)
	for _, want := range []string{"Uploading ", "██████████", "░", " 64%", "880 MB of 1.37 GB", "12 MB/s", "left"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line %q lacks %q", line, want)
		}
	}
	if !strings.Contains(line, "41s left") { // (1.37 GB - 880 MB) / 12 MB/s
		t.Errorf("the time left is not what the rate says: %q", line)
	}
}

func TestBarNumbersAreTrue(t *testing.T) {
	const total = 100_000_000
	b, _, clk, ev := testBar(total, 0, "off")
	for done := int64(0); done < total; done += 3_000_000 {
		clk.t = clk.t.Add(250 * time.Millisecond)
		b.update(done)
		b.update(done / 2) // going backwards is ignored
	}
	if b.percent() >= 100 {
		t.Fatalf("100%% before the last byte was acknowledged: %d", b.percent())
	}
	b.update(total - 1)
	if b.percent() != 99 {
		t.Fatalf("one byte short is %d%%, not 99", b.percent())
	}
	b.finish()
	last := -1
	var prev int64
	for i, e := range *ev {
		if e.Percent < last || e.Done < prev || e.Percent > 100 || e.Done > e.Total {
			t.Fatalf("event %d is not monotonic or is over 100: %+v", i, e)
		}
		if e.Percent == 100 && !e.Final {
			t.Fatalf("100%% shown before the end: %+v", e)
		}
		if e.Rate < 0 || e.ETA < 0 {
			t.Fatalf("event %d: %+v", i, e)
		}
		last, prev = e.Percent, e.Done
	}
	if f := (*ev)[len(*ev)-1]; !f.Final || f.Percent != 100 || f.Done != total {
		t.Fatalf("the last event is %+v", f)
	}
}

func TestBarResumeStartsAtTheAcknowledgedOffset(t *testing.T) {
	b, buf, clk, _ := testBar(1000, 400, "plain")
	if b.percent() != 40 {
		t.Fatalf("a resume at 400 of 1000 starts at %d%%", b.percent())
	}
	clk.t = clk.t.Add(time.Second)
	b.update(450)
	b.update(1000)
	b.finish()
	if got := buf.String(); strings.Contains(got, "0%") && !strings.Contains(got, "100%") || strings.Contains(got, "uploading: 10%") || strings.Contains(got, "uploading: 30%") {
		t.Fatalf("a resumed bar shows what is already there as new:\n%s", got)
	}
}

func TestBarPlainPrintsOneLinePerTenPercent(t *testing.T) {
	b, buf, clk, _ := testBar(1000, 0, "plain")
	for d := int64(1); d <= 1000; d++ {
		clk.t = clk.t.Add(10 * time.Millisecond)
		b.update(d)
	}
	b.finish()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 10 {
		t.Fatalf("%d lines, want 10:\n%s", len(lines), buf.String())
	}
	for i, l := range lines {
		want := fmt.Sprintf("uploading: %d%% (", (i+1)*10)
		if !strings.HasPrefix(l, want) {
			t.Fatalf("line %d is %q, want it to start %q", i, l, want)
		}
	}
	if strings.ContainsAny(buf.String(), "\r\x1b") {
		t.Fatal("a plain bar redraws")
	}
	if !strings.HasSuffix(lines[9], "(1 KB of 1 KB)") {
		t.Fatalf("the last line: %q", lines[9])
	}
}

func TestBarIsSilentForJSONAndQuiet(t *testing.T) {
	save := out
	defer func() { out = save }()
	for _, o := range []outputSettings{{json: true}, {quiet: true}} {
		out = o
		if barMode() != "off" {
			t.Fatalf("%+v: mode %s", o, barMode())
		}
	}
	out = outputSettings{plain: true}
	if barMode() != "plain" {
		t.Fatalf("--plain: %s", barMode())
	}
}

func TestBarTTYLeavesItsLastLineOnScreen(t *testing.T) {
	b, buf, clk, _ := testBar(2_000_000, 0, "tty")
	clk.t = clk.t.Add(2 * time.Second)
	b.update(1_000_000)
	clk.t = clk.t.Add(2 * time.Second)
	b.update(2_000_000)
	b.finish()
	s := buf.String()
	if !strings.HasSuffix(s, "\n") || !strings.Contains(s, "100%") || !strings.Contains(s, "2.0 MB of 2.0 MB") {
		t.Fatalf("the last line is erased or wrong: %q", s)
	}
	if strings.HasSuffix(strings.TrimSuffix(s, "\n"), "\r\x1b[K") {
		t.Fatalf("the screen ends on an erase: %q", s)
	}
}

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
func uploadSparse(t *testing.T, size int64, delay time.Duration) ([]barEvent, *slowUploads, time.Duration) {
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
	out = outputSettings{quiet: true}
	defer func() { out = save }()
	ev := &[]barEvent{}
	u := &uploader{ctx: context.Background(), cr: hostedCreds{CTL: ts.URL, Token: "t"}, inv: &Invocation{set: map[string]bool{}, strs: map[string]string{}},
		sess: inventoryRow{ID: "s", RecordID: "s"}, agent: "main", ackBar: true, barEvents: ev,
		plan: &uploadPlan{Kind: "file", Wire: "conversation", Name: "the conversation", Local: p, To: "claude-code/7c1d2e3f-4a5b-4c6d-8e7f-0a1b2c3d4e5f", Mode: "0644", Content: size, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}}
	start := time.Now()
	u.declare(false)
	u.send()
	took := time.Since(start)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return *ev, srv, took
}

func checkBarRun(t *testing.T, name string, size int64, ev []barEvent, srv *slowUploads) {
	t.Helper()
	if srv.received != size {
		t.Fatalf("%s: the server holds %d of %d bytes", name, srv.received, size)
	}
	if len(ev) == 0 {
		t.Fatalf("%s: the bar showed nothing", name)
	}
	var prev int64
	last := -1
	for i, e := range ev {
		if e.Total != size {
			t.Fatalf("%s: event %d total %d, the file is %d", name, i, e.Total, size)
		}
		if e.Percent > 100 || e.Percent < last || e.Done < prev || e.Done > size {
			t.Fatalf("%s: event %d is not monotonic or is over 100: %+v", name, i, e)
		}
		if e.Done != 0 && !srv.acked[e.Done] {
			t.Fatalf("%s: event %d shows %d bytes, which the server never acknowledged", name, i, e.Done)
		}
		if e.Percent == 100 && !(e.Final && e.Done == size && srv.acked[size]) {
			t.Fatalf("%s: 100%% before the server acknowledged the last byte: %+v", name, e)
		}
		if e.Rate < 0 || e.ETA < 0 || e.ETA > 400*time.Hour {
			t.Fatalf("%s: event %d rate or time left is not sane: %+v", name, i, e)
		}
		last, prev = e.Percent, e.Done
	}
	if f := ev[len(ev)-1]; !f.Final || f.Done != size || f.Percent != 100 || f.ETA != 0 {
		t.Fatalf("%s: the last event is %+v", name, f)
	}
}

func TestUploadBarAtTheBoundaries(t *testing.T) {
	const chunk = 16 << 20
	for _, size := range []int64{1, chunk - 1, chunk, chunk + 1, 2*chunk - 1, 2 * chunk, 2*chunk + 1} {
		ev, srv, _ := uploadSparse(t, size, 0)
		checkBarRun(t, fmt.Sprintf("%d bytes", size), size, ev, srv)
		want := int((size + chunk - 1) / chunk)
		if srv.puts != want {
			t.Fatalf("%d bytes took %d chunks, want %d", size, srv.puts, want)
		}
	}
}

func TestUploadBarThrottled300MB(t *testing.T) {
	const size = 300_000_000
	ev, srv, took := uploadSparse(t, size, 150*time.Millisecond)
	checkBarRun(t, "300 MB", size, ev, srv)
	// the rate the bar showed is near the rate that was true
	truth := float64(size) / took.Seconds()
	var seen bool
	for _, e := range ev {
		if e.Rate > 0 {
			seen = true
			if e.Rate < truth/4 || e.Rate > truth*4 {
				t.Fatalf("the bar says %.0f B/s, the transfer ran at %.0f B/s", e.Rate, truth)
			}
			if e.ETA > 0 {
				left := float64(size-e.Done) / e.Rate
				if d := e.ETA.Seconds() - left; d > 1.5 || d < -1.5 {
					t.Fatalf("time left %v does not follow the rate (%.1fs)", e.ETA, left)
				}
			}
		}
	}
	if !seen {
		t.Fatal("no rate was ever shown on a three second transfer")
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
	ev, srv, _ := uploadSparse(t, importMaxBytes, 0)
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

var pctLine = regexp.MustCompile(`(?m)^(cleaning|uploading): (\d+)% \(([^)]*)\)$`)

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
	want := bytesTextForBar(int64(len(u.Got)))
	if !strings.Contains(errs, "uploading: 100% ("+want+" of "+want+")") {
		t.Fatalf("the last line does not say %s, the bytes the service holds:\n%s", want, errs)
	}
	if !strings.Contains(errs, "placing on the service") {
		t.Fatalf("placing is not said:\n%s", errs)
	}
}

func bytesTextForBar(n int64) string { return barBytes(n) }

func TestImportJSONShowsNoProgress(t *testing.T) {
	e := newImportEnv(t)
	e.bigSession(1)
	_, errs, code := e.run("import", "claude-code", "--latest", "--yes", "--json")
	if code != 0 || strings.Contains(errs, "%") || strings.Contains(errs, "cleaning") || strings.Contains(errs, "uploading:") {
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
	if !strings.Contains(errs, barBytes(acked)+" of ") {
		t.Fatalf("the stop does not say what arrived (%s):\n%s", barBytes(acked), errs)
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
	if !strings.Contains(got, "the upload was stopped") || !strings.Contains(got, barBytes(held)+" of ") || !strings.Contains(got, "run the same command again") || !strings.Contains(got, "Next: ks import") {
		t.Fatalf("the message (the service holds %d bytes):\n%s", held, got)
	}
	e.c.putDelay = 0
	out, errs, code := e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || !strings.Contains(out, "Imported") || !strings.Contains(errs, "uploading: 100%") {
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
