// import_commands.go: the rows of the registry for ks import.
package main

func importFlags(cursor bool) []Flag {
	fs := []Flag{
		{Name: "latest", Kind: flagBool, Summary: "the newest session for --project (default: the current folder), or the newest on this machine when none matches"},
	}
	if cursor {
		fs = []Flag{
			{Name: "latest", Kind: flagBool, Summary: "the newest Cursor chat that holds text"},
			{Name: "chat", Kind: flagString, Value: "ID", Summary: "the Cursor chat to import; a unique start of its id is accepted"},
			{Name: "store", Kind: flagString, Value: "FILE", Summary: "Cursor's chat store (state.vscdb); default: Cursor's own folder for this system. It is copied first and the copy is read; the original is never changed"},
		}
	} else {
		fs = append(fs, Flag{Name: "session", Kind: flagString, Value: "ID", Summary: "the session to import; a unique start of its id is accepted (ks import list shows them)"})
	}
	fs = append(fs,
		Flag{Name: "project", Kind: flagString, Value: "DIR", Summary: "the project folder: which session --latest takes, and where --with-files reads the repository from"},
		Flag{Name: "with-files", Kind: flagBool, Summary: "bring the project's git repository at the session's folder (its files and history) into the new session's workspace, under the upload policy: .env files, credential-named files, node_modules and what .gitignore lists are left out"},
		Flag{Name: "name", Kind: flagString, Value: "NAME", Summary: "the new session's name; default the project's folder name and \"import\""},
		Flag{Name: "model", Kind: flagString, Value: "MODEL", Summary: "the model the new agent calls, one KeepState offers for its harness; default the service's"},
		Flag{Name: "key", Kind: flagString, Value: "KEY", Summary: "the key the agent calls with: a key's id or alias, own for your own key, or keepstate for KeepState's key; default your account's default for the provider"},
		Flag{Name: "budget", Aliases: []string{"spend-limit"}, Kind: flagString, Value: "DOLLARS", Summary: "the new session's spend limit on KeepState's key, in dollars (with --key keepstate)"},
		Flag{Name: "harness", Kind: flagString, Value: "H", Summary: "the agent the new session runs, claude-code or codex, for a written summary; default the session's own tool (Cursor: claude-code). A conversation is resumed only in its own tool; into the other one it goes across as a summary"},
	)
	if !cursor {
		fs = append(fs, Flag{Name: "brief", Kind: flagBool, Summary: "send a written summary of the conversation instead of the conversation (always the case into another tool, and for a conversation over the size limit)"})
	}
	return append(fs,
		Flag{Name: "plan", Kind: flagBool, Summary: "plan only: what would go up, what is replaced and what it costs; creates nothing and prints a digest"},
		Flag{Name: "execute", Kind: flagString, Value: "DIGEST", Summary: "do exactly the plan with this digest (the conversation is read again; if it changed since the plan, nothing is created)"},
		Flag{Name: "out", Kind: flagString, Value: "FILE", Summary: "write what would be sent (the sanitized conversation, or the summary) to this new file and stop; nothing is sent, no sign-in is needed"},
	)
}

func importRow(tool string) *Command {
	label := importToolLabel(tool)
	c := &Command{Path: []string{"import", tool}, Surface: "hosted", Flags: importFlags(tool == "cursor"), Needs: "agent.workspace",
		Nothing: "Nothing was created."}
	switch tool {
	case "cursor":
		c.Summary = "a Cursor chat as a written summary for a new agent session (best effort: Cursor's chat store is not documented)"
		c.Effects = "reads Cursor's chat store from a private copy, never the file itself, and builds a written summary of one chat on this machine by fixed rules (no model); secrets in it are replaced before it goes anywhere. --plan sends the declaration to the service and prints a digest; --execute DIGEST creates an agent session (no instruction is sent, so no model is called), uploads the repository when --with-files is given, and the summary reaches the agent with your first instruction, as data. Nothing is resumed: Cursor is not an agent KeepState runs"
		c.Examples = []string{"ks import cursor --latest --plan", "ks import cursor --latest --execute <digest>", "ks import cursor --chat 3f2a1c --with-files --plan"}
	default:
		c.Summary = "a " + label + " session on this machine as a new agent session that continues the same conversation (or, with --brief, a written summary of it)"
		c.Effects = "reads the session's file on this machine, takes out what must not travel (the old machine's system prompt and tool list, account ids, permission modes, file history, signed thinking, encrypted reasoning), says the old project paths as the new workspace's, replaces anything shaped like a credential, and refuses above 50 MB. --plan sends the declaration to the service and prints what goes up and a digest; --execute DIGEST creates an agent session (no instruction is sent, so no model is called), uploads the repository when --with-files is given and the conversation through the chunked upload, and the service places it where " + label + " keeps its own and makes it the agent's conversation. The first reply re-reads the whole conversation, which costs more than a short message; the bytes count toward your storage. Approvals from the old session carry nothing"
		c.Examples = []string{"ks import " + tool + " --latest --plan", "ks import " + tool + " --latest --execute <digest>", "ks import " + tool + " --session 3f2a1c --with-files --key keepstate --budget 20 --plan"}
	}
	c.Run = importTool(tool)
	return c
}

func importListRow() *Command {
	return &Command{Path: []string{"import", "list"}, Summary: "the " + "Claude Code and Codex sessions on this machine, newest first: tool, id, project, date, size and message count (never a message)", Surface: "client",
		Flags: []Flag{
			{Name: "cursor", Kind: flagBool, Summary: "also list Cursor chats (best effort; Cursor's store is copied first and the copy is read)"},
			{Name: "store", Kind: flagString, Value: "FILE", Summary: "Cursor's chat store to list from (implies --cursor); default: Cursor's own folder for this system"},
		},
		Effects:  "reads the session files under ~/.claude/projects and ~/.codex/sessions (or CLAUDE_CONFIG_DIR and CODEX_HOME) as streams for their identity and counts, never printing a message, and changes nothing; needs no sign-in",
		Examples: []string{"ks import list", "ks import list --json", "ks import list --cursor"},
		Nothing:  "Nothing was read.", Run: importList}
}
