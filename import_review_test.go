//go:build unix

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Deploy order: this client may meet a service that does not know est_tokens
// yet. It asks again without the count and shows no estimate.
func TestImportSurvivesAServiceThatKnowsNoEstimate(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	e.c.rejectEst = true
	out, errs, code := e.run("import", "claude-code", "--latest", "--plan", "--json")
	doc := decodeJSONOut(t, out)
	if code != 0 || len(asStr(doc["digest"])) != 64 {
		t.Fatalf("--plan against an older service: %d\n%s\n%s", code, out, errs)
	}
	if _, has := doc["estimate"]; has || strings.Contains(strings.ToLower(out), "estimate (not a quote)") {
		t.Fatalf("an estimate was shown by a service that gave none:\n%s", out)
	}
	if n := e.c.count("POST /api/v2/imports"); n != 2 {
		t.Fatalf("the service was asked %d times, want 2 (with the count, refused, then without)", n)
	}
	if len(e.c.plans) != 1 {
		t.Fatalf("the service planned %d times", len(e.c.plans))
	}
	if _, has := asMap(e.c.plans[0]["source"])["est_tokens"]; has {
		t.Fatal("the second ask still carries the count")
	}
	// the whole import works, and the apply carries the declaration that was planned
	_, errs, code = e.run("import", "claude-code", "--latest", "--yes")
	if code != 0 || len(e.c.applies) != 1 {
		t.Fatalf("--yes against an older service: %d\n%s", code, errs)
	}
	if _, has := asMap(asMap(e.c.applies[0]["declaration"])["source"])["est_tokens"]; has {
		t.Fatal("the apply carries the count the service does not know")
	}
}

// An estimate the service does give is in the plan's JSON, additively.
func TestPlanJSONCarriesTheServicesEstimate(t *testing.T) {
	e := newImportEnv(t)
	e.ready()
	e.c.estimate = fakeEstimate(false)
	out, _, code := e.run("import", "claude-code", "--latest", "--plan", "--json")
	doc := decodeJSONOut(t, out)
	if est := asMap(doc["estimate"]); code != 0 || est["tokens"] != float64(812345) || est["is_estimate"] != true {
		t.Fatalf("the plan's estimate: %v", doc["estimate"])
	}
}

// Ctrl-C while the conversation is being placed stops at once, says the
// placement may still finish, and exits 130; it is not swallowed.
func TestCtrlCDuringPlacementStopsAndSaysSo(t *testing.T) {
	for _, phase := range []string{"the apply", "the final check"} {
		e := newImportEnv(t)
		e.ready()
		if phase == "the apply" {
			e.c.applyDelay = 30 * time.Second
		} else {
			e.c.completeDelay = 30 * time.Second
		}
		got, code := e.ptyImport([]keyStep{{"Placing the conversation on KeepState", "\x03"}}, "import", "claude-code", "--latest", "--no-files", "--yes")
		if code != exitInterrupt || e.lastExit > 2*time.Second {
			t.Fatalf("%s: exit %d, %v after the key\n%s", phase, code, e.lastExit, got)
		}
		if f := flat(got); !strings.Contains(f, "The placement may still finish on KeepState") || !strings.Contains(f, "Run the same command again to see it.") {
			t.Fatalf("%s: the message:\n%s", phase, got)
		}
		if strings.Contains(got, "every byte arrived, but whether") {
			t.Fatalf("%s: the old wording survives:\n%s", phase, got)
		}
	}
}

// --yes takes the default for the files question (the conversation only) and
// does not ask.
func TestYesTakesTheDefaultForTheFilesQuestion(t *testing.T) {
	e := newImportEnv(t)
	repo := gitRepo(t)
	e.session("claude-code", "claude-code-2.1.312.jsonl", repo)
	e.c.runCtl.record = map[string]any{"id": "session_abc123", "runtime_state": "running"}
	got, code := e.ptyImport(nil, "import", "claude-code", "--latest", "--yes")
	if code != 0 || strings.Contains(got, "Bring the project files too?") || e.c.upload("folder") != nil || !strings.Contains(got, "Imported into session") {
		t.Fatalf("--yes: %d\n%s", code, got)
	}
}

// With raw keys unavailable the numbered fallback reads a line: end of input
// (Ctrl-D) is a cancel, never an Enter.
func TestNumberedFallbackTreatsEndOfInputAsCancel(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close() // end of input at once
	save := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = save; r.Close() }()
	if got := igChoose("title", []string{"a", "b"}, 0); got != -1 {
		t.Fatalf("end of input chose %d, want a cancel (-1)", got)
	}
}
