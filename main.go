// ks — the KeepState thin client. A key to a building only we operate: every
// verb terminates at the KeepState control plane over HTTPS. No engine code
// lives here. MIT licensed; built in public CI; releases carry SHA256SUMS and
// provenance attestations, and `ks update` verifies before trusting.
package main

import (
	"fmt"
	"os"
)

// version is stamped by CI: -ldflags "-X main.version=v0.1.0"
var version = "dev"

// registry is the schema (command.go) with its handlers: the one list the
// parser dispatches from, the usage renders from, commands.json is checked
// against, and help and completion are generated from.
var registry = []*Command{
	{Path: []string{"login"}, Summary: "sign in (browser device flow)", Surface: "client",
		Effects:  "stores a device token for this machine in your config directory (0600); nothing is billed",
		Examples: []string{"ks login"},
		Flags:    []Flag{{Name: "ctl", Kind: flagString, Value: "URL", Summary: "the control plane to sign in to; default https://ctl.keepstate.ai"}},
		Nothing:  "No sign-in was started.", Run: func(inv *Invocation) {
			if err := runLogin(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"run"}, Summary: "start a hosted session; with --agent, an agent session whose main agent is Ready", Surface: "hosted",
		Effects:  "starts a session on the fleet: session time is metered from this moment until the session is killed or parked. With --agent: preflight first (nothing is created on a blocker), then one session, its primary agent and a machine; Ready is printed only when the agent reports it, a setup that does not complete is destroyed and said so, and no model is called unless --task gives it work",
		Examples: []string{"ks run", "ks run --budget-tokens 750000", "ks run --agent", "ks run --agent --name checkout --task \"run the tests\" --open"},
		Flags: []Flag{
			{Name: "image", Kind: flagString, Value: "I", Summary: "the image of a plain session; default your onboarding choice (not with --agent)"},
			{Name: "budget-tokens", Aliases: []string{"budget"}, Kind: flagInt, Positive: true, Value: "N", Summary: "the session's token budget; default per tier (500,000 free, 2,000,000 paid); zero and negative are refused"},
			{Name: "agent", Kind: flagBool, Summary: "start an agent session: a session, its primary agent and a machine, Ready only when the agent says so"},
			{Name: "name", Kind: flagString, Value: "NAME", Summary: "the agent session's name (with --agent); default run-<UTC time>"},
			{Name: "agent-name", Kind: flagString, Value: "NAME", Summary: "the primary agent's name (with --agent); default main"},
			{Name: "task", Kind: flagString, Value: "TEXT", Summary: "a first task for the agent, submitted after Ready (with --agent)"},
			{Name: "open", Kind: flagBool, Summary: "open the agent's window after Ready (with --agent)"},
		},
		Nothing: "No session was started.", Run: hosted(hostedRun)},
	{Path: []string{"checkpoint"}, Aliases: [][]string{{"save"}}, Summary: "save the session (durably); --stop parks it", Surface: "hosted",
		Effects:  "writes a checkpoint: storage is metered from this point; with --stop the session parks and session time stops",
		Examples: []string{"ks checkpoint <session>", "ks checkpoint <session> --stop"},
		Args:     []Arg{{Name: "session", Required: true}},
		Flags:    []Flag{{Name: "stop", Kind: flagBool, Summary: "park the session after the save succeeds"}},
		Nothing:  "No checkpoint was taken.", Run: hosted(hostedCheckpoint)},
	{Path: []string{"wake"}, Aliases: [][]string{{"resume"}}, Summary: "resume a parked session (mid-sentence)", Surface: "hosted",
		Effects:  "restores the last checkpoint on the fleet: session time is metered again",
		Examples: []string{"ks wake <session>"},
		Args:     []Arg{{Name: "session", Required: true}},
		Nothing:  "Nothing was resumed.", Run: hosted(hostedWake)},
	{Path: []string{"kill"}, Summary: "destroy a session (guarded: checkpoint first)", Surface: "hosted",
		Effects:  "destroys the running machine; refused without a saved checkpoint unless --force; session time stops",
		Examples: []string{"ks kill <session>", "ks kill <session> --force"},
		Args:     []Arg{{Name: "session", Required: true}},
		Flags:    []Flag{{Name: "force", Kind: flagBool, Summary: "destroy it even without a saved checkpoint"}},
		Nothing:  "Nothing was destroyed.", Run: hosted(hostedKill)},
	{Path: []string{"session"}, Group: true, Summary: "the session inventory", Surface: "hosted"},
	{Path: []string{"session", "list"}, Aliases: [][]string{{"ls"}}, Summary: "your sessions, newest activity first: state, agent, task, last activity, last save", Surface: "hosted",
		Flags:    []Flag{{Name: "state", Kind: flagString, Value: "STATE", Summary: "only running, parked or dead"}, {Name: "all", Kind: flagBool, Summary: "include dead sessions"}},
		Needs:    "session.list",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks session list", "ks ls --state running", "ks session list --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionList)},
	{Path: []string{"session", "show"}, Summary: "one session in full; a short id is accepted when it is unique among yours", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Needs:    "session.list",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks session show 3f2a1c", "ks session show <session> --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionShow)},
	{Path: []string{"session", "use"}, Summary: "bind this project to a session, so commands without --session mean it: per account, project root and control plane", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: false}},
		Flags: []Flag{
			{Name: "clear", Kind: flagBool, Summary: "remove this project's binding"},
			{Name: "trust-repo", Kind: flagBool, Summary: "adopt the session this repository's .keepstate/session.json names, once you confirm it with --confirm"},
			{Name: "confirm", Kind: flagString, Value: "SESSION", Summary: "the id of the session the repository names; required with --trust-repo, never implied by --yes"},
		},
		Needs:    "session.list",
		Effects:  "with a session: records a binding in the client's own configuration (never in the project, never uploaded), keyed by the signed-in account, the canonical project root and the control plane's origin, so it applies nowhere else; with no argument: shows the binding and what the repository suggests, changing nothing; --clear removes the binding. Nothing on the service changes. A repository's own file is never used until confirmed",
		Examples: []string{"ks session use 3f2a1c", "ks session use", "ks session use --clear", "ks session use --trust-repo --confirm 3f2a1c"},
		Nothing:  "Nothing was bound.", Run: hosted(hostedSessionUse)},
	{Path: []string{"session", "rename"}, Summary: "rename a session by id; its id and everything in it are unchanged", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}, {Name: "new-name", Required: true}},
		Needs:    "api.v2.workspace",
		Effects:  "shows the session and the name it will get, then renames the session record against the revision just read; a record that changed in between is refused and nothing is renamed",
		Examples: []string{"ks session rename 3f2a1c checkout-v2"},
		Nothing:  "Nothing was renamed.", Run: hosted(hostedSessionRename)},
	{Path: []string{"session", "usage"}, Summary: "a session's model use, runtime and storage, apart, with units, source and whether each figure is actual, estimated or unavailable", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Needs:    "budget.admission",
		Effects:  "reads the session's usage ledger; nothing changes. A figure the service does not have reads unavailable, never $0",
		Examples: []string{"ks session usage 3f2a1c", "ks session usage 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionUsage)},
	{Path: []string{"session", "fork"}, Summary: "plan a fork of a named saved point, then execute exactly that plan by its digest", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "plan", Kind: flagBool, Summary: "plan only: what the fork would do and cost; creates nothing"},
			{Name: "checkpoint", Kind: flagString, Value: "CHECKPOINT", Summary: "the saved point to branch (with --plan); never chosen for you"},
			{Name: "children", Kind: flagInt, Positive: true, Value: "N", Summary: "how many children (with --plan; 1 to 8, default 1)"},
			{Name: "execute", Kind: flagString, Value: "DIGEST", Summary: "execute the plan with this digest"},
			{Name: "plan-id", Kind: flagString, Value: "ID", Summary: "the plan the digest belongs to (with --execute)"},
		},
		Needs:    "agent.workspace",
		Effects:  "--plan creates nothing and shows the saved point, each child's cost, the key bindings that survive re-authorization, the pending instructions that become held templates and what is not inherited (approvals, adviser connections). --execute creates the planned children -- each a new session metered on its own -- or refuses if the session or saved point moved since the plan",
		Examples: []string{"ks session fork 3f2a1c --plan --checkpoint ck_0123 --children 2", "ks session fork 3f2a1c --execute sha256:ab12... --plan-id fplan_0123"},
		Nothing:  "Nothing was forked.", Run: hosted(hostedSessionFork)},
	{Path: []string{"session", "restore"}, Summary: "restore a session from a named saved point, and say exactly what continues from it", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "checkpoint", Kind: flagString, Value: "CHECKPOINT", Summary: "the saved point; never chosen for you"},
			{Name: "reason", Kind: flagString, Value: "TEXT", Summary: "why, recorded with the restore (required)"},
			{Name: "accept-affected", Kind: flagString, Value: "DIGEST", Summary: "consent to the exact list of work the service said the restore affects"},
		},
		Needs:    "agent.workspace",
		Effects:  "a WHOLE-SESSION restore: replaces the guest with the saved point (irreversible), rotates the execution generation and holds the queue; the authoritative records are not rolled back. Prints the service's continuation (exact_runtime, none or unknown) exactly as given; an unknown outcome exits as unknown, never as success",
		Examples: []string{"ks session restore 3f2a1c --checkpoint ck_0123 --reason \"the build broke after this point\""},
		Nothing:  "Nothing was restored.", Run: hosted(hostedSessionRestore)},
	{Path: []string{"session", "idle"}, Summary: "a session's idle policy and where it stands: counting, warned with the park time and countdown, deferred with the reason", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Needs:    "agent.workspace",
		Effects:  "reads the idle policy and state; nothing changes. An idle agent session is saved and paused, never killed",
		Examples: []string{"ks session idle 3f2a1c"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionIdle)},
	{Path: []string{"session", "delete"}, Summary: "delete a session record through a plan; the runtime's stop and the content's standing are reported apart", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "plan", Kind: flagBool, Summary: "plan only: what deletion would remove, stop and retain; changes nothing"},
			{Name: "execute", Kind: flagString, Value: "PLAN", Summary: "delete by this plan"},
			{Name: "confirm", Kind: flagString, Value: "SESSION", Summary: "the session's id, confirming the deletion (never implied by --yes)"},
		},
		Needs:    "deletion.plans",
		Effects:  "--plan changes nothing. --execute removes the session's live records (agents, queued work, approvals, connections, results) in one commit and stops its runtime for good through a recorded cleanup operation; saved content is retained under the published policy, not erased, and the answer says which is which. Forks are separate sessions and are not deleted",
		Examples: []string{"ks session delete 3f2a1c --plan", "ks session delete 3f2a1c --execute dplan_0123 --confirm 3f2a1c"},
		Nothing:  "Nothing was deleted.", Run: hosted(hostedSessionDelete)},
	{Path: []string{"session", "checkpoint-policy"}, Summary: "a session's automatic-save policy and where it stands; --on/--off changes it against the session's revision", Surface: "hosted",
		Args: []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "on", Kind: flagBool, Summary: "turn automatic saves on"},
			{Name: "off", Kind: flagBool, Summary: "turn automatic saves off"},
			{Name: "interval", Kind: flagInt, Positive: true, Value: "MINUTES", Summary: "how often, with --on (5 to 120; default the service's 5)"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads the policy (off, idle, scheduled or a pending save and why), its certification standing and its storage effect; --on/--off changes only the policy, erasing nothing and saving nothing now; automatic saves are off by default because no runner is certified for safe capture",
		Examples: []string{"ks session checkpoint-policy 3f2a1c", "ks session checkpoint-policy 3f2a1c --on --interval 15"},
		Nothing:  "Nothing changed.", Run: hosted(hostedCheckpointPolicy)},
	{Path: []string{"session", "checkpoints"}, Summary: "the saved points a restore may be named against: boundary, state, scope and manifest identity", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Needs:    "agent.workspace",
		Effects:  "reads the saved points the service recorded for this session, newest first, with the boundary each was taken at, whether it is still restorable, and what it covers. Nothing is restored, nothing is chosen and nothing changes. A KeepState saved point is a WHOLE-SESSION saved point and this says so rather than letting a reader assume it rolls back one instruction",
		Examples: []string{"ks session checkpoints 3f2a1c", "ks session checkpoints 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedSessionCheckpoints)},
	{Path: []string{"session", "key", "set"}, Summary: "bind a session's provider to one of your keys by id; that key is used or nothing is", Surface: "hosted",
		Args:     []Arg{{Name: "session", Required: true}},
		Flags:    []Flag{{Name: "key", Kind: flagString, Value: "KEY", Summary: "the key's id or alias"}},
		Needs:    "keys.inventory",
		Effects:  "changes the session's key binding at its next safe boundary; no key is substituted",
		Examples: []string{"ks session key set 3f2a1c --key vlt_0123abcd"},
		Nothing:  "Nothing was bound.", Run: hosted(hostedSessionKeySet)},
	{Path: []string{"project"}, Group: true, Summary: "project defaults: proposed from the repository, trusted by you", Surface: "hosted"},
	{Path: []string{"project", "configure"}, Summary: "propose .keepstate/project.json as the project's defaults, see the exact changes, and trust that digest only if you confirm it", Surface: "hosted",
		Flags: []Flag{
			{Name: "project", Kind: flagString, Value: "ID", Summary: "the project (default: the project of --session or of this project's binding)"},
			{Name: "session", Kind: flagString, Value: "ID", Summary: "a session of the project; without it, this project's binding (ks session use)"},
			{Name: "file", Kind: flagString, Value: "PATH", Summary: "the defaults file (default .keepstate/project.json at the project root)"},
			{Name: "confirm", Kind: flagString, Value: "DIGEST", Summary: "trust the proposal with this digest (the 12 characters shown, or all of it); never implied by --yes"},
		},
		Needs:    "agent.workspace",
		Effects:  "sends the file as a PROPOSAL (refused here, unsent, if anything in it looks like key material) and shows the exact changes it would make; the proposal authorizes nothing. Only a confirmed digest is trusted, and then applies to sessions created afterwards",
		Examples: []string{"ks project configure", "ks project configure --project proj_0123 --confirm 3f2a1c9b8d7e"},
		Nothing:  "Nothing was proposed.", Run: hosted(hostedProjectConfigure)},
	{Path: []string{"key"}, Group: true, Summary: "your provider keys: never the secret", Surface: "hosted"},
	{Path: []string{"key", "list"}, Summary: "your keys: id, provider, alias, last four characters, enabled or disabled", Surface: "hosted",
		Needs:    "keys.inventory",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks key list", "ks key list --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedKeyList)},
	{Path: []string{"key", "show"}, Summary: "one key's metadata", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "reads the inventory; nothing changes",
		Examples: []string{"ks key show vlt_0123abcd"},
		Nothing:  "Nothing was read.", Run: hosted(hostedKeyShow)},
	{Path: []string{"key", "add"}, Summary: "store a provider key read from standard input; it is never an argument and never comes back", Surface: "hosted",
		Flags:    []Flag{{Name: "provider", Kind: flagString, Value: "PROVIDER", Summary: "anthropic, openai or openrouter"}, {Name: "alias", Kind: flagString, Value: "NAME", Summary: "a name for the key"}},
		Needs:    "keys.inventory",
		Effects:  "stores the key on the service, sealed; a key for a provider you already hold is rotated in place and keeps its id",
		Examples: []string{"ks key add --provider anthropic --alias prod < key.txt"},
		Nothing:  "Nothing was stored.", Run: hosted(hostedKeyAdd)},
	{Path: []string{"key", "enable"}, Summary: "allow new admissions with a key", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "changes the key's standing",
		Examples: []string{"ks key enable vlt_0123abcd"},
		Nothing:  "Nothing changed.", Run: hosted(hostedKeySetEnabled(true))},
	{Path: []string{"key", "disable"}, Summary: "refuse new admissions with a key; in-flight usage is reconciled", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "changes the key's standing; sessions bound to it admit nothing until rebound",
		Examples: []string{"ks key disable vlt_0123abcd"},
		Nothing:  "Nothing changed.", Run: hosted(hostedKeySetEnabled(false))},
	{Path: []string{"key", "delete"}, Summary: "plan a key deletion, then execute it with --yes", Surface: "hosted",
		Args:     []Arg{{Name: "key", Required: true}},
		Needs:    "keys.inventory",
		Effects:  "without --yes: a plan, nothing deleted; with --yes (the global flag): the secret is destroyed and the bindings that named it are dropped",
		Examples: []string{"ks key delete vlt_0123abcd", "ks key delete vlt_0123abcd --yes"},
		Nothing:  "Nothing was deleted.", Run: hosted(hostedKeyDelete)},
	{Path: []string{"adviser"}, Group: true, Summary: "connections that let one of your agents consult another: made and withdrawn by you", Surface: "hosted"},
	{Path: []string{"adviser", "connect"}, Summary: "let an agent consult another agent of yours, after you have seen both ends and confirmed the adviser by name", Surface: "hosted",
		Args: []Arg{{Name: "adviser", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session of the agent that will ask; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "agent", Kind: flagString, Value: "NAME", Summary: "the agent that will ask (default main)"},
			{Name: "confirm", Kind: flagString, Value: "SESSION/AGENT", Summary: "confirm the adviser without a prompt; it must name the adviser shown, as session/agent or by its id"},
			{Name: "expires", Kind: flagString, Value: "DURATION", Summary: "how long the connection lasts (default 24h, at most 720h)"},
			{Name: "max-consultations", Kind: flagInt, Value: "N", Summary: "how many consultations the connection carries (default 50)"},
			{Name: "max-tokens", Kind: flagInt, Value: "N", Summary: "input plus output tokens admitted per consultation (default 16384)"},
			{Name: "max-output-tokens", Kind: flagInt, Value: "N", Summary: "output tokens admitted per consultation (default 4096)"},
		},
		Needs: "advisers.connect", Fallback: "agent.workspace",
		Effects:  "shows both ends by session and agent, the scope, what is shared (the question only), the limits, the expiry and whose session pays for the advice, then records one connection once you confirm the adviser by name. --yes does not confirm a connection; --no-input without --confirm connects nothing. Nothing is sent to either agent",
		Examples: []string{"ks adviser connect reviewer/main --session 3f2a1c", "ks adviser connect reviewer/main --session 3f2a1c --confirm reviewer/main --json"},
		Nothing:  "Nothing was connected.", Run: hosted(hostedAdviserConnect)},
	{Path: []string{"adviser", "list"}, Summary: "an agent's adviser connections, live, revoked and expired, in both directions", Surface: "hosted",
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "agent", Kind: flagString, Value: "NAME", Summary: "the agent (default main)"},
		},
		Needs: "advisers.connect", Fallback: "agent.workspace",
		Effects:  "reads the connections into and out of one agent; nothing changes",
		Examples: []string{"ks adviser list --session 3f2a1c", "ks adviser list --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedAdviserList)},
	{Path: []string{"adviser", "disconnect"}, Summary: "withdraw a connection: every future consultation over it stops at once", Surface: "hosted",
		Args:  []Arg{{Name: "grant", Required: true}},
		Needs: "advisers.connect", Fallback: "agent.workspace",
		Effects:  "revokes the connection; consultations over it stop at once, and advice already delivered is not recalled",
		Examples: []string{"ks adviser disconnect grant_0123abcd"},
		Nothing:  "Nothing was withdrawn.", Run: hosted(hostedAdviserDisconnect)},
	{Path: []string{"approval"}, Group: true, Summary: "permission requests your agents are waiting on: read here, decided with ks agent approve / deny", Surface: "hosted"},
	{Path: []string{"approval", "list"}, Summary: "a session's permission requests with the instruction each was asked for, what it touches and what deciding costs", Surface: "hosted",
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the request belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "all", Kind: flagBool, Summary: "every request, decided ones too, with who decided and when"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads the requests; nothing is decided and nothing changes. The pending ones printed are recorded as shown, so a decision made afterwards is compared against exactly this display",
		Examples: []string{"ks approval list --session 3f2a1c", "ks approval list --session 3f2a1c --all --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedApprovalList)},
	{Path: []string{"approval", "show"}, Summary: "one permission request in full: the action, its instruction, what it touches, the cost, expiry and decision", Surface: "hosted",
		Args:     []Arg{{Name: "request", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the request belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "reads one request; nothing is decided and nothing changes. A pending request printed is recorded as shown, so deciding it afterwards is compared against this display",
		Examples: []string{"ks approval show apr_7f3c --session 3f2a1c"},
		Nothing:  "Nothing was read.", Run: hosted(hostedApprovalShow)},
	{Path: []string{"agent"}, Group: true, Summary: "the agents inside a session", Surface: "hosted"},
	{Path: []string{"agent", "list"}, Summary: "the agents of a session: name, id, activity, role, current task", Surface: "hosted",
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agents live in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "reads the session's agents; nothing changes",
		Examples: []string{"ks agent list --session 3f2a1c", "ks agent list --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedAgentList)},
	{Path: []string{"agent", "open"}, Summary: "the agent window: its state, then its events as they happen", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "take-control", Kind: flagBool, Summary: "take the control lease from the window that holds it; without it a held agent is refused, never taken"},
			{Name: "view", Kind: flagBool, Summary: "open a watching window: it follows the agent and steers nothing, so it opens beside a window that holds control and never takes it"},
			{Name: "no-follow", Kind: flagBool, Summary: "print the window's current state and exit, following nothing"},
			{Name: "resume", Kind: flagBool, Summary: "if the session is saved and stopped, resume it (runtime is charged again) and then open; without it a stopped session is never woken"},
			{Name: "project", Kind: flagString, Value: "PROJECT", Summary: "narrow a search by name to one project (without --session)"},
		},
		Needs: "agents.open", Fallback: "agent.workspace",
		Effects:  "opens (or rejoins) a window on an agent that already exists and follows its events: it creates no agent, queues nothing by itself; Ctrl-C interrupts the instruction in flight (a stop request through the cancel route, shown, never text to the agent) and q leaves the window while the agent keeps working; with --take-control the control lease moves here and the other window keeps watching. Inside the window, \"a <id>\" approves a permission request and \"d <id>\" denies it, and any other line is sent to the agent as an instruction and metered like the session; a window that holds control sends instructions under that control, and a window that has lost it sends nothing at all until it is reopened with --take-control (--no-input reads nothing typed into the window)",
		Examples: []string{"ks agent open main --session 3f2a1c", "ks agent open main --session 3f2a1c --no-follow", "ks agent open main --session 3f2a1c --take-control", "ks agent open main --session 3f2a1c --view"},
		Nothing:  "No window was opened.", Run: hosted(hostedAgentOpen)},
	{Path: []string{"agent", "approve"}, Aliases: [][]string{{"approval", "approve"}}, Summary: "approve one permission request an agent is waiting on, by its id", Surface: "hosted",
		Args:     []Arg{{Name: "request", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the request belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "approves the exact action that was shown for this request: it reads the request again and compares it with what you were shown, and only a request that still reads the same is approved, so the agent proceeds with that action and its work is metered like the session; a request whose action changed since it was shown is displayed and refused without sending any decision, and one that expired or was already decided is refused too — in every case nothing is approved and you are asked again",
		Examples: []string{"ks agent approve apr_7f3c --session 3f2a1c"},
		Nothing:  "Nothing was approved.", Run: hosted(hostedAgentDecide("approve"))},
	{Path: []string{"agent", "deny"}, Aliases: [][]string{{"approval", "deny"}}, Summary: "deny one permission request an agent is waiting on, by its id", Surface: "hosted",
		Args:     []Arg{{Name: "request", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the request belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "denies the exact action that was shown for this request: it reads the request again and compares it with what you were shown, and only a request that still reads the same is denied, so the agent is refused that action and carries on without it; a request whose action changed since it was shown is displayed and refused without sending any decision, and one that expired or was already decided is refused too — in every case nothing is denied and you are asked again",
		Examples: []string{"ks agent deny apr_7f3c --session 3f2a1c"},
		Nothing:  "Nothing was denied.", Run: hosted(hostedAgentDecide("deny"))},
	{Path: []string{"agent", "pause"}, Summary: "save the session an agent lives in and park it, waiting until it is parked", Surface: "hosted",
		Args:     []Arg{{Name: "name", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "saves the session durably and parks it: every agent in it stops, storage is metered from the save, and session time stops once it reads parked; the save is safe before the stopping finishes, and this waits for the stopping within the bound (--wait-timeout) rather than reporting it as done",
		Examples: []string{"ks agent pause main --session 3f2a1c", "ks agent pause main --session 3f2a1c --wait-timeout 5m"},
		Nothing:  "Nothing was paused.", Run: hosted(hostedAgentPause)},
	{Path: []string{"agent", "resume"}, Summary: "resume the parked session an agent lives in, waiting out a save that is still completing", Surface: "hosted",
		Args:     []Arg{{Name: "name", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "restores the session's last save on the fleet and its agents carry on: session time is metered again; while a previous save is still completing nothing is resumed, and this waits for that save to finish within the bound (--wait-timeout) and then resumes",
		Examples: []string{"ks agent resume main --session 3f2a1c"},
		Nothing:  "Nothing was resumed.", Run: hosted(hostedAgentResume)},
	{Path: []string{"agent", "create"}, Summary: "create the primary agent of a session that has none", Surface: "hosted",
		Args:     []Arg{{Name: "name", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "records one agent as the session's primary (the target is shown first); nothing runs and nothing is queued. A session that already has a primary is refused: another agent lives in its own session (ks run --agent)",
		Examples: []string{"ks agent create main --session 3f2a1c"},
		Nothing:  "Nothing was created.", Run: hosted(hostedAgentCreate)},
	{Path: []string{"agent", "remove"}, Summary: "remove an agent through a plan: what it cancels and keeps, then the removal naming that plan", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "plan", Kind: flagBool, Summary: "plan only: what removal would cancel, release and keep; changes nothing"},
			{Name: "execute", Kind: flagString, Value: "PLAN", Summary: "remove the agent by this plan"},
		},
		Needs:    "agent.workspace",
		Effects:  "--plan changes nothing. --execute cancels the agent's pending instructions, asks running ones to stop, releases its control and keeps the session, its files, saved points, journal and results; a plan that went stale or expired removes nothing. An agent with work in flight is refused with that work named: stop it first (ks agent stop)",
		Examples: []string{"ks agent remove helper --session 3f2a1c --plan", "ks agent remove helper --session 3f2a1c --execute dplan_0123"},
		Nothing:  "Nothing was removed.", Run: hosted(hostedAgentRemove)},
	{Path: []string{"agent", "stop"}, Summary: "stop an agent: its instruction in flight is asked to stop and its queue is held; the session keeps running", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "confirm", Kind: flagString, Value: "NAME", Summary: "confirm by naming the agent (required without a terminal; never implied by --yes)"},
			{Name: "reason", Kind: flagString, Value: "TEXT", Summary: "why, recorded with the stop"},
		},
		Needs: "agents.stop", Fallback: "agent.workspace",
		Effects:  "asks the instruction in flight to stop at a safe boundary (never claimed stopped) and holds the agent's queue so nothing further starts until a person releases it. The session is preserved and keeps running, so runtime and storage may still be charged; Save and pause (ks agent pause) is offered as a separate action and never assumed. Not leaving a window (Ctrl-C), not interrupting one instruction (ks task cancel), not parking",
		Examples: []string{"ks agent stop main --session 3f2a1c", "ks agent stop main --session 3f2a1c --confirm main --reason \"wrong approach\""},
		Nothing:  "Nothing was stopped.", Run: hosted(hostedAgentStop)},
	{Path: []string{"agent", "view"}, Summary: "the live window's model: header, runner, footer counts and the KS menu, as the service gives them, within 80x24", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "choose", Kind: flagInt, Positive: true, Value: "N", Summary: "perform menu item N through the request the service names for it"},
			{Name: "confirm", Kind: flagString, Value: "CHOICE", Summary: "the dialog choice that performs an item that changes the session (never preselected, never implied by --yes)"},
		},
		Needs: "agents.view", Fallback: "agent.workspace",
		Effects:  "reads the window's model; nothing changes. With --choose, performs one menu item exactly through the service request it names: reads for the read items, and for Save and pause or Take control only after --confirm names the dialog's confirming choice",
		Examples: []string{"ks agent view main --session 3f2a1c", "ks agent view main --session 3f2a1c --choose 2", "ks agent view main --session 3f2a1c --choose 6 --confirm \"Save and pause\""},
		Nothing:  "Nothing was done.", Run: hosted(hostedAgentView)},
	{Path: []string{"agent", "logs"}, Summary: "the session's setup log and its agent's log from a cursor, safe to print; --follow polls until the service says it ends", Surface: "hosted",
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "follow", Kind: flagBool, Summary: "keep reading new lines from the cursor until the service says following ends (a session that pauses ends it) or Ctrl-C"},
			{Name: "cursor", Kind: flagString, Value: "CURSOR", Summary: "start after this cursor (from a previous page)"},
			{Name: "limit", Kind: flagInt, Positive: true, Value: "N", Summary: "at most N lines per page"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads log lines the service redacted, redacts again and shows escape sequences as text; an agent log that is absent, stale or unavailable and any gap are said so. Nothing changes: following never wakes a session, and Ctrl-C only stops this reader",
		Examples: []string{"ks agent logs --session 3f2a1c", "ks agent logs --session 3f2a1c --follow", "ks agent logs --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedAgentLogs)},
	{Path: []string{"agent", "rename"}, Summary: "rename an agent by id; its tasks, approvals and adviser connections keep resolving", Surface: "hosted",
		Args:     []Arg{{Name: "name", Required: true}, {Name: "new-name", Required: true}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "shows the agent, its session and the name it will get, then renames it against the revision just read; a label change only: everything that refers to the agent does so by id. A record that changed in between is refused and nothing is renamed",
		Examples: []string{"ks agent rename main lead --session 3f2a1c", "ks agent rename agent_0123abcd reviewer --session 3f2a1c --json"},
		Nothing:  "Nothing was renamed.", Run: hosted(hostedAgentRename)},
	{Path: []string{"agent", "status"}, Summary: "one agent's activity, queue depth and current task", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "watch", Kind: flagBool, Summary: "read it again every 2 seconds until Ctrl-C, which stops watching and changes nothing"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads the agent and its queue; nothing changes, and no control is taken",
		Examples: []string{"ks agent status main --session 3f2a1c", "ks agent status main --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedAgentStatus)},
	{Path: []string{"agent", "tell"}, Summary: "queue one instruction for an agent from outside any window", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}, {Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "resume", Kind: flagBool, Summary: "if the session is parked, resume it after the instruction is accepted; without it a parked session is never woken and the instruction waits, held"},
		},
		Needs:    "agent.workspace",
		Effects:  "adds one instruction to the agent's queue and the agent's work on it is metered like the session; the instruction is submitted once, under an id recorded locally before it is sent, so a repeat is the same submission and never a second one. This command holds no window and therefore no control of the agent: the instruction is submitted on its own, it does not steer or displace the window that holds control, and it never claims to (to send an instruction as the controller, type it into ks agent open). An instruction for a session that is not running is accepted and HELD and the session is not woken; only --resume, or ks agent resume, starts it",
		Examples: []string{"ks agent tell main \"run the tests\" --session 3f2a1c"},
		Nothing:  "Nothing was queued.", Run: hosted(hostedAgentTell)},
	{Path: []string{"agent", "queue", "show"}, Summary: "why an agent's queue is not moving: what blocks it, what waits behind it, what each choice would do", Surface: "hosted",
		Args:  []Arg{{Name: "name", Required: true}},
		Flags: []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs: "tasks.cancel", Fallback: "agent.workspace",
		Effects:  "reads the agent's queue and the hold on it: which instruction failed or was left unresolved, what it last reported, what is waiting behind it in the order it was committed, and what each available decision would do, cost and leave behind. Nothing is decided, nothing is started and nothing changes. Everything shown comes from what the service recorded: where it recorded nothing, this says so rather than filling the gap, and what you are shown here is what a later resume or hold is compared against",
		Examples: []string{"ks agent queue show main --session 3f2a1c", "ks agent queue show main --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedAgentQueueShow)},
	{Path: []string{"agent", "queue", "list"}, Summary: "an agent's pending work in dispatch order, the instruction executing now apart, and the queue revision a move names", Surface: "hosted",
		Args:  []Arg{{Name: "name", Required: true}},
		Flags: []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs: "tasks.queue_edit", Fallback: "agent.workspace",
		Effects:  "reads the pending instructions: position, state, held reason, submitter, age and -- only where the service returns it, which needs the operator role -- the instruction's first line. Nothing changes",
		Examples: []string{"ks agent queue list main --session 3f2a1c", "ks agent queue list main --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedAgentQueueList)},
	{Path: []string{"agent", "queue", "cancel"}, Summary: "cancel an agent's pending instructions in bulk: previewed first, then confirmed by naming the preview's queue revision", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "preview", Kind: flagBool, Summary: "show exactly what would be cancelled, and the queue revision a confirmation must name; changes nothing"},
			{Name: "confirm", Kind: flagString, Value: "QREV", Summary: "cancel exactly what the preview at this queue revision showed; if the queue changed since, nothing is cancelled"},
			{Name: "states", Kind: flagString, Value: "STATES", Summary: "queued, held, or queued,held (default both)"},
			{Name: "reason", Kind: flagString, Value: "TEXT", Summary: "why, recorded with each cancellation"},
		},
		Needs: "tasks.queue_edit", Fallback: "agent.workspace",
		Effects:  "with --preview: reads what would be cancelled, nothing changes. With --confirm: cancels exactly the previewed pending instructions before anything executes them, each journalled with its content kept, or -- if the queue changed since the preview -- none, showing the current preview and retrying nothing. The instruction executing now is never included",
		Examples: []string{"ks agent queue cancel main --session 3f2a1c --preview", "ks agent queue cancel main --session 3f2a1c --confirm qrev_0123"},
		Nothing:  "Nothing was cancelled.", Run: hosted(hostedAgentQueueCancel)},
	{Path: []string{"agent", "queue", "resume"}, Summary: "continue the instructions held behind a failed or unresolved one; that one is never restarted", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "finding", Kind: flagString, Value: "TEXT", Summary: "what you established about the blocked instruction; required, and recorded with the decision"},
		},
		Needs: "tasks.cancel", Fallback: "agent.workspace",
		Effects:  "returns the held instructions to the agent's queue in the order they were committed, so a worker may start them and their work is metered like the session. The instruction that blocked them is NOT restarted and its recorded outcome is not changed: continuing the rest of the queue and running failed or unresolved work again are different actions, and this verb is only the first. The decision is bound to what you were shown — the hold is read again and compared immediately before anything is sent, and a queue that changed since it was displayed (its blocked instruction resolved, different work waiting behind it, the session restored underneath it) is displayed and refused without sending any decision at all",
		Examples: []string{"ks agent queue resume main --session 3f2a1c --finding \"checked by hand: the publish never happened\""},
		Nothing:  "Nothing was resumed.", Run: hosted(hostedQueueDecision(decisionResume))},
	{Path: []string{"agent", "queue", "hold"}, Summary: "record that an agent's queue stays held, and what you established", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "finding", Kind: flagString, Value: "TEXT", Summary: "what you established about the blocked instruction; required, and recorded with the decision"},
		},
		Needs: "tasks.cancel", Fallback: "agent.workspace",
		Effects:  "records the decision to leave the queue held, with what you established. Nothing is released, nothing is started and nothing is metered; the held instructions keep their id, their committed order and their content. A queue that waits because somebody decided it should is a different record from a queue nobody has looked at, and this is how the first one gets written. The same comparison applies as for a resume: a queue that changed since it was displayed is shown and refused without sending any decision",
		Examples: []string{"ks agent queue hold main --session 3f2a1c --finding \"the release may have gone out; waiting on the registry\""},
		Nothing:  "Nothing was decided.", Run: hosted(hostedQueueDecision(decisionKeep))},
	{Path: []string{"task"}, Group: true, Summary: "the instructions in an agent's queue", Surface: "hosted"},
	{Path: []string{"task", "list"}, Aliases: [][]string{{"task", "ls"}}, Summary: "the instructions of a session's agents, in the order they were committed", Surface: "hosted",
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session whose instructions to read; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "agent", Kind: flagString, Value: "NAME", Summary: "read one agent's queue instead of every agent's"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads the instructions the service recorded for each agent: id, queue position, state, origin, author and when it was submitted. Nothing is decided, nothing is started, nothing is metered and nothing changes. A queue that could not be READ is named as unreadable and is never reported as an empty queue, and the command exits as a failure when any queue is missing from what it showed",
		Examples: []string{"ks task list --session 3f2a1c", "ks task list --session 3f2a1c --agent main --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedTaskList)},
	{Path: []string{"task", "move"}, Summary: "move one queued instruction before or after another pending one of the same agent, against a queue revision", Surface: "hosted",
		Args: []Arg{{Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "before", Kind: flagString, Value: "TASK", Summary: "place it just before this pending instruction"},
			{Name: "after", Kind: flagString, Value: "TASK", Summary: "place it just after this pending instruction"},
			{Name: "queue-revision", Kind: flagString, Value: "QREV", Summary: "the queue revision of the view you decided from (ks agent queue list prints it); without it the queue is read now and shown first"},
		},
		Needs: "tasks.queue_edit", Fallback: "agent.workspace",
		Effects:  "reorders the pending queue once: only a queued instruction moves, only among pending ones, and an executing or settled one is never edited. If the queue changed since the revision the move names, nothing moves, the refreshed positions are printed and nothing is retried",
		Examples: []string{"ks task move tsk_9c21 --before tsk_77ab --session 3f2a1c --queue-revision qrev_0123", "ks task move tsk_9c21 --after tsk_77ab --session 3f2a1c"},
		Nothing:  "Nothing was moved.", Run: hosted(hostedTaskMove)},
	{Path: []string{"task", "replace"}, Summary: "replace one pending instruction: it is cancelled with its content kept and a new one takes its place, against a queue revision", Surface: "hosted",
		Args: []Arg{{Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "text", Kind: flagString, Value: "TEXT", Summary: "the replacement instruction (at most 64 KiB)"},
			{Name: "queue-revision", Kind: flagString, Value: "QREV", Summary: "the queue revision of the view you decided from; without it the queue is read now and shown first"},
			{Name: "reason", Kind: flagString, Value: "TEXT", Summary: "why, recorded with the replacement"},
		},
		Needs: "tasks.queue_edit", Fallback: "agent.workspace",
		Effects:  "in one commit the pending instruction is cancelled (its content kept, never edited) and a new instruction naming it is queued in its place and metered like the session when it runs; submitted once under an id recorded locally first, so a repeat is the same replacement. An instruction that has started is never replaced, and a queue that changed since the revision named is refused with the refreshed positions and not retried",
		Examples: []string{"ks task replace tsk_9c21 --text \"run only the unit tests\" --session 3f2a1c --queue-revision qrev_0123"},
		Nothing:  "Nothing was replaced.", Run: hosted(hostedTaskReplace)},
	{Path: []string{"task", "show"}, Summary: "one instruction in full: where it sits, what it says, what it last reported and what attempt exists", Surface: "hosted",
		Args: []Arg{{Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "refuse unless the instruction belongs to this session; optional, and only ever a guard"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads one instruction, its exact words from the content store, and whether it is what holds its agent's queue. Nothing is decided, nothing is started, nothing is metered and nothing changes. Where the service recorded nothing — no held reason, no independent verification, no attempt — this says so in those words rather than filling the gap, and it never reports an instruction as verified on the strength of it having finished",
		Examples: []string{"ks task show tsk_9c21", "ks task show tsk_9c21 --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedTaskShow)},
	{Path: []string{"task", "cancel"}, Summary: "end one instruction: it will not run, and the instructions held behind it stay held", Surface: "hosted",
		Args: []Arg{{Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the instruction belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "reason", Kind: flagString, Value: "TEXT", Summary: "why you are cancelling it; optional, and recorded with the request"},
		},
		Needs: "tasks.cancel", Fallback: "agent.workspace",
		Effects:  "asks the service to end ONE instruction. Queued or held work is prevented from ever dispatching, with no runner execution and no model call; active work has interruption REQUESTED through the supported runner integration, and reads `cancelling` until the real stopping outcome is known. It never claims a tool's or a provider's completed effects were undone, and unresolved effects are printed as unresolved. It is bound to the revision that was read and to the execution generation it was prepared under. It does not release the instructions held behind it, does not retry anything, does not delete the session and does not park the machine; if the instruction finished while the request was in flight, the outcome that genuinely won the race is what is reported",
		Examples: []string{"ks task cancel tsk_9c21 --session 3f2a1c", "ks task cancel tsk_9c21 --session 3f2a1c --finding \"superseded by the rollback\""},
		Run:      hosted(hostedTaskCancel)},
	{Path: []string{"task", "reconcile"}, Summary: "record that a running attempt's outcome could NOT be established, after the service refused its close", Surface: "hosted",
		Args: []Arg{{Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the instruction belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "finding", Kind: flagString, Value: "TEXT", Summary: "what you established about the attempt; required, and recorded with the decision"},
		},
		Needs: "tasks.cancel", Fallback: "agent.workspace",
		Effects:  "records that this attempt's outcome could not be established, and holds the instructions waiting behind it in the same commit. It records reconciliation_required and it CANNOT record succeeded or failed: nobody measured those, and a person writing a verdict they did not measure is exactly what the service's refusal existed to prevent. It is bound to the revision that was read and to the execution generation it was prepared under, so a decision prepared before a restore cannot land after one. It starts nothing, retries nothing, meters nothing, and never releases the queue: continuing the instructions held behind it is a separate decision with its own command",
		Examples: []string{"ks task reconcile tsk_9c21 --session 3f2a1c --finding \"checked the registry by hand: nothing was published\""},
		Nothing:  "Nothing was recorded.", Run: hosted(hostedTaskReconcile)},
	{Path: []string{"task", "resume"}, Summary: "run one blocked instruction again as a new attempt from a saved point you name; it never continues the rest of the queue instead", Surface: "hosted",
		Args: []Arg{{Name: "instruction", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the instruction belongs to; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "checkpoint", Kind: flagString, Value: "ID", Summary: "the saved point to start the new attempt from; required, and never chosen for you"},
			{Name: "finding", Kind: flagString, Value: "TEXT", Summary: "why you are running it again; required, and recorded with the restore"},
			{Name: "accept-affected", Kind: flagString, Value: "DIGEST", Summary: "the digest the service printed with the exact list of other work a restore would return to that moment; consent is bound to that list and never to the word yes"},
		},
		Needs:    "agent.workspace",
		Effects:  "running an instruction again is a NEW attempt under it, started from a saved point, and the attempt that already ran keeps its outcome and its cost — a retry is never a rewrite of the attempt before it. A saved point is NEVER chosen for you: without --checkpoint this lists the restorable ones and stops. A KeepState saved point is a WHOLE-SESSION saved point, so restoring one returns every agent and every instruction in the session to that moment; where other work would be affected the service refuses with the exact list and a digest of it, and only a caller citing that digest proceeds. The restore holds dispatch and starts nothing: continuing the instructions held behind this one stays a separate decision with its own command (ks agent queue resume), and this verb never performs it in its place",
		Examples: []string{"ks task resume tsk_9c21 --session 3f2a1c", "ks task resume tsk_9c21 --session 3f2a1c --checkpoint ckpt_8f10 --finding \"the publish never happened\""},
		Nothing:  "Nothing was retried.", Run: hosted(hostedTaskResume)},
	{Path: []string{"check"}, Group: true, Summary: "project checks and setup operations: defined, trusted over their exact inputs by you, run by the agent's worker", Surface: "hosted"},
	{Path: []string{"check", "define"}, Summary: "store a check: an argument vector with no shell, the files it depends on, its artifacts and bounds; runs nothing", Surface: "hosted",
		Args: []Arg{{Name: "name", Required: true}}, Rest: "argv",
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "agent", Kind: flagString, Value: "NAME", Summary: "the agent the check belongs to (default main)"},
			{Name: "input", Kind: flagList, Value: "PATH", Summary: "a workspace file the check's behaviour depends on (repeatable; at least one); a change to any of them needs your trust again"},
			{Name: "artifact", Kind: flagList, Value: "PATH", Summary: "a workspace file the check produces, whose digest is recorded (repeatable)"},
			{Name: "workdir", Kind: flagString, Value: "PATH", Summary: "the workspace directory it runs in (default the workspace root)"},
			{Name: "kind", Kind: flagString, Value: "KIND", Summary: "check or setup (default check)"},
			{Name: "timeout", Kind: flagInt, Positive: true, Value: "SECONDS", Summary: "the deadline, after which its process group is killed (default 600, at most 3600)"},
			{Name: "max-output", Kind: flagInt, Positive: true, Value: "BYTES", Summary: "the output kept (default 256 KiB, at most 1 MiB)"},
		},
		Needs:    "agent.workspace",
		Effects:  "records a definition and its hash; nothing runs, and nothing is trusted: the first run stops for your trust",
		Examples: []string{"ks check define --session 3f2a1c --input package.json --input test/run.sh unit -- npm test"},
		Nothing:  "Nothing was defined.", Run: hosted(hostedCheckDefine)},
	{Path: []string{"check", "list"}, Summary: "an agent's checks: the exact action, the definition hash and the input sets you trusted", Surface: "hosted",
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session the agent lives in; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}, {Name: "agent", Kind: flagString, Value: "NAME", Summary: "the agent the check belongs to (default main)"}},
		Needs:    "agent.workspace",
		Effects:  "reads the definitions; nothing runs and nothing changes",
		Examples: []string{"ks check list --session 3f2a1c"},
		Nothing:  "Nothing was read.", Run: hosted(hostedCheckList)},
	{Path: []string{"check", "run"}, Summary: "request a run of a check; it executes only over inputs you trusted", Surface: "hosted",
		Args:     []Arg{{Name: "check", Required: true}},
		Needs:    "agent.workspace",
		Effects:  "queues one run for the agent's worker, which takes it between instructions and reports the inputs it finds; over trusted inputs it runs (in the session, metered as session time), otherwise it stops at approval_required and runs nothing",
		Examples: []string{"ks check run chk_0123abcd"},
		Nothing:  "Nothing was requested.", Run: hosted(hostedCheckRun)},
	{Path: []string{"check", "show"}, Summary: "one run: state, inputs seen, exit status, output, artifacts' digests, the action to trust", Surface: "hosted",
		Args:     []Arg{{Name: "run", Required: true}},
		Needs:    "agent.workspace",
		Effects:  "reads one run; nothing changes. Output is shown with escape sequences as text. A check's result is an ordinary check, never a verification",
		Examples: []string{"ks check show crun_0123abcd", "ks check show crun_0123abcd --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedCheckShow)},
	{Path: []string{"check", "trust"}, Summary: "trust the exact action a run stopped at: this definition over the inputs it saw", Surface: "hosted",
		Args:     []Arg{{Name: "run", Required: true}},
		Flags:    []Flag{{Name: "confirm", Kind: flagString, Value: "HASH", Summary: "the action hash you were shown (all of it, or the 12 characters shown); required without a terminal, never implied by --yes"}},
		Needs:    "agent.workspace",
		Effects:  "shows the exact action and its inputs digest, then records your trust once you confirm its hash; the run is queued again and re-reads its inputs before it executes, and any change to them stops it again",
		Examples: []string{"ks check trust crun_0123abcd", "ks check trust crun_0123abcd --confirm 9f8e7d6c5b4a"},
		Nothing:  "Nothing was trusted.", Run: hosted(hostedCheckTrust)},
	{Path: []string{"file"}, Group: true, Summary: "a session's workspace, read without a shell", Surface: "hosted"},
	{Path: []string{"file", "list"}, Summary: "one directory of a session's workspace: type, mode, size, name; secret files marked redacted", Surface: "hosted",
		Args:     []Arg{{Name: "path", Required: false}},
		Flags:    []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session whose workspace to read; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs:    "agent.workspace",
		Effects:  "reads one workspace directory by a relative path; nothing changes, no command runs and a session that is not running is not woken. An absolute path or one with .. is refused before anything is asked, and the service refuses links and host paths",
		Examples: []string{"ks file list --session 3f2a1c", "ks file list src --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedFileList)},
	{Path: []string{"file", "show"}, Summary: "one workspace file, at most 1 MiB, printed as safe text or written to a new file", Surface: "hosted",
		Args: []Arg{{Name: "path", Required: true}},
		Flags: []Flag{
			{Name: "session", Kind: flagString, Value: "ID", Summary: "the session whose workspace to read; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "max-bytes", Kind: flagInt, Positive: true, Value: "N", Summary: "read at most N bytes (default and maximum 1 MiB)"},
			{Name: "out", Kind: flagString, Value: "FILE", Summary: "write the bytes to this NEW file instead of printing them"},
		},
		Needs:    "agent.workspace",
		Effects:  "reads one workspace file, bounded, and checks the bytes against the size and sha256 the service states; escape sequences are shown, never acted on, a file that is not text is not printed, and a secret file's contents are never returned. --out writes a new file and never replaces one. No command runs and a session that is not running is not woken",
		Examples: []string{"ks file show package.json --session 3f2a1c", "ks file show build/app.log --session 3f2a1c --max-bytes 65536", "ks file show dist/app.bin --session 3f2a1c --out app.bin"},
		Nothing:  "Nothing was read.", Run: hosted(hostedFileShow)},
	{Path: []string{"result"}, Group: true, Summary: "what an agent's tasks produced: listed, described, downloaded and verified", Surface: "hosted"},
	{Path: []string{"result", "list"}, Summary: "the results of one task, or of a session's tasks: name, size, sha256, task, verification", Surface: "hosted",
		Flags: []Flag{
			{Name: "task", Kind: flagString, Value: "ID", Summary: "one instruction's results"},
			{Name: "session", Kind: flagString, Value: "ID", Summary: "every task of this session's agents; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"},
			{Name: "agent", Kind: flagString, Value: "NAME", Summary: "only this agent's tasks (with --session)"},
		},
		Needs: "tasks.results", Fallback: "agent.workspace",
		Effects:  "reads result metadata; nothing is downloaded and nothing changes. A task or queue that could not be read is named as unread, never shown as having no results, and the command then exits as a failure",
		Examples: []string{"ks result list --task tsk_9c21", "ks result list --session 3f2a1c --agent main --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedResultList)},
	{Path: []string{"result", "show"}, Summary: "one result: size, sha256, task and attempt, the producer's checks, the task's verification, provenance", Surface: "hosted",
		Args:  []Arg{{Name: "result", Required: true}},
		Needs: "tasks.results", Fallback: "agent.workspace",
		Effects:  "reads one result's metadata; nothing is downloaded and nothing changes. The producer's checks are labelled as the producer's; the task's independent verification is shown beside them and a result is never called verified because it exists",
		Examples: []string{"ks result show res_0123abcd", "ks result show res_0123abcd --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedResultShow)},
	{Path: []string{"result", "download"}, Summary: "download a result, verify its size and sha256, and place it without replacing anything", Surface: "hosted",
		Args: []Arg{{Name: "result", Required: true}},
		Flags: []Flag{
			{Name: "out", Kind: flagString, Value: "FILE", Summary: "where to place it; default the result's own file name in this directory"},
			{Name: "force", Kind: flagBool, Summary: "replace an existing file at the target (atomically, and only with verified bytes)"},
			{Name: "extract", Kind: flagString, Value: "DIR", Summary: "unpack a tar, tar.gz or zip result into this NEW directory after every entry passes the C09 checks"},
			{Name: "task", Kind: flagString, Value: "ID", Summary: "refuse unless the result belongs to this task"},
			{Name: "grant-ttl", Kind: flagString, Value: "DURATION", Summary: "how long the download grant lives (1s to 1h; default the service's 10m)"},
			{Name: "keep-partial", Kind: flagBool, Summary: "keep an interrupted download's incomplete bytes (.ks-partial-<result>) so the same command can resume; without it they are removed"},
		},
		Needs: "tasks.results", Fallback: "agent.workspace",
		Effects:  "asks for an expiring download grant (never shown), downloads to a partial file beside the target, verifies size and sha256 against the result's record, then places the file atomically; an existing file is never replaced without --force and one that appears during the download is left untouched. An interrupted download's partial file is removed unless --keep-partial keeps it, named as incomplete, for the same command to resume (only if the service still holds the same bytes); bytes that do not verify are always removed. Nothing is extracted unless --extract, and nothing is ever run",
		Examples: []string{"ks result download res_0123abcd", "ks result download res_0123abcd --out report.tar.gz", "ks result download res_0123abcd --extract ./report"},
		Nothing:  "Nothing was written.", Run: hosted(hostedResultDownload)},
	{Path: []string{"result", "diff"}, Summary: "what an attempt's ks-changeset.json says it changed, against its recorded base, with every path escaped", Surface: "hosted",
		Args:  []Arg{{Name: "result", Required: true}},
		Flags: []Flag{{Name: "content", Kind: flagBool, Summary: "also show the new text of each changed text file (escaped)"}},
		Needs: "tasks.results", Fallback: "agent.workspace",
		Effects:  "downloads and verifies the changeset result and prints each addition, modification, deletion and mode change with its before and after digests; nothing here changes",
		Examples: []string{"ks result diff res_0123abcd", "ks result diff res_0123abcd --content"},
		Nothing:  "Nothing was read.", Run: hosted(hostedResultDiff)},
	{Path: []string{"result", "apply"}, Summary: "apply an attempt's changeset to this project, only onto the recorded base, transactionally", Surface: "hosted",
		Args: []Arg{{Name: "result", Required: false}},
		Flags: []Flag{
			{Name: "dir", Kind: flagString, Value: "DIR", Summary: "the project root to apply to (default this project's root)"},
			{Name: "confirm", Kind: flagString, Value: "DIGEST", Summary: "the changeset's digest (the 12 characters shown, or all of it); required without a terminal, never implied by --yes"},
			{Name: "recover", Kind: flagBool, Summary: "put every path an interrupted apply touched back as it was"},
		},
		Needs: "tasks.results", Fallback: "agent.workspace",
		Effects:  "checks every file the changeset touches against its recorded before-state and refuses the WHOLE apply on any local difference or any path outside the project; then, once confirmed, stages and verifies the new bytes, backs up every file it will change under .keepstate/apply, replaces them one at a time in a journal and verifies each against its after-state. An interrupted apply blocks the next one until --recover restores the backup. Nothing is committed, pushed, run or deployed",
		Examples: []string{"ks result apply res_0123abcd", "ks result apply res_0123abcd --confirm 3f2a1c9b8d7e", "ks result apply --recover"},
		Nothing:  "Nothing was changed.", Run: hosted(hostedResultApply)},
	{Path: []string{"preflight"}, Summary: "what a run needs and what is missing: identity, keys, credit, capabilities, and the local upload preview; starts nothing", Surface: "hosted",
		Flags: []Flag{{Name: "provider", Kind: flagString, Value: "PROVIDER", Summary: "the provider the run will use (default anthropic)"},
			{Name: "mode", Kind: flagString, Value: "MODE", Summary: "what you are about to start: cruise (default) or agent"},
			{Name: "check", Kind: flagString, Value: "COMMAND", Summary: "the acceptance check a Cruise job would run (default: the one in .keepstate/cruise.json)"},
			{Name: "ladder", Kind: flagString, Value: "FAMILIES", Summary: "the model families a Cruise job would climb, comma-separated (default: the draft's ladder)"}},
		Needs:    "preflight",
		Effects:  "measures the workspace selection here and sends its size and file count with the acceptance check and the ladder, then prints each check with its category and what was not checked; uploads nothing, provisions nothing, calls no model and reserves nothing. Exit 0 ready, 5 blocked, 4 when a check could not be made or the service could not be reached",
		Examples: []string{"ks preflight", "ks preflight --mode agent", "ks preflight --json"},
		Nothing:  "Nothing was started.", Run: hosted(hostedPreflight)},
	{Path: []string{"meter"}, Summary: "spend, budget, key sources", Surface: "hosted",
		Effects:  "reads the meter; nothing changes",
		Examples: []string{"ks meter <session>", "ks meter <session> --json"},
		Args:     []Arg{{Name: "session", Required: true}},
		Nothing:  "Nothing was read.", Run: hosted(hostedMeter)},
	{Path: []string{"exec"}, Summary: "run one command in the session", Surface: "hosted",
		Flags:    []Flag{{Name: "shell", Kind: flagBool, Summary: "send the one argument verbatim for the session's shell to interpret (pipes, globs, variables); without it every argument reaches the program exactly as typed"}},
		Effects:  "runs the command inside the session's guest as root; whatever it does, it does",
		Examples: []string{"ks exec <session> -- python --version", "ks exec <session> -- printf %s 'a b'", "ks exec --shell <session> 'ls | wc -l'"},
		Args:     []Arg{{Name: "session", Required: true}}, Rest: "command",
		Nothing: "No command was sent.", Run: hosted(hostedExec)},
	{Path: []string{"attach"}, Summary: "interactive terminal into a running session (tmux)", Surface: "hosted",
		Effects:  "opens a terminal into the guest tmux; closing it changes nothing in the session",
		Examples: []string{"ks attach <session>"},
		Args:     []Arg{{Name: "session", Required: true}},
		Nothing:  "Nothing was attached.", Run: hosted(hostedAttachCmd)},
	{Path: []string{"fork"}, Summary: "branch a checkpoint into diverging children", Surface: "hosted",
		Effects:  "starts N more running sessions from the checkpoint (paid plans): each child is metered like a session",
		Examples: []string{"ks fork <session> -n 2"},
		Args:     []Arg{{Name: "session", Required: true}},
		Flags: []Flag{
			{Name: "children", Short: "n", Kind: flagInt, Positive: true, Value: "N", Summary: "how many children to branch; default 1"},
			{Name: "steer", Kind: flagString, Value: "FILE", Summary: "a steer file applied to every child"},
		},
		Nothing: "Nothing was forked.", Run: hosted(hostedFork)},
	{Path: []string{"cruise"}, Group: true, Summary: "accepted work on the fleet", Surface: "hosted"},
	{Path: []string{"cruise", "init"}, Summary: "draft the acceptance manifest from the repository into .keepstate/cruise.json", Surface: "client",
		Effects:  "discovers the repository's Python, Go and Node tests the way the verifier does (nested included) and pins every verifier input -- the tests, their configuration, lockfiles and fixtures -- by relative path; writes .keepstate/cruise.json and .keepstate/verifier.json; makes no request and calls no model. Tests of more than one ecosystem are never chosen between: name the check with --check",
		Examples: []string{"ks cruise init --goal \"make the inventory tests pass\"", "ks cruise init --tests \"make check\" --spend 1.50"},
		Flags: []Flag{{Name: "allow", Kind: flagList, Value: "PATH", Variadic: true, Summary: "review a sensitive or ignored path into the upload (repeatable); see ks cruise preview"},
			{Name: "goal", Kind: flagString, Value: "TEXT", Summary: "the goal the agent is given; without it, the draft's previous goal, else \"make the check pass: CMD\""},
			{Name: "tests", Kind: flagString, Value: "CMD", Summary: "the command that decides done, when init should not detect it (pytest, npm test, go test)"},
			{Name: "check", Kind: flagString, Value: "RUNNER", Summary: "the check to pin and run: pytest, go or node; required when tests of more than one ecosystem are found"},
			{Name: "paths", Kind: flagList, Variadic: true, Value: "GLOB", Summary: "the paths an attempt may change; default every tracked file except the tests and .keepstate"},
			{Name: "ladder", Kind: flagString, Value: "family:model,...", Summary: "the rungs in order; default the model table's default ladder"},
			{Name: "spend", Kind: flagUSD, Value: "USD", Summary: "the spend ceiling in dollars; default 2"},
		},
		Nothing: "No draft was written.", Run: cruiseInit},
	{Path: []string{"cruise", "preview"}, Aliases: [][]string{{"workspace", "preview"}}, Summary: "what an upload would carry and what it would not, with reasons: names and sizes, never content", Surface: "client",
		Flags:    []Flag{{Name: "allow", Kind: flagList, Value: "PATH", Variadic: true, Summary: "review a sensitive or ignored path into the upload (repeatable); never a symlink or .git"}},
		Effects:  "reads the workspace; no request, no file written",
		Examples: []string{"ks cruise preview", "ks cruise preview --json", "ks cruise preview --allow config/.env.production"},
		Nothing:  "Nothing was uploaded or written.", Run: cruisePreview},
	{Path: []string{"cruise", "approve"}, Summary: "lock the draft: its digest and the time into .keepstate/cruise.lock", Surface: "client",
		Effects:  "writes .keepstate/cruise.lock; makes no request",
		Examples: []string{"ks cruise approve"},
		Nothing:  "Nothing was approved.", Run: cruiseApprove},
	{Path: []string{"cruise", "run"}, Summary: "upload the workspace and the locked manifest; start the job", Surface: "hosted",
		Effects:  "uploads the packed workspace and starts a job that bills as the sessions it runs, up to the approved spend ceiling",
		Examples: []string{"ks cruise run"},
		Nothing:  "No job was started.", Run: func(inv *Invocation) {
			if err := cruiseRun(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"cruise", "proposal"}, Group: true, Summary: "Cruise jobs your agents proposed: nothing runs until you approve the exact manifest", Surface: "hosted"},
	{Path: []string{"cruise", "proposal", "list"}, Summary: "a session's proposed Cruise jobs, newest first: state, goal, manifest digest and the job each became", Surface: "hosted",
		Flags: []Flag{{Name: "session", Kind: flagString, Value: "ID", Summary: "the session whose agents proposed; a short id is accepted when it is unique among yours; without it, this project's binding (ks session use)"}},
		Needs: "cruise.proposals", Fallback: "agent.workspace",
		Effects:  "reads the proposals; nothing is approved, created or run",
		Examples: []string{"ks cruise proposal list --session 3f2a1c", "ks cruise proposal list --session 3f2a1c --json"},
		Nothing:  "Nothing was read.", Run: hosted(hostedCruiseProposalList)},
	{Path: []string{"cruise", "proposal", "show"}, Summary: "one proposed job: its manifest in full, the digest the service states and the digest computed here, and its job", Surface: "hosted",
		Args:  []Arg{{Name: "proposal", Required: true}},
		Needs: "cruise.proposals", Fallback: "agent.workspace",
		Effects:  "reads one proposal and recomputes its manifest digest locally; nothing is approved, created or run",
		Examples: []string{"ks cruise proposal show cprop_0123abcd"},
		Nothing:  "Nothing was read.", Run: hosted(hostedCruiseProposalShow)},
	{Path: []string{"cruise", "proposal", "approve"}, Summary: "run a proposed job by confirming the digest of the exact manifest shown", Surface: "hosted",
		Args:  []Arg{{Name: "proposal", Required: true}},
		Flags: []Flag{{Name: "confirm", Kind: flagString, Value: "DIGEST", Summary: "the manifest digest you reviewed (all of it, or the 12 characters shown); required without a terminal, never implied by --yes"}},
		Needs: "cruise.proposals", Fallback: "agent.workspace",
		Effects:  "shows the manifest and the digest computed here from it (refusing if the service's digest differs), then, once you confirm that digest, creates ONE Cruise job through the ordinary job intake: it runs, spends against your key and credit, and is verified by the pinned verifier. Only this approval starts it, and only its own cancel stops it",
		Examples: []string{"ks cruise proposal approve cprop_0123abcd", "ks cruise proposal approve cprop_0123abcd --confirm 3f2a1c9b8d7e"},
		Nothing:  "Nothing was approved.", Run: hosted(hostedCruiseProposalApprove)},
	{Path: []string{"cruise", "proposal", "decline"}, Summary: "decline a proposed job; it never runs", Surface: "hosted",
		Args:  []Arg{{Name: "proposal", Required: true}},
		Flags: []Flag{{Name: "reason", Kind: flagString, Value: "TEXT", Summary: "why, recorded with the decline"}},
		Needs: "cruise.proposals", Fallback: "agent.workspace",
		Effects:  "records the decline; the proposal never becomes a job and nothing runs or is spent",
		Examples: []string{"ks cruise proposal decline cprop_0123abcd --reason \"the check is too weak\""},
		Nothing:  "Nothing was declined.", Run: hosted(hostedCruiseProposalDecline)},
	{Path: []string{"cruise", "status"}, Summary: "the job (or the newest): state, rung, each attempt, save points, spend, verdict, artifact", Surface: "hosted",
		Effects:  "reads the job; nothing changes",
		Examples: []string{"ks cruise status", "ks cruise status <job>"},
		Args:     []Arg{{Name: "job", Required: false}},
		Nothing:  "Nothing was read.", Run: cruiseStatus},
	{Path: []string{"cruise", "logs"}, Summary: "the event stream, one event per line: seq, ts, type, detail", Surface: "hosted",
		Effects:  "reads the events; nothing changes",
		Examples: []string{"ks cruise logs <job>"},
		Args:     []Arg{{Name: "job", Required: true}},
		Nothing:  "Nothing was read.", Run: cruiseLogs},
	{Path: []string{"cruise", "cancel"}, Summary: "cancel a job", Surface: "hosted",
		Effects:  "stops the job and releases its parent session; spend already incurred stands",
		Examples: []string{"ks cruise cancel <job>"},
		Args:     []Arg{{Name: "job", Required: true}},
		Nothing:  "Nothing was cancelled.", Run: cruiseCancel},
	{Path: []string{"cruise", "resume"}, Summary: "resume a job from review, on the same or a wider ladder", Surface: "hosted",
		Effects:  "starts new attempts from the parked parent, billed like the first ones; refused outside review",
		Examples: []string{"ks cruise resume <job> --ladder anthropic:claude-sonnet-5"},
		Args:     []Arg{{Name: "job", Required: true}},
		Flags:    []Flag{{Name: "ladder", Kind: flagString, Value: "family:model,...", Summary: "the rungs to continue on, in order"}},
		Nothing:  "Nothing was resumed.", Run: cruiseResume},
	{Path: []string{"cruise", "artifact"}, Summary: "download the accepted tarball", Surface: "hosted",
		Effects:  "writes one file locally after its sha256 matches the job record; never extracts or runs it",
		Examples: []string{"ks cruise artifact <job> --out result.tar.gz"},
		Args:     []Arg{{Name: "job", Required: true}},
		Flags:    []Flag{{Name: "out", Kind: flagString, Value: "FILE", Summary: "where to write it; default JOB.tar.gz"}},
		Nothing:  "Nothing was written.", Run: func(inv *Invocation) {
			if err := cruiseArtifact(inv); err != nil {
				die(err)
			}
		}},
	{Path: []string{"cruise", "models"}, Summary: "the model table the control plane serves: families, rungs, default ladder", Surface: "hosted",
		Effects:  "reads the table; nothing changes",
		Examples: []string{"ks cruise models"},
		Nothing:  "Nothing was read.", Run: cruiseModels},
	{Path: []string{"operation"}, Group: true, Summary: "operation records", Surface: "hosted"},
	{Path: []string{"operation", "show"}, Summary: "read an operation back by its id or key: state, result, timestamps", Surface: "hosted",
		Args:     []Arg{{Name: "operation", Required: true}},
		Needs:    "operations.idempotent",
		Effects:  "reads the record; nothing changes",
		Examples: []string{"ks operation show ksop_0123456789abcdef0123456789abcdef"},
		Nothing:  "Nothing was read.", Run: hosted(hostedOperationShow)},
	{Path: []string{"operation", "wait"}, Summary: "wait, within the bound, for an operation to finish and print its result", Surface: "hosted",
		Args:     []Arg{{Name: "operation", Required: true}},
		Needs:    "operations.idempotent",
		Effects:  "reads the record until it finishes or the wait bound passes; nothing changes; Ctrl-C stops the local waiting only",
		Examples: []string{"ks operation wait ksop_0123456789abcdef0123456789abcdef"},
		Nothing:  "Nothing was read.", Run: hosted(hostedOperationWait)},
	{Path: []string{"completion"}, Summary: "print a shell completion script generated from the command schema", Surface: "client",
		Args:     []Arg{{Name: "shell", Required: true}},
		Effects:  "prints the script; installs nothing and edits no shell file (the script's first lines say where to put it)",
		Examples: []string{"ks completion bash", "ks completion zsh > \"${fpath[1]}/_ks\"", "ks completion fish > ~/.config/fish/completions/ks.fish"},
		Nothing:  "Nothing was printed.", Run: func(inv *Invocation) {
			script, err := completionScript(inv.Arg(0))
			if err != nil {
				fail(&cliError{Code: exitUsage, Kind: "usage", Message: err.Error()})
			}
			fmt.Print(script)
		}},
	{Path: []string{"reference"}, Summary: "print the command reference as a Markdown table generated from the command schema", Surface: "client",
		Effects:  "prints the table; nothing changes",
		Examples: []string{"ks reference"},
		Nothing:  "Nothing was printed.", Run: func(*Invocation) { fmt.Print(referenceTable(reg)) }},
	{Path: []string{"doctor"}, Summary: "connectivity, token, version", Surface: "client",
		Effects:  "reads: the control plane health, your token, the latest release; nothing changes",
		Examples: []string{"ks doctor"},
		Run:      func(*Invocation) { os.Exit(runDoctor()) }},
	{Path: []string{"update"}, Summary: "self-update (checksum-verified, provenance-checked with gh)", Surface: "client",
		Flags:    []Flag{{Name: "require-provenance", Kind: flagBool, Summary: "replace nothing unless gh verifies the new binary's build provenance"}},
		Effects:  "replaces this binary with the verified release; a checksum mismatch, or a provenance attestation gh rejects, replaces nothing. Without gh the provenance reads NOT verified and, unless --require-provenance, the checksum-verified binary is installed with that said",
		Examples: []string{"ks update", "ks update --require-provenance"},
		Nothing:  "Nothing was updated.", Run: func(inv *Invocation) {
			requireProvenance = inv.Bool("require-provenance")
			if err := runUpdate(); err != nil {
				die(err)
			}
		}},
	{Path: []string{"logout"}, Summary: "remove the stored token", Surface: "client",
		Effects:  "deletes the stored token on this machine; revoke it server-side from your account page",
		Examples: []string{"ks logout"},
		Run: func(*Invocation) {
			if err := runLogout(); err != nil {
				die(err)
			}
		}},
	{Path: []string{"uninstall"}, Summary: "how to remove ks completely", Surface: "client",
		Effects:  "prints the removal command; removes nothing itself",
		Examples: []string{"ks uninstall"},
		Run:      func(*Invocation) { runUninstall() }},
	{Path: []string{"version"}, Summary: "print the version; with --verify, check this binary's build provenance", Surface: "client",
		Flags:    []Flag{{Name: "verify", Kind: flagBool, Summary: "check this binary's GitHub build-provenance attestation with gh; without gh, or on any error, it reads NOT verified and exits non-zero"}},
		Effects:  "prints the version; --verify runs gh attestation verify on this binary (a network read by gh) and changes nothing",
		Examples: []string{"ks version", "ks version --verify"},
		Run:      runVersion},
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "--version", "-v":
		fmt.Println("ks", version)
		return
	case "--help", "-h", "help":
		// ks help <command>: that command's help; a group's usage for a group
		if len(args) > 1 {
			if c, _, _ := lookup(registry, args[1:]); c != nil {
				if c.Group {
					groupUsage(c)
				} else {
					commandHelp(c)
				}
				return
			}
		}
		usage()
		return
	}
	if looksLikeFlag(args[0]) {
		fmt.Fprintf(os.Stderr, "Unknown option: %s\n", args[0])
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(2)
	}

	c, rest, sugg := lookup(registry, args)
	if c == nil {
		fmt.Fprintf(os.Stderr, "unknown verb %q\n", args[0])
		if sugg != "" {
			fmt.Fprintln(os.Stderr, sugg)
		}
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		usage()
		os.Exit(2)
	}
	if c.Group {
		// a head with no subcommand, or a help word: the group's usage
		if len(rest) == 0 {
			groupUsage(c)
			os.Exit(2)
		}
		if isHelpWord(rest[0]) {
			groupUsage(c)
			return
		}
		fmt.Fprintf(os.Stderr, "unknown %s verb %q\n", c.Name(), rest[0])
		if s := suggest(c.Name()+" "+rest[0], sortedNames(registry)); s != "" {
			fmt.Fprintln(os.Stderr, s)
		}
		fmt.Fprintln(os.Stderr, "Nothing was done.")
		fmt.Fprintln(os.Stderr)
		groupUsage(c)
		os.Exit(2)
	}

	// The option region is parsed BEFORE the handler exists to the process:
	// a malformed command line, or a help word inside the option region,
	// stops here, and nothing below it has run. This is the general form of
	// the project ruling of 2026-09-09 (`ks run --help` started a billable
	// session): help never acts, and neither does a typo.
	inv, uerr := c.parse(rest)
	if uerr != nil {
		uerr.print()
		os.Exit(2)
	}
	if err := applyGlobals(inv); err != nil {
		(&UsageError{Cmd: c, Message: err.Error()}).print()
		os.Exit(2)
	}
	if inv.Help {
		if c.Path[0] == "cruise" {
			sabotageHelpHook() // test-only, KS_CLI_SABOTAGE_HELP=1: gate CR-7's sabotage
		}
		commandHelp(c)
		return
	}
	c.Run(inv)
}

func usage() { fmt.Print(registryUsage(registry)) }

// groupUsage prints a group's subcommands from the registry; cruise keeps
// its authored text, which the gates and tests read.
func groupUsage(g *Command) {
	if g.Name() == "cruise" {
		cruiseUsage()
		return
	}
	fmt.Printf("ks %s: %s\n\nusage:\n", g.Name(), g.Summary)
	for _, c := range registry {
		if !c.Group && len(c.Path) > 1 && c.Path[0] == g.Path[0] {
			fmt.Printf("  %-44s %s\n", c.Usage(), c.Summary)
		}
	}
	fmt.Println("\nHelp makes no request and changes nothing.")
}

// die reports a failure in the mode's shape and exits by the table.
func die(err error) { fail(err) }

// hosted wraps a handler that needs the stored sign-in. A signed-out client
// stops here with exit 2 and no request.
func hosted(run func(cr hostedCreds, inv *Invocation)) func(*Invocation) {
	return func(inv *Invocation) {
		cr, ok := hostedToken()
		if !ok {
			fail(&cliError{Code: exitAuth, Kind: "not_signed_in", Message: "Not signed in. Run: ks login", NextAction: "ks login"})
		}
		requireCapability(cr, inv.Cmd)
		run(cr, inv)
	}
}
