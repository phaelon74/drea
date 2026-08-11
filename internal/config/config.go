// Package config resolves harness configuration.
//
// Precedence (highest first): command-line flags, environment variables, the
// persisted settings file and key file (see the settings package), and built-in
// defaults. The API key may come from the environment or the 0600 key file;
// it is never written into the settings JSON or session transcript.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dreaagent/drea/internal/policy"
)

// Config holds every tunable the harness needs at runtime.
type Config struct {
	// BaseURL is the root of an OpenAI-compatible API (without a trailing
	// slash). The chat endpoint is derived as BaseURL + "/chat/completions".
	BaseURL string
	// APIKey is sent as a Bearer token. May be empty for local endpoints
	// (e.g. llama.cpp) that do not require authentication.
	APIKey string
	// Model is the model name passed to the endpoint.
	Model string

	// Workdir is the absolute root that all file tools are confined to.
	Workdir string

	// AutoApprove skips interactive confirmation before commands and writes.
	AutoApprove bool
	// MaxSteps caps how many LLM turns a single task may take.
	MaxSteps int
	// Temperature is the sampling temperature forwarded to the endpoint.
	Temperature float64
	// TopP is the nucleus-sampling probability forwarded to the endpoint. Zero
	// means "omit from the request" so the endpoint default is used.
	TopP float64
	// ReasoningEffort is the reasoning effort level forwarded to the endpoint.
	// Empty means omit from the request so the endpoint default is used.
	ReasoningEffort string
	// JSONMode asks the endpoint to constrain output to a strict JSON schema
	// describing a tool call (response_format). It is on by default because the
	// harness is designed to work with small models that lack reliable native
	// tool-calling; the harness parses the JSON back into a tool call. Endpoints
	// that do not support response_format can opt out with --no-json-mode.
	JSONMode bool
	// JSONFormat selects the response_format variant to send.
	//   - "json_schema" (default): OpenAI structured outputs.
	//   - "json_object":           json_object with a top-level schema; preferred
	//                              for llama.cpp and older servers.
	JSONFormat string
	// RequestTimeout bounds a single (streamed) HTTP request.
	RequestTimeout time.Duration
	// CommandTimeout is the default wall-clock budget for run_command.
	CommandTimeout time.Duration

	// ContextTokens is the prompt-size budget (in tokens) above which the
	// agent compacts older conversation history so long tasks do not overflow
	// the model's context window.
	ContextTokens int
	// Verify is an optional shell command (e.g. "go build ./... && go test
	// ./...") run automatically when the model believes the task is done. A
	// non-zero exit is fed back so the agent can self-correct.
	Verify string
	// VerifyAttempts caps how many times a failing verification command is fed
	// back to the model for self-correction within one task.
	VerifyAttempts int
	// Checkpoint makes the agent commit the workspace before each task (and,
	// when Verify is set, measure it first), so a task can be rolled back and a
	// task that turns a passing verification into a failing one is recognised.
	// It requires a git repository.
	Checkpoint bool
	// Worktree runs the task in a scratch git worktree of the workspace instead
	// of the workspace itself, so an attempt cannot damage the repository it is
	// working on. Set on the command line only; it changes Workdir.
	Worktree bool
	// Promote merges a worktree's work back into the workspace when the run
	// ends, but only when Verify is set and passing and the merge is a clean
	// fast-forward — the point is that promotion is earned by the measure, not
	// granted by the run having finished. Command line only; needs Worktree.
	Promote bool
	// Persist enables saving the conversation transcript so a session can be
	// resumed later. It never stores secrets (the API key is never in the
	// transcript).
	Persist bool

	// AllowCommands and DenyCommands are regular expressions applied to
	// run_command commands. A command matching DenyCommands is refused
	// outright (even with AutoApprove); one matching AllowCommands runs
	// without a prompt (even without AutoApprove) only when the allow pattern
	// matches the entire command. Deny wins over allow. Unmatched commands
	// follow the normal approval flow. A built-in deny list for catastrophic
	// commands is always applied on top of these.
	AllowCommands []string
	DenyCommands  []string

	// PassEnv lists credential-like environment variable names that child
	// processes may inherit despite scrubbing. DREA_API_KEY and OPENAI_API_KEY
	// are never passed through even when listed.
	PassEnv []string

	// AllowInsecureKeyTransport permits sending an API key over plain HTTP.
	// Prefer HTTPS; this is an explicit escape hatch even for loopback.
	AllowInsecureKeyTransport bool

	// AllowExternalDebugLog permits --debug paths outside the workspace.
	// Debug logs contain raw prompts, model replies, and tool data.
	AllowExternalDebugLog bool

	// Debug dumps raw request/response traffic to the given path when non-empty.
	Debug string
}

// Built-in fallbacks used when neither a saved setting, environment variable
// nor flag provides a value.
const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-4o"
)

// DefaultVerifyAttempts is the self-correction budget used when none is set.
const DefaultVerifyAttempts = 3

// DefaultContextTokens is the prompt-size budget used when none is configured.
// It is deliberately conservative so it fits comfortably inside common
// context windows (e.g. 128k) while leaving room for the model's reply.
const DefaultContextTokens = 96000

// Defaults returns a Config populated from (in decreasing precedence)
// environment variables, the persisted settings file, the persisted API key
// file, and built-in defaults, before command-line flags are applied. Saved
// holds the values loaded from the settings file (zero when unset).
func Defaults(saved Saved) Config {
	wd, _ := os.Getwd()
	ctxTokens := DefaultContextTokens
	if saved.ContextTokens > 0 {
		ctxTokens = saved.ContextTokens
	}
	key := firstEnv("DREA_API_KEY", "OPENAI_API_KEY")
	if key == "" {
		key = firstNonEmpty(saved.APIKey)
	}
	return Config{
		BaseURL:         envOr("DREA_BASE_URL", firstNonEmpty(saved.BaseURL, DefaultBaseURL)),
		APIKey:          key,
		Model:           envOr("DREA_MODEL", firstNonEmpty(saved.Model, DefaultModel)),
		Workdir:         envOr("DREA_WORKDIR", wd),
		AutoApprove:     envBool("DREA_AUTO_APPROVE"),
		MaxSteps:        50,
		Temperature:     savedTemperature(saved),
		TopP:            savedTopP(saved),
		ReasoningEffort: envOr("DREA_REASONING_EFFORT", saved.ReasoningEffort),
		JSONMode:        true,
		JSONFormat:      envOr("DREA_JSON_FORMAT", firstNonEmpty(saved.JSONFormat, "json_schema")),
		RequestTimeout:  10 * time.Minute,
		CommandTimeout:  120 * time.Second,
		ContextTokens:   envInt("DREA_CONTEXT_TOKENS", ctxTokens),
		Verify:          envOr("DREA_VERIFY", saved.Verify),
		VerifyAttempts:  envInt("DREA_VERIFY_ATTEMPTS", verifyAttempts(saved)),
		Checkpoint:      envBool("DREA_CHECKPOINT") || saved.Checkpoint,
		Persist:         !envBool("DREA_NO_PERSIST"),
		AllowCommands:   envList("DREA_ALLOW_COMMANDS", saved.AllowCommands),
		DenyCommands:    envList("DREA_DENY_COMMANDS", saved.DenyCommands),
		Debug:           envOr("DREA_DEBUG", ""),
	}
}

// DefaultTemperature is the sampling temperature used when none is configured.
// Zero prefers deterministic replies and matches the documented default.
const DefaultTemperature = 0.0

// DefaultTopP is the nucleus-sampling value used when none is configured.
// Zero means "omit top_p from the request" so the endpoint default applies.
const DefaultTopP = 0.0

func savedTemperature(saved Saved) float64 {
	if env := os.Getenv("DREA_TEMPERATURE"); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil {
			return v
		}
	}
	if saved.Temperature != 0 {
		return saved.Temperature
	}
	return DefaultTemperature
}

func savedTopP(saved Saved) float64 {
	if env := os.Getenv("DREA_TOP_P"); env != "" {
		if v, err := strconv.ParseFloat(env, 64); err == nil {
			return v
		}
	}
	if saved.TopP != 0 {
		return saved.TopP
	}
	return DefaultTopP
}

// Saved carries the persisted, non-secret preferences the config layer reads
// from the settings file. It mirrors settings.Settings without importing it,
// keeping the dependency direction one-way.
type Saved struct {
	BaseURL         string
	APIKey          string
	Model           string
	Verify          string
	VerifyAttempts  int
	Checkpoint      bool
	ContextTokens   int
	JSONFormat      string
	Temperature     float64
	TopP            float64
	ReasoningEffort string
	AllowCommands   []string
	DenyCommands    []string
}

// Normalize validates the config and canonicalises paths. It must be called
// after flags are parsed and before the config is used.
func (c *Config) Normalize() error {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL == "" {
		return errors.New("base URL is empty (set --base-url or DREA_BASE_URL)")
	}
	if c.Model == "" {
		return errors.New("model is empty (set --model or DREA_MODEL)")
	}
	if c.Workdir == "" {
		return errors.New("workdir is empty")
	}
	abs, err := filepath.Abs(c.Workdir)
	if err != nil {
		return err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve workdir: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("workdir is not a directory: " + abs)
	}
	c.Workdir = abs
	if c.MaxSteps <= 0 {
		c.MaxSteps = 50
	}
	if c.ContextTokens <= 0 {
		c.ContextTokens = DefaultContextTokens
	}
	if c.TopP < 0 || c.TopP > 1 {
		return fmt.Errorf("invalid --top-p %v; expected a value between 0 and 1", c.TopP)
	}
	if c.ReasoningEffort != "" {
		switch c.ReasoningEffort {
		case "low", "medium", "high":
			// ok
		default:
			return fmt.Errorf("invalid --reasoning-effort %q; expected low, medium, or high", c.ReasoningEffort)
		}
	}
	if c.VerifyAttempts <= 0 {
		c.VerifyAttempts = DefaultVerifyAttempts
	}
	switch c.JSONFormat {
	case "", "json_schema", "json_object":
		// ok
	default:
		return fmt.Errorf("invalid --json-format %q; expected json_schema or json_object", c.JSONFormat)
	}
	if c.JSONFormat == "" {
		c.JSONFormat = "json_schema"
	}
	if _, err := policy.New(c.AllowCommands, c.DenyCommands); err != nil {
		return fmt.Errorf("invalid command policy pattern: %w", err)
	}
	if err := c.validateKeyTransport(); err != nil {
		return err
	}
	if err := c.normalizeDebugPath(); err != nil {
		return err
	}
	return nil
}

// normalizeDebugPath confines --debug under the workspace unless the user
// explicitly opts into an external destination. Existing files are repaired
// to 0600. Symlinks and non-regular destinations are rejected.
func (c *Config) normalizeDebugPath() error {
	if c.Debug == "" {
		return nil
	}
	raw := c.Debug
	var abs string
	var err error
	if filepath.IsAbs(raw) {
		abs, err = filepath.Abs(raw)
	} else {
		abs, err = filepath.Abs(filepath.Join(c.Workdir, raw))
	}
	if err != nil {
		return err
	}
	parent, err := canonicalExisting(filepath.Dir(abs))
	if err != nil {
		return fmt.Errorf("debug log parent %q: %w", filepath.Dir(abs), err)
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	if !c.AllowExternalDebugLog {
		rel, err := filepath.Rel(c.Workdir, abs)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("debug log %q is outside the workspace; use a relative path under --workdir or pass --allow-external-debug-log", raw)
		}
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("debug log %q must not be a symlink", abs)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("debug log %q must be a regular file", abs)
		}
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		opened, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return statErr
		}
		if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			f.Close()
			return fmt.Errorf("debug log %q changed during validation", abs)
		}
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return fmt.Errorf("secure debug log %q: %w", abs, err)
		}
		if err := f.Close(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	c.Debug = abs
	return nil
}

// canonicalExisting resolves symlinks through the deepest existing ancestor,
// retaining any not-yet-created suffix.
func canonicalExisting(path string) (string, error) {
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
	realParent, err := canonicalExisting(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(path)), nil
}

// validateKeyTransport refuses to send an API key over plain HTTP unless the
// user explicitly accepts cleartext key transport.
func (c *Config) validateKeyTransport() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL %q: %w", c.BaseURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if c.AllowInsecureKeyTransport {
			return nil
		}
		host := strings.ToLower(u.Hostname())
		return fmt.Errorf("refusing to send API key over plain HTTP to %q; use HTTPS or --allow-insecure-key-transport", host)
	default:
		return fmt.Errorf("unsupported base URL scheme %q; expected http or https", u.Scheme)
	}
}

// ChatURL is the fully-qualified chat completions endpoint.
func (c *Config) ChatURL() string { return c.BaseURL + "/chat/completions" }

// SetReasoningEffort sets the reasoning effort level, validating it against the
// same low/medium/high set enforced at startup.
func (c *Config) SetReasoningEffort(level string) error {
	switch level {
	case "low", "medium", "high":
		c.ReasoningEffort = level
		return nil
	default:
		return fmt.Errorf("invalid reasoning level %q; expected low, medium, or high", level)
	}
}

func verifyAttempts(saved Saved) int {
	if saved.VerifyAttempts > 0 {
		return saved.VerifyAttempts
	}
	return DefaultVerifyAttempts
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// envList reads a newline-separated list from an environment variable, falling
// back to the provided saved list. Newline separation is used (rather than
// commas) because the values are regular expressions that may contain commas.
func envList(key string, fallback []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var out []string
	for _, line := range strings.Split(v, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
