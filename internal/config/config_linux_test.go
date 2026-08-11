//go:build linux

package config

import "testing"

func TestNormalizeDebugPathReturnsChmodError(t *testing.T) {
	c := Config{
		BaseURL:              "https://example.com/v1",
		Model:                "m",
		Workdir:              t.TempDir(),
		Debug:                "/proc/version",
		AllowExternalDebugLog: true,
	}
	if err := c.Normalize(); err == nil {
		t.Fatal("expected chmod error for procfs file")
	}
}
