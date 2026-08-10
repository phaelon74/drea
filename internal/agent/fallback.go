package agent

import (
	"encoding/json"
	"strings"

	"github.com/dreaagent/drea/internal/llm"
)

// parseToolCallsFromText recovers tool calls that a model printed as plain
// text instead of emitting through the native tool_calls mechanism. Small
// models (and some local OpenAI-compatible shims) lack reliable tool-calling
// support and respond with JSON like [{"name":"list_dir","arguments":{...}}];
// without this the harness would treat the turn as "done" and drop back to the
// prompt. ok is false when the text is not a recognisable tool call, in which
// case it is ordinary assistant prose.
func parseToolCallsFromText(content string) ([]llm.ToolCall, bool) {
	s := strings.TrimSpace(content)
	s = stripCodeFence(s)
	if s == "" {
		return nil, false
	}

	var calls []llm.ToolCall
	if strings.HasPrefix(s, "[") {
		var arr []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return nil, false
		}
		for _, obj := range arr {
			if tc, ok := parseOneToolCall(obj); ok {
				calls = append(calls, tc)
			}
		}
	} else if strings.HasPrefix(s, "{") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return nil, false
		}
		if tc, ok := parseOneToolCall(obj); ok {
			calls = append(calls, tc)
		}
	}
	return calls, len(calls) > 0
}

// parseOneToolCall converts a single JSON object into a ToolCall, tolerating
// both the explicit {"name":..., "arguments":...} shape and the inline
// {"tool":"list_dir","path":"."} shorthand.
// The special name "reply" is preserved so JSON-mode plain-text replies are
// recognised even when the model prints them as text rather than using the
// native tool_calls mechanism.
func parseOneToolCall(obj map[string]json.RawMessage) (llm.ToolCall, bool) {
	name := firstString(obj, "name", "tool", "function")
	if name == "" {
		return llm.ToolCall{}, false
	}
	tc := llm.ToolCall{Type: "function", Function: llm.FunctionCall{Name: name}}
	if args, ok := obj["arguments"]; ok {
		// The arguments may be a JSON object or a JSON-encoded string; the
		// native format expects the latter, so normalise to it.
		var argsStr string
		if json.Unmarshal(args, &argsStr) == nil {
			tc.Function.Arguments = argsStr
		} else {
			tc.Function.Arguments = string(args)
		}
	} else {
		// No explicit arguments field: treat the remaining fields as the
		// arguments object (e.g. {"tool":"list_dir","path":"."}).
		delete(obj, "name")
		delete(obj, "tool")
		delete(obj, "function")
		if b, err := json.Marshal(obj); err == nil && string(b) != "{}" {
			tc.Function.Arguments = string(b)
		}
	}
	return tc, true
}

// stripCodeFence removes a surrounding ```json ... ``` fence if present, so a
// model that wraps its JSON in a code block is still understood.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// parseToolCallFromText recovers a single tool call from plain text. It is a
// thin wrapper around parseToolCallsFromText for callers that only need one.
func parseToolCallFromText(content string) (llm.ToolCall, bool) {
	calls, ok := parseToolCallsFromText(content)
	if !ok || len(calls) == 0 {
		return llm.ToolCall{}, false
	}
	return calls[0], true
}

// firstString returns the first non-empty string value among the given keys.
func firstString(obj map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		var v string
		if raw, ok := obj[k]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
			return v
		}
	}
	return ""
}
