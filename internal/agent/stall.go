package agent

import (
	"fmt"

	"github.com/dreaagent/drea/internal/llm"
)

// A model that is stuck rarely stops: it repeats the same call, gets the same
// result, and burns the step budget. MaxSteps eventually ends that, but only
// after paying for every step. Detecting an exactly-repeated action is a
// deliberately narrow signal — identical tool *and* identical arguments — so it
// cannot misfire on legitimate repetition like reading two different files, or
// re-running a test command after an edit changed the code around it.
const (
	// stallNudge is how many identical consecutive calls trigger a warning
	// fed back to the model.
	stallNudge = 3
	// stallAbort is how many identical consecutive calls end the task. The
	// model has by then ignored the nudge, so more turns will not help.
	stallAbort = 5
)

// stallDetector tracks consecutive identical tool calls within one task.
type stallDetector struct {
	last  string
	count int
	// nudged records that the model has already been told it is repeating,
	// so it is warned once rather than on every subsequent repeat.
	nudged bool
}

// reset clears the detector at the start of a task.
func (s *stallDetector) reset() { *s = stallDetector{} }

// observe records a batch of tool calls and reports whether the model is
// repeating itself. nudge is true the first time the threshold is crossed;
// abort is true once repetition has continued past the point of usefulness.
func (s *stallDetector) observe(calls []llm.ToolCall) (nudge, abort bool) {
	for _, tc := range calls {
		sig := tc.Function.Name + "\x00" + tc.Function.Arguments
		if sig == s.last {
			s.count++
		} else {
			s.last, s.count, s.nudged = sig, 1, false
		}
		if s.count >= stallAbort {
			return false, true
		}
		if s.count >= stallNudge && !s.nudged {
			s.nudged = true
			nudge = true
		}
	}
	return nudge, false
}

// stallMessage is fed back to the model as a user turn, describing the loop it
// is in concretely enough to break out of rather than just "try again".
func (s *stallDetector) stallMessage() string {
	name := s.last
	for i := 0; i < len(name); i++ {
		if name[i] == 0 {
			name = name[:i]
			break
		}
	}
	return fmt.Sprintf(
		"You have called %s with identical arguments %d times in a row and are getting the same result, so this is not making progress. Do not repeat it. Either investigate differently (read a different file, run a different command to get more information), or explain what is blocking you and stop.",
		name, s.count)
}
