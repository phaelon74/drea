// Package eval is a minimal evaluation scaffold: it loads task specifications
// from disk so the harness can be run against a fixed set of tasks and scored
// by a per-task verification command. It exists so repeated self-improvement
// can be measured ("did this change make more tasks pass?") rather than being
// unguided drift. It is deliberately small and stdlib-only.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spec is a single evaluation task. Specs are stored as JSON files in a
// directory (one task per file).
type Spec struct {
	// Name identifies the task in the report (defaults to the file name).
	Name string `json:"name"`
	// Prompt is the instruction handed to the agent.
	Prompt string `json:"prompt"`
	// Workdir is the directory the task runs in, relative to the specs
	// directory unless absolute. It must already exist.
	Workdir string `json:"workdir"`
	// Setup, when set, is a shell command run before the task (e.g. to seed
	// fixture files). A non-zero exit fails the task before the agent runs.
	Setup string `json:"setup,omitempty"`
	// Verify is the shell command that decides pass/fail: exit 0 is a pass.
	Verify string `json:"verify"`
}

// maxSpecSize bounds how much of a spec file is read.
const maxSpecSize = 1 << 20

// Load reads every *.json file in dir as a Spec, in stable (sorted) order.
// Relative Workdir values are resolved against dir. It errors if dir has no
// specs so a mistyped path is not silently reported as "0 tasks".
func Load(dir string) ([]Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var specs []Spec
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		p := filepath.Join(dir, name)
		s, err := loadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if s.Name == "" {
			s.Name = strings.TrimSuffix(name, ".json")
		}
		if s.Workdir == "" {
			s.Workdir = dir
		} else if !filepath.IsAbs(s.Workdir) {
			s.Workdir = filepath.Join(dir, s.Workdir)
		}
		specs = append(specs, s)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no .json task specs found in %s", dir)
	}
	return specs, nil
}

func loadFile(path string) (Spec, error) {
	var s Spec
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSpecSize))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return s, errors.New("prompt is required")
	}
	if strings.TrimSpace(s.Verify) == "" {
		return s, errors.New("verify command is required")
	}
	return s, nil
}

// Result records the outcome of running one Spec.
type Result struct {
	Name   string
	Passed bool
	Detail string
}

// Summary renders a human-readable report of results and the pass count.
func Summary(rs []Result) string {
	var b strings.Builder
	passed := 0
	for _, r := range rs {
		mark := "FAIL"
		if r.Passed {
			mark = "PASS"
			passed++
		}
		fmt.Fprintf(&b, "  [%s] %s", mark, r.Name)
		if r.Detail != "" {
			fmt.Fprintf(&b, " — %s", r.Detail)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%d/%d tasks passed", passed, len(rs))
	return b.String()
}

// Passed reports whether every result passed.
func Passed(rs []Result) bool {
	for _, r := range rs {
		if !r.Passed {
			return false
		}
	}
	return len(rs) > 0
}
