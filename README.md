# krowk

Permalinks for agent output. Push a screenshot, get a URL that unfurls in
GitHub, Slack, Basecamp and Linear with the run metadata attached.

Proof of concept. The CLI is complete and tested against a local mock of the
registry; the registry itself does not exist yet (see
[What the POC still needs](#what-the-poc-still-needs)).

```bash
krowk uploads create ../../tmp/screenshots/foobar.jpg \
  --pull-request="https://github.com/acme/storefront/pull/412" \
  --reference="https://linear.app/acme/issue/ENG-9" \
  --session="3fe6808d-088d-4a6f-a04c-cc9690bcf852"
```

```
✓ uploaded  foobar.jpg  412 KB
  https://krowk.com/a/9f3c2e1
  expires in 47h
```

Go, standard library only. One static binary, no runtime to install — agent
containers rarely have Node, and the whole point is that the upload step never
fails for boring reasons.

## Install

```bash
go install github.com/krowkcom/cli/cmd/krowk@latest
```

Release binaries, a Homebrew tap and the `npx krowk` wrapper the website
advertises all land with the first tagged release — see the prerequisites below.

## Commands

| Command | What it does |
| --- | --- |
| `krowk uploads create <file...>` | Upload artifacts, get one canonical URL |
| `krowk push <file...>` | Alias for `uploads create` — the form the website advertises |
| `krowk auth login --token <token>` | Store an API token in `~/.config/krowk/credentials.json` (0600) |
| `krowk auth token` | Print the stored token, for scripts |
| `krowk doctor` | Report version, API reachability, auth and detected run context |

Upload flags: `--pull-request`, `--reference` (repeatable), `--session`,
`--title`, plus `--repo` / `--commit` / `--agent` to override detection. Flags
may follow the filenames.

Global flags: `--format human|json|markdown`, `--json`, `--quiet`, `--help`,
`--version`. Output is human on a TTY and JSON when piped, so an agent that
captures stdout gets structured data without asking for it.

## Metadata

Flags win; everything else is detected so the agent never has to type it.

| Field | Source |
| --- | --- |
| `repo` | `GITHUB_REPOSITORY`, else `git remote get-url origin` |
| `commit` | `GITHUB_SHA`, else `git rev-parse HEAD` |
| `branch` | `git rev-parse --abbrev-ref HEAD` |
| `agent` | `KROWK_AGENT`, else `CLAUDECODE` → `claude-code`, `CURSOR_TRACE_ID` → `cursor`, `GITHUB_ACTIONS` → `github-actions` |
| `pull_request` | `--pull-request`, else derived from `GITHUB_REF` in a PR build |
| `session` | `--session`, else `KROWK_SESSION`, `CLAUDE_CODE_SESSION_ID`, `CURSOR_TRACE_ID`, `GITHUB_RUN_ID` |
| `reference`, `title` | Flags only |

## Wire contract

Reconstructed from what krowk.com already promises. The registry has to
implement this for the CLI to work unchanged.

```
POST {KROWK_API_URL}/artifacts
Authorization: Bearer <token>        # optional — anonymous uploads allowed
Content-Type: multipart/form-data
  file      one or more files, repeated field, streamed off disk
  metadata  JSON blob (table above)
```

```jsonc
// 201 Created — 200 OK when the same bytes were already uploaded
{
  "id": "9f3c2e1",
  "url": "https://krowk.com/a/9f3c2e1",
  "preview_url": "https://krowk.com/a/9f3c2e1/preview.png",
  "bytes": 421888,
  "expires_at": "2026-08-05T14:32:00Z",
  "files": [{ "filename": "foobar.jpg", "bytes": 421888, "content_type": "image/jpeg" }],
  "metadata": { }
}
```

Every failure carries the same shape, so an agent can fix the call from the
body alone:

```json
{
  "error": "artifact_too_large",
  "limit_bytes": 104857600,
  "got_bytes": 214958080,
  "fix": "re-encode below 100 MB or push frames separately",
  "retryable": false
}
```

- **Idempotency** — the ID is the file digest, so a retrying agent gets one
  artifact and the same link.
- **Rate limits** — `X-RateLimit-Limit`, `X-RateLimit-Remaining` on every
  response; `Retry-After` on the 429.
- **Retries** — the client retries up to 3 times on `retryable: true`
  (default for 429 and 5xx), honouring `Retry-After`.
- **Expiry** — an expired free link returns `410 Gone` carrying the original
  filename and upload time.

## Development

```bash
make check       # go vet + go test ./...
make build       # → bin/krowk, version stamped from git describe
make mock        # stand-in registry on :8787
make install     # → $GOPATH/bin/krowk

KROWK_API_URL=http://localhost:8787/v1 ./bin/krowk uploads create screenshot.png
```

```
cmd/krowk               the binary
cmd/krowk-mock          the mock registry
internal/cli            flag parsing, routing, commands
internal/api            HTTP client, streamed multipart, retries, credentials
internal/runctx         git and CI metadata detection
internal/output         human / json / markdown rendering
internal/registry       the mock registry handler, shared with the tests
```

`internal/registry` is a working stand-in for `api.krowk.com`: digest-derived
IDs, the error envelope, rate-limit headers. It exists so the CLI can be
developed and demoed before the registry ships — and doubles as the spec the
registry has to satisfy. The CLI tests drive the real binary against it over a
real socket via `httptest`.

Environment: `KROWK_TOKEN`, `KROWK_API_URL`, `KROWK_AGENT`.

## What the POC still needs

Blocking, in order:

1. **The registry.** `api.krowk.com` does not resolve. Nothing ships until
   something implements `internal/registry` for real — storage, digest-keyed
   IDs, 48h expiry with a `410` tombstone.
2. **The unfurl layer.** The whole pitch is the preview card. That means
   `krowk.com/a/{id}` serving OpenGraph tags plus a rendered `preview.png`, and
   a Slack app for Slack's own unfurl path. The CLI is done when this exists;
   without it the URL is a bare link.
3. **Distribution.** `krowk` and `@krowk/mcp` are unclaimed on npm. The site
   tells people to run `npx krowk push`, so the first release needs GoReleaser
   for the binaries plus a thin npm wrapper that pulls the right one through
   per-platform `optionalDependencies` — the pattern esbuild uses. A Homebrew
   tap and `curl | bash` come off the same build.
4. **Tokens.** `auth login --token` is a placeholder. The site promises scoped
   keys per agent per repo with their own quota; that needs an issuing endpoint
   and a dashboard, or at minimum a device flow like the Basecamp CLI's.

Non-blocking, in rough value order: the `@krowk/mcp` server wrapping
`uploads create` as `krowk_push`; a Claude Code `PostToolUse` hook so
screenshots upload with no prompting; a GitHub Action.

## Open contract questions

- A repeat push of identical bytes with *different* metadata currently keeps
  the first upload's metadata, because the digest is the identity. Should the
  second push merge metadata, or is the artifact genuinely immutable?
- Multiple files in one call return one URL. Is that one artifact with many
  files, or a gallery of artifacts under a group ID? The website shows one
  URL for three files, so this assumes the former.
- Anonymous uploads have no key to rate-limit against. IP, or a machine-scoped
  anonymous token minted on first push?

MIT.
