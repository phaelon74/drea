package process

import (
	"os"
	"strings"
	"testing"
)

func TestEnvIsMinimalAndSupportsExplicitPassThrough(t *testing.T) {
	t.Setenv("DREA_API_KEY", "drea-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-secret")
	t.Setenv("UNRELATED_DREA_TEST_VAR", "unrelated")
	t.Setenv("GH_TOKEN", "explicit-token")
	SetPassEnv([]string{"GH_TOKEN", "DREA_API_KEY"})
	defer SetPassEnv(nil)

	got := envMap(Env())
	if got["DREA_API_KEY"] != "" || got["OPENAI_API_KEY"] != "" {
		t.Fatal("provider API keys must never reach child processes")
	}
	if got["AWS_ACCESS_KEY_ID"] != "" || got["UNRELATED_DREA_TEST_VAR"] != "" {
		t.Fatal("non-allow-listed environment reached child process")
	}
	if got["GH_TOKEN"] != "explicit-token" {
		t.Fatal("explicit pass-through variable was dropped")
	}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG"} {
		if parent := os.Getenv(name); parent != "" && got[name] != parent {
			t.Fatalf("%s was not retained", name)
		}
	}
}

func envMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			out[strings.ToUpper(name)] = value
		}
	}
	return out
}
