package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dreaagent/drea/internal/agent"
	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/eval"
	"github.com/dreaagent/drea/internal/llm"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/ui"
)

// runEval loads task specs from a directory, runs the agent (auto-approving,
// non-persistent) against each, scores it with the spec's verify command, and
// prints a report. It exits non-zero if any task fails, so it can gate
// self-improvement in CI or a loop.
//
// Spec files are trusted executable input: their setup and verify fields run
// as shell commands. Default workdirs stay under the specs directory; use
// --allow-external-workdir for trusted suites that intentionally point elsewhere.
func runEval(args []string, errOut io.Writer, newUI func() *ui.UI) error {
	opts, err := parseEvalArgs(args)
	if opts.help {
		printEvalUsage(errOut)
		return nil
	}
	if err != nil {
		if !strings.HasPrefix(err.Error(), "unknown eval flag") {
			printEvalUsage(errOut)
		}
		return err
	}

	specs, err := eval.LoadWithOptions(opts.dir, eval.LoadOptions{AllowExternalWorkdir: opts.allowExternal})
	if err != nil {
		return err
	}
	tool.SetPassEnv(opts.passEnv)
	return runEvalSpecs(specs, opts.allowExternal, newUI())
}

type evalOptions struct {
	dir           string
	allowExternal bool
	passEnv       []string
	help          bool
}

func parseEvalArgs(args []string) (evalOptions, error) {
	var opts evalOptions
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--allow-external-workdir":
			opts.allowExternal = true
		case "--pass-env":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return opts, errors.New("--pass-env requires an environment variable name")
			}
			i++
			opts.passEnv = append(opts.passEnv, args[i])
		case "-h", "--help":
			opts.help = true
			return opts, nil
		default:
			if strings.HasPrefix(a, "--pass-env=") {
				name := strings.TrimSpace(strings.TrimPrefix(a, "--pass-env="))
				if name == "" {
					return opts, errors.New("--pass-env requires an environment variable name")
				}
				opts.passEnv = append(opts.passEnv, name)
				continue
			}
			if strings.HasPrefix(a, "-") {
				return opts, fmt.Errorf("unknown eval flag %q", a)
			}
			filtered = append(filtered, a)
		}
	}
	if len(filtered) != 1 {
		return opts, errors.New("eval requires exactly one directory argument")
	}
	opts.dir = filtered[0]
	return opts, nil
}

func runEvalSpecs(specs []eval.Spec, allowExternal bool, u *ui.UI) error {
	saved, err := loadSaved()
	if err != nil {
		return err
	}
	base := config.Defaults(saved)

	u.Warn("eval specs are trusted executable input: setup and verify run as shell commands")
	if allowExternal {
		u.Warn("--allow-external-workdir is set; workdirs may leave the specs directory")
	}
	u.Info(fmt.Sprintf("running %d eval task(s) with model %s", len(specs), base.Model))

	var results []eval.Result
	for _, s := range specs {
		results = append(results, runOne(base, s, u))
	}

	u.Info("\n" + eval.Summary(results))
	if !eval.Passed(results) {
		return errors.New("one or more eval tasks failed")
	}
	return nil
}

func printEvalUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: drea eval [flags] <dir>")
	fmt.Fprintln(w, "\nRuns every .json task spec in <dir> and reports pass/fail.")
	fmt.Fprintln(w, "Task specifications are trusted executable input: setup and verify run as shell.")
	fmt.Fprintln(w, "Relative workdirs must stay under <dir> unless --allow-external-workdir is set.")
	fmt.Fprintln(w, "\nFlags:")
	fmt.Fprintln(w, "  --allow-external-workdir   permit absolute or escaping workdirs in specs")
	fmt.Fprintln(w, "  --pass-env NAME            expose a non-secret environment variable to setup/verify (repeatable)")
}

// runOne executes a single eval task in its own agent, unattended.
func runOne(base config.Config, s eval.Spec, u *ui.UI) eval.Result {
	u.Info(fmt.Sprintf("\n=== %s ===", s.Name))

	cfg := base
	cfg.Workdir = s.Workdir
	cfg.AutoApprove = true // unattended
	cfg.Persist = false    // eval runs are ephemeral
	cfg.Verify = ""        // scored explicitly below, not inside the loop
	if err := cfg.Normalize(); err != nil {
		return eval.Result{Name: s.Name, Detail: "invalid workdir: " + err.Error()}
	}

	ctx := context.Background()

	if s.Setup != "" {
		out, code, _, err := tool.RunShell(ctx, cfg.Workdir, s.Setup, cfg.CommandTimeout)
		if err != nil || code != 0 {
			return eval.Result{Name: s.Name, Detail: "setup failed: " + firstLine(out)}
		}
	}

	client := llm.NewClientWithReasoning(cfg.ChatURL(), cfg.APIKey, cfg.Model, cfg.Temperature, cfg.TopP, cfg.ReasoningEffort, cfg.RequestTimeout, cfg.JSONMode, cfg.JSONFormat)
	tools := tool.NewRegistry(cfg.Workdir)
	ag := agent.New(cfg, client, tools, u)
	if err := ag.Run(ctx, s.Prompt); err != nil {
		return eval.Result{Name: s.Name, Detail: "agent error: " + err.Error()}
	}

	out, code, timedOut, err := tool.RunShell(ctx, cfg.Workdir, s.Verify, cfg.CommandTimeout)
	if err != nil {
		return eval.Result{Name: s.Name, Detail: "verify could not run: " + err.Error()}
	}
	if code == 0 && !timedOut {
		return eval.Result{Name: s.Name, Passed: true}
	}
	return eval.Result{Name: s.Name, Detail: "verify failed: " + firstLine(out)}
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
