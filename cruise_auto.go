// cruise_auto.go: Cruise Autopilot, from a terminal.
//
//	ks cruise auto brief     start a plan: what you want, and the draft of your files
//	ks cruise auto answer    answer the questions (none given: every default)
//	ks cruise auto plan      have the checks written by a model that is not on the job's
//	                         ladder, and measured to fail on your files
//	ks cruise auto status    the plan in words: the checks as plain sentences, what cannot
//	                         be checked, what it can cost, what planning cost
//	ks cruise auto approve   run it, by confirming the digests of the exact manifest shown
//	ks cruise auto receipt   the receipt's five lines, each from the job's own records
//	ks cruise auto discard   delete the plan and its copy of the files
//
// Autopilot makes an ordinary Cruise job: the same locked manifest, the same
// ladder and limits, the same receipt. What is new is the front door. This
// client never calls a model. It does not take the service's word for what it
// approves: the digests it shows and sends are computed here from the manifest
// the service returned (the same canonical rule ks cruise approve uses), the
// sentences it shows are the ones inside that manifest, and a plan whose
// sentences do not digest to what the manifest names is refused.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const autoRoute = "/api/v2/cruise-auto"

type autoCheck struct {
	ID       string   `json:"id"`
	Sentence string   `json:"sentence"`
	CheckRef string   `json:"check_ref"`
	Red      []string `json:"red"`
}

type autoQuestion struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
	Default string   `json:"default"`
}

type autoView struct {
	ID         string            `json:"id"`
	State      string            `json:"state"`
	StateLabel string            `json:"state_label"`
	Lane       string            `json:"lane"`
	Brief      string            `json:"brief"`
	DraftID    string            `json:"draft_id"`
	Hint       string            `json:"hint"`
	Questions  []autoQuestion    `json:"questions"`
	Answers    map[string]string `json:"answers"`
	JobID      string            `json:"job_id"`
	Card       *struct {
		Checks      []autoCheck `json:"checks"`
		AlreadyTrue []autoCheck `json:"already_true"`
		Today       struct {
			Pass int    `json:"pass"`
			Of   int    `json:"of"`
			Line string `json:"line"`
		} `json:"today"`
		NotChecked []string `json:"not_checked"`
		Cost       struct {
			Low  any   `json:"low_microusd"`
			High int64 `json:"high_microusd"`
			Cap  int64 `json:"cap_microusd"`
		} `json:"cost"`
		Routing           string   `json:"routing"`
		Ladder            []string `json:"ladder"`
		PlainChecksDigest string   `json:"plain_checks_digest"`
		ManifestSHA       string   `json:"manifest_sha"`
		HeldOut           string   `json:"held_out"`
		Checker           struct {
			Model string `json:"model"`
		} `json:"checker"`
		DataClass string `json:"data_classification"`
		Delivery  string `json:"delivery"`
	} `json:"card"`
	Planning struct {
		Line string `json:"line"`
	} `json:"planning"`
	Refusal *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"refusal"`
	Next *struct {
		Label  string `json:"label"`
		Action string `json:"action"`
	} `json:"next"`
}

func decodeAuto(raw json.RawMessage) (*autoView, error) {
	var v autoView
	if err := json.Unmarshal(raw, &v); err != nil || v.ID == "" {
		return nil, fmt.Errorf("the control plane answered with a plan this client could not read")
	}
	return &v, nil
}

func fetchAuto(cr hostedCreds, id string) (*autoView, json.RawMessage) {
	var env draftEnvelope
	if err := hostedCall(cr, "GET", inContext(cr, autoRoute+"/"+url.PathEscape(id)), nil, &env); err != nil {
		die(err)
	}
	v, err := decodeAuto(env.Data)
	if err != nil {
		die(err)
	}
	return v, env.Data
}

// autoPlainChecksDigest is sha256 over the manifest-canonical JSON of the
// list of {id, sentence} objects, in the order listed: the digest the
// approval binds (the shared vectors: testdata/autopilot).
func autoPlainChecksDigest(checks []autoCheck) string {
	rows := make([]any, 0, len(checks))
	for _, c := range checks {
		rows = append(rows, map[string]any{"id": c.ID, "sentence": c.Sentence})
	}
	b, err := canonicalJSON(rows)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// autoBlock reads the autopilot block out of a manifest: the sentences the
// approval binds and the digest the manifest names for them.
func autoBlock(m map[string]any) ([]autoCheck, string, []string, error) {
	if v, ok := m["binding_version"].(json.Number); !ok || v.String() != "2" {
		return nil, "", nil, fmt.Errorf("the manifest is not an Autopilot manifest (binding_version 2)")
	}
	ao, ok := m["autopilot"].(map[string]any)
	if !ok {
		return nil, "", nil, fmt.Errorf("the manifest names no autopilot block")
	}
	raw, _ := json.Marshal(ao["plain_checks"])
	var checks []autoCheck
	if json.Unmarshal(raw, &checks) != nil || len(checks) == 0 {
		return nil, "", nil, fmt.Errorf("the manifest's checks are not readable")
	}
	digest, _ := ao["plain_checks_digest"].(string)
	var nc []string
	rawNC, _ := json.Marshal(ao["not_checked"])
	_ = json.Unmarshal(rawNC, &nc)
	return checks, strings.ToLower(digest), nc, nil
}

// autoTerms reads the spend limit and the ladder's models out of the manifest
// whose digest an approval names.
func autoTerms(m map[string]any) (int64, []string) {
	var capMicro int64
	if lim, ok := m["limits"].(map[string]any); ok {
		if n, ok := lim["spend_microusd"].(json.Number); ok {
			capMicro, _ = n.Int64()
		}
	}
	var ladder []string
	if l, ok := m["ladder"].([]any); ok {
		for _, it := range l {
			if o, ok := it.(map[string]any); ok {
				if mod, _ := o["model"].(string); mod != "" {
					ladder = append(ladder, mod)
				}
			}
		}
	}
	return capMicro, ladder
}

// autoCheckedCard refuses a plan card whose cap or models are not the
// manifest's: what is shown is what the digest binds. It returns the manifest,
// its digest, the sentences and what cannot be checked, all read from the
// manifest itself.
func autoCheckedCard(cr hostedCreds, v *autoView, nothing string) (m map[string]any, computed string, checks []autoCheck, notChecked []string) {
	d, _ := fetchDraft(cr, v.DraftID)
	if !d.hasManifest() {
		fail(&cliError{Code: exitConflict, Kind: "draft_no_manifest", Message: fmt.Sprintf("plan %s has no manifest; %s", v.ID, nothing), NextAction: "ks cruise auto status " + v.ID})
	}
	computed = d.checkedDigest(nothing)
	m, err := d.manifestMap()
	if err != nil {
		fail(integrity("manifest_unreadable", err.Error()+"; "+nothing))
	}
	checks, named, notChecked, err := autoBlock(m)
	if err != nil {
		fail(integrity("autopilot_block_unreadable", err.Error()+"; "+nothing))
	}
	plain := autoPlainChecksDigest(checks)
	if plain != named {
		fail(integrity("plain_checks_digest_mismatch", fmt.Sprintf("the manifest names digest %s for its sentences, but they digest to %s here; %s", short(named), short(plain), nothing)))
	}
	if strings.ToLower(v.Card.PlainChecksDigest) != plain {
		fail(integrity("plain_checks_digest_mismatch", fmt.Sprintf("the service states digest %s for the sentences it showed, but the manifest's sentences digest to %s here; %s", short(v.Card.PlainChecksDigest), short(plain), nothing)))
	}
	if strings.ToLower(v.Card.ManifestSHA) != computed {
		fail(integrity("manifest_digest_mismatch", fmt.Sprintf("the plan states manifest digest %s, but the manifest hashes to %s here; %s", short(v.Card.ManifestSHA), short(computed), nothing)))
	}
	capMicro, ladder := autoTerms(m)
	if capMicro != v.Card.Cost.Cap || strings.Join(ladder, ",") != strings.Join(v.Card.Ladder, ",") {
		fail(integrity("card_terms_mismatch", fmt.Sprintf("the plan card shows a spend limit of %s and models %s, but the manifest it names says %s and %s; %s",
			autoMoney(v.Card.Cost.Cap), strings.Join(v.Card.Ladder, ", "), autoMoney(capMicro), strings.Join(ladder, ", "), nothing)))
	}
	return m, computed, checks, notChecked
}

func autoWords(w io.Writer, indent string, s string) {
	for _, l := range wrapText(sanitize(s), 78-len(indent)) {
		fmt.Fprintln(w, indent+l)
	}
}

func autoMoney(micro int64) string {
	return fmt.Sprintf("$%d.%02d", micro/1_000_000, (micro%1_000_000)/10_000)
}

// printAuto writes the plan for reading: where it stands, the questions, the
// checks, what cannot be checked, the cost, and the one next step.
func printAuto(w io.Writer, v *autoView, checks []autoCheck, notChecked []string) {
	fmt.Fprintf(w, "plan %s  %s\n", v.ID, strings.ToUpper(sanitize(v.StateLabel)))
	fmt.Fprintln(w, "  brief:")
	autoWords(w, "    ", v.Brief)
	if v.Hint != "" {
		autoWords(w, "  ", v.Hint)
	}
	if v.State == "questions" || (v.State == "answered" && len(v.Questions) > 0) {
		fmt.Fprintln(w, "  questions (each already answered with its default):")
		for i, q := range v.Questions {
			ans := v.Answers[q.ID]
			if ans == "" {
				ans = q.Default
			}
			autoWords(w, "    ", fmt.Sprintf("%d. %s [%s]  ks cruise auto answer %s --set %s=VALUE", i+1, q.Text, ans, v.ID, q.ID))
		}
	}
	if v.Card != nil {
		c := v.Card
		fmt.Fprintf(w, "  what done means (%s):\n", sanitize(c.Today.Line))
		for i, k := range checks {
			autoWords(w, "    ", fmt.Sprintf("%d. %s", i+1, k.Sentence))
		}
		if len(c.AlreadyTrue) > 0 {
			fmt.Fprintln(w, "  already true, not counted:")
			for _, k := range c.AlreadyTrue {
				autoWords(w, "    ", k.Sentence)
			}
		}
		fmt.Fprintln(w, "  what cannot be checked:")
		for _, t := range notChecked {
			autoWords(w, "    ", "- "+t)
		}
		fmt.Fprintf(w, "  most it can spend: %s (it stops and asks rather than go past it)\n", autoMoney(c.Cost.Cap))
		autoWords(w, "  ", c.Routing+".")
		autoWords(w, "  ", "checks written by "+sanitize(c.Checker.Model)+", which is not on the job's ladder. "+c.HeldOut+".")
		autoWords(w, "  ", c.DataClass+".")
		autoWords(w, "  ", "you get "+c.Delivery+".")
	}
	autoWords(w, "  ", v.Planning.Line+" Planning is free to you.")
	if v.Refusal != nil && v.Refusal.Message != "" {
		autoWords(w, "  ", v.Refusal.Message)
	}
}

func finishAutoShow(v *autoView, raw json.RawMessage) {
	var checks []autoCheck
	var nc []string
	if v.Card != nil {
		checks, nc = v.Card.Checks, v.Card.NotChecked
	}
	emit(raw, func() {
		printAuto(os.Stdout, v, checks, nc)
		switch v.State {
		case "questions":
			fmt.Printf("next: ks cruise auto answer %s   (no --set accepts every default)\n", v.ID)
		case "answered", "plan_failed":
			fmt.Printf("next: ks cruise auto plan %s\n", v.ID)
		case "planning":
			fmt.Printf("next: ks cruise auto status %s   (the checks take a few minutes)\n", v.ID)
		case "plan_ready":
			fmt.Printf("next: ks cruise auto approve %s\n", v.ID)
		case "plan_capped":
			fmt.Printf("next: ks cruise auto discard %s, then start again with a simpler brief\n", v.ID)
		case "approved":
			fmt.Printf("next: ks cruise auto receipt %s\n", v.ID)
		}
	})
}

// ---- brief, answer, plan ---------------------------------------------------------------

func hostedCruiseAutoBrief(cr hostedCreds, inv *Invocation) {
	text := strings.TrimSpace(inv.Str("text"))
	if text == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "say what you want in a few sentences with --text; nothing was started", NextAction: `ks cruise auto brief <draft> --text "..." --consent`})
	}
	if !inv.Bool("consent") {
		fail(&cliError{Code: exitUsage, Kind: "consent_required",
			Message:    "Autopilot keeps your files for this plan only, and the model that writes the checks reads them. Say you agree with --consent; nothing was started and nothing was sent",
			NextAction: fmt.Sprintf("ks cruise auto brief %s --text %q --consent", inv.Arg(0), clip(text, 30))})
	}
	var env draftEnvelope
	body := map[string]any{"brief": text, "draft_id": inv.Arg(0), "storage_consent": true}
	if err := hostedMutate(cr, "POST", inContext(cr, autoRoute), body, &env); err != nil {
		die(cruiseCtxErr(cr, err))
	}
	v, err := decodeAuto(env.Data)
	if err != nil {
		die(err)
	}
	finishAutoShow(v, env.Data)
}

func hostedCruiseAutoAnswer(cr hostedCreds, inv *Invocation) {
	answers := map[string]string{}
	for _, kv := range inv.List("set") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			fail(&cliError{Code: exitUsage, Kind: "usage", Message: fmt.Sprintf("--set takes QUESTION=ANSWER (got %q); nothing was changed", kv)})
		}
		answers[strings.TrimSpace(k)] = val
	}
	var env draftEnvelope
	if err := hostedMutate(cr, "POST", inContext(cr, autoRoute+"/"+url.PathEscape(inv.Arg(0))+"/answers"), map[string]any{"answers": answers}, &env); err != nil {
		die(cruiseCtxErr(cr, err))
	}
	v, err := decodeAuto(env.Data)
	if err != nil {
		die(err)
	}
	finishAutoShow(v, env.Data)
}

func hostedCruiseAutoPlan(cr hostedCreds, inv *Invocation) {
	var env draftEnvelope
	if err := hostedMutate(cr, "POST", inContext(cr, autoRoute+"/"+url.PathEscape(inv.Arg(0))+"/plan"), map[string]any{}, &env); err != nil {
		die(cruiseCtxErr(cr, err))
	}
	v, err := decodeAuto(env.Data)
	if err != nil {
		die(err)
	}
	if inv.Bool("wait") {
		deadline := time.Now().Add(20 * time.Minute)
		for v.State == "planning" && time.Now().Before(deadline) {
			time.Sleep(3 * time.Second)
			var raw json.RawMessage
			v, raw = fetchAuto(cr, v.ID)
			env.Data = raw
		}
	}
	finishAutoShow(v, env.Data)
}

func hostedCruiseAutoStatus(cr hostedCreds, inv *Invocation) {
	if inv.Arg(0) == "" {
		var env struct {
			Data struct {
				Items []json.RawMessage `json:"items"`
			} `json:"data"`
		}
		if err := hostedCall(cr, "GET", inContext(cr, autoRoute), nil, &env); err != nil {
			die(cruiseCtxErr(cr, err))
		}
		emit(map[string]any{"plans": env.Data.Items, "count": len(env.Data.Items), "context": contextWord(cr)}, func() {
			if len(env.Data.Items) == 0 {
				fmt.Println(`no plans yet; start one with ks cruise auto brief <draft> --text "..." --consent`)
				return
			}
			for _, it := range env.Data.Items {
				v, err := decodeAuto(it)
				if err != nil {
					die(err)
				}
				fmt.Println(sanitize(fmt.Sprintf("%-22s %-40s %s", v.ID, clip(v.StateLabel, 40), clip(v.Brief, 50))))
			}
		})
		return
	}
	v, raw := fetchAuto(cr, inv.Arg(0))
	if v.State == "plan_ready" && v.Card != nil {
		autoCheckedCard(cr, v, "nothing is shown as approvable")
	}
	finishAutoShow(v, raw)
}

// ---- approve ---------------------------------------------------------------------------

func hostedCruiseAutoApprove(cr hostedCreds, inv *Invocation) {
	v, _ := fetchAuto(cr, inv.Arg(0))
	if v.State == "approved" && v.JobID != "" {
		fmt.Printf("plan %s is approved already: job %s; ks cruise status %s\n", v.ID, v.JobID, v.JobID)
		return
	}
	if v.State != "plan_ready" || v.Card == nil {
		fail(&cliError{Code: exitConflict, Kind: "plan_state", Message: fmt.Sprintf("plan %s is %s; only a plan whose checks are measured can be approved, and nothing was approved", v.ID, sanitize(v.StateLabel)),
			NextAction: "ks cruise auto status " + v.ID})
	}
	// the digests are computed here, from the manifest the service holds
	_, computed, checks, notChecked := autoCheckedCard(cr, v, "nothing was approved")
	plain := autoPlainChecksDigest(checks)
	keep := inv.Int("keep-days")
	if keep < 0 || keep > 30 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--keep-days is 0 to 30; nothing was approved"})
	}
	// the review, from the manifest's own sentences, before anything is sent
	shown := *v
	shownCard := *v.Card
	shown.Card = &shownCard
	shown.Card.Checks = checks
	printAuto(os.Stderr, &shown, checks, notChecked)
	fmt.Fprintf(os.Stderr, "  manifest digest %s; the sentences digest to %s\n", short(computed), short(plain))
	given := strings.ToLower(strings.TrimSpace(inv.Str("confirm")))
	matches := func(s string) bool {
		s = strings.ToLower(strings.TrimSpace(s))
		return s == computed || (len(s) == 12 && s == short(computed))
	}
	switch {
	case given != "":
		if !matches(given) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q is not the digest of this plan's manifest (%s); nothing was approved", given, short(computed))})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required",
			Message:    "an approval is confirmed by the manifest's digest and none was given; --yes does not approve a Cruise job, and nothing was approved",
			NextAction: fmt.Sprintf("ks cruise auto approve %s --confirm %s", v.ID, short(computed))})
	default:
		fmt.Fprintf(os.Stderr, "type the digest (%s) to run this job, or anything else to stop: ", short(computed))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if !matches(line) {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "the digest was not confirmed; nothing was approved"})
		}
	}
	body := map[string]any{"manifest_sha": computed, "plain_checks_digest": plain}
	if keep > 0 {
		body["retention_days"] = keep
	}
	var env draftEnvelope
	if err := hostedMutate(cr, "POST", inContext(cr, autoRoute+"/"+url.PathEscape(v.ID)+"/approve"), body, &env); err != nil {
		die(cruiseCtxErr(cr, err))
	}
	after, err := decodeAuto(env.Data)
	if err != nil {
		die(err)
	}
	emit(env.Data, func() {
		fmt.Printf("approved %s: job %s runs the manifest with digest %s\n", after.ID, after.JobID, short(computed))
		fmt.Printf("follow it: ks cruise status %s; the receipt: ks cruise auto receipt %s\n", after.JobID, after.ID)
	})
}

// ---- receipt, discard ------------------------------------------------------------------------

func hostedCruiseAutoReceipt(cr hostedCreds, inv *Invocation) {
	var env draftEnvelope
	if err := hostedCall(cr, "GET", inContext(cr, autoRoute+"/"+url.PathEscape(inv.Arg(0))+"/receipt"), nil, &env); err != nil {
		die(cruiseCtxErr(cr, err))
	}
	var rc struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
		Lines []struct {
			N       int    `json:"n"`
			Label   string `json:"label"`
			Text    string `json:"text"`
			Holds   bool   `json:"holds"`
			Sources []struct {
				Kind string `json:"kind"`
				Ref  string `json:"ref"`
			} `json:"sources"`
		} `json:"lines"`
		Planning    string `json:"planning"`
		ManifestSHA string `json:"manifest_sha"`
		EventHead   string `json:"event_head"`
	}
	if err := json.Unmarshal(env.Data, &rc); err != nil || len(rc.Lines) != 5 {
		die(fmt.Errorf("the control plane answered with a receipt this client could not read"))
	}
	emit(env.Data, func() {
		fmt.Printf("receipt for job %s (%s)\n", rc.JobID, sanitize(rc.State))
		for _, l := range rc.Lines {
			mark := "  "
			if !l.Holds {
				mark = "! "
			}
			autoWords(os.Stdout, mark, fmt.Sprintf("%d. %s %s", l.N, l.Label, l.Text))
			if inv.Bool("sources") {
				var refs []string
				for _, s := range l.Sources {
					refs = append(refs, s.Kind+" "+s.Ref)
				}
				sort.Strings(refs)
				autoWords(os.Stdout, "     from: ", strings.Join(refs, "; "))
			}
		}
		if rc.Planning != "" {
			autoWords(os.Stdout, "  ", "planning: free. "+rc.Planning)
		}
		fmt.Printf("  manifest %s, event chain head %s\n", short(rc.ManifestSHA), short(rc.EventHead))
	})
}

func hostedCruiseAutoDiscard(cr hostedCreds, inv *Invocation) {
	var env draftEnvelope
	if err := hostedMutate(cr, "DELETE", inContext(cr, autoRoute+"/"+url.PathEscape(inv.Arg(0))), nil, &env); err != nil {
		die(cruiseCtxErr(cr, err))
	}
	v, err := decodeAuto(env.Data)
	if err != nil {
		die(err)
	}
	emit(env.Data, func() {
		fmt.Printf("discarded %s; its copy of the files is deleted and nothing runs\n", v.ID)
	})
}
