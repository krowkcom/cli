package api

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// credentials is the on-disk shape of ~/.config/krowk/credentials.json. The
// token is the only field that has to be there; the rest is what the registry
// said about that token when it was stored, kept so nothing has to ask again.
type credentials struct {
	Token     string `json:"token"`
	KeyID     string `json:"key_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// Identity is what the registry said the stored key was, recorded once at
// login. It is a record of an answer, not a claim about the key now: a key can
// be revoked between logging in and using it, and only the registry can say so.
type Identity struct {
	KeyID     string `json:"key_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// Where ReadToken would take its token from, as reported by TokenSource.
const (
	TokenSourceEnv  = "KROWK_TOKEN"
	TokenSourceFile = "credentials file"
	TokenSourceNone = "none"
)

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

// readCredentials returns what is on disk. Every failure reads the same as an
// empty file, because there is nothing a caller could do differently: a missing
// file, an unreadable one and a corrupt one all mean "no key here", and the fix
// for all three is to log in again.
func readCredentials() credentials {
	data, err := os.ReadFile(CredentialsPath())
	if err != nil {
		return credentials{}
	}
	var c credentials
	if json.Unmarshal(data, &c) != nil {
		return credentials{}
	}
	return c
}

// ReadToken prefers KROWK_TOKEN so CI never has to write a file.
func ReadToken(env func(string) string) string {
	if t := env("KROWK_TOKEN"); t != "" {
		return t
	}
	return readCredentials().Token
}

// TokenSource names where ReadToken just got its token, so diagnostics can say
// which of the two a surprising key came from. Answering "no key" is a source
// too — it is the difference between anonymous by choice and a login that never
// landed.
func TokenSource(env func(string) string) string {
	switch {
	case env("KROWK_TOKEN") != "":
		return TokenSourceEnv
	case readCredentials().Token != "":
		return TokenSourceFile
	}
	return TokenSourceNone
}

// ReadIdentity returns the identity recorded at login, and whether there is one
// worth reporting.
//
// It is deliberately silent when KROWK_TOKEN is set. ReadToken prefers the
// environment, so the key doing the work is not the key the file describes, and
// the file's workspace would name somewhere uploads are not going — a wrong
// answer given confidently, which is worse than no answer. An identity is also
// withheld when the file holds no token: a workspace with nothing to reach it
// with is left over from a login that has since been replaced.
func ReadIdentity(env func(string) string) (Identity, bool) {
	if env("KROWK_TOKEN") != "" {
		return Identity{}, false
	}
	c := readCredentials()
	if c.Token == "" || c.KeyID == "" {
		return Identity{}, false
	}
	return Identity{KeyID: c.KeyID, Workspace: c.Workspace}, true
}

// SaveCredentials writes the token and what the registry said about it,
// owner-only, and returns where it landed.
//
// The write is atomic — a temporary file in the same directory, then a rename —
// so a crash or a full disk partway through leaves the previous credentials
// intact. A half-written file reads as "not logged in", and losing a working
// key to a failed write of that same key would be its own bug.
//
// The identity is written exactly as given, including empty. Storing a key the
// registry could not confirm has to clear whatever the last key recorded, or
// the file would keep naming a workspace that belongs to a token no longer in
// it.
func SaveCredentials(token string, id Identity) (string, error) {
	path := CredentialsPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	data, err := json.MarshalIndent(credentials{
		Token:     token,
		KeyID:     id.KeyID,
		Workspace: id.Workspace,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	// Same directory as the target, so the rename stays within one filesystem
	// and is therefore atomic. os.CreateTemp opens at 0600, which is what keeps
	// the token from being briefly world-readable — the mode is not something to
	// repair after the bytes are already down.
	tmp, err := os.CreateTemp(dir, "credentials-*.json")
	if err != nil {
		return "", err
	}
	// A no-op once the rename has consumed the name, so it can run on every
	// path and still not delete the credentials it just wrote.
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	// Durable before visible: the rename must not publish a name pointing at
	// bytes the kernel has not committed.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
