// session_runner.go: what a session runs, and one change of it.
//
//	ks session runner <session>                          the harness, the model and the key,
//	                                                     and a change under way
//	ks session runner <session> [--harness H] [--model M] [--key K] [--spend-limit D]
//	                  [--resume] [--stop-running]        change any of them, in any state
//	ks session runner <session> --cancel                 cancel a change still waiting
//
// The service takes the steps (POST /api/v2/sessions/{id}/runner-change): it
// checks every refusal first, waits for a running instruction when the
// harness changes (--stop-running asks it to stop instead), saves and pauses
// only when a part needs it, applies every part at once, and starts the
// session again with --resume. Each step is printed as it happens; a failure
// says what changed and what did not.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

type runnerKeyRead struct {
	Provider string `json:"provider"`
	Route    string `json:"route"`
	Key      *struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Last4 string `json:"last4"`
	} `json:"key"`
	Problem *struct {
		Words string `json:"words"`
	} `json:"problem"`
}

type runnerChangeRead struct {
	ID          string   `json:"id"`
	State       string   `json:"state"`
	Step        string   `json:"step"`
	Steps       []string `json:"steps"`
	Words       string   `json:"words"`
	Outcome     string   `json:"outcome"`
	WaitingTask string   `json:"waiting_task"`
}

type sessionRunnerRecord struct {
	ID           string            `json:"id"`
	Revision     int64             `json:"revision"`
	RuntimeState string            `json:"runtime_state"`
	RunnerModel  string            `json:"runner_model"`
	Harness      string            `json:"harness"`
	RunnerKey    *runnerKeyRead    `json:"runner_key"`
	RunnerChange *runnerChangeRead `json:"runner_change"`
}

// runnerStepWords: a change's steps, as a person reads them
var runnerStepWords = map[string]string{"checked": "checked", "waiting": "waiting for the instruction in progress to finish",
	"pausing": "saving the session", "applying": "applying the change", "resuming": "starting it again", "done": "done"}

// runnerKeyWords: the key a session calls its runner's provider with
func runnerKeyWords(k *runnerKeyRead) string {
	if k == nil || k.Provider == "" {
		return "not reported"
	}
	switch k.Route {
	case "keepstate":
		return "KeepState's key"
	case "own":
		if k.Key != nil {
			label := firstNonEmptyStr(k.Key.Label, "your key")
			if strings.HasPrefix(label, "Your ") {
				label = "y" + label[1:]
			}
			if k.Key.Last4 != "" {
				label += " …" + k.Key.Last4
			}
			return label
		}
		return "your key"
	}
	return "no key for " + k.Provider
}

func readSessionRunner(cr hostedCreds, recordID string) sessionRunnerRecord {
	var env struct {
		Data sessionRunnerRecord `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(recordID), nil, &env); err != nil {
		die(err)
	}
	return env.Data
}

func hostedSessionRunner(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record",
			Message:    fmt.Sprintf("session %s has no workspace record, so it runs no agent and has no runner. Nothing changed", sess.ShortID),
			NextAction: "ks run --agent"})
	}
	rec := readSessionRunner(cr, sess.RecordID)
	changing := inv.Set("harness") || inv.Set("model") || inv.Set("key") || inv.Set("spend-limit")
	if inv.Bool("cancel") {
		if changing || inv.Set("resume") || inv.Set("stop-running") {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--cancel cancels the change under way and takes no change of its own. Nothing was sent", NextAction: "ks session runner " + sess.ShortID + " --cancel"})
		}
		var env struct {
			Data runnerChangeRead `json:"data"`
		}
		if err := hostedMutate(cr, "DELETE", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/runner-change", nil, &env); err != nil {
			die(runnerChangeRefusal(cr, err, sess))
		}
		emit(map[string]any{"change": env.Data}, func() {
			fmt.Printf("session %s: %s\n", sess.ShortID, sanitize(firstNonEmptyStr(env.Data.Words, "Cancelled. Nothing was changed.")))
		})
		return
	}
	if !changing {
		if inv.Set("resume") || inv.Set("stop-running") {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--resume and --stop-running go with a change: --harness, --model, --key or --spend-limit"})
		}
		emit(map[string]any{"session": sess.ID, "record": rec.ID, "harness": firstNonEmptyStr(rec.Harness, defaultHarness), "runner_model": rec.RunnerModel,
			"runner_key": rec.RunnerKey, "runner_change": rec.RunnerChange, "runtime_state": rec.RuntimeState, "revision": rec.Revision}, func() {
			fmt.Printf("session %s runs %s on %s, calling %s\n", sess.ShortID, sanitize(firstNonEmptyStr(rec.Harness, defaultHarness)), sanitize(notRecorded(rec.RunnerModel)), sanitize(runnerKeyWords(rec.RunnerKey)))
			fmt.Printf("  state          %s\n", stateWords("session_runtime", rec.RuntimeState))
			if rec.RunnerKey != nil && rec.RunnerKey.Route == "none" && rec.RunnerKey.Problem != nil {
				fmt.Printf("  key            %s\n", sanitize(rec.RunnerKey.Problem.Words))
			}
			if c := rec.RunnerChange; c != nil && c.State == "open" {
				fmt.Printf("  changing       %s: %s\n", sanitize(runnerStepWords[c.Step]), sanitize(c.Words))
			}
			fmt.Printf("  change         ks session runner %s --harness <h> --model <m> --key <key-id|keepstate>; the service saves and starts it again as needed\n", sess.ShortID)
		})
		return
	}
	body := map[string]any{"expected_revision": rec.Revision}
	if inv.Set("harness") {
		body["harness"] = strings.TrimSpace(inv.Str("harness"))
	}
	if inv.Set("model") {
		body["runner_model"] = strings.TrimSpace(inv.Str("model"))
	}
	if inv.Set("key") {
		body["key"] = strings.TrimSpace(inv.Str("key"))
	}
	if inv.Set("spend-limit") {
		micros, perr := parseDollars("spend-limit", inv.Str("spend-limit"))
		if perr != nil {
			fail(perr)
		}
		body["spend_limit_microusd"] = micros
	}
	if inv.Bool("resume") {
		body["resume"] = true
	}
	if inv.Bool("stop-running") {
		body["stop_running"] = true
	}
	progress("change session %s's runner, against revision %d; the service checks first, then saves and starts it again only as a part needs", sess.ShortID, rec.Revision)
	// each step as the service reaches it, read beside the operation
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		seen := ""
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			var env struct {
				Data runnerChangeRead `json:"data"`
			}
			if hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/runner-change", nil, &env) != nil || env.Data.State != "open" {
				continue
			}
			if env.Data.Step != seen && env.Data.Step != "checked" {
				seen = env.Data.Step
				progress("%s", runnerStepWords[seen])
			}
		}
	}()
	var out struct {
		Data runnerChangeRead `json:"data"`
	}
	err = hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/runner-change", body, &out)
	close(stop)
	wg.Wait()
	if err != nil {
		die(runnerChangeRefusal(cr, err, sess))
	}
	after := readSessionRunner(cr, sess.RecordID)
	emit(map[string]any{"change": out.Data, "session": after}, func() {
		fmt.Printf("session %s runs %s on %s, calling %s\n", sess.ShortID, sanitize(firstNonEmptyStr(after.Harness, defaultHarness)), sanitize(notRecorded(after.RunnerModel)), sanitize(runnerKeyWords(after.RunnerKey)))
		fmt.Printf("  %s\n", sanitize(firstNonEmptyStr(out.Data.Words, "Done.")))
		if out.Data.Outcome == "applied_not_resumed" {
			fmt.Printf("  resume it when ready: ks session resume %s\n", sess.ShortID)
		}
	})
}

// runnerChangeRefusal words a refused or failed change, with what to do.
// A change that failed after its check is read back, so its own words say
// what changed and what did not.
func runnerChangeRefusal(cr hostedCreds, err error, sess inventoryRow) error {
	var he *hostedErr
	if !errors.As(err, &he) {
		return err
	}
	ce := classify(err)
	switch he.Type {
	case "": // a change that failed after its check answers with its own record
		var env struct {
			Data runnerChangeRead `json:"data"`
		}
		if hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/runner-change", nil, &env) == nil && env.Data.State == "failed" {
			ce.Message = sanitize(env.Data.Words)
			ce.NextAction = "ks session runner " + sess.ShortID
			if env.Data.Step != "checked" && env.Data.Step != "waiting" {
				ce.WorkStarted = workYes // it got as far as saving the session; its words say where it stands
			}
			return ce
		}
		return err
	case "ks_change_in_progress":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks session runner " + sess.ShortID + " (it shows the change under way)"
	case "ks_revision_conflict":
		ce.Message = "the session changed since it was read, so nothing changed; read it again and choose again"
		ce.NextAction = "ks session runner " + sess.ShortID
	case "ks_no_inference_key", "ks_key_invalid", "ks_key_unroutable", "ks_key_ambiguous":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks key list, then ks session runner " + sess.ShortID + " --key <key-id|keepstate>"
	case "ks_harness_invalid", "ks_harness_same", "ks_harness_unavailable", "ks_harness_not_in_image", "ks_model_invalid", "ks_model_harness_mismatch":
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks cruise models (the harnesses and the models each runs)"
	case "ks_change_running":
		ce.Message = sanitize(he.Message)
		ce.NextAction = "ks session runner " + sess.ShortID + " (it says what the change did once it finishes)"
	case "ks_not_found":
		ce.Message = "no change of this session is waiting; nothing was cancelled"
		ce.NextAction = "ks session runner " + sess.ShortID
	case "ks_session_killed":
		ce.Message = sanitize(he.Message)
		ce.NextAction = "ks session list"
	default:
		ce.Message = sanitize(he.Message) + "; nothing changed"
		ce.NextAction = "ks session runner " + sess.ShortID
	}
	return ce
}
