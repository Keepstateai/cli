package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// printedJob returns what printJob writes for a job resource.
func printedJob(t *testing.T, job map[string]any) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	printJob(job)
	w.Close()
	os.Stdout = old
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The job resource never carries a record_url, so the status line "record:"
// always read "unavailable", even for an accepted job whose signed receipt
// exists and verifies. The line now points at the real thing.
func TestCruiseStatusPointsAtTheReceiptOfAnAcceptedJob(t *testing.T) {
	sha := strings.Repeat("a", 64)
	for name, job := range map[string]map[string]any{
		"an artifact and a verdict of accepted": {"id": "job_8faa290ec75a", "state": "accepted", "verdict": "accepted", "artifact_sha": sha},
		"a verdict of accepted alone":           {"id": "job_8faa290ec75a", "state": "accepted", "verdict": "accepted"},
		"an artifact alone":                     {"id": "job_8faa290ec75a", "state": "accepted", "artifact_sha": sha},
	} {
		out := printedJob(t, job)
		if !strings.Contains(out, "receipt: ks cruise receipt job_8faa290ec75a\n") {
			t.Errorf("%s: no pointer to the receipt:\n%s", name, out)
		}
		if strings.Contains(out, "record:") {
			t.Errorf("%s: the line that always read unavailable is still printed:\n%s", name, out)
		}
	}
}

func TestCruiseStatusSaysHonestlyWhenThereIsNoReceipt(t *testing.T) {
	for name, c := range map[string]struct {
		job  map[string]any
		want string
	}{
		"running":          {map[string]any{"id": "job_r", "state": "running"}, "receipt: none yet (a job that is accepted has one)"},
		"not accepted":     {map[string]any{"id": "job_x", "state": "review", "verdict": "ladder-exhausted"}, "receipt: none (this job was not accepted)"},
		"cancelled":        {map[string]any{"id": "job_c", "state": "cancelled", "verdict": "cancelled"}, "receipt: none (this job was not accepted)"},
		"nothing reported": {map[string]any{"id": "job_n"}, "receipt: none yet (a job that is accepted has one)"},
	} {
		out := printedJob(t, c.job)
		if !strings.Contains(out, c.want+"\n") {
			t.Errorf("%s: want %q:\n%s", name, c.want, out)
		}
		if strings.Contains(out, "ks cruise receipt") || strings.Contains(out, "record:") {
			t.Errorf("%s: pointed at a receipt that is not there:\n%s", name, out)
		}
	}
}
