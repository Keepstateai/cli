// import_estimate.go: how many tokens the first reply of an imported
// conversation will read, so the person is told what it costs before it is
// paid for. The service prices the count (it holds the rate book); this file
// only counts, and says how.
//
// Method (an estimate, always labelled one):
//
//   - What counts is what the harness sends on the first reply, not the file.
//     Claude Code rebuilds the conversation from its last compaction boundary:
//     the summary message that follows a `compact_boundary` record and every
//     record after it; what came before the boundary is not sent. A Codex
//     rollout is rebuilt from its last `compacted` record (its summary) and
//     the records after it. With no boundary, everything counts.
//   - Per record, the text the model reads is counted in bytes: message text,
//     a tool call's name and its input as JSON, a tool result's text. The
//     record's JSON envelope (ids, timestamps, paths of the old machine) is
//     not sent to the model and is not counted. Images and signed or
//     encrypted reasoning are already gone from the cleaned file.
//   - Tokens = ceil(text bytes / 3) + 6 per record. Three bytes to a token is
//     deliberately low: English prose runs near four and code and JSON near
//     three, so the figure leans high rather than low. Six tokens a record
//     stand for the role and block framing.
//   - Plus a fixed 15,000 tokens for the harness's own system prompt and tool
//     list, which the new session sends whatever the conversation is.
//
// It is not the provider's tokenizer, and a different model counts a little
// differently: the real figure is usually below this one.
package main

import (
	"encoding/json"
)

const (
	estimateBytesPerToken = 3
	estimateRecordTokens  = 6
	estimateOverheadToken = 15000
)

type tokenEstimator struct {
	tool      string
	textBytes int64
	records   int64
	compacted bool // a compaction boundary was found: what came before is not counted
}

func newTokenEstimator(tool string) *tokenEstimator { return &tokenEstimator{tool: tool} }

// Tokens is the estimate of what the first reply reads.
func (e *tokenEstimator) Tokens() int64 {
	if e == nil {
		return 0
	}
	return (e.textBytes+estimateBytesPerToken-1)/estimateBytesPerToken + e.records*estimateRecordTokens + estimateOverheadToken
}

func (e *tokenEstimator) reset() {
	e.textBytes, e.records, e.compacted = 0, 0, true
}

// add counts one cleaned record, in the order the file holds them.
func (e *tokenEstimator) add(rec map[string]any) {
	if e == nil {
		return
	}
	switch e.tool {
	case "claude-code":
		e.addClaude(rec)
	case "codex":
		e.addCodex(rec)
	}
}

func (e *tokenEstimator) addClaude(rec map[string]any) {
	switch asStr(rec["type"]) {
	case "system":
		if asStr(rec["subtype"]) == "compact_boundary" {
			e.reset()
		}
	case "user", "assistant":
		m := asMap(rec["message"])
		if m == nil {
			return
		}
		e.records++
		e.textBytes += contentBytes(m["content"])
	}
}

func (e *tokenEstimator) addCodex(rec map[string]any) {
	p := asMap(rec["payload"])
	switch asStr(rec["type"]) {
	case "compacted":
		e.reset()
		e.records++
		e.textBytes += int64(len(asStr(p["message"])))
		for _, it := range asList(p["replacement_history"]) {
			e.records++
			e.textBytes += contentBytes(asMap(it)["content"])
		}
	case "response_item":
		if p == nil {
			return
		}
		e.records++
		switch asStr(p["type"]) {
		case "message":
			e.textBytes += contentBytes(p["content"])
		case "function_call", "custom_tool_call", "local_shell_call":
			e.textBytes += int64(len(asStr(p["name"]))) + int64(len(asStr(p["arguments"]))) + int64(len(asStr(p["input"]))) + jsonBytes(p["action"])
		case "function_call_output", "custom_tool_call_output":
			if s, ok := p["output"].(string); ok {
				e.textBytes += int64(len(s))
			} else {
				e.textBytes += jsonBytes(p["output"])
			}
		}
	}
}

func jsonBytes(v any) int64 {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// contentBytes is the text a content value gives the model: a string, or the
// text of a list of blocks (text, a tool call's name and input, a tool
// result's text).
func contentBytes(v any) int64 {
	switch c := v.(type) {
	case string:
		return int64(len(c))
	case []any:
		var n int64
		for _, b := range c {
			m := asMap(b)
			switch asStr(m["type"]) {
			case "text", "input_text", "output_text":
				n += int64(len(asStr(m["text"])))
			case "tool_use":
				n += int64(len(asStr(m["name"]))) + jsonBytes(m["input"])
			case "tool_result":
				n += contentBytes(m["content"])
			}
		}
		return n
	}
	return 0
}
