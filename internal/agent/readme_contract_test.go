package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestREADMEConfigurationContract(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test source")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(data)

	for _, token := range []string{
		"`--json-format`",
		"`DREA_JSON_FORMAT`",
		"`--pass-env`",
		"`DREA_TEMPERATURE`",
		"`DREA_TOP_P`",
		"--allow '^go test ./\\.\\.\\.$'",
		"--allow '^git status$'",
	} {
		if !strings.Contains(readme, token) {
			t.Errorf("README missing configuration contract %q", token)
		}
	}
	if strings.Contains(readme, "DREA_PASS_ENV") {
		t.Error("README documents nonexistent DREA_PASS_ENV")
	}
}
