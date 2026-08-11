// Package eval is a minimal evaluation scaffold: it loads task specifications
// from disk so the harness can be run against a fixed set of tasks and scored
// by a per-task verification command. It exists so repeated self-improvement
// can be measured ("did this change make more tasks pass?") rather than being
// unguided drift. It is deliberately small and stdlib-only.
//
// Specs are trusted executable input: setup and verify run as shell commands.
// Relative workdirs are confined under the specs directory unless the caller
// opts into external workdirs.
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
	// directory unless absolute. Absolute or escaping paths require
	// LoadOptions.AllowExternalWorkdir.
	Workdir string `json:"workdir"`
	// Setup, when set, is a shell command run before the task (e.g. to seed
	// fixture files). A non-zero exit fails the task before the agent runs.
	Setup string `json:"setup,omitempty"`
	// Verify is the shell command that decides pass/fail: exit 0 is a pass.
	Verify string `json:"verify"`
}

// LoadOptions controls how Spec.Workdir values are validated.
type LoadOptions struct {
	// AllowExternalWorkdir permits absolute workdirs and paths that escape
	// the specs directory. Off by default.
	AllowExternalWorkdir bool
}

// maxSpecSize bounds how much of a spec file is read.
const maxSpecSize = 1 << 20

// Load reads every *.json file in dir as a Spec, in stable (sorted) order.
// Relative Workdir values are resolved against dir and must remain beneath it
// unless opts.AllowExternalWorkdir is set. It errors if dir has no specs so a
// mistyped path is not silently reported as "0 tasks".
func Load(dir string) ([]Spec, error) {
	return LoadWithOptions(dir, LoadOptions{})
}

// LoadWithOptions is Load with explicit workdir containment policy.
func LoadWithOptions(dir string, opts LoadOptions) ([]Spec, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	absDir, err = filepath.EvalSymlinks(absDir)
	if err != nil {
		return nil, fmt.Errorf("resolve specs directory: %w", err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", absDir)
	}
	entries, err := os.ReadDir(absDir)
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
		p := filepath.Join(absDir, name)
		s, err := loadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if s.Name == "" {
			s.Name = strings.TrimSuffix(name, ".json")
		}
		raw := s.Workdir
		if raw == "" {
			s.Workdir = absDir
		} else if filepath.IsAbs(raw) {
			s.Workdir = filepath.Clean(raw)
		} else {
			s.Workdir = filepath.Join(absDir, raw)
		}
		absWD, err := filepath.Abs(s.Workdir)
		if err != nil {
			return nil, fmt.Errorf("%s: workdir: %w", name, err)
		}
		realWD, err := evalPath(absWD)
		if err != nil {
			return nil, fmt.Errorf("%s: resolve workdir: %w", name, err)
		}
		if err := confineWorkdir(absDir, realWD, raw, opts.AllowExternalWorkdir); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		info, err := os.Stat(realWD)
		if err != nil {
			return nil, fmt.Errorf("%s: workdir: %w", name, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s: workdir is not a directory: %s", name, realWD)
		}
		s.Workdir = realWD
		specs = append(specs, s)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no .json task specs found in %s", absDir)
	}
	return specs, nil
}

// evalPath resolves symlinks through the deepest existing ancestor. This
// exposes an escaping parent even when the final path has not been created.
func evalPath(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	realParent, err := evalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

// confineWorkdir requires workdir to stay under specsDir unless allowExternal.
func confineWorkdir(specsDir, workdir, raw string, allowExternal bool) error {
	if allowExternal {
		return nil
	}
	if filepath.IsAbs(raw) {
		return fmt.Errorf("absolute workdir %q requires --allow-external-workdir", raw)
	}
	rel, err := filepath.Rel(specsDir, workdir)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("workdir %q escapes the specs directory; use --allow-external-workdir for trusted suites", raw)
	}
	return nil
}

func loadFile(path string) (Spec, error) {
	var s Spec
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSpecSize+1))
	if err != nil {
		return s, err
	}
	if len(data) > maxSpecSize {
		return s, fmt.Errorf("spec file exceeds %d byte limit", maxSpecSize)
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
