package api

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type credentials struct {
	Token string `json:"token"`
}

// CredentialsPath is ~/.config/krowk/credentials.json, XDG_CONFIG_HOME honoured.
// Not os.UserConfigDir: that is ~/Library/Application Support on macOS, and the
// CLI documents one path on every platform.
func CredentialsPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".krowk", "credentials.json")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "krowk", "credentials.json")
}

// ReadToken prefers KROWK_TOKEN so CI never has to write a file.
func ReadToken(env func(string) string) string {
	if t := env("KROWK_TOKEN"); t != "" {
		return t
	}
	data, err := os.ReadFile(CredentialsPath())
	if err != nil {
		return ""
	}
	var c credentials
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.Token
}

// SaveToken writes the token owner-only and returns where it landed.
func SaveToken(token string) (string, error) {
	path := CredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(credentials{Token: token}, "", "  ")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, append(data, '\n'), 0o600)
}
