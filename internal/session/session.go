// Package session persists a conversation transcript to disk so a long task
// can survive an exit or crash and be resumed later.
//
// One transcript is kept per workspace root (the most recent), stored as JSON
// under the user's config directory. No secrets are written: the API key is
// never part of the transcript, only the messages exchanged with the model.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dreaagent/drea/internal/llm"
)

// Session is a resumable conversation transcript for one workspace.
type Session struct {
	Workdir  string        `json:"workdir"`
	Model    string        `json:"model,omitempty"`
	Updated  time.Time     `json:"updated"`
	Messages []llm.Message `json:"messages"`
}

// maxSize bounds how much of a transcript file is read back, so a corrupt or
// hostile file cannot exhaust memory on resume.
const maxSize = 32 * 1024 * 1024

// Dir is the directory holding saved transcripts (…/drea/sessions).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "drea", "sessions"), nil
}

// pathFor returns the transcript file path for a workspace root. The workdir is
// hashed so arbitrary paths map to a safe, fixed-length filename.
func pathFor(workdir string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(workdir))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+".json"), nil
}

// Save writes (overwrites) the transcript for its workdir. The file is written
// with 0600 permissions.
func Save(s Session) error {
	if s.Workdir == "" {
		return nil
	}
	s.Updated = time.Now()
	p, err := pathFor(s.Workdir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// Write to a temp file and rename so a crash mid-write cannot leave a
	// truncated, unparseable transcript behind.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads the transcript for a workspace root. A missing file returns
// ok=false and no error.
func Load(workdir string) (Session, bool, error) {
	var s Session
	p, err := pathFor(workdir)
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

	data, err := io.ReadAll(io.LimitReader(f, maxSize))
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, false, err
	}
	return s, true, nil
}
