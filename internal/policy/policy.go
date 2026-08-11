// Package policy implements a granular allow/deny policy for shell commands so
// the harness can run unattended without blanket trust. It is defence in depth,
// layered on top of workspace confinement and the approval prompt — not a
// sandbox. Deny always wins over allow; anything unmatched falls back to the
// normal approval flow.
package policy

import (
	"regexp"
	"strings"
)

// Decision is how a command should be handled.
type Decision int

const (
	// Ask defers to the normal approval flow (prompt unless auto-approve).
	Ask Decision = iota
	// Allow runs the command without prompting, even when auto-approve is off.
	Allow
	// Deny refuses the command outright, even when auto-approve is on.
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	default:
		return "ask"
	}
}

// DefaultDeny is a small, best-effort set of patterns for unambiguously
// catastrophic commands (wiping the filesystem or home directory, formatting
// disks, powering off the host, a classic fork bomb). It is always applied in
// addition to any user-supplied deny patterns. It is intentionally incomplete:
// it catches only a few obvious foot-guns and is easy to bypass with creative
// spelling or indirection. It is not a sandbox and does not replace least
// privilege or reviewing what the agent runs.
var DefaultDeny = []string{
	// rm -rf (in either flag order) targeting the filesystem root, home, or a
	// bare glob. Anchored to command position so "rm -rf ./build" is fine.
	cmdStart + `rm\s+(-\S+\s+)*-\S*r\S*f\S*\s+(-\S+\s+)*(/|~|/\*|\*|\$HOME)(\s|/|$)`,
	cmdStart + `rm\s+(-\S+\s+)*-\S*f\S*r\S*\s+(-\S+\s+)*(/|~|/\*|\*|\$HOME)(\s|/|$)`,
	cmdStart + `mkfs(\.\S+)?\s`,
	cmdStart + `dd\s[^\n;&|]*\bof=/dev/`,
	`(?i)>\s*/dev/(sd|nvme|hd|vd)`,
	cmdStart + `(shutdown|reboot|halt|poweroff)(\s|$)`,
	cmdStart + `init\s+[06](\s|$)`,
	`:\s*\(\s*\)\s*\{.*\}\s*;\s*:`,
}

// cmdStart matches the start of a command: the beginning of the string or just
// after a shell separator (; & |), optionally preceded by sudo. It keeps the
// built-in deny patterns from matching a dangerous word buried in an argument
// (e.g. "echo reboot later").
const cmdStart = `(?i)(^|[;&|]\s*)(sudo\s+)?`

// Policy classifies commands using compiled allow and deny patterns.
type Policy struct {
	allow []*regexp.Regexp
	deny  []*regexp.Regexp
}

// New compiles a policy from user allow and deny pattern strings. DefaultDeny
// is always prepended to the deny set. A pattern that fails to compile is
// returned as an error so misconfiguration fails fast rather than silently
// weakening safety.
func New(allow, deny []string) (*Policy, error) {
	a, err := compile(allow)
	if err != nil {
		return nil, err
	}
	d, err := compile(append(append([]string(nil), DefaultDeny...), deny...))
	if err != nil {
		return nil, err
	}
	return &Policy{allow: a, deny: d}, nil
}

func compile(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, re)
	}
	return out, nil
}

// Decide classifies a command. Deny takes precedence over allow. Deny rules
// may match anywhere in the command; allow rules must match the entire
// trimmed command (a prefix-only hit is treated as Ask so compound suffixes
// like "; curl …" cannot ride an allowlisted prefix).
func (p *Policy) Decide(command string) Decision {
	if p == nil {
		return Ask
	}
	command = strings.TrimSpace(command)
	for _, re := range p.deny {
		if re.MatchString(command) {
			return Deny
		}
	}
	for _, re := range p.allow {
		loc := re.FindStringIndex(command)
		if loc != nil && loc[0] == 0 && loc[1] == len(command) {
			return Allow
		}
	}
	return Ask
}
