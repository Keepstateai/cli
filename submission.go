// submission.go: sending one instruction, and knowing whether it arrived
// (KS-036).
//
// An instruction is in one of three states on this side: Draft (typed, not
// sent), Sending (sent under a submission id recorded before it left), and
// Accepted (the service returned its task receipt). When the acknowledgement
// is lost -- the connection drops, or an answer comes back that says nothing
// certain -- the client does not guess and does not blindly resend: it asks
// GET /api/v2/agents/{id}/submissions/{submission_id}.
//
//	accepted       the receipt is shown; nothing is sent again
//	not_received   the instruction is sent again under the SAME submission
//	               id, so even a late arrival of the first send is one task
//	anything else  an error: whether it arrived is unknown, and nothing is
//	               resent
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type submissionLookup struct {
	Status   string   `json:"status"`
	Task     *taskRow `json:"task"`
	Position int      `json:"queue_position"`
	Note     string   `json:"note"`
}

// sendInstruction sends body (which carries submission_id sid) to the
// agent's task route and answers the accepted task, resolving a lost
// acknowledgement through the submission lookup.
func sendInstruction(cr hostedCreds, agentID, sid string, body map[string]any) (submittedTask, bool, error) {
	path := "/api/v2/agents/" + url.PathEscape(agentID) + "/tasks"
	raw, _ := json.Marshal(body)
	key := newIdempotencyKey()
	if err := recordOperation(localOp{Key: key, Method: "POST", Path: path, BodySHA: sha256Hex(raw), CTL: cr.CTL, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return submittedTask{}, false, fmt.Errorf("could not record the submission locally before sending it (%v); nothing was sent", err)
	}
	send := func() (submittedTask, bool, error) {
		progress("sending: submission %s", sid)
		resp, rb, err := doBounded(cr, "POST", path, map[string]string{"Idempotency-Key": key}, raw)
		switch {
		case err != nil:
			return submittedTask{}, true, err
		case resp.StatusCode/100 == 2:
			var env struct {
				Data submittedTask `json:"data"`
			}
			if json.Unmarshal(rb, &env) != nil || env.Data.ID == "" {
				return submittedTask{}, true, fmt.Errorf("the service answered %s with a receipt this client could not read", resp.Status)
			}
			return env.Data, false, nil
		case definiteRefusal(resp, rb):
			return submittedTask{}, false, hostedError("POST", path, resp, rb)
		}
		return submittedTask{}, true, hostedError("POST", path, resp, rb)
	}
	t, uncertain, err := send()
	for round := 0; uncertain && round < 2; round++ {
		progress("the acknowledgement was lost (%s); asking whether submission %s arrived", sanitize(err.Error()), sid)
		var env struct {
			Data submissionLookup `json:"data"`
		}
		lp := path[:len(path)-len("/tasks")] + "/submissions/" + url.PathEscape(sid)
		resp, rb, lerr := doBounded(cr, "GET", lp, nil, nil)
		if lerr == nil && resp.StatusCode == http.StatusOK && json.Unmarshal(rb, &env) == nil {
			switch env.Data.Status {
			case "accepted":
				if env.Data.Task == nil {
					break
				}
				return submittedTask{taskRow: *env.Data.Task}, true, nil
			case "not_received":
				progress("submission %s did not arrive; sending it again under the same id", sid)
				t, uncertain, err = send()
				continue
			}
		}
		why := "the answer could not be read"
		if lerr != nil {
			why = sanitize(lerr.Error())
		} else if resp.StatusCode != http.StatusOK {
			why = sanitize(hostedError("GET", lp, resp, rb).Error())
		}
		return submittedTask{}, false, &cliError{Code: exitTemporary, Kind: "submission_unknown", WorkStarted: workUnknown,
			Message:    fmt.Sprintf("whether instruction %s arrived could not be established (%s); it was NOT sent again", sid, why),
			NextAction: "ks task list (a repeat of the same command reuses submission " + sid + ", so it can never queue a second copy)"}
	}
	if uncertain {
		return submittedTask{}, false, &cliError{Code: exitTemporary, Kind: "submission_unknown", WorkStarted: workUnknown,
			Message: fmt.Sprintf("instruction %s could not be delivered after asking twice whether it arrived: %s", sid, sanitize(err.Error()))}
	}
	return t, false, err
}
