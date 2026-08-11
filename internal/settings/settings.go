// Package settings persists preferences to a JSON file under the user's config
// directory. The API key is stored in its own 0600 file (never in settings.json
// or the session transcript) so `drea` works without exporting it every time.
package settings

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Settings holds the persisted, non-secret preferences. The API key is kept
// separately (see KeyFile).
type Settings struct {
	BaseURL string `json:"base_url,omitempty"`
	Model   string `json:"model,omitempty"`
	// Verify is an optional command run to check the workspace after a task.
	Verify string `json:"verify,omitempty"`
	// VerifyAttempts caps the self-correction rounds after a failed verify.
	VerifyAttempts int `json:"verify_attempts,omitempty"`
	// Checkpoint commits the workspace before each task so it can be rolled back.
	Checkpoint bool `json:"checkpoint,omitempty"`
	// ContextTokens is the prompt-size budget above which history is compacted.
	ContextTokens int `json:"context_tokens,omitempty"`
	// JSONFormat selects the response_format variant sent to the endpoint.
	// Valid values: json_schema (default) or json_object.
	JSONFormat string `json:"json_format,omitempty"`
	// Temperature is the sampling temperature forwarded to the endpoint.
	Temperature float64 `json:"temperature,omitempty"`
	// TopP is the nucleus-sampling probability forwarded to the endpoint.
	TopP float64 `json:"top_p,omitempty"`
	// ReasoningEffort is the reasoning effort level forwarded to the endpoint.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// AllowCommands and DenyCommands are regex patterns for the run_command
	// policy (see the config package). No secrets are ever stored here.
	AllowCommands []string `json:"allow_commands,omitempty"`
	DenyCommands  []string `json:"deny_commands,omitempty"`
}

// Path returns the settings file location (…/drea/settings.json under the
// user's config dir, e.g. ~/.config/drea/settings.json on Linux).
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "drea", "settings.json"), nil
}

// Load reads the settings file. A missing file is not an error: it returns an
// empty Settings and ok=false so callers can tell "unset" from "set to empty".
func Load() (Settings, bool, error) {
	var s Settings
	p, err := Path()
	if err != nil {
		return s, false, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return s, false, nil
		}
		return s, false, err
	}
	defer f.Close()

	// The settings file is a couple of short strings; refuse anything absurd
	// rather than reading an unbounded amount into memory.
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return s, false, err
	}
	if len(data) > maxSize {
		return s, false, fmt.Errorf("settings file exceeds %d byte limit", maxSize)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, false, err
	}
	return s, true, nil
}

// maxSize bounds how much of the settings file is read.
const maxSize = 64 * 1024

// maxKeySize bounds how much of the API key file is read.
const maxKeySize = 8 * 1024

// Save writes the settings file, creating the config directory as needed. The
// file is written with 0600 permissions.
func Save(s Settings) (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := writePrivate(p, data); err != nil {
		return "", err
	}
	return p, nil
}

// KeyFile is the file holding the API key (…/drea/key under the user's config
// dir, e.g. ~/.config/drea/key on Linux). It is stored separately from
// settings.json so secrets are never mixed with preferences or transcripts.
func KeyFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "drea", "key"), nil
}

// LoadKey reads the API key from the key file. A missing file is not an error:
// it returns ok=false so callers can tell "no stored key" from other failures.
// The key is trimmed of surrounding whitespace (a trailing newline written by
// echo is the normal case).
func LoadKey() (string, bool, error) {
	p, err := KeyFile()
	if err != nil {
		return "", false, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxKeySize+1))
	if err != nil {
		return "", false, err
	}
	if len(data) > maxKeySize {
		return "", false, fmt.Errorf("key file exceeds %d byte limit", maxKeySize)
	}
	k := strings.TrimSpace(string(data))
	return k, k != "", nil
}

// SaveKey writes the API key to the key file, creating the config directory as
// needed. The file is written with 0600 permissions, like the settings file.
// Saving an empty key removes the file.
func SaveKey(key string) (string, error) {
	p, err := KeyFile()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		return p, nil
	}
	if err := writePrivate(p, []byte(key+"\n")); err != nil {
		return "", err
	}
	return p, nil
}

// writePrivate writes data via a temp file with 0600 mode, then renames into
// place. It also Chmods the final path so a pre-existing permissive file is
// repaired even on filesystems where rename preserves the destination inode.
func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".drea-private-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	// Repair unusual filesystems that retain destination permission bits.
	return os.Chmod(path, 0o600)
}
