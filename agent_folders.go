// agent_folders.go: folders for sorting your agents, the terminal's side of
// the console menu's folders.
//
//	ks agent folder list                       your folders and the agents in each
//	ks agent folder create <name>              a new folder, last in your order
//	ks agent folder rename <folder> <name>     a new name
//	ks agent folder order <folder> --position N  a place among your folders (from 1)
//	ks agent folder delete <folder>            its agents go back to the top level
//	ks agent move <agent> --folder <folder>    put an agent in a folder
//	ks agent move <agent> --top                back to the top level
//
// A folder is a label on your own view: it changes no agent, session or
// permission, and nobody else sees it. Folders are kept by the service, per
// person and per context (your personal account, or the organization ks
// context use names), so the console and ks show the same ones. A folder is
// named by its name (spaces are fine inside quotes; capitals do not matter)
// or by its id.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type agentFolder struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Position int      `json:"position"`
	AgentIDs []string `json:"agent_ids"`
}

type agentFolderList struct {
	Context string        `json:"context"`
	Folders []agentFolder `json:"folders"`
}

func fetchAgentFolders(cr hostedCreds) (agentFolderList, error) {
	var env struct {
		Data agentFolderList `json:"data"`
	}
	err := hostedCall(cr, "GET", inContext(cr, "/api/v2/agent-folders"), nil, &env)
	if env.Data.Folders == nil {
		env.Data.Folders = []agentFolder{}
	}
	return env.Data, err
}

// pickFolder is the one folder an argument names: its id, or its name
// (capitals do not matter). Nothing is guessed: no folder, or two, is an error.
func pickFolder(folders []agentFolder, arg string) (agentFolder, error) {
	arg = strings.Join(strings.Fields(arg), " ")
	if arg == "" {
		return agentFolder{}, &cliError{Code: exitUsage, Kind: "usage", Message: "a folder name is required", NextAction: "ks agent folder list"}
	}
	var hit []agentFolder
	for _, f := range folders {
		if f.ID == arg {
			return f, nil
		}
		if strings.EqualFold(f.Name, arg) {
			hit = append(hit, f)
		}
	}
	if len(hit) == 1 {
		return hit[0], nil
	}
	return agentFolder{}, &cliError{Code: exitUsage, Kind: "not_found", Message: fmt.Sprintf("you have no folder named %q", arg), NextAction: "ks agent folder list"}
}

func folderErr(err error, nothing string) error {
	var he *hostedErr
	if errors.As(err, &he) {
		switch he.Type {
		case "ks_folder_name_taken", "ks_folder_name_invalid", "ks_folder_limit", "ks_folder_not_found":
			ce := classify(err)
			ce.Code = exitUsage
			ce.Message = nothing + ": " + ce.Message
			return ce
		}
	}
	return err
}

func hostedAgentFolderList(cr hostedCreds, inv *Invocation) {
	list, err := fetchAgentFolders(cr)
	if err != nil {
		die(err)
	}
	// names for the ids: the status list is every agent of yours in this context
	names := map[string]string{}
	if st, serr := fetchAgentStatusList(cr); serr == nil {
		for _, r := range st.Items {
			names[r.AgentID] = r.Name + " (" + r.SessionName + ")"
		}
	}
	emit(list, func() {
		if len(list.Folders) == 0 {
			fmt.Println("You have no folders. Make one with: ks agent folder create <name>")
			return
		}
		for i, f := range list.Folders {
			fmt.Printf("%d. %s (%d)  %s\n", i+1, f.Name, len(f.AgentIDs), f.ID)
			for _, id := range f.AgentIDs {
				who := names[id]
				if who == "" {
					who = id
				}
				fmt.Printf("     %s\n", who)
			}
		}
	})
}

func hostedAgentFolderCreate(cr hostedCreds, inv *Invocation) {
	var env struct {
		Data agentFolder `json:"data"`
	}
	if err := hostedMutate(cr, "POST", inContext(cr, "/api/v2/agent-folders"), map[string]any{"name": inv.Arg(0)}, &env); err != nil {
		die(folderErr(err, "nothing was created"))
	}
	emit(env.Data, func() { fmt.Printf("created the folder %s (%s)\n", env.Data.Name, env.Data.ID) })
}

func hostedAgentFolderRename(cr hostedCreds, inv *Invocation) {
	list, err := fetchAgentFolders(cr)
	if err != nil {
		die(err)
	}
	f, err := pickFolder(list.Folders, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var env struct {
		Data agentFolder `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", inContext(cr, "/api/v2/agent-folders/"+url.PathEscape(f.ID)), map[string]any{"name": inv.Arg(1)}, &env); err != nil {
		die(folderErr(err, "nothing was renamed"))
	}
	emit(env.Data, func() { fmt.Printf("renamed the folder %s to %s\n", f.Name, env.Data.Name) })
}

func hostedAgentFolderOrder(cr hostedCreds, inv *Invocation) {
	list, err := fetchAgentFolders(cr)
	if err != nil {
		die(err)
	}
	f, err := pickFolder(list.Folders, inv.Arg(0))
	if err != nil {
		die(err)
	}
	pos := int(inv.Int("position"))
	if pos < 1 {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--position is the folder's place among your folders, counted from 1; nothing was moved"})
	}
	var env struct {
		Data agentFolder `json:"data"`
	}
	if err := hostedMutate(cr, "PATCH", inContext(cr, "/api/v2/agent-folders/"+url.PathEscape(f.ID)), map[string]any{"position": pos - 1}, &env); err != nil {
		die(folderErr(err, "nothing was moved"))
	}
	emit(env.Data, func() { fmt.Printf("the folder %s is now number %d\n", env.Data.Name, env.Data.Position+1) })
}

func hostedAgentFolderDelete(cr hostedCreds, inv *Invocation) {
	list, err := fetchAgentFolders(cr)
	if err != nil {
		die(err)
	}
	f, err := pickFolder(list.Folders, inv.Arg(0))
	if err != nil {
		die(err)
	}
	var env struct {
		Data struct {
			Deleted     bool  `json:"deleted"`
			AgentsMoved int64 `json:"agents_moved"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "DELETE", inContext(cr, "/api/v2/agent-folders/"+url.PathEscape(f.ID)), nil, &env); err != nil {
		die(folderErr(err, "nothing was deleted"))
	}
	emit(env.Data, func() {
		fmt.Printf("deleted the folder %s; its %d agent(s) are back at the top level, and no agent was deleted\n", f.Name, env.Data.AgentsMoved)
	})
}

func hostedAgentMove(cr hostedCreds, inv *Invocation) {
	top, name, before := inv.Bool("top"), inv.Str("folder"), inv.Str("before")
	if top == (strings.TrimSpace(name) != "") {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "say where the agent goes: --folder NAME for a folder, or --top for the top level; nothing was moved", NextAction: "ks agent folder list"})
	}
	if top && before != "" {
		fail(&cliError{Code: exitUsage, Kind: "usage", Message: "--before places an agent inside a folder; it cannot go with --top; nothing was moved"})
	}
	sess := agentSession(cr, inv)
	a, err := resolveAgent(cr, sess, inv.Arg(0))
	if err != nil {
		die(err)
	}
	body := map[string]any{"folder_id": ""}
	where := "the top level"
	if !top {
		list, err := fetchAgentFolders(cr)
		if err != nil {
			die(err)
		}
		f, err := pickFolder(list.Folders, name)
		if err != nil {
			die(err)
		}
		body["folder_id"] = f.ID
		where = "the folder " + f.Name
		if before != "" {
			b, err := resolveAgent(cr, sess, before)
			if err != nil {
				die(err)
			}
			body["before_agent_id"] = b.ID
		}
	}
	var env struct {
		Data struct {
			AgentID  string `json:"agent_id"`
			FolderID string `json:"folder_id"`
		} `json:"data"`
	}
	if err := hostedMutate(cr, "PUT", inContext(cr, "/api/v2/agents/"+url.PathEscape(a.ID)+"/folder"), body, &env); err != nil {
		die(folderErr(err, "nothing was moved"))
	}
	emit(env.Data, func() { fmt.Printf("moved %s to %s\n", a.Name, where) })
}
