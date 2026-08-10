package config

import (
	"os"
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
