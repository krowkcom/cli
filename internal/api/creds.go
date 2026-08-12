package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// storedKey is one key in the store: the token, plus what the registry said
// about it when it was stored, kept so nothing has to ask again. The token is
// the only field that has to be there.
//
// Workspace repeats the name the entry is filed under almost every time, and is
// kept anyway because of the one time it does not. A key the registry could not
// confirm has no reported workspace at all, so it files under "default" with
// this field empty — and that emptiness is the honest record of what the
// registry said, which is nothing. Reading the map key instead would invent an
// answer the registry never gave.
type storedKey struct {
	Token     string `json:"token"`
	KeyID     string `json:"key_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// WorkspaceName is the workspace's human title as the registry reported it
	// at login — the only thing a picker has to offer a person, since the slug
	// above identifies but does not describe. Keys stored before the registry
	// sent one simply have none until their next login.
	WorkspaceName string `json:"workspace_name,omitempty"`
}

// credentials is the on-disk shape of ~/.config/krowk/credentials.json: one key
// per workspace, and a pointer at whichever of them a command should reach for
// when nothing names one.
//
// It is a map rather than a list because logging in to a workspace twice is a
// re-login, not a second key, and a map makes that the only thing it can be.
type credentials struct {
	Default    string               `json:"default,omitempty"`
	Workspaces map[string]storedKey `json:"workspaces"`
}

// defaultEntry is the name a key files under when the registry never said which
// workspace it belongs to. It is a real entry name, not a sentinel: an offline
// login still has to be storable, findable and replaceable like any other.
const defaultEntry = "default"

// Identity is what the registry said the stored key was, recorded once at
// login. It is a record of an answer, not a claim about the key now: a key can
// be revoked between logging in and using it, and only the registry can say so.
type Identity struct {
	KeyID         string `json:"key_id,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

// WorkspaceKey is one stored key as `krowk workspaces` lists it: the name it is
// filed under, what the registry said about it, and whether it is the one a
// command with no workspace named would use.
type WorkspaceKey struct {
	Name      string `json:"name"`
	KeyID     string `json:"key_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// WorkspaceName is the human title, where login recorded one.
	WorkspaceName string `json:"workspace_name,omitempty"`
	Default       bool   `json:"default"`
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

// readCredentials returns what is on disk, in the current shape whatever shape
// it was written in. Every failure reads the same as an empty file, because
// there is nothing a caller could do differently: a missing file, an unreadable
// one and a corrupt one all mean "no key here", and the fix for all three is to
// log in again.
//
// Migration happens here and only here, so that reading an old file is enough
// to keep working after an upgrade — nobody logs in again to get their key back.
// The file on disk is left as it is until the next SaveCredentials, which writes
// the new shape.
func readCredentials() credentials {
	data, err := os.ReadFile(CredentialsPath())
	if err != nil {
		return credentials{}
	}

	// Decoding to raw members first is what keeps a corrupt file and a legacy
	// file apart. Both fail to be the current shape, but for opposite reasons:
	// the legacy one is valid JSON that simply predates the "workspaces" member,
	// while a truncated file is not JSON at all. Unmarshalling straight into
	// credentials could not tell them apart — it would silently accept the old
	// file as an empty store and lose a working key.
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return credentials{}
	}
	if _, ok := raw["workspaces"]; !ok {
		if _, legacy := raw["token"]; legacy {
			return migrateLegacy(data)
		}
		return credentials{}
	}

	var c credentials
	if json.Unmarshal(data, &c) != nil {
		return credentials{}
	}
	return c
}

// migrateLegacy reads the one-token shape `{"token":..., "key_id":...,
// "workspace":...}` as the one-entry store it always was. The single key files
// under the workspace it recorded, or under "default" when it recorded none —
// which is exactly where a key the registry could not confirm goes today — and
// the default pointer names it, because with one key there is nothing else it
// could sensibly point at.
func migrateLegacy(data []byte) credentials {
	var legacy storedKey
	if json.Unmarshal(data, &legacy) != nil {
		return credentials{}
	}
	if legacy.Token == "" {
		return credentials{}
	}
	name := legacy.Workspace
	if name == "" {
		name = defaultEntry
	}
	return credentials{
		Default:    name,
		Workspaces: map[string]storedKey{name: legacy},
	}
}

// entry resolves the workspace a caller named against the store. An empty name
// means "whichever one is the default", which is what every command that has
// not been told otherwise asks for. A name that is not stored is not an error
// here — it reads as no key, the same as an empty store, and the caller finds
// out the same way it would have anyway.
func (c credentials) entry(workspace string) (storedKey, bool) {
	if workspace == "" {
		workspace = c.Default
	}
	if workspace == "" {
		return storedKey{}, false
	}
	k, ok := c.Workspaces[workspace]
	return k, ok
}

// ReadToken prefers KROWK_TOKEN so CI never has to write a file. An empty
// workspace takes the store's default entry; a name takes that entry, and comes
// back empty when no key is stored under it.
func ReadToken(env func(string) string, workspace string) string {
	if t := env("KROWK_TOKEN"); t != "" {
		return t
	}
	k, _ := readCredentials().entry(workspace)
	return k.Token
}

// TokenSource names where ReadToken just got its token, so diagnostics can say
// which of the two a surprising key came from. Answering "no key" is a source
// too — it is the difference between anonymous by choice and a login that never
// landed. The workspace means what it means for ReadToken, so that the two
// always describe the same lookup.
func TokenSource(env func(string) string, workspace string) string {
	if env("KROWK_TOKEN") != "" {
		return TokenSourceEnv
	}
	if k, _ := readCredentials().entry(workspace); k.Token != "" {
		return TokenSourceFile
	}
	return TokenSourceNone
}

// ReadIdentity returns the identity recorded at login for the named workspace —
// empty meaning the default entry — and whether there is one worth reporting.
//
// It is deliberately silent when KROWK_TOKEN is set. ReadToken prefers the
// environment, so the key doing the work is not the key the file describes, and
// the file's workspace would name somewhere uploads are not going — a wrong
// answer given confidently, which is worse than no answer. An identity is also
// withheld when the entry holds no token: a workspace with nothing to reach it
// with is left over from a login that has since been replaced.
//
// The Workspace reported is the entry's own recorded field, not the name it is
// filed under, so a key the registry could not confirm still reports no
// workspace rather than the placeholder it is stored beside.
func ReadIdentity(env func(string) string, workspace string) (Identity, bool) {
	if env("KROWK_TOKEN") != "" {
		return Identity{}, false
	}
	k, _ := readCredentials().entry(workspace)
	if k.Token == "" || k.KeyID == "" {
		return Identity{}, false
	}
	return Identity{KeyID: k.KeyID, Workspace: k.Workspace}, true
}

// SaveCredentials stores the token under the workspace the registry named for
// it — or under "default" when the registry named none — points the default at
// it, and returns where the store landed.
//
// Every other stored key is left exactly as it was. That is the whole point:
// logging in to a second workspace used to cost you the first, and adding a key
// should never be a way to lose one.
//
// Storing under a name that already holds a key replaces it. A second key for
// the same workspace is a re-login, and the token it replaces is very likely the
// one that was just revoked to make it — keeping it would leave a dead key in
// the store for someone to be confused by later.
//
// The fresh key becomes the default because logging in is how a person says
// which workspace they mean to be working in now. Anything else would have the
// login appear to do nothing.
func SaveCredentials(token string, id Identity) (string, error) {
	c := readCredentials()
	if c.Workspaces == nil {
		c.Workspaces = map[string]storedKey{}
	}

	name := id.Workspace
	if name == "" {
		name = defaultEntry
	}
	// The identity is recorded exactly as given, including empty. The entry has
	// to say what the registry actually said about this key, not what the entry
	// it happens to be replacing knew about a different one.
	c.Workspaces[name] = storedKey{
		Token: token, KeyID: id.KeyID,
		Workspace: id.Workspace, WorkspaceName: id.WorkspaceName,
	}
	c.Default = name

	return writeCredentials(c)
}

// StoredWorkspaces lists every stored key, sorted by name so the listing is the
// same twice in a row — Go's map order is not, and a list that reshuffles
// between runs is hard to read and impossible to diff. Nothing stored is an
// empty slice, not an error: having no keys is a normal state, not a failure.
//
// Tokens are deliberately not among the fields. This is what a listing command
// prints, and printing secrets into a terminal, a scrollback buffer or a CI log
// is not something a list command should be able to do.
func StoredWorkspaces() []WorkspaceKey {
	c := readCredentials()
	out := make([]WorkspaceKey, 0, len(c.Workspaces))
	for name, k := range c.Workspaces {
		out = append(out, WorkspaceKey{
			Name:          name,
			KeyID:         k.KeyID,
			Workspace:     k.Workspace,
			WorkspaceName: k.WorkspaceName,
			Default:       name == c.Default,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetDefaultWorkspace repoints the default at a key that is already stored, and
// says which path it wrote so the caller can name it.
//
// Naming an entry that is not there is an error, and no write happens. A pointer
// at nothing would make every later command report "not logged in" while the
// keys are all still sitting in the file — a confusing way to fail, and one the
// user could not connect to the command that caused it. The error lists what is
// stored instead, because the usual cause is a typo or a half-remembered name
// and the answer is right there.
func SetDefaultWorkspace(name string) (string, error) {
	c := readCredentials()
	if _, ok := c.Workspaces[name]; !ok {
		stored := StoredWorkspaces()
		if len(stored) == 0 {
			return "", errors.New("no workspace keys are stored — run `krowk auth login` first")
		}
		names := make([]string, 0, len(stored))
		for _, k := range stored {
			names = append(names, k.Name)
		}
		return "", errors.New("no stored key named " + name + " — stored: " + strings.Join(names, ", "))
	}
	c.Default = name
	return writeCredentials(c)
}

// writeCredentials puts the store on disk, owner-only, and returns where it
// landed.
//
// The write is atomic — a temporary file in the same directory, then a rename —
// so a crash or a full disk partway through leaves the previous credentials
// intact. A half-written file reads as "not logged in", and losing a working
// key to a failed write of that same key would be its own bug. That reasoning
// only got stronger with more than one key in the file: a torn write now costs
// every workspace, not just the one being touched.
func writeCredentials(c credentials) (string, error) {
	path := CredentialsPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	if c.Workspaces == nil {
		c.Workspaces = map[string]storedKey{}
	}
	data, err := json.MarshalIndent(c, "", "  ")
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
