package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/tool"
	"github.com/dreaagent/drea/internal/vcs"
)

// The verification command is the harness's only objective measure of whether
// a task succeeded. Everything here generalises that single check into a goal:
// establish the measure's state before the work starts, iterate until it
// passes, and — because "before" is known — recognise the one case that can be
// judged objectively, a task that turned a passing measure into a failing one.

// result is one run of the verification command: the harness's measure of the
// workspace at a point in time.
type result struct {
	passed bool
	ran    bool // false when the command could not be started at all
	output string
	status string // human/model-readable description of how it ended
}

// measure runs the verification command once.
func (a *Agent) measure(ctx context.Context) result {
	out, code, timedOut, err := tool.RunShell(ctx, a.cfg.Workdir, a.cfg.Verify, a.cfg.CommandTimeout)
	if err != nil {
		return result{}
	}
	r := result{ran: true, output: out, status: fmt.Sprintf("exit code %d", code)}
	if timedOut {
		r.status = "timed out"
		return r
	}
	r.passed = code == 0
	return r
}

// beginTask records the state of the goal before any work happens: an optional
// checkpoint commit to roll back to, and whether the verification command
// passed to begin with. Both are only done when checkpointing is enabled, since
// each costs something the user should opt into — a commit in their repository
// and a full run of the verification command.
func (a *Agent) beginTask(ctx context.Context, task string) {
	a.checkpoint, a.baseline, a.objective = "", MeasureUnknown, MeasureUnknown
	if !a.cfg.Checkpoint {
		return
	}
	if !vcs.IsRepo(ctx, a.cfg.Workdir) {
		a.ui.Warn("  checkpointing is on but the workspace is not a git repository; skipping")
		return
	}
	ref, committed, err := vcs.Checkpoint(ctx, a.cfg.Workdir, "drea: checkpoint before "+oneLine(task, 60))
	if err != nil {
		a.ui.Warn("  could not checkpoint: " + err.Error())
		return
	}
	a.checkpoint = ref
	if committed {
		a.ui.Info("  checkpointed uncommitted work as " + ref)
	} else {
		a.ui.Info("  checkpoint: " + ref + " (working tree already clean)")
	}

	if a.cfg.Verify == "" {
		return
	}
	a.ui.Info("  measuring the starting state: " + a.cfg.Verify)
	r := a.measure(ctx)
	switch {
	case !r.ran:
		a.ui.Warn("  verification command could not run; no starting measure")
	case r.passed:
		a.baseline = MeasurePassing
		a.ui.Info("  starting state: verification passes")
	default:
		a.baseline = MeasureFailing
		a.ui.Info("  starting state: verification already fails")
	}
	a.objective = a.baseline
}

// verify runs the verification command when the model believes the task is
// done. It returns feedback and retry=true when the command failed and another
// self-correction attempt is allowed; otherwise retry is false (no command
// configured, it passed, or the attempt budget is exhausted).
func (a *Agent) verify(ctx context.Context) (feedback string, retry bool) {
	if a.cfg.Verify == "" {
		return "", false
	}
	attempts := a.cfg.VerifyAttempts
	if attempts <= 0 {
		attempts = config.DefaultVerifyAttempts
	}
	if a.verifyRounds >= attempts {
		a.ui.Warn(fmt.Sprintf("  verification still failing after %d attempts; stopping.", attempts))
		a.offerRollback(ctx)
		return "", false
	}
	a.verifyRounds++

	a.ui.Info(fmt.Sprintf("  verifying: %s", a.cfg.Verify))
	r := a.measure(ctx)
	if !r.ran {
		// Could not even start the command; report but do not loop on it.
		a.ui.Warn("  verification could not run")
		return "", false
	}
	if r.passed {
		a.objective = MeasurePassing
		a.ui.Info("  verification passed")
		return "", false
	}
	a.objective = MeasureFailing

	a.ui.Warn(fmt.Sprintf("  verification failed; feeding the output back (attempt %d of %d)", a.verifyRounds, attempts))
	fb := fmt.Sprintf(
		"The verification command failed (%s):\n\n$ %s\n\n%s\n\nFix the problem, then it will be verified again. Do not stop until it passes.",
		r.status, a.cfg.Verify, trimForModel(r.output))
	return fb, true
}

// offerRollback undoes the task when it demonstrably made things worse: the
// verification command passed before the task and fails after it. That is the
// only regression the harness can judge objectively, so it is the only case
// where it rolls anything back. Everything else — a measure that was already
// failing, or no measure at all — is left for the user to decide.
//
// The rollback is not a deletion: the attempt is committed to a branch of its
// own first, so what it tried remains reviewable (and the next attempt can be
// shown it, instead of walking into the same dead end).
func (a *Agent) offerRollback(ctx context.Context) {
	if a.checkpoint == "" || a.baseline != MeasurePassing {
		return
	}
	a.ui.Warn("  this task turned a passing verification into a failing one.")
	if !a.cfg.AutoApprove && !a.ui.Confirm("  roll back to "+a.checkpoint+"? (the attempt is kept on a branch)") {
		a.ui.Info("  keeping the changes; roll back later with: git reset --hard " + a.checkpoint)
		return
	}
	branch := vcs.UniqueBranch(ctx, a.cfg.Workdir, fmt.Sprintf("drea/attempt-%d", time.Now().Unix()))
	attempt, preserved, err := vcs.Rollback(ctx, a.cfg.Workdir, a.checkpoint, branch)
	if err != nil {
		a.ui.Error("rollback failed: " + err.Error())
		return
	}
	a.ui.Info("  rolled back to " + a.checkpoint)
	if preserved {
		a.ui.Info("  the attempt is kept on " + attempt + "; review it with: git diff " + a.checkpoint + " " + attempt)
	}
}

// Measure is what the verification command last reported about the workspace.
type Measure int

const (
	MeasureUnknown Measure = iota // no verification command, or it could not run
	MeasurePassing
	MeasureFailing
)

// Objective reports the latest state of the verification command. It is the
// only evidence the harness has that a task actually achieved anything, so
// callers deciding what to do with the work (promote it, keep it for review)
// go through this rather than guessing from the absence of errors.
func (a *Agent) Objective() Measure { return a.objective }
