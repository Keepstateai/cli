// session_migrate.go: ks session migrate (KS-083). A legacy session becomes
// an agent session only through a preview the person reviewed:
//
//	GET  /api/v2/sessions/{id}/migration   the preview, under a digest:
//	     runner compatibility, workspace, key bindings (provider names only),
//	     saved points, what each legacy command does afterwards, what is
//	     preserved, the rollback point, blockers
//	POST /api/v2/sessions/{id}/migrate     {preview_digest}: exactly that
//	     conversion, or nothing
//
// The preview is always shown first. A preview with blockers is refused with
// each blocker named (a running legacy guest must be saved and stopped
// first). Applying is confirmed by the preview's digest -- --yes never
// confirms it -- and the digest sent is the one of the preview just shown,
// so a confirmation made against an older preview is refused. The result is
// read back from the session record afterwards.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type migrationPreviewDoc struct {
	SessionID string   `json:"session_id"`
	Mode      string   `json:"mode"`
	Eligible  bool     `json:"eligible"`
	Blockers  []string `json:"blockers"`
	Runner    struct {
		Compatible bool   `json:"compatible"`
		Detail     string `json:"detail"`
	} `json:"runner"`
	Workspace struct {
		EngineLinked bool   `json:"engine_linked"`
		RuntimeState string `json:"runtime_state"`
	} `json:"workspace"`
	KeyBindings []string `json:"key_bindings"`
	SavedPoints struct {
		Count  int    `json:"count"`
		Latest string `json:"latest,omitempty"`
	} `json:"saved_points"`
	Preserved struct {
		Tasks  int `json:"tasks"`
		Agents int `json:"agents"`
		Grants int `json:"adviser_grants"`
	} `json:"preserved"`
	Rollback      string `json:"rollback"`
	ScriptChanges []struct {
		Command string `json:"command"`
		Before  string `json:"before"`
		After   string `json:"after"`
	} `json:"script_changes"`
	PreviewDigest string `json:"preview_digest"`
}

type migrationResultDoc struct {
	SessionID        string `json:"session_id"`
	Outcome          string `json:"outcome"`
	Mode             string `json:"mode"`
	RollbackPoint    string `json:"rollback_point,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	Note             string `json:"note"`
	PreservedTasks   int    `json:"preserved_tasks"`
	PreservedGrants  int    `json:"preserved_adviser_grants"`
	PreservedMembers int    `json:"preserved_members"`
}

// migrationRecordID answers the workspace session record an argument names:
// a session_... record id as given, or a fleet session resolved to its record.
func migrationRecordID(cr hostedCreds, arg string) string {
	if strings.HasPrefix(arg, "session_") {
		return arg
	}
	r, err := resolveSession(cr, arg)
	if err != nil {
		die(err)
	}
	if r.RecordID == "" {
		fail(&cliError{Code: exitFailed, Kind: "no_record", Message: fmt.Sprintf("session %s has no workspace record to migrate; nothing was done", r.ShortID)})
	}
	return r.RecordID
}

func previewLines(p migrationPreviewDoc) []string {
	out := []string{
		fmt.Sprintf("migration preview for session %s (now %s), digest %s", p.SessionID, sanitize(figure(p.Mode)), short(p.PreviewDigest)),
		"  runner         " + map[bool]string{true: "compatible: ", false: "NOT compatible: "}[p.Runner.Compatible] + sanitize(notRecorded(p.Runner.Detail)),
		fmt.Sprintf("  workspace      engine linked %v, runtime %s", p.Workspace.EngineLinked, stateLabel("session_runtime", p.Workspace.RuntimeState)),
		"  key bindings   " + notRecorded(sanitize(strings.Join(p.KeyBindings, ", "))) + " (provider names only)",
		fmt.Sprintf("  saved points   %d; latest %s", p.SavedPoints.Count, notRecorded(p.SavedPoints.Latest)),
		fmt.Sprintf("  preserved      %d task(s), %d agent(s), %d adviser connection(s)", p.Preserved.Tasks, p.Preserved.Agents, p.Preserved.Grants),
		"  rollback       " + sanitize(notRecorded(p.Rollback)),
		"  legacy commands afterwards:",
	}
	for _, c := range p.ScriptChanges {
		out = append(out, "    "+sanitize(c.Command), "      before: "+sanitize(c.Before), "      after:  "+sanitize(c.After))
	}
	if len(p.Blockers) > 0 {
		out = append(out, "  BLOCKED:")
		for _, b := range p.Blockers {
			out = append(out, "    - "+sanitize(b))
		}
	}
	return out
}

func fetchMigrationPreview(cr hostedCreds, id string) (migrationPreviewDoc, error) {
	var env struct {
		Data migrationPreviewDoc `json:"data"`
	}
	err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id)+"/migration", nil, &env)
	return env.Data, err
}

func hostedSessionMigrate(cr hostedCreds, inv *Invocation) {
	id := migrationRecordID(cr, strings.TrimSpace(inv.Arg(0)))
	p, err := fetchMigrationPreview(cr, id)
	if err != nil {
		die(err)
	}
	if p.PreviewDigest == "" {
		fail(integrity("migration_preview_unsigned", "the preview carries no digest, so it cannot be confirmed; nothing was changed"))
	}
	for _, l := range previewLines(p) {
		progress("%s", l)
	}
	if len(p.Blockers) > 0 || !p.Eligible {
		next := "ks session migrate " + id + " (again, once the blockers are resolved)"
		if !p.Runner.Compatible && p.Workspace.EngineLinked {
			next = "save and stop the running legacy guest first: ks checkpoint " + id + " --stop, then ks session migrate " + id
		}
		fail(&cliError{Code: exitConflict, Kind: "migration_blocked", Detail: p,
			Message:    "the session cannot migrate now: " + sanitize(strings.Join(p.Blockers, "; ")) + "; nothing was changed",
			NextAction: next})
	}
	want := short(p.PreviewDigest)
	given := strings.TrimSpace(inv.Str("confirm"))
	switch {
	case given != "":
		if given != want && given != p.PreviewDigest {
			fail(&cliError{Code: exitConflict, Kind: "confirmation_mismatch", Message: fmt.Sprintf("--confirm %q is not the preview shown now (%s): the session changed since, or another preview was confirmed; nothing was changed", given, want),
				NextAction: fmt.Sprintf("review the preview above, then ks session migrate %s --confirm %s", id, want)})
		}
	case out.noInput || !stdinIsTerminal():
		fail(&cliError{Code: exitUsage, Kind: "confirmation_required", Message: "migrating is confirmed by the preview's digest; --yes does not confirm it, and nothing was changed",
			NextAction: fmt.Sprintf("ks session migrate %s --confirm %s", id, want)})
	default:
		fmt.Fprintf(os.Stderr, "type %s to migrate, or anything else to stop: ", want)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != want {
			fail(&cliError{Code: exitUsage, Kind: "confirmation_declined", Message: "not confirmed; nothing was changed"})
		}
	}
	var env struct {
		Data migrationResultDoc `json:"data"`
	}
	if err := hostedMutate(cr, "POST", "/api/v2/sessions/"+url.PathEscape(id)+"/migrate", map[string]any{"preview_digest": p.PreviewDigest}, &env); err != nil {
		var he *hostedErr
		if errors.As(err, &he) {
			switch he.Type {
			case "ks_migration_failed":
				fail(&cliError{Code: exitFailed, Kind: he.Type, Message: "the conversion failed and changed nothing: " + sanitize(he.Message), NextAction: "ks session migrate " + id + " (a new preview)"})
			case "ks_migration_preview_changed":
				fail(&cliError{Code: exitConflict, Kind: he.Type, Message: "the session changed after this preview; nothing was changed", NextAction: "ks session migrate " + id + " (review the new preview)"})
			case "ks_migration_blocked":
				fail(&cliError{Code: exitConflict, Kind: he.Type, Message: sanitize(he.Message) + "; nothing was changed", NextAction: "ks session migrate " + id})
			}
		}
		die(err)
	}
	res := env.Data
	// read back: the record says what it is now
	var rec struct {
		Data struct {
			Mode           string `json:"mode"`
			PrimaryAgentID string `json:"primary_agent_id"`
		} `json:"data"`
	}
	readBack := "the session record could not be read back"
	if err := hostedCall(cr, "GET", "/api/v2/sessions/"+url.PathEscape(id), nil, &rec); err == nil {
		readBack = fmt.Sprintf("the session record now reads mode %s, primary agent %s", sanitize(figure(rec.Data.Mode)), notRecorded(rec.Data.PrimaryAgentID))
	}
	emit(map[string]any{"result": res, "preview_digest": p.PreviewDigest, "record_mode": rec.Data.Mode, "record_primary_agent": rec.Data.PrimaryAgentID}, func() {
		fmt.Printf("session %s: %s (mode %s)\n", id, sanitize(figure(res.Outcome)), sanitize(figure(res.Mode)))
		fmt.Printf("  agent          %s\n", notRecorded(res.AgentID))
		fmt.Printf("  rollback point %s\n", notRecorded(res.RollbackPoint))
		fmt.Printf("  preserved      %d task(s), %d adviser connection(s), %d member(s)\n", res.PreservedTasks, res.PreservedGrants, res.PreservedMembers)
		fmt.Printf("  %s\n", sanitize(res.Note))
		fmt.Printf("  read back: %s\n", readBack)
		if res.Mode == "agent" && rec.Data.Mode != "" && rec.Data.Mode != "agent" {
			fmt.Printf("  WARNING: the result says agent, and the record reads %s\n", sanitize(rec.Data.Mode))
		}
	})
}
