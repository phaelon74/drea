package tool

import (
	"context"
	"encoding/json"
)

// reply is a pseudo-tool that lets the model send plain-text messages to the
// user. It does not mutate state and needs no approval; the agent recognises
// it and ends the turn instead of dispatching it like a real tool.
type reply struct{}

func (reply) Name() string   { return "reply" }
func (reply) Mutating() bool { return false }
func (reply) Run(_ context.Context, _ json.RawMessage) (string, error) {
	return "", nil
}

func (reply) Description() string {
	return "Send a plain-text message to the user. Use this to greet, ask questions, report status, explain blockers, or give a final summary. Do not mix a reply with other tool calls in the same turn."
}

func (reply) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"message": {"type": "string", "minLength": 1, "description": "The plain-text message to show the user."}
		},
		"required": ["message"],
		"additionalProperties": false
	}`)
}

func (reply) Summary(args json.RawMessage) string {
	var a struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(args, &a)
	if a.Message == "" {
		return "(no message)"
	}
	return a.Message
}
