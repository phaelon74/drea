// Package agent implements the core reason-act loop: send the conversation and
// tool schemas to the model, execute the tool calls it requests (subject to
// approval), feed the results back, and repeat until the task is complete.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/diff"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/patch"
	"github.com/dreaagent/drea/internal/policy"
	"github.com/dreaagent/drea/internal/session"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

// Agent ties together the model client, tool registry, UI and conversation
// state. It is safe to reuse across multiple user turns (interactive mode).
type Agent struct {
	cfg      config.Config
	client   modelClient
	tools    *tool.Registry
	ui       *ui.UI
	confirm  func(string) bool
	policy   *policy.Policy
	messages []llm.Message

	// lastPromptTokens is the prompt-token count reported by the endpoint for
	// the most recent request (0 when unknown); it drives compaction.
	lastPromptTokens int
	// verifyRounds counts how many times the verification command has failed
	// and been fed back within the current Run.
	verifyRounds int

	// checkpoint is the commit taken before the current task, and baseline is
	// what the verification command reported at that point. Together they let
	// a task that regressed the goal be recognised and undone.
	checkpoint string
	baseline   Measure
	// objective is the latest state of the verification command, exposed so a
	// caller can tell work that met the goal from work that merely finished.
	objective Measure

	stall stallDetector
	// emptyNudged records that an empty assistant turn already received one
	// corrective nudge; a second empty turn fails the task instead of looping.
	emptyNudged bool
	// task and total accumulate reported token usage for the current task and
	// for the session, so a runaway loop is visible in what it costs.
	task  Usage
	total Usage
}

type modelClient interface {
	Stream(context.Context, []llm.Message, []llm.Tool, llm.Handlers) (*llm.Result, error)
	StreamWithOptions(context.Context, []llm.Message, []llm.Tool, llm.Handlers, llm.StreamOpts) (*llm.Result, error)
	JSONMode() bool
}

// Usage is cumulative token accounting as reported by the endpoint.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	Requests         int
}

func (u Usage) add(v llm.Usage, attempts int) Usage {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	if attempts <= 0 {
		attempts = 1
	}
	u.Requests += attempts
	return u
}

// String renders usage for display. An endpoint that reports no usage yields
// zeroes, which is honest: nothing is estimated here.
func (u Usage) String() string {
	return fmt.Sprintf("%d prompt + %d completion tokens over %d request(s)",
		u.PromptTokens, u.CompletionTokens, u.Requests)
}

// Usage reports token accounting for the session so far.
func (a *Agent) Usage() Usage { return a.total }

// SetAutoApprove updates whether mutating tools skip confirmation. Slash
// commands must call this rather than mutating a copy of Config that the
// agent does not see.
func (a *Agent) SetAutoApprove(v bool) { a.cfg.AutoApprove = v }

// SetVerify updates the verification command used when a task finishes.
func (a *Agent) SetVerify(cmd string) { a.cfg.Verify = cmd }

// SetCheckpoint updates whether each task is checkpointed before work starts.
func (a *Agent) SetCheckpoint(v bool) { a.cfg.Checkpoint = v }

// AutoApprove reports whether mutating tools currently skip confirmation.
func (a *Agent) AutoApprove() bool { return a.cfg.AutoApprove }

// VerifyCommand reports the current verification command, if any.
func (a *Agent) VerifyCommand() string { return a.cfg.Verify }

// Checkpointing reports whether per-task checkpointing is enabled.
func (a *Agent) Checkpointing() bool { return a.cfg.Checkpoint }

// New constructs an agent with a seeded system prompt.
func New(cfg config.Config, client *llm.Client, tools *tool.Registry, u *ui.UI) *Agent {
	// Config.Normalize already validated the patterns; on the off chance of an
	// error here, fall back to the built-in deny list only rather than failing.
	pol, err := policy.New(cfg.AllowCommands, cfg.DenyCommands)
	if err != nil {
		pol, _ = policy.New(nil, nil)
	}
	return &Agent{
		cfg:    cfg,
		client: client,
		tools:  tools,
		ui:     u,
		confirm: u.Confirm,
		policy: pol,
		messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt(cfg.Workdir, tools.Names(), cfg.JSONMode)},
		},
	}
}

// Run processes a single user instruction, looping through model turns and
// tool executions until the model stops requesting tools or MaxSteps is hit.
func (a *Agent) Run(ctx context.Context, userInput string) error {
	a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: userInput})
	a.verifyRounds = 0
	a.task = Usage{}
	a.emptyNudged = false
	a.stall.reset()
	a.persist()
	a.beginTask(ctx, userInput)
	defer a.reportUsage()

	for step := 0; step < a.cfg.MaxSteps; step++ {
		a.maybeCompact(ctx)
		a.updateStatus()
		printedHeader := false
		contentOpen := false
		ensureNL := func() {
			if contentOpen {
				a.ui.Assistant("\n")
				contentOpen = false
			}
		}
		live := newLiveWrites(a.ui, a.tools, ensureNL)
		a.ui.StartThinking()
		res, err := a.client.Stream(ctx, a.messages, a.tools.Specs(), llm.Handlers{
			OnContent: func(delta string) {
				a.ui.StopThinking()
				if !printedHeader {
					a.ui.AssistantHeader()
					printedHeader = true
				}
				a.ui.Assistant(delta)
				contentOpen = !strings.HasSuffix(delta, "\n")
			},
			OnToolName: live.onName,
			OnToolArgs: live.onArgs,
			OnRetry: func(attempt int, delay time.Duration, err error) {
				ensureNL()
				a.ui.Warn(fmt.Sprintf("│  %s; retrying in %s (attempt %d)",
					shortErr(err), delay.Round(100*time.Millisecond), attempt))
			},
		})
		live.close()
		a.ui.StopThinking()
		if res != nil {
			if res.Usage.PromptTokens > 0 {
				a.lastPromptTokens = res.Usage.PromptTokens
			}
			a.task, a.total = a.task.add(res.Usage, res.Attempts), a.total.add(res.Usage, res.Attempts)
			a.updateStatus()
		}
		if err != nil {
			return err
		}
		ensureNL()

		// Record the assistant turn (content and/or tool calls).
		// Keep the recorded message in sync with any post-processing below
		// (text-recovered tool calls, reply pseudo-tools, etc.) so the transcript
		// always matches what the agent actually dispatches.
		a.messages = append(a.messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   res.Content,
			ToolCalls: res.ToolCalls,
		})
		recorded := &a.messages[len(a.messages)-1]

		if len(res.ToolCalls) == 0 {
			// A model without reliable native tool-calling may have printed tool
			// calls as JSON text instead of emitting a tool_calls block.
			// Recover them so the turn is executed rather than mistaken for done.
			if tcs, ok := parseToolCallsFromText(res.Content); ok {
				res.ToolCalls = tcs
				res.Content = ""
				recorded.ToolCalls = res.ToolCalls
				recorded.Content = res.Content
			}
		}
		// A turn containing only reply pseudo-tools is assistant prose, including
		// the empty case. Classify it before verification so a vacuous reply
		// receives the same bounded correction as empty native content.
		if onlyReplies(res.ToolCalls) {
			msg := joinReplyMessages(res.ToolCalls)
			res.Content = msg
			res.ToolCalls = nil
			recorded.Content = msg
			recorded.ToolCalls = nil
			if msg != "" {
				if !printedHeader {
					a.ui.AssistantHeader()
					printedHeader = true
				}
				a.ui.Assistant(msg)
				contentOpen = !strings.HasSuffix(msg, "\n")
			}
		}

		// If the model mixed a reply with real tools, drop the reply and execute
		// only the real tools. The reply can be re-emitted on the next turn once
		// the tool results are known.
		filtered := res.ToolCalls[:0]
		for _, tc := range res.ToolCalls {
			if !llm.IsReplyCall(tc) {
				filtered = append(filtered, tc)
			}
		}
		res.ToolCalls = filtered
		recorded.ToolCalls = res.ToolCalls

		if len(res.ToolCalls) == 0 {
			if emptyAssistantContent(res.Content, a.client.JSONMode()) {
				res.Content = ""
				recorded.Content = ""
				if !a.emptyNudged {
					a.emptyNudged = true
					a.ui.Warn("│  model returned an empty reply; asking for a real action.")
					a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: "Your last response was empty. Please continue the task by issuing a real tool call or a non-empty reply."})
					a.persist()
					continue
				}
				return errors.New("model returned repeated empty replies")
			}
			// Only meaningful final output is eligible for verification.
			if fb, retry := a.verify(ctx); retry {
				a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: fb})
				a.persist()
				continue
			}
			a.persist()
			return nil
		}

		nudge, abort := a.stall.observe(res.ToolCalls)
		for _, tc := range res.ToolCalls {
			result := a.dispatch(ctx, tc)
			a.messages = append(a.messages, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    trimForModel(result),
			})
		}
		switch {
		case abort:
			a.ui.Warn("│  the same action is being repeated with no progress; stopping this task.")
			a.persist()
			return nil
		case nudge:
			a.ui.Warn("│  repeated identical action; prompting for a different approach.")
			a.messages = append(a.messages, llm.Message{Role: llm.RoleUser, Content: a.stall.stallMessage()})
		}
		a.persist()
	}

	a.ui.Warn(fmt.Sprintf("reached step limit (%d); stopping.", a.cfg.MaxSteps))
	return nil
}

// dispatch resolves and runs a single tool call, handling approval and
// returning the text to feed back to the model (errors are returned as text so
// the model can recover rather than aborting the whole run).
func (a *Agent) dispatch(ctx context.Context, tc llm.ToolCall) string {
	t, ok := a.tools.Get(tc.Function.Name)
	if !ok {
		a.ui.ToolCall(tc.Function.Name, "(unknown)")
		return toolError(fmt.Errorf("unknown tool %q", tc.Function.Name))
	}

	args := json.RawMessage(tc.Function.Arguments)
	summary := t.Summary(args)
	a.ui.ToolCall(t.Name(), summary)

	// For file edits, preview the change as a diff (before approval).
	if d, added, removed, ok := a.prospectiveDiff(t.Name(), args); ok {
		a.ui.Diff(d)
		a.ui.Info(fmt.Sprintf("│  (+%d/-%d)", added, removed))
	}

	requireApproval := (t.Mutating() && !a.cfg.AutoApprove) || t.AlwaysConfirm()
	// The command policy governs run_command specifically: a denied command is
	// refused regardless of auto-approve; an allowed one runs without a prompt.
	if t.Name() == "run_command" {
		switch a.policy.Decide(commandArg(args)) {
		case policy.Deny:
			a.ui.Warn("│  blocked by command policy (deny list)")
			return "This command is blocked by the configured command policy and was not run. Choose a safe alternative or ask the user to adjust the policy."
		case policy.Allow:
			requireApproval = false
			a.ui.Info("│  (auto-approved by command policy)")
		}
	}
	if requireApproval {
		if t.Name() == "run_command" {
			for _, line := range approvalCommandLines(commandArg(args)) {
				a.ui.Info("│  " + line)
			}
		}
		if !a.confirm("│  approve this action?") {
			a.ui.Warn("│  denied by user")
			return "The user denied this action. Consider a different approach or ask what to do."
		}
	}

	out, err := t.Run(ctx, args)
	if err != nil {
		a.ui.ToolResult(err.Error(), true)
		return toolError(err)
	}
	a.ui.ToolResult(out, false)
	return out
}

// onlyReplies reports whether every tool call is a "reply" pseudo-tool.
func onlyReplies(tcs []llm.ToolCall) bool {
	if len(tcs) == 0 {
		return false
	}
	for _, tc := range tcs {
		if !llm.IsReplyCall(tc) {
			return false
		}
	}
	return true
}

func emptyAssistantContent(content string, jsonMode bool) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return true
	}
	if !jsonMode || !strings.HasPrefix(content, "[") {
		return false
	}
	var items []json.RawMessage
	return json.Unmarshal([]byte(content), &items) == nil && len(items) == 0
}

// joinReplyMessages concatenates the messages from reply pseudo-tools,
// preserving order and dropping empty ones.
func joinReplyMessages(tcs []llm.ToolCall) string {
	var parts []string
	for _, tc := range tcs {
		if msg := llm.ReplyMessage(tc); msg != "" {
			parts = append(parts, msg)
		}
	}
	return strings.Join(parts, "\n")
}

// reportUsage prints what the task cost, when the endpoint reported anything.
func (a *Agent) reportUsage() {
	if a.task.PromptTokens+a.task.CompletionTokens == 0 {
		return
	}
	a.ui.Info("╰─ usage: " + a.task.String())
}

// Reset clears the conversation history, keeping only the system prompt.
func (a *Agent) Reset() {
	a.messages = a.messages[:1]
	a.lastPromptTokens = 0
	a.persist()
	a.updateStatus()
}

// updateStatus refreshes the bottom status bar with the current context usage.
func (a *Agent) updateStatus() {
	used := a.lastPromptTokens
	if used == 0 {
		used = estimateTokens(a.messages)
	}
	a.ui.SetStatus(used, a.cfg.ContextTokens)
	a.ui.ShowStatus()
}

// persist saves the current transcript for this workspace when persistence is
// enabled. It never writes the API key (only messages are stored) and failures
// are non-fatal — persistence must never break an in-progress task.
func (a *Agent) persist() {
	if !a.cfg.Persist {
		return
	}
	_ = session.Save(session.Session{
		Workdir:  a.cfg.Workdir,
		Model:    a.cfg.Model,
		Messages: a.messages,
	})
}

// Restore replaces the conversation with a previously saved transcript so a
// session can be resumed after exit or a crash. An empty transcript is ignored.
func (a *Agent) Restore(msgs []llm.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	a.messages = append([]llm.Message(nil), msgs...)
	a.lastPromptTokens = estimateTokens(a.messages)
	return true
}

// MessageCount reports how many messages are in the current transcript.
func (a *Agent) MessageCount() int { return len(a.messages) }

// prospectiveDiff computes the diff a write_file/edit_file/apply_patch call
// would produce, so it can be previewed before the change is applied. Edits are
// replayed through the same patch engine the tools use, so the preview cannot
// diverge from what is written. ok is false for other tools or when the change
// cannot be determined (e.g. an ambiguous edit, which the tool itself rejects).
func (a *Agent) prospectiveDiff(name string, args json.RawMessage) (text string, added, removed int, ok bool) {
	var whole string // full content for write_file
	path, edits, isPatch := tool.ParseEdits(name, args)
	if !isPatch {
		var a2 struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if name != "write_file" || json.Unmarshal(args, &a2) != nil || a2.Path == "" {
			return "", 0, 0, false
		}
		path, whole = a2.Path, a2.Content
	}
	// Bound and securely open the old file before constructing preview text.
	// Reading max+1 distinguishes an oversized file without allocating it all.
	const maxDiffBytes = 256 * 1024
	oldData, _, err := a.tools.ReadFileLimited(path, maxDiffBytes)
	if err != nil {
		return "", 0, 0, false
	}
	oldText := string(oldData)

	newText := whole
	if isPatch {
		res, err := patch.Apply(oldText, edits)
		if err != nil {
			return "", 0, 0, false
		}
		newText = res.Text
	}
	if newText == oldText {
		return "", 0, 0, false
	}
	// Bound the O(n*m) line diff: skip the preview for very large files or
	// pathological many-short-line inputs that would OOM the LCS table.
	if len(oldText) > maxDiffBytes || len(newText) > maxDiffBytes {
		return "", 0, 0, false
	}
	if int64(diffLineCount(oldText))*int64(diffLineCount(newText)) > diff.MaxLineProduct {
		return "", 0, 0, false
	}
	lines := diff.Lines(oldText, newText)
	added, removed = diff.StatLines(lines)
	return diff.UnifiedLines(lines, 3), added, removed, true
}

func diffLineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// shortErr collapses an error to a single, length-bounded line so a retry
// notice stays readable even when the endpoint returns a large HTML body.
func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "<"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = stripControl(s)
	const max = 120
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// stripControl removes escape and other control characters from a single-line
// string so a provider-supplied message cannot corrupt the terminal.
func stripControl(s string) string {
	var b strings.Builder
	inEsc, inCSI := false, false
	for _, r := range s {
		switch {
		case inCSI:
			if r >= 0x40 && r <= 0x7e {
				inCSI = false
			}
		case inEsc:
			inEsc = false
			if r == '[' {
				inCSI = true
			}
		case r == 0x1b:
			inEsc = true
		case r < 0x20 || r == 0x7f:
			// drop control characters.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// commandArg extracts the "command" field from a run_command tool call's
// arguments so the policy can classify it. It returns "" when absent.
func commandArg(args json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Command
}

// maxApprovalCommandBytes caps how much of a run_command string is shown in
// the approval prompt. Beyond this, a head and visible tail are kept so the
// operator can still see how the command ends.
const maxApprovalCommandBytes = 8 * 1024

// approvalCommandLines formats a command for the approval prompt: multiline
// preserved, capped at maxApprovalCommandBytes with an explicit truncation
// marker and a visible tail. Line sanitisation is left to the UI.
func approvalCommandLines(cmd string) []string {
	cmd = strings.ReplaceAll(cmd, "\r\n", "\n")
	cmd = strings.ReplaceAll(cmd, "\r", "\n")
	if len(cmd) > maxApprovalCommandBytes {
		const tail = 256
		marker := "\n… (truncated; showing head and tail) …\n"
		head := maxApprovalCommandBytes - len(marker) - tail
		if head < 64 {
			head = 64
		}
		if tail+head+len(marker) > len(cmd) {
			// Degenerate tiny input past the cap: just clip.
			cmd = cmd[:maxApprovalCommandBytes] + "\n… (truncated)"
		} else {
			cmd = cmd[:head] + marker + cmd[len(cmd)-tail:]
		}
	}
	return strings.Split(cmd, "\n")
}

func toolError(err error) string {
	if err == nil {
		err = errors.New("unknown error")
	}
	return "ERROR: " + err.Error()
}
