package main

import (
	"fmt"

	"github.com/dreaagent/drea/internal/config"
	"github.com/dreaagent/drea/internal/settings"
)

// loadSaved reads the settings file and key file into a config.Saved value so
// both the normal CLI and eval paths apply every persisted field. Load errors
// are reported without printing secret contents.
func loadSaved() (config.Saved, error) {
	saved, ok, err := settings.Load()
	if err != nil {
		return config.Saved{}, fmt.Errorf("settings: %w", err)
	}
	key, _, keyErr := settings.LoadKey()
	if keyErr != nil {
		return config.Saved{}, fmt.Errorf("API key file: %w", keyErr)
	}
	if !ok {
		return config.Saved{APIKey: key}, nil
	}
	return config.Saved{
		BaseURL:         saved.BaseURL,
		APIKey:          key,
		Model:           saved.Model,
		Verify:          saved.Verify,
		VerifyAttempts:  saved.VerifyAttempts,
		Checkpoint:      saved.Checkpoint,
		ContextTokens:   saved.ContextTokens,
		JSONFormat:      saved.JSONFormat,
		Temperature:     saved.Temperature,
		TopP:            saved.TopP,
		ReasoningEffort: saved.ReasoningEffort,
		AllowCommands:   saved.AllowCommands,
		DenyCommands:    saved.DenyCommands,
	}, nil
}
