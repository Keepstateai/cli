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
	"strconv"
	"strings"
	"time"
)

// importMaxBytes is the most a conversation may be once what must not travel
// is taken out: 120 MB (120,000,000 bytes), what a session can resume. It was
// measured: on the guest (2 GiB of memory) the pinned runners resume up to about
// 150 MB and run out of memory above.
const importMaxBytes = 120_000_000

// importLimit is the limit in force. KS_IMPORT_MAX_BYTES may only lower it,
// so a test needs no 120 MB file.
func importLimit() int64 {
	if v, err := strconv.ParseInt(os.Getenv("KS_IMPORT_MAX_BYTES"), 10, 64); err == nil && v > 0 && v < importMaxBytes {
		return v
	}
	return importMaxBytes
}

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

// importReadProg, when set, is told how many bytes the sanitizing passes have
// read in all (set only around sanitizeTranscript, never for a light read).
var importReadProg func(n int64)

func readImportLines(path string, f func(line []byte) error) error {
	prog := importReadProg
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	r := bufio.NewReaderSize(fh, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if prog != nil {
			prog(int64(len(line)))
		}
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
	EstTokens      int64 // the first reply reads about this many (see import_estimate.go)
	EstCompacted   bool  // a compaction boundary cut what the harness sends
	// Boundary is the index (among the file's non-blank lines) of the last
	// compaction record kept, or -1. A trim starts there.
	Boundary int
	// NoTrimWhy: the file holds a compaction but there is nothing to trim to (and Boundary
	// is then -1): its summary cannot travel, or it is not in the file.
	NoTrimWhy string
	// Records and Prompts are what the written file holds: its records, and the person's
	// own messages among them.
	Records int
	Prompts int
}

// trimOpts says to write only what the harness sends the model: from the
// compaction record at index Start, and, for a Codex rollout, the session's
// first record before it (a rollout starts with its session_meta).
type trimOpts struct {
	On    bool
	Start int
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
	est      *tokenEstimator
	trim     trimOpts
	boundary int // the last compaction record's index, -1 if none
	// the last compaction's summary: whether it can travel in a trim, and if not why
	boundaryOK  bool
	boundaryWhy string
	afterBound  bool // a Claude Code boundary was just kept: the next record must be its summary
	records     int
	prompts     int
	sawMeta     bool // trim: the session_meta has been written
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
	s.est.add(rec) // the record as it is written: what the harness will read
	s.records++
	if s.isPrompt(rec) {
		s.prompts++
	}
	n, err := s.out.Write(b)
	s.written += int64(n)
	return err
}

// isPrompt: a record that is the person's own message.
func (s *sanitizer) isPrompt(rec map[string]any) bool {
	switch asStr(rec["type"]) {
	case "user":
		if c, _ := rec["isCompactSummary"].(bool); c {
			return false
		}
		if m := asMap(rec["message"]); m != nil {
			text, ok := humanTextOf(m["content"])
			return ok && !notAPrompt(text)
		}
	case "response_item":
		p := asMap(rec["payload"])
		if asStr(p["type"]) == "message" && asStr(p["role"]) == "user" {
			text, ok := humanTextOf(p["content"])
			return ok && !notAPrompt(text)
		}
	}
	return false
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
	return sanitizeTranscriptTrim(info, rw, sb, outPath, trimOpts{})
}

// sanitizeTranscriptTrim is sanitizeTranscript, optionally writing only what
// the harness sends after the last compaction (see trimOpts).
func sanitizeTranscriptTrim(info transcriptInfo, rw *pathRewriter, sb *summaryBuilder, outPath string, trim trimOpts) (*sanitizeResult, error) {
	fh, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(fh, h), 1<<20)
	s := &sanitizer{rw: rw, red: redactCounts{}, strip: map[string]int{}, summary: sb, id: info.ID, out: bw, est: newTokenEstimator(info.Tool), trim: trim, boundary: -1}
	rwBefore := 0
	if rw != nil {
		rwBefore = rw.Count
	}
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
		Stripped: s.strip, Redactions: s.red, EstTokens: s.est.Tokens(), EstCompacted: s.est.compacted, Boundary: s.boundary, NoTrimWhy: s.boundaryWhy, Records: s.records, Prompts: s.prompts}
	if s.boundary >= 0 && !s.boundaryOK { // a compaction the harness reads, but whose summary cannot travel
		res.Boundary = -1
	}
	if rw != nil {
		res.PathsRewritten = rw.Count - rwBefore
	}
	return res, nil
}

func (s *sanitizer) claude(path string) error {
	useIDs, resIDs := map[string]bool{}, map[string]bool{}
	n1 := -1
	err := readImportLines(path, func(line []byte) error {
		n1++
		if s.trim.On && n1 < s.trim.Start {
			return nil // a trim: what came before the last compaction is not sent
		}
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
	n2 := -1
	return readImportLines(path, func(line []byte) error {
		n2++
		if s.trim.On && n2 < s.trim.Start {
			return nil
		}
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
			if s.afterBound { // the record right after a boundary must be its summary
				s.afterBound = false
				if c, _ := rec["isCompactSummary"].(bool); c {
					s.boundaryOK, s.boundaryWhy = true, ""
				}
			}
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
		} else {
			s.boundary, s.boundaryOK, s.afterBound = n2, false, true
			s.boundaryWhy = "its compaction summary is not in the file, after the boundary"
			if cm := asMap(rec["compactMetadata"]); cm != nil {
				delete(cm, "preservedSegment")
				delete(cm, "preservedMessages")
			}
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
			if s.lastUUID == nil {
				delete(rec, "logicalParentUuid") // a trim's first record has nothing before it
			} else {
				rec["logicalParentUuid"] = s.lastUUID
			}
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
	n := -1
	return readImportLines(path, func(line []byte) error {
		n++
		rec, err := decodeImportLine(line)
		if err != nil {
			if !s.trim.On || n >= s.trim.Start {
				s.strip["unreadable line"]++
			}
			return nil
		}
		typ := asStr(rec["type"])
		p := asMap(rec["payload"])
		if s.trim.On && n < s.trim.Start {
			// a trim keeps, of what came before the compaction, only the
			// session's first record: a rollout starts with its session_meta
			if typ != "session_meta" || s.sawMeta {
				return nil
			}
		}
		if typ == "session_meta" {
			s.sawMeta = true
		}
		switch typ {
		case "compacted":
			s.boundary = n
			hist, hasHist := p["replacement_history"].([]any)
			_ = hist
			switch {
			case !hasHist:
				s.boundaryOK, s.boundaryWhy = false, "it has no replacement history (Codex rebuilds it from the earlier messages, which a trim would drop)"
			case strings.TrimSpace(asStr(p["message"])) == "":
				s.boundaryOK, s.boundaryWhy = false, "its summary is kept by Codex only in an encrypted form that cannot be carried"
			default:
				s.boundaryOK, s.boundaryWhy = true, ""
			}
			if !s.trim.On {
				s.strip["compacted"]++
				return nil
			}
			// kept in a trim: its summary and its replacement history, each
			// item held to the rules of any other record
			np := map[string]any{"message": asStr(p["message"])}
			if hist, ok := p["replacement_history"].([]any); ok {
				var kept []any
				for _, it := range hist {
					if s.codexHistoryItemOK(asMap(it)) {
						kept = append(kept, it)
					}
				}
				np["replacement_history"] = kept
			}
			rec["payload"] = np
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

// codexHistoryItemOK: whether an item of a compaction's replacement history may
// travel. The rules are those of a response_item record: no reasoning, no
// encrypted compaction item, no tool list, no developer message, no
// environment message.
func (s *sanitizer) codexHistoryItemOK(it map[string]any) bool {
	if it == nil {
		return false
	}
	switch asStr(it["type"]) {
	case "reasoning":
		s.strip["reasoning"]++
		return false
	case "compaction":
		s.strip["compaction"]++
		return false
	case "additional_tools":
		s.strip["tool list"]++
		return false
	case "message":
		text, _ := humanTextOf(it["content"])
		switch asStr(it["role"]) {
		case "developer":
			s.strip["developer message"]++
			return false
		case "user":
			if strings.Contains(text, "<environment_context>") {
				s.strip["environment message"]++
				return false
			}
		}
	}
	return true
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

// trimTranscript cleans a session again from its last compaction on (see trimOpts),
// into outPath, and answers the trim. A failure is an error, never a quiet fallback to
// the whole conversation: the file is read a second time and may have changed or gone.
func trimTranscript(info transcriptInfo, rw *pathRewriter, full *sanitizeResult, outPath string) (*sanitizeResult, error) {
	if full.Boundary < 0 {
		return nil, fmt.Errorf("there is no compaction to trim to")
	}
	tr, err := sanitizeTranscriptTrim(info, rw, nil, outPath, trimOpts{On: true, Start: full.Boundary})
	if err != nil {
		return nil, fmt.Errorf("the session could not be read again to trim it to its last compaction: %w", err)
	}
	if tr.Bytes == 0 || tr.Boundary < 0 {
		os.Remove(outPath)
		return nil, fmt.Errorf("the trim to the last compaction came out empty (the file changed between the two readings)")
	}
	return tr, nil
}
