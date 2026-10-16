// session_brief.go: the handoff brief of a session's latest harness switch.
//
//	ks session brief <session>
//
// When a session switches its harness, the next instruction starts a new
// conversation in the other harness, and the new agent is handed KeepState's
// handoff brief with it: a record of the work so far, written from the
// session's records, with a short narrative by the outgoing harness's
// smallest model on the session's own key, every line of it checked against
// the journal. This reads that brief as the agent received it (or will),
// with who wrote its narrative, its size and its checks. It changes nothing.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type handoffBriefView struct {
	HandoffID       string      `json:"handoff_id"`
	SessionID       string      `json:"session_id"`
	State           string      `json:"state"`
	From            string      `json:"from"`
	To              string      `json:"to"`
	FromLabel       string      `json:"from_label"`
	ToLabel         string      `json:"to_label"`
	CreatedAt       string      `json:"created_at"`
	WrittenAt       string      `json:"written_at,omitempty"`
	DeliveredAt     string      `json:"delivered_at,omitempty"`
	DeliveredTaskID string      `json:"delivered_task_id,omitempty"`
	NarrativeModel  string      `json:"narrative_model"`
	Provider        string      `json:"provider"`
	Fallback        bool        `json:"fallback"`
	FallbackReason  string      `json:"fallback_reason,omitempty"`
	Tokens          int         `json:"tokens"`
	SHA256          string      `json:"sha256,omitempty"`
	Brief           string      `json:"brief,omitempty"`
	Checks          briefChecks `json:"checks"`
	Cost            string      `json:"cost"`
}

// briefChecks is what the service's code checks kept and dropped of the
// model's lines, and where the journal held text addressed to the agent
// that no person wrote.
type briefChecks struct {
	Lines             int     `json:"lines"`
	Kept              int     `json:"kept"`
	DroppedFormat     int     `json:"dropped_format"`
	DroppedCitation   int     `json:"dropped_citation"`
	DroppedPersonOnly int     `json:"dropped_person_only"`
	DroppedInjection  int     `json:"dropped_injection"`
	DroppedOverCap    int     `json:"dropped_over_cap"`
	Redactions        int     `json:"redactions"`
	InjectionSeqs     []int64 `json:"injection_seqs,omitempty"`
}

func hostedSessionBrief(cr hostedCreds, inv *Invocation) {
	sess, err := resolveSession(cr, inv.Arg(0))
	if err != nil {
		die(err)
	}
	if sess.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record",
			Message: fmt.Sprintf("session %s has no workspace record, so it runs no harness and has no handoff brief", sess.ShortID)})
	}
	var env struct {
		Data handoffBriefView `json:"data"`
	}
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/handoff-brief", nil, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_not_found" {
			ce := classify(err)
			ce.Message = sanitize(he.Message) + "; nothing was read"
			ce.NextAction = "ks session harness " + sess.ShortID
			die(ce)
		}
		die(err)
	}
	v := env.Data
	emit(v, func() {
		fmt.Printf("handoff brief of session %s: %s to %s (%s)\n", sess.ShortID, sanitize(firstNonEmptyStr(v.FromLabel, v.From)), sanitize(firstNonEmptyStr(v.ToLabel, v.To)), sanitize(v.HandoffID))
		switch v.State {
		case "delivered":
			fmt.Printf("  state      delivered with instruction %s at %s\n", sanitize(v.DeliveredTaskID), sanitize(v.DeliveredAt))
		case "written":
			fmt.Printf("  state      written at %s; it goes to the agent with the next instruction\n", sanitize(v.WrittenAt))
		default:
			fmt.Printf("  state      not written yet: it is written when the session first runs after the switch\n")
		}
		if v.State == "pending" || v.State == "" {
			return
		}
		if v.Fallback {
			fmt.Printf("  written    by KeepState from its records alone, a facts card: %s\n", sanitize(v.FallbackReason))
		} else {
			fmt.Printf("  written    by KeepState from its records; its narrative by %s on the session's own key\n", sanitize(v.NarrativeModel))
		}
		fmt.Printf("  size       %d tokens\n", v.Tokens)
		fmt.Printf("  cost       %s\n", sanitize(v.Cost))
		if !v.Fallback {
			fmt.Printf("  checks     %s\n", briefChecksText(v.Checks))
		}
		fmt.Printf("  sha256     %s\n\n", sanitize(v.SHA256))
		for _, line := range strings.Split(v.Brief, "\n") {
			fmt.Println(sanitize(line))
		}
	})
}

// briefChecksText says what the code checks kept and dropped of the model's lines.
func briefChecksText(c briefChecks) string {
	out := fmt.Sprintf("kept %d of %d model lines", c.Kept, c.Lines)
	var dropped []string
	for _, d := range []struct {
		n     int
		words string
	}{
		{c.DroppedCitation, "without a valid citation"},
		{c.DroppedPersonOnly, "naming a constraint no person stated"},
		{c.DroppedInjection, "addressed to the agent that no person wrote"},
		{c.DroppedFormat, "outside the sections the model writes"},
		{c.DroppedOverCap, "over a section's cap"},
	} {
		if d.n > 0 {
			dropped = append(dropped, fmt.Sprintf("%d %s", d.n, d.words))
		}
	}
	if len(dropped) > 0 {
		out += "; dropped " + strings.Join(dropped, ", ")
	}
	if c.Redactions > 0 {
		out += fmt.Sprintf("; %d secrets redacted", c.Redactions)
	}
	return out
}
