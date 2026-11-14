// import_sanitize.go: what an imported transcript is made of, and what is
// taken out of it before it leaves this machine.
//
// A Claude Code session file or a Codex rollout carries more than the
// conversation: the system prompt and tool list of the machine that wrote it
// (a pinned runner replays a recorded one as the model's system prompt and
// ignores the new session's own), the account's ids, permission modes, file
// history, signed thinking, encrypted reasoning. None of that travels. What is
// kept is the conversation itself: the person's messages, the assistant's,
// the tool calls and what they returned. The rules were measured against the
// pinned runners, not assumed: a transcript made this way resumes in them,
// with the same session id, and the model is sent this session's own system
// prompt.
//
// Every string that is kept then has the old machine's project paths said as
// paths of the new workspace, and every credential-shaped match replaced.
// The files are read as streams: a transcript can reach hundreds of
// megabytes and is never held whole.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const importMaxBytes = 50_000_000

// transcriptInfo is what a light read of a transcript says about it: no
// message text, only identity and counts.
type transcriptInfo struct {
	Tool      string    `json:"tool"`
	Path      string    `json:"-"`
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	Model     string    `json:"model"`
	StartedAt string    `json:"started_at,omitempty"`
	Cwd       string    `json:"project"`
	Cwds      []string  `json:"-"`
	Turns     int       `json:"turns"`
	Messages  int       `json:"messages"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"-"`
}

func readImportLines(path string, f func(line []byte) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	r := bufio.NewReaderSize(fh, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if ferr := f(line); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ---- the light read ------------------------------------------------------------

type claudeLight struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	SessionID        string `json:"sessionId"`
	Cwd              string `json:"cwd"`
	Version          string `json:"version"`
	IsSidechain      bool   `json:"isSidechain"`
	IsMeta           bool   `json:"isMeta"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Timestamp        string `json:"timestamp"`
	Message          *struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type codexLight struct {
	Type    string `json:"type"`
	Payload struct {
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		ID         string          `json:"id"`
		Cwd        string          `json:"cwd"`
		CliVersion string          `json:"cli_version"`
		Timestamp  string          `json:"timestamp"`
		Model      string          `json:"model"`
		Content    json.RawMessage `json:"content"`
	} `json:"payload"`
}

// humanText answers the text of a content value (a string, or the text blocks
// of a list), and whether anything but tool results is in it.
func humanText(content json.RawMessage) (string, bool) {
	c := bytes.TrimSpace(content)
	if len(c) == 0 {
		return "", false
	}
	if c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) != nil {
			return "", false
		}
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(c, &blocks) != nil {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if (b.Type == "text" || b.Type == "input_text" || b.Type == "output_text") && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), len(parts) > 0
}

// notAPrompt: text the tool wrote into the person's turn (reminders,
// command echoes), not words the person typed.
func notAPrompt(s string) bool {
	t := strings.TrimSpace(s)
	for _, p := range []string{"<system-reminder>", "<command-name>", "<command-message>", "<local-command", "<environment_context>", "<user_instructions>", "Caveat:", "[Request interrupted"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return t == ""
}

type freq map[string]int

func (f freq) top() []string {
	ks := make([]string, 0, len(f))
	for k := range f {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if f[ks[i]] != f[ks[j]] {
			return f[ks[i]] > f[ks[j]]
		}
		return ks[i] < ks[j]
	})
	return ks
}

// scanTranscript reads a transcript's identity and counts, never its text.
func scanTranscript(tool, path string) (transcriptInfo, error) {
	info := transcriptInfo{Tool: tool, Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return info, err
	}
	info.Size, info.ModTime = fi.Size(), fi.ModTime()
	cwds := freq{}
	switch tool {
	case "claude-code":
		info.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		err = readImportLines(path, func(line []byte) error {
			var r claudeLight
			if json.Unmarshal(line, &r) != nil {
				return nil
			}
			if r.Version != "" {
				info.Version = r.Version
			}
			if r.Timestamp != "" && info.StartedAt == "" && (r.Type == "user" || r.Type == "assistant") {
				info.StartedAt = r.Timestamp
			}
			if r.IsSidechain || (r.Type != "user" && r.Type != "assistant") {
				return nil
			}
			if r.Cwd != "" {
				cwds[r.Cwd]++
			}
			info.Messages++
			if r.Message != nil {
				if r.Type == "assistant" && r.Message.Model != "" {
					info.Model = r.Message.Model
				}
				if r.Type == "user" && !r.IsMeta && !r.IsCompactSummary {
					if text, ok := humanText(r.Message.Content); ok && !notAPrompt(text) {
						info.Turns++
					}
				}
			}
			return nil
		})
	case "codex":
		err = readImportLines(path, func(line []byte) error {
			var r codexLight
			if json.Unmarshal(line, &r) != nil {
				return nil
			}
			switch r.Type {
			case "session_meta":
				info.ID, info.Cwd, info.Version, info.StartedAt = r.Payload.ID, r.Payload.Cwd, r.Payload.CliVersion, r.Payload.Timestamp
				if r.Payload.Cwd != "" {
					cwds[r.Payload.Cwd]++
				}
			case "turn_context":
				if r.Payload.Model != "" {
					info.Model = r.Payload.Model
				}
				if r.Payload.Cwd != "" {
					cwds[r.Payload.Cwd]++
				}
			case "response_item":
				if r.Payload.Type == "message" && (r.Payload.Role == "user" || r.Payload.Role == "assistant") {
					text, _ := humanText(r.Payload.Content)
					if r.Payload.Role == "user" && notAPrompt(text) {
						return nil
					}
					info.Messages++
					if r.Payload.Role == "user" {
						info.Turns++
					}
				}
			}
			return nil
		})
		if info.ID == "" {
			base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			if i := strings.LastIndex(base, "-"); i > 0 && len(base) >= 36 {
				info.ID = base[len(base)-36:]
			}
		}
	default:
		return info, fmt.Errorf("unknown tool %q", tool)
	}
	if err != nil {
		return info, err
	}
	info.Cwds = cwds.top()
	if info.Cwd == "" && len(info.Cwds) > 0 {
		info.Cwd = info.Cwds[0]
	}
	if tool == "codex" && len(info.Cwds) > 0 {
		info.Cwd = info.Cwds[0]
	}
	return info, nil
}

// ---- the sanitizing -----------------------------------------------------------

type sanitizeResult struct {
	Info           transcriptInfo
	OutPath        string
	Bytes          int64
	SHA256         string
	Stripped       map[string]int
	Redactions     redactCounts
	PathsRewritten int
}

type sanitizer struct {
	rw       *pathRewriter
	red      redactCounts
	strip    map[string]int
	summary  *summaryBuilder
	id       string
	out      *bufio.Writer
	written  int64
	lastUUID any
}

func (s *sanitizer) mapStrings(v any) any {
	switch x := v.(type) {
	case string:
		return redactImportText(s.rw.apply(x), s.red)
	case []any:
		for i := range x {
			x[i] = s.mapStrings(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = s.mapStrings(x[k])
		}
		return x
	}
	return v
}

func (s *sanitizer) write(rec map[string]any) error {
	b, err := importJSON(s.mapStrings(rec))
	if err != nil {
		return err
	}
	n, err := s.out.Write(b)
	s.written += int64(n)
	return err
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func asList(v any) []any         { l, _ := v.([]any); return l }
func asStr(v any) string         { s, _ := v.(string); return s }

const imagePlaceholder = "[an image was attached here and was not imported]"
const documentPlaceholder = "[a document was attached here and was not imported]"

// claudeBlocks filters one content list; useIDs and resIDs are the tool call
// and tool result ids that have a partner in the kept records.
func (s *sanitizer) claudeBlocks(list []any, useIDs, resIDs map[string]bool) []any {
	var out []any
	for _, b := range list {
		m := asMap(b)
		if m == nil {
			out = append(out, b)
			continue
		}
		switch asStr(m["type"]) {
		case "thinking", "redacted_thinking":
			s.strip["thinking"]++
			continue
		case "image":
			s.strip["image"]++
			out = append(out, map[string]any{"type": "text", "text": imagePlaceholder})
			continue
		case "document":
			s.strip["document"]++
			out = append(out, map[string]any{"type": "text", "text": documentPlaceholder})
			continue
		case "tool_use":
			if !resIDs[asStr(m["id"])] {
				s.strip["tool call without a result"]++
				continue
			}
		case "tool_result":
			if !useIDs[asStr(m["tool_use_id"])] {
				s.strip["tool result without a call"]++
				continue
			}
			if inner := asList(m["content"]); inner != nil {
				m["content"] = s.claudeBlocks(inner, useIDs, resIDs)
				if len(asList(m["content"])) == 0 {
					m["content"] = ""
				}
			}
		}
		out = append(out, m)
	}
	return out
}

var claudeDroppedFields = []string{"toolUseResult", "serverClassifierContext", "wireIngestContext", "permissionMode", "promptId", "sourceToolAssistantUUID"}

// claudeCandidate: a record that may be kept (type and sidechain only).
func claudeCandidate(r *claudeLight) bool {
	if r.IsSidechain {
		return false
	}
	switch r.Type {
	case "user", "assistant":
		return true
	case "system":
		return r.Subtype == "compact_boundary"
	}
	return false
}

// sanitizeTranscript writes the sanitized transcript to outPath and answers
// what it holds and what was taken out.
func sanitizeTranscript(info transcriptInfo, rw *pathRewriter, sb *summaryBuilder, outPath string) (*sanitizeResult, error) {
	fh, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(fh, h), 1<<20)
	s := &sanitizer{rw: rw, red: redactCounts{}, strip: map[string]int{}, summary: sb, id: info.ID, out: bw}
	switch info.Tool {
	case "claude-code":
		err = s.claude(info.Path)
	case "codex":
		err = s.codex(info.Path)
	default:
		err = fmt.Errorf("unknown tool %q", info.Tool)
	}
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(outPath)
		return nil, err
	}
	res := &sanitizeResult{Info: info, OutPath: outPath, Bytes: s.written, SHA256: hex.EncodeToString(h.Sum(nil)),
		Stripped: s.strip, Redactions: s.red}
	if rw != nil {
		res.PathsRewritten = rw.Count
	}
	return res, nil
}

func (s *sanitizer) claude(path string) error {
	useIDs, resIDs := map[string]bool{}, map[string]bool{}
	err := readImportLines(path, func(line []byte) error {
		var r struct {
			claudeLight
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &r) != nil || !claudeCandidate(&r.claudeLight) || r.Message == nil {
			return nil
		}
		c := bytes.TrimSpace(r.Message.Content)
		if len(c) == 0 || c[0] != '[' {
			return nil
		}
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(c, &blocks) != nil {
			return nil
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" && r.Type == "assistant" {
				useIDs[b.ID] = true
			}
			if b.Type == "tool_result" && b.ToolUseID != "" && r.Type == "user" {
				resIDs[b.ToolUseID] = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return readImportLines(path, func(line []byte) error {
		rec, err := decodeImportLine(line)
		if err != nil {
			s.strip["unreadable line"]++
			return nil
		}
		typ := asStr(rec["type"])
		side, _ := rec["isSidechain"].(bool)
		keep := !side && (typ == "user" || typ == "assistant" || (typ == "system" && asStr(rec["subtype"]) == "compact_boundary"))
		if !keep {
			switch {
			case side:
				s.strip["subagent record"]++
			case typ == "":
				s.strip["record of no type"]++
			default:
				s.strip[typ]++
			}
			return nil
		}
		if meta, _ := rec["isMeta"].(bool); meta && typ == "user" {
			s.strip["meta message"]++
			return nil
		}
		if typ == "user" || typ == "assistant" {
			if _, ok := rec["toolUseResult"]; ok {
				s.strip["tool result detail"]++
			}
			for _, f := range claudeDroppedFields {
				delete(rec, f)
			}
			if m := asMap(rec["message"]); m != nil {
				if list, ok := m["content"].([]any); ok {
					nl := s.claudeBlocks(list, useIDs, resIDs)
					if len(nl) == 0 {
						s.strip["message left empty"]++
						return nil
					}
					m["content"] = nl
				}
			}
		} else if cm := asMap(rec["compactMetadata"]); cm != nil {
			delete(cm, "preservedSegment")
			delete(cm, "preservedMessages")
		}
		if _, ok := rec["cwd"]; ok {
			rec["cwd"] = importGuestWorkspace
		}
		if _, ok := rec["sessionId"]; ok {
			rec["sessionId"] = s.id
		}
		if _, ok := rec["session_id"]; ok {
			rec["session_id"] = s.id
		}
		if _, ok := rec["parentUuid"]; ok || rec["uuid"] != nil {
			rec["parentUuid"] = s.lastUUID
		}
		if _, ok := rec["logicalParentUuid"]; ok {
			rec["logicalParentUuid"] = s.lastUUID
		}
		if u := rec["uuid"]; u != nil {
			s.lastUUID = u
		}
		if s.summary != nil && (typ == "user" || typ == "assistant") {
			if m := asMap(rec["message"]); m != nil {
				if text, ok := humanTextOf(m["content"]); ok {
					if typ == "assistant" {
						s.summary.add("assistant", text)
					} else if meta, _ := rec["isCompactSummary"].(bool); !meta && !notAPrompt(text) {
						s.summary.add("user", text)
					}
				}
			}
		}
		return s.write(rec)
	})
}

// humanTextOf is humanText over a decoded value.
func humanTextOf(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []any:
		var parts []string
		for _, b := range x {
			m := asMap(b)
			if m == nil {
				continue
			}
			switch asStr(m["type"]) {
			case "text", "input_text", "output_text":
				if t := asStr(m["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n"), len(parts) > 0
	}
	return "", false
}

var codexEventsKept = map[string]bool{"user_message": true, "agent_message": true, "task_started": true, "task_complete": true, "turn_aborted": true}

func (s *sanitizer) codex(path string) error {
	return readImportLines(path, func(line []byte) error {
		rec, err := decodeImportLine(line)
		if err != nil {
			s.strip["unreadable line"]++
			return nil
		}
		typ := asStr(rec["type"])
		p := asMap(rec["payload"])
		switch typ {
		case "session_meta", "turn_context":
		case "response_item":
			pt := asStr(p["type"])
			switch pt {
			case "reasoning":
				s.strip["reasoning"]++
				return nil
			case "compaction":
				s.strip["compaction"]++
				return nil
			case "additional_tools":
				s.strip["tool list"]++
				return nil
			case "message":
				text, _ := humanTextOf(p["content"])
				switch asStr(p["role"]) {
				case "developer":
					s.strip["developer message"]++
					return nil
				case "user":
					if strings.Contains(text, "<environment_context>") {
						s.strip["environment message"]++
						return nil
					}
					if s.summary != nil && !notAPrompt(text) {
						s.summary.add("user", text)
					}
				case "assistant":
					if s.summary != nil {
						s.summary.add("assistant", text)
					}
				}
			}
		case "event_msg":
			if p == nil || !codexEventsKept[asStr(p["type"])] {
				s.strip["event"]++
				return nil
			}
		default:
			if typ == "" {
				typ = "record of no type"
			}
			s.strip[typ]++
			return nil
		}
		if p != nil && typ == "session_meta" {
			for _, k := range []string{"base_instructions", "creator_account_id", "creator_user_id", "parent_thread_id", "forked_from_id"} {
				if _, ok := p[k]; ok {
					delete(p, k)
				}
			}
			p["cwd"] = importGuestWorkspace
			if _, ok := p["runtime_workspace_roots"]; ok {
				p["runtime_workspace_roots"] = []any{importGuestWorkspace}
			}
		}
		if p != nil && typ == "turn_context" {
			p["cwd"] = importGuestWorkspace
			if _, ok := p["workspace_roots"]; ok {
				p["workspace_roots"] = []any{importGuestWorkspace}
			}
		}
		return s.write(rec)
	})
}

// strippedWords is what was taken out, in plain words, biggest first.
func strippedWords(m map[string]int) string {
	if len(m) == 0 {
		return "nothing"
	}
	type kv struct {
		k string
		n int
	}
	var kvs []kv
	for k, n := range m {
		kvs = append(kvs, kv{k, n})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].n != kvs[j].n {
			return kvs[i].n > kvs[j].n
		}
		return kvs[i].k < kvs[j].k
	})
	var parts []string
	for _, x := range kvs {
		parts = append(parts, fmt.Sprintf("%d %s", x.n, x.k))
	}
	return strings.Join(parts, ", ")
}
