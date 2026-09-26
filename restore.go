// restore.go: ks session restore (KS-052): restore a session from a named
// saved point and print the service's continuation (exact_runtime | none |
// unknown) and its note exactly as given; unknown is never success.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

func hostedSessionRestore(cr hostedCreds, inv *Invocation) {
	sess := sessionRecordOf(cr, inv)
	ck := strings.TrimSpace(inv.Str("checkpoint"))
	if ck == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--checkpoint names the saved point; none is chosen for you. Nothing was restored", NextAction: "ks session checkpoints " + sess.ShortID})
	}
	reason := strings.TrimSpace(inv.Str("reason"))
	if reason == "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--reason is required: a restore is recorded with why somebody asked for it. Nothing was restored"})
	}
	rec, err := fetchSessionRecord(cr, sess.RecordID)
	if err != nil {
		die(err)
	}
	body := map[string]any{"checkpoint_id": ck, "reason": reason, "expected_revision": rec.Revision, "epoch": rec.ExecutionEpoch}
	if d := strings.TrimSpace(inv.Str("accept-affected")); d != "" {
		body["accept_affected_digest"] = d
	}
	var env struct {
		Data restoreAnswer `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(sess.RecordID)+"/restore", body, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) && he.Type == "ks_restore_scope" {
			fail(&cliError{Code: exitConflict, Kind: "restore_scope", Message: sanitize(he.Message),
				NextAction: fmt.Sprintf("ks session restore %s --checkpoint %s --reason \"...\" --accept-affected <the digest above>", sess.ShortID, ck)})
		}
		die(err)
	}
	res := env.Data
	emit(res, func() {
		fmt.Printf("restore of session %s from saved point %s (%s)\n", sess.ShortID, res.CheckpointID, figure(res.Scope))
		for _, p := range res.Phases {
			mark := "done"
			if !p.Done {
				mark = "STOPPED"
			}
			fmt.Printf("  %-14s %s %s\n", p.Phase, mark, sanitize(p.Detail))
		}
		if res.StoppedAt != "" {
			fmt.Printf("  the restore STOPPED at %s\n", res.StoppedAt)
		}
		continuationLines(res)
		if res.HoldID != "" {
			fmt.Printf("  queue          HELD (%s); nothing was released\n", res.HoldID)
		}
		if res.Note != "" {
			fmt.Printf("  note           %s\n", sanitize(res.Note))
		}
	})
	continuationUnknown(res, sess.ShortID)
}
