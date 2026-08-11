package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsSavedAPIKey(t *testing.T) {
	os.Unsetenv("DREA_API_KEY")
	os.Unsetenv("OPENAI_API_KEY")
	c := Defaults(Saved{APIKey: "sk-saved"})
	if c.APIKey != "sk-saved" {
		t.Fatalf("APIKey = %q, want saved key %q", c.APIKey, "sk-saved")
	}
}

func TestDefaultsEnvKeyWinsOverSaved(t *testing.T) {
	os.Setenv("DREA_API_KEY", "sk-env")
	os.Unsetenv("OPENAI_API_KEY")
	defer os.Unsetenv("DREA_API_KEY")
	c := Defaults(Saved{APIKey: "sk-saved"})
	if c.APIKey != "sk-env" {
		t.Fatalf("APIKey = %q, want env key %q", c.APIKey, "sk-env")
	}
}

func TestDefaultsSavedReasoningEffort(t *testing.T) {
	os.Unsetenv("DREA_REASONING_EFFORT")
	c := Defaults(Saved{ReasoningEffort: "low"})
	if c.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort = %q, want saved %q", c.ReasoningEffort, "low")
	}
}

func TestDefaultsEnvReasoningWinsOverSaved(t *testing.T) {
	os.Setenv("DREA_REASONING_EFFORT", "high")
	defer os.Unsetenv("DREA_REASONING_EFFORT")
	c := Defaults(Saved{ReasoningEffort: "low"})
	if c.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want env %q", c.ReasoningEffort, "high")
	}
}

func TestSetReasoningEffort(t *testing.T) {
	c := &Config{}
	for _, lvl := range []string{"low", "medium", "high"} {
		if err := c.SetReasoningEffort(lvl); err != nil {
			t.Fatalf("SetReasoningEffort(%q) unexpected error: %v", lvl, err)
		}
		if c.ReasoningEffort != lvl {
			t.Fatalf("ReasoningEffort = %q, want %q", c.ReasoningEffort, lvl)
		}
	}
}

func TestSetReasoningEffortInvalid(t *testing.T) {
	c := &Config{ReasoningEffort: "high"}
	if err := c.SetReasoningEffort("turbo"); err == nil {
		t.Fatal("expected error for invalid reasoning level")
	}
	if c.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort changed on failure = %q, want unchanged \"high\"", c.ReasoningEffort)
	}
}

func TestNormalizeKeyTransport(t *testing.T) {
	wd := t.TempDir()
	base := Config{
		BaseURL: "https://api.example.com/v1",
		APIKey:  "sk-test",
		Model:   "m",
		Workdir: wd,
	}
	if err := base.Normalize(); err != nil {
		t.Fatalf("https+key should be ok: %v", err)
	}

	httpRemote := base
	httpRemote.BaseURL = "http://api.example.com/v1"
	if err := httpRemote.Normalize(); err == nil {
		t.Fatal("expected error for HTTP+key to remote host")
	}

	httpKeyless := base
	httpKeyless.BaseURL = "http://api.example.com/v1"
	httpKeyless.APIKey = ""
	if err := httpKeyless.Normalize(); err != nil {
		t.Fatalf("keyless HTTP should be ok: %v", err)
	}

	loopback := base
	loopback.BaseURL = "http://127.0.0.1:8080/v1"
	if err := loopback.Normalize(); err == nil {
		t.Fatal("loopback HTTP+key should require explicit opt-in")
	}

	localhost := base
	localhost.BaseURL = "http://localhost:8080/v1"
	if err := localhost.Normalize(); err == nil {
		t.Fatal("localhost HTTP+key should require explicit opt-in")
	}

	ipv6 := base
	ipv6.BaseURL = "http://[::1]:8080/v1"
	if err := ipv6.Normalize(); err == nil {
		t.Fatal("::1 HTTP+key should require explicit opt-in")
	}

	override := base
	override.BaseURL = "http://127.0.0.1:8080/v1"
	override.AllowInsecureKeyTransport = true
	if err := override.Normalize(); err != nil {
		t.Fatalf("AllowInsecureKeyTransport should permit HTTP+key: %v", err)
	}
}

func TestDefaultsHonorSavedTopPAndZeroTemperature(t *testing.T) {
	os.Unsetenv("DREA_TOP_P")
	os.Unsetenv("DREA_TEMPERATURE")
	c := Defaults(Saved{Temperature: 0.2, TopP: 0.7})
	if c.TopP != 0.7 {
		t.Fatalf("TopP = %v, want saved 0.7", c.TopP)
	}
	if c.Temperature != 0.2 {
		t.Fatalf("Temperature = %v, want saved 0.2", c.Temperature)
	}
	c2 := Defaults(Saved{})
	if c2.TopP != DefaultTopP {
		t.Fatalf("TopP = %v, want default %v", c2.TopP, DefaultTopP)
	}
	if c2.Temperature != DefaultTemperature {
		t.Fatalf("Temperature = %v, want default %v", c2.Temperature, DefaultTemperature)
	}
}

func TestNormalizeDebugPathConfined(t *testing.T) {
	wd := t.TempDir()
	outside := t.TempDir()
	base := Config{
		BaseURL: "https://api.example.com/v1",
		Model:   "m",
		Workdir: wd,
	}

	ok := base
	ok.Debug = "traffic.log"
	if err := ok.Normalize(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wd, "traffic.log")
	if ok.Debug != want {
		t.Fatalf("Debug = %q, want %q", ok.Debug, want)
	}

	escape := base
	escape.Debug = filepath.Join(outside, "out.log")
	if err := escape.Normalize(); err == nil {
		t.Fatal("expected outside debug path to be rejected")
	}

	allowed := base
	allowed.Debug = filepath.Join(outside, "out.log")
	allowed.AllowExternalDebugLog = true
	if err := allowed.Normalize(); err != nil {
		t.Fatal(err)
	}

	traversal := base
	traversal.Debug = filepath.Join("..", filepath.Base(outside), "out.log")
	if err := traversal.Normalize(); err == nil {
		t.Fatal("expected traversal debug path to be rejected")
	}
}

func TestNormalizeDebugPathRejectsSymlinks(t *testing.T) {
	parent := t.TempDir()
	wd := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	base := Config{BaseURL: "https://example.com/v1", Model: "m", Workdir: wd}

	if err := os.Symlink(outside, filepath.Join(wd, "parent-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	parentLink := base
	parentLink.Debug = filepath.Join("parent-link", "debug.log")
	if err := parentLink.Normalize(); err == nil {
		t.Fatal("expected escaping parent symlink to be rejected")
	}

	target := filepath.Join(wd, "target.log")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(wd, "debug.log")); err != nil {
		t.Fatal(err)
	}
	finalLink := base
	finalLink.Debug = "debug.log"
	if err := finalLink.Normalize(); err == nil {
		t.Fatal("expected final symlink to be rejected")
	}
}

func TestNormalizeDebugPathCanonicalizesAndRepairs(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	debug := filepath.Join(real, "debug.log")
	if err := os.WriteFile(debug, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	c := Config{BaseURL: "https://example.com/v1", Model: "m", Workdir: alias, Debug: "debug.log"}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Workdir != real || c.Debug != debug {
		t.Fatalf("canonical paths = (%q, %q), want (%q, %q)", c.Workdir, c.Debug, real, debug)
	}
	info, err := os.Stat(debug)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("debug permissions = %o, want 600", info.Mode().Perm())
	}
}
