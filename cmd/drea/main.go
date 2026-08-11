// Command drea is a minimalist, dependency-free AI agent harness for the
// terminal. It drives an OpenAI-compatible model through a reason-act loop with
// a small set of file, search and shell tools, confined to a workspace root.
//
// Usage:
//
//	drea [flags] [task...]
//
// With a task on the command line it runs once and exits. With no task it
// starts an interactive session that keeps conversation context across turns.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dreaagent/drea/internal/agent"
	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/policy"
	"github.com/dreaagent/drea/internal/session"
	"github.com/dreaagent/drea/internal/settings"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

// version is the current release of drea. It follows Semantic Versioning and is
// bumped and tagged explicitly with each release.
const version = "v0.1.0-alpha.1"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "drea: "+err.Error())
		os.Exit(1)
	}
}

type cliApp struct {
	args           []string
	in             io.Reader
	out            io.Writer
	errOut         io.Writer
	loadSaved      func() (config.Saved, error)
	newUI          func() *ui.UI
	runOneShot     func(context.Context, *agent.Agent, *config.Config, string) error
	runInteractive func(<-chan os.Signal, *agent.Agent, *ui.UI, *config.Config, *llm.Client, *tool.Registry, io.Reader, io.Writer) error
	signals        func() (<-chan os.Signal, func())
}

func run(args []string, in io.Reader, out, errOut io.Writer) error {
	a := cliApp{
		args:      args,
		in:        in,
		out:       out,
		errOut:    errOut,
		loadSaved: loadSaved,
		newUI: func() *ui.UI {
			return ui.NewWithIO(in, out)
		},
		runOneShot: func(ctx context.Context, ag *agent.Agent, _ *config.Config, task string) error {
			return ag.Run(ctx, task)
		},
		runInteractive: interactive,
		signals: func() (<-chan os.Signal, func()) {
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
			return ch, func() { signal.Stop(ch) }
		},
	}
	return a.run()
}

func (a cliApp) run() error {
	// "drea eval <dir>" runs the evaluation harness instead of a task/session.
	if len(a.args) > 0 && a.args[0] == "eval" {
		return runEval(a.args[1:], a.errOut, a.newUI)
	}

	// Handle --version before flag parsing so it works even if config files or
	// the workspace are missing.
	for _, arg := range a.args {
		if arg == "--version" || arg == "-v" {
			fmt.Fprintln(a.out, "drea "+version)
			return nil
		}
	}

	saved, err := a.loadSaved()
	if err != nil {
		return err
	}
	cfg := config.Defaults(saved)

	var resume bool
	fs := flag.NewFlagSet("drea", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	fs.Var((*stringList)(&cfg.AllowCommands), "allow", "regex for a run_command command to auto-run without a prompt (repeatable)")
	fs.Var((*stringList)(&cfg.DenyCommands), "deny", "regex for a run_command command to block outright (repeatable)")
	fs.Var((*stringList)(&cfg.PassEnv), "pass-env", "credential-like env var name to pass through to child processes (repeatable; never DREA_API_KEY/OPENAI_API_KEY)")
	fs.StringVar(&cfg.Debug, "debug", cfg.Debug, "dump raw request/response traffic to a file (for debugging)")
	fs.StringVar(&cfg.BaseURL, "base-url", cfg.BaseURL, "OpenAI-compatible API base URL")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model name")
	fs.StringVar(&cfg.APIKey, "key", cfg.APIKey, "API key (prefer DREA_API_KEY env var)")
	fs.StringVar(&cfg.Workdir, "workdir", cfg.Workdir, "workspace root the agent is confined to")
	fs.BoolVar(&cfg.AutoApprove, "auto", cfg.AutoApprove, "auto-approve commands and file writes (no confirmation)")
	fs.BoolVar(&cfg.AllowInsecureKeyTransport, "allow-insecure-key-transport", cfg.AllowInsecureKeyTransport, "allow sending the API key over plain HTTP (including loopback)")
	fs.BoolVar(&cfg.AllowExternalDebugLog, "allow-external-debug-log", cfg.AllowExternalDebugLog, "allow --debug paths outside the workspace (log contains raw prompts and tool data)")
	fs.IntVar(&cfg.MaxSteps, "max-steps", cfg.MaxSteps, "maximum model turns per task")
	fs.Float64Var(&cfg.Temperature, "temperature", cfg.Temperature, "sampling temperature")
	fs.Float64Var(&cfg.TopP, "top-p", cfg.TopP, "nucleus-sampling probability")
	fs.StringVar(&cfg.ReasoningEffort, "reasoning-effort", cfg.ReasoningEffort, "reasoning effort level: low, medium, or high (empty = endpoint default)")
	noJSONMode := !cfg.JSONMode
	fs.BoolVar(&noJSONMode, "no-json-mode", noJSONMode, "disable the default strict JSON tool-call schema (response_format); only use with endpoints that do not support response_format")
	fs.StringVar(&cfg.JSONFormat, "json-format", cfg.JSONFormat, "response_format variant: json_schema (default/OpenAI) or json_object (llama.cpp/older servers)")
	fs.StringVar(&cfg.Verify, "verify", cfg.Verify, "command run to verify the workspace when a task completes (failures are fed back for self-correction)")
	fs.IntVar(&cfg.VerifyAttempts, "verify-attempts", cfg.VerifyAttempts, "how many times a failing verify command is fed back for self-correction")
	fs.BoolVar(&cfg.Checkpoint, "checkpoint", cfg.Checkpoint, "commit the workspace before each task (and measure verify first) so the task can be rolled back if it regresses it")
	fs.BoolVar(&cfg.Worktree, "worktree", false, "run in a scratch git worktree of the workspace so an attempt cannot damage it")
	fs.BoolVar(&cfg.Promote, "promote", false, "with --worktree, merge the work back when the verify command passes and it fast-forwards cleanly")
	fs.IntVar(&cfg.ContextTokens, "context-tokens", cfg.ContextTokens, "prompt-size budget above which older history is compacted")
	fs.BoolVar(&cfg.Persist, "persist", cfg.Persist, "save the conversation transcript so it can be resumed (never stores the API key)")
	fs.BoolVar(&resume, "resume", false, "resume the most recent saved session for this workspace")
	fs.Usage = func() {
		fmt.Fprintln(a.errOut, "Usage: drea [flags] [task...]")
		fmt.Fprintln(a.errOut, "       drea eval <dir>   run the evaluation harness over task specs in <dir>")
		fmt.Fprintln(a.errOut, "\nRun a task once by passing it as arguments, or omit it for an interactive session.")
		fmt.Fprintln(a.errOut, "\nFlags:")
		fs.PrintDefaults()
		fmt.Fprintln(a.errOut, "\nEnvironment: DREA_BASE_URL, DREA_API_KEY (or OPENAI_API_KEY), DREA_MODEL, DREA_WORKDIR,")
		fmt.Fprintln(a.errOut, "             DREA_AUTO_APPROVE, DREA_VERIFY, DREA_VERIFY_ATTEMPTS, DREA_CHECKPOINT,")
		fmt.Fprintln(a.errOut, "             DREA_CONTEXT_TOKENS, DREA_NO_PERSIST, DREA_JSON_FORMAT, DREA_DEBUG,")
		fmt.Fprintln(a.errOut, "             DREA_ALLOW_COMMANDS, DREA_DENY_COMMANDS (newline-separated regexes),")
		fmt.Fprintln(a.errOut, "             DREA_TEMPERATURE, DREA_TOP_P, DREA_REASONING_EFFORT")
	}
	if err := fs.Parse(a.args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	keyFromFlag := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "key" {
			keyFromFlag = true
		}
	})

	cfg.JSONMode = !noJSONMode
	if err := cfg.Normalize(); err != nil {
		return err
	}
	tool.SetPassEnv(cfg.PassEnv)

	u := a.newUI()
	if keyFromFlag {
		u.Warn("--key puts the API key in process argv (visible to ps and shell history); prefer DREA_API_KEY")
	}
	if cfg.AllowExternalDebugLog && cfg.Debug != "" {
		u.Warn("--allow-external-debug-log: debug file may leave the workspace and contains raw prompts, replies, and tool data")
	}

	// Isolation is set up before anything else looks at Workdir: the tools, the
	// agent and the session file must all agree on where the work happens.
	// What becomes of the worktree depends on the verification command's final
	// state, which only exists once the agent does; the closure defers reading
	// it until the run is over.
	objective := func() agent.Measure { return agent.MeasureUnknown }
	if cfg.Worktree {
		wt, err := enterWorktree(&cfg, u)
		if err != nil {
			return err
		}
		defer func() { finishWorktree(wt, &cfg, objective(), u) }()
	} else if cfg.Promote {
		u.Warn("--promote does nothing without --worktree")
	}

	client := llm.NewClientWithReasoning(cfg.ChatURL(), cfg.APIKey, cfg.Model, cfg.Temperature, cfg.TopP, cfg.ReasoningEffort, cfg.RequestTimeout, cfg.JSONMode, cfg.JSONFormat)
	if err := client.SetDebug(cfg.Debug); err != nil {
		return fmt.Errorf("open debug log: %w", err)
	}
	tools := tool.NewRegistry(cfg.Workdir)
	ag := agent.New(cfg, client, tools, u)
	objective = ag.Objective

	if resume {
		if s, ok, _ := session.Load(cfg.Workdir); ok && ag.Restore(s.Messages) {
			u.Info(fmt.Sprintf("resumed session (%d messages) for %s", ag.MessageCount(), cfg.Workdir))
		} else {
			u.Warn("no saved session found for this workspace; starting fresh")
		}
	}

	// A single, long-lived signal channel. Each task derives a fresh context
	// cancelled by the next signal, so Ctrl-C aborts the in-flight task
	// without killing the process or poisoning later tasks.
	sigCh, stopSignals := a.signals()
	defer stopSignals()

	u.Banner(cfg.Model, cfg.Workdir, cfg.AutoApprove)
	u.SetStatus(0, cfg.ContextTokens)
	u.SetStatusWorkdir(cfg.Workdir)
	u.ShowStatus()

	if task := strings.TrimSpace(strings.Join(fs.Args(), " ")); task != "" {
		ctx, cancel := taskContext(sigCh)
		defer cancel()
		return a.runOneShot(ctx, ag, &cfg, task)
	}
	return a.runInteractive(sigCh, ag, u, &cfg, client, tools, a.in, a.out)
}

// taskContext returns a context cancelled when the first signal arrives on
// sigCh, or when the returned cancel func is called (which also stops the
// watcher goroutine).
func taskContext(sigCh <-chan os.Signal) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// interactive runs a read-eval loop, keeping conversation context across turns.
func interactive(sigCh <-chan os.Signal, ag *agent.Agent, u *ui.UI, cfg *config.Config, client *llm.Client, tools *tool.Registry, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	u.Info("Interactive session. Type a task, /help for commands, or Ctrl-D to quit.")
	for {
		u.Prompt()
		line, err := readInput(reader, u.Multiline())
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(out)
			return nil
		}
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" || strings.HasPrefix(line, "/") {
			if quit := command(line, u, ag, cfg, client, tools); quit {
				return nil
			}
			continue
		}
		// Drop any signal received while idle at the prompt so it does not
		// immediately cancel the task we are about to start.
		select {
		case <-sigCh:
		default:
		}
		ctx, cancel := taskContext(sigCh)
		err = ag.Run(ctx, line)
		cancel()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				u.Warn("\ninterrupted.")
				continue
			}
			u.Error(err.Error())
		}
	}
}

func readInput(in *bufio.Reader, multiline bool) (string, error) {
	if !multiline {
		line, err := in.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if errors.Is(err, io.EOF) && line != "" {
			return line, nil
		}
		return line, err
	}
	var b strings.Builder
	for {
		line, err := in.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			if b.Len() > 0 {
				return strings.TrimRight(b.String(), "\r\n"), nil
			}
			return "", err
		}
	}
}

// command handles slash commands and the bare exit/quit words. It returns true
// when the session should end.
func command(line string, u *ui.UI, ag *agent.Agent, cfg *config.Config, client *llm.Client, tools *tool.Registry) bool {
	fields := strings.Fields(line)
	name := strings.TrimPrefix(fields[0], "/")
	arg := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))

	switch name {
	case "exit", "quit":
		return true
	case "help":
		u.Info(helpText)
	case "config":
		showConfig(u, cfg)
	case "tools":
		u.Info("tools: " + strings.Join(tools.Names(), ", "))
	case "model":
		if arg == "" {
			u.Info("model: " + cfg.Model)
			break
		}
		cfg.Model = arg
		client.SetModel(arg)
		u.Info("model set to " + arg)
	case "host":
		if arg == "" {
			u.Info("host: " + cfg.BaseURL)
			break
		}
		prev := cfg.BaseURL
		cfg.BaseURL = arg
		if err := cfg.Normalize(); err != nil {
			cfg.BaseURL = prev
			u.Error(err.Error())
			break
		}
		client.SetChatURL(cfg.ChatURL())
		u.Info("host set to " + cfg.BaseURL)
	case "auto":
		switch arg {
		case "on", "true", "yes":
			cfg.AutoApprove = true
			ag.SetAutoApprove(true)
		case "off", "false", "no":
			cfg.AutoApprove = false
			ag.SetAutoApprove(false)
		case "":
		default:
			u.Warn("usage: /auto [on|off]")
		}
		u.Info(fmt.Sprintf("auto-approve: %v", cfg.AutoApprove))
	case "verify":
		if arg == "" {
			if cfg.Verify == "" {
				u.Info("verify: (unset)")
			} else {
				u.Info("verify: " + cfg.Verify)
			}
			break
		}
		if arg == "off" || arg == "none" {
			cfg.Verify = ""
			ag.SetVerify("")
			u.Info("verify command cleared")
			break
		}
		cfg.Verify = arg
		ag.SetVerify(arg)
		u.Info("verify command set to: " + arg)
	case "multiline":
		switch arg {
		case "on", "true", "yes":
			u.SetMultiline(true)
		case "off", "false", "no":
			u.SetMultiline(false)
		case "":
		default:
			u.Warn("usage: /multiline [on|off]")
		}
		u.Info(fmt.Sprintf("multi-line input: %v", u.Multiline()))
	case "usage":
		u.Info("session usage: " + ag.Usage().String())
	case "checkpoint":
		switch arg {
		case "on", "true", "yes":
			cfg.Checkpoint = true
			ag.SetCheckpoint(true)
		case "off", "false", "no":
			cfg.Checkpoint = false
			ag.SetCheckpoint(false)
		case "":
		default:
			u.Warn("usage: /checkpoint [on|off]")
		}
		u.Info(fmt.Sprintf("checkpoint: %v", cfg.Checkpoint))
	case "policy":
		showPolicy(u, cfg)
	case "reasoning":
		if arg == "" {
			if cfg.ReasoningEffort == "" {
				u.Info("reasoning: (endpoint default)")
			} else {
				u.Info("reasoning: " + cfg.ReasoningEffort)
			}
			break
		}
		if arg == "off" || arg == "none" {
			cfg.ReasoningEffort = ""
			client.SetReasoningEffort("")
			u.Info("reasoning effort cleared (endpoint default)")
			break
		}
		if err := cfg.SetReasoningEffort(arg); err != nil {
			u.Error(err.Error())
			break
		}
		client.SetReasoningEffort(cfg.ReasoningEffort)
		u.Info("reasoning effort set to " + cfg.ReasoningEffort)
	case "key":
		if arg == "" || arg == "show" {
			if cfg.APIKey == "" {
				u.Info("key: (unset)")
			} else {
				u.Info("key: " + maskKey(cfg.APIKey))
			}
			break
		}
		if arg == "off" || arg == "none" {
			cfg.APIKey = ""
			client.SetAPIKey("")
			u.Info("key cleared")
			break
		}
		prev := cfg.APIKey
		cfg.APIKey = arg
		if err := cfg.Normalize(); err != nil {
			cfg.APIKey = prev
			u.Error(err.Error())
			break
		}
		client.SetAPIKey(arg)
		u.Info("key set")
	case "save":
		p, err := settings.Save(settings.Settings{
			BaseURL:         cfg.BaseURL,
			Model:           cfg.Model,
			Verify:          cfg.Verify,
			VerifyAttempts:  cfg.VerifyAttempts,
			Checkpoint:      cfg.Checkpoint,
			ContextTokens:   cfg.ContextTokens,
			JSONFormat:      cfg.JSONFormat,
			Temperature:     cfg.Temperature,
			TopP:            cfg.TopP,
			ReasoningEffort: cfg.ReasoningEffort,
			AllowCommands:   cfg.AllowCommands,
			DenyCommands:    cfg.DenyCommands,
		})
		if err != nil {
			u.Error(err.Error())
			break
		}
		kp, err := settings.SaveKey(cfg.APIKey)
		if err != nil {
			u.Error(err.Error())
			break
		}
		u.Info("saved settings to " + p)
		u.Info("saved API key to " + kp)
	case "resume":
		s, ok, err := session.Load(cfg.Workdir)
		if err != nil {
			u.Error("load session: " + err.Error())
		} else if ok && ag.Restore(s.Messages) {
			u.Info(fmt.Sprintf("resumed session (%d messages)", ag.MessageCount()))
		} else {
			u.Warn("no saved session found for this workspace")
		}
	case "reset":
		ag.Reset()
		u.Info("conversation cleared")
	default:
		u.Warn("unknown command: /" + name + " (try /help)")
	}
	return false
}

const helpText = `commands:
  /help           show this help
  /config         show current configuration
  /model [name]   show or set the model
  /host [url]     show or set the API base URL
  /auto [on|off]  show or toggle auto-approve
  /verify [cmd]   show or set the verify command (/verify off to clear)
  /checkpoint [on|off]  show or toggle per-task checkpointing
  /reasoning [low|medium|high|off]  show or set the reasoning effort level
  /key [value|off|show]  show or set the API key (masked); /save persists it
  /multiline [on|off]  toggle multi-line input (Enter starts a new line; Ctrl-D sends)
  /usage          show token usage for this session
  /policy         show the run_command allow/deny policy
  /save           persist model + host + sampling + verify + policy + reasoning + API key
  /resume         reload the saved transcript for this workspace
  /reset          clear the conversation history
  /tools          list available tools
  /exit           quit (also: /quit, exit, Ctrl-D)

Input: Enter sends the message. For multi-line text, use /multiline on, then
press Ctrl-D to send (or /multiline off to return to single-line).`

func showConfig(u *ui.UI, cfg *config.Config) {
	p, _ := settings.Path()
	verify := cfg.Verify
	if verify == "" {
		verify = "(unset)"
	}
	u.Info(fmt.Sprintf("model:    %s", cfg.Model))
	u.Info(fmt.Sprintf("host:     %s", cfg.BaseURL))
	u.Info(fmt.Sprintf("workdir:  %s", cfg.Workdir))
	u.Info(fmt.Sprintf("auto:     %v", cfg.AutoApprove))
	u.Info(fmt.Sprintf("verify:   %s (up to %d self-correction attempts)", verify, cfg.VerifyAttempts))
	u.Info(fmt.Sprintf("checkpt:  %v", cfg.Checkpoint))
	u.Info(fmt.Sprintf("context:  %d tokens", cfg.ContextTokens))
	u.Info(fmt.Sprintf("persist:  %v", cfg.Persist))
	u.Info(fmt.Sprintf("json:     %s (mode: %v)", cfg.JSONFormat, cfg.JSONMode))
	u.Info(fmt.Sprintf("temp:     %v", cfg.Temperature))
	u.Info(fmt.Sprintf("top-p:    %v", cfg.TopP))
	reasoning := cfg.ReasoningEffort
	if reasoning == "" {
		reasoning = "(unset)"
	}
	u.Info(fmt.Sprintf("reasoning: %s", reasoning))
	u.Info(fmt.Sprintf("allow:    %d pattern(s)", len(cfg.AllowCommands)))
	u.Info(fmt.Sprintf("deny:     %d pattern(s) (+ built-in)", len(cfg.DenyCommands)))
	u.Info(fmt.Sprintf("pass-env: %d variable(s)", len(cfg.PassEnv)))
	u.Info(fmt.Sprintf("insecure-key-transport: %v", cfg.AllowInsecureKeyTransport))
	if cfg.Debug == "" {
		u.Info("debug:    (off)")
	} else {
		u.Info(fmt.Sprintf("debug:    %s (external allowed: %v)", cfg.Debug, cfg.AllowExternalDebugLog))
	}
	u.Info(fmt.Sprintf("key:      %s", maskKey(cfg.APIKey)))
	u.Info(fmt.Sprintf("settings: %s", p))
}

// showPolicy prints the run_command allow/deny policy, including the built-in
// deny list that is always in force.
func showPolicy(u *ui.UI, cfg *config.Config) {
	u.Info("run_command policy (deny wins; unmatched commands prompt as usual):")
	if len(cfg.AllowCommands) == 0 {
		u.Info("  allow: (none)")
	} else {
		for _, p := range cfg.AllowCommands {
			u.Info("  allow: " + p)
		}
	}
	for _, p := range cfg.DenyCommands {
		u.Info("  deny:  " + p)
	}
	for _, p := range policy.DefaultDeny {
		u.Info("  deny:  " + p + "  (built-in)")
	}
}

// stringList is a flag.Value that accumulates repeated occurrences of a flag
// into a slice (e.g. --allow 'go test' --allow '^git ').
type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ", ")
}

func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) != "" {
		*s = append(*s, v)
	}
	return nil
}

func maskKey(k string) string {
	if k == "" {
		return "(unset)"
	}
	if len(k) <= 6 {
		return "set"
	}
	return "set (…" + k[len(k)-4:] + ")"
}
