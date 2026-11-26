// ks file upload with several paths: up to 12 in one command, sent one after
// the other, each its own result, a refusal of one never stopping the others,
// and no storage sentence in any output.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for i, n := range names {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, n)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte("content of "+n+strings.Repeat("-", i*100)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileUploadSeveralPathsGoOneAfterTheOtherToOneFolder(t *testing.T) {
	c, dir, run := uploadHarness(t)
	writeFiles(t, dir, "a.txt", "b.txt", "sub/c.txt")
	out, errs, code := run("file", "upload", "a.txt", "b.txt", "sub/c.txt", "--to", "inputs", "--session", "fleetins")
	if code != 0 {
		t.Fatalf("several paths: %d\n%s\n%s", code, out, errs)
	}
	for _, p := range []string{"inputs/a.txt", "inputs/b.txt", "inputs/c.txt"} {
		if !strings.Contains(out, "Uploaded "+filepath.Base(p)) || !strings.Contains(out, "to "+p) {
			t.Errorf("no result line for %s:\n%s", p, out)
		}
	}
	if !strings.Contains(out, "Uploaded 3 of 3 paths") {
		t.Errorf("no summary:\n%s", out)
	}
	c.with(func() {
		for _, p := range []string{"inputs/a.txt", "inputs/b.txt", "inputs/c.txt"} {
			if len(c.workspace[p]) == 0 {
				t.Errorf("%s was not placed", p)
			}
		}
		// one at a time: each upload is placed before the next is declared
		open := 0
		for _, call := range c.calls {
			switch {
			case strings.HasPrefix(call, "POST ") && strings.HasSuffix(call, "/uploads"):
				open++
				if open > 1 {
					t.Errorf("a second upload was declared before the first was placed: %v", c.calls)
				}
			case strings.HasPrefix(call, "POST ") && strings.HasSuffix(call, "/complete"):
				open--
			}
		}
	})
}

func TestFileUploadMoreThanTwelvePathsIsRefusedBeforeAnythingIsSent(t *testing.T) {
	c, dir, run := uploadHarness(t)
	args := []string{"file", "upload"}
	for i := 1; i <= 13; i++ {
		n := "f" + string(rune('a'+i)) + ".txt"
		writeFiles(t, dir, n)
		args = append(args, n)
	}
	args = append(args, "--session", "fleetins")
	_, errs, code := run(args...)
	if code != exitUsage || !strings.Contains(errs, "13 paths") || !strings.Contains(errs, "up to 12") || !strings.Contains(errs, "Nothing was sent") {
		t.Fatalf("13 paths: %d\n%s", code, errs)
	}
	if uploadsAsked(c) {
		t.Fatalf("something was sent: %v", c.seen())
	}
}

func TestFileUploadOnePathRefusedTheOthersStillGo(t *testing.T) {
	c, dir, run := uploadHarness(t)
	writeFiles(t, dir, "a.txt", "b.txt", "c.txt")
	c.with(func() { c.workspace["data/b.txt"] = []byte("old b") })
	out, errs, code := run("file", "upload", "a.txt", "b.txt", "c.txt", "--to", "data", "--session", "fleetins")
	if code != exitConflict {
		t.Fatalf("exit %d, want %d\n%s\n%s", code, exitConflict, out, errs)
	}
	if !strings.Contains(out, "Uploaded a.txt") || !strings.Contains(out, "Uploaded c.txt") || strings.Contains(out, "Uploaded b.txt") {
		t.Errorf("results:\n%s", out)
	}
	if !strings.Contains(errs, "already exists in the workspace") || !strings.Contains(errs, "--replace") {
		t.Errorf("b.txt's refusal is not in words with the fix:\n%s", errs)
	}
	if !strings.Contains(out+errs, "2 of 3 paths") {
		t.Errorf("no summary of 2 of 3:\n%s\n%s", out, errs)
	}
	c.with(func() {
		if string(c.workspace["data/b.txt"]) != "old b" {
			t.Errorf("b.txt was overwritten without --replace: %q", c.workspace["data/b.txt"])
		}
		if len(c.workspace["data/a.txt"]) == 0 || len(c.workspace["data/c.txt"]) == 0 {
			t.Errorf("the others were not placed")
		}
	})
	if _, errs, code := run("file", "upload", "b.txt", "--to", "data/b.txt", "--replace", "--session", "fleetins"); code != 0 {
		t.Fatalf("replace: %d %s", code, errs)
	}
}

func TestFileUploadTwoPathsWithTheSameNameAreRefusedBeforeAnythingIsSent(t *testing.T) {
	c, dir, run := uploadHarness(t)
	writeFiles(t, dir, "a.txt", "sub/a.txt")
	_, errs, code := run("file", "upload", "a.txt", "sub/a.txt", "--session", "fleetins")
	if code != exitUsage || !strings.Contains(errs, "both go to") || !strings.Contains(errs, "Nothing was sent") {
		t.Fatalf("same name: %d\n%s", code, errs)
	}
	if uploadsAsked(c) {
		t.Fatalf("something was sent: %v", c.seen())
	}
	if _, errs, code := run("file", "upload", "a.txt", "sub/a.txt", "--discard", "--session", "fleetins"); code != exitUsage || !strings.Contains(errs, "one path") {
		t.Fatalf("--discard with two paths: %d\n%s", code, errs)
	}
}

func TestFileUploadSeveralPathsTellOnceAndJSONHasNoStorageSentence(t *testing.T) {
	c, dir, run := uploadHarness(t)
	writeFiles(t, dir, "a.txt", "b.txt")
	out, errs, code := run("file", "upload", "a.txt", "b.txt", "--tell", "--session", "fleetins")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	c.with(func() {
		if len(c.tasks) != 1 || !strings.Contains(c.tasks[0], "2 files") {
			t.Errorf("the agent is told once, with the count: %v", c.tasks)
		}
	})
	jout, _, code := run("file", "upload", "a.txt", "--to", "again/a.txt", "--json", "--session", "fleetins")
	if code != 0 {
		t.Fatalf("json: %d", code)
	}
	for _, s := range []string{out, errs, jout} {
		if strings.Contains(s, "toward your storage") || strings.Contains(s, "storage_note") {
			t.Errorf("the storage sentence is still printed: %s", s)
		}
	}
	var env struct{ Data map[string]any }
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.Split(jout, "\n")[0])), &env); err != nil || env.Data["path"] != "again/a.txt" {
		t.Errorf("json: %v %s", err, jout)
	}
}
