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
  expires in 47h

  GitHub, Linear, Notion — renders the image
  [![foobar.jpg](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)

  Slack, Basecamp — they unfurl the link themselves
  https://krowk.com/a/9f3c2e1
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
| `krowk auth verify` | Ask the registry what the key is and what it may do |
| `krowk doctor` | Report version, API reachability, auth and detected run context |
| `krowk registry serve` | Run a local registry to develop and test against |

Upload flags: `--pull-request`, `--reference` (repeatable), `--session`,
`--title`, plus `--repo` / `--commit` / `--agent` to override detection. Flags
may follow the filenames.

Global flags: `--format human|json|markdown|url`, `--json`, `--quiet`,
`--help`, `--version`. Output is human on a TTY and JSON when piped, so an agent
that captures stdout gets structured data without asking for it.

## Pasting the result

There is no single paste-ready string, because the surfaces disagree.

| Destination | Use | Why |
| --- | --- | --- |
| GitHub, Linear, Notion | `--format markdown` | GitHub builds preview cards only for its own resources, so a bare third-party link renders as a plain blue anchor no matter what OpenGraph tags the page carries. It *does* render image URLs inline, so `[![title](preview)](page)` is what actually shows the artifact in a PR comment. |
| Slack, Basecamp | `--format url` | Both unfurl a bare URL into a card of their own. Slack renders no markdown image embeds at all — pasting the embed form there shows raw text. |

Human output prints both, labelled. `--json` carries both under `paste`:

```json
{
  "ok": true,
  "data": { },
  "paste": {
    "markdown": "[![foobar.jpg](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)",
    "url": "https://krowk.com/a/9f3c2e1"
  }
}
```

## MCP server

`krowk-mcp` is the same thing for agents that cannot shell out — a thin client
over the same `/v1` API, speaking MCP over stdio. No logic lives there that is
not also in the CLI.

```jsonc
// Claude Code: .mcp.json — or `claude mcp add krowk -- krowk-mcp`
{
  "mcpServers": {
    "krowk": { "command": "krowk-mcp", "env": { "KROWK_TOKEN": "krk_..." } }
  }
}
```

| Tool | What it does |
| --- | --- |
| `krowk_push` | Upload files, get both paste forms back |
| `krowk_get_artifact` | Look one up by the ID at the end of its link |
| `krowk_get_run` | Report the repo, commit, branch and agent that will be attached |
| `krowk_verify_key` | Whether a key is configured, and what it may do |

Every result carries the markdown embed and the bare URL, each labelled with the
surfaces it belongs to, so the agent's job is copy-paste rather than templating.
The machine-readable artifact rides along as `structuredContent`. A failed call
comes back as a tool result with `isError` and the registry's own `fix`, not as a
transport error, so the agent can read the reason and correct the call.

The upload handshake is not exposed as separate begin/finalize tools: the `PUT`
step needs access to the local file, which the MCP client does not have, so the
handshake stays behind `krowk_push` where it belongs.

**`krowk_push` is confined to a root** — the working directory, or `--root` /
`KROWK_MCP_ROOT`. Paths are resolved with symlinks followed *before* the check,
and anything landing outside is refused. This matters more here than in the CLI:
there a person types the path, here a model picks it, and a model reads
repository files, web pages and issue bodies. An artifact is published at a URL
that needs no credential to read, so without the boundary an instruction hidden
in any of those would turn `krowk_push` into "read any file on this machine and
publish it somewhere I can fetch". Symlink order is the subtle half — a link
inside the repo pointing at `~/.ssh/id_rsa` sails through a prefix test done
before resolving.

Two things a plain prefix check gets wrong, both worth stating because both are
the common case rather than the exotic one:

- **The root may not be the home directory, or `/`.** It defaults to the working
  directory, so an agent started outside a checkout would otherwise take `$HOME`
  as its boundary — and `~/.ssh`, `~/.aws` and `~/.config/krowk` are all inside
  that. A root that broad is refused with a message saying to pass `--root`.
- **Credential files are refused inside the root too.** A checkout is not free of
  secrets: `.env` files live in them, and so do stray keys and service-account
  JSON. `.env*`, `.ssh`, `.aws`, `.gnupg`, `.kube`, `.docker`, `.netrc`, `.npmrc`,
  `.pypirc`, `.git-credentials`, `credentials.json` and `id_rsa` / `id_ed25519` /
  `id_ecdsa` are refused wherever they sit. Nobody publishes these on purpose, so
  the cost of refusing is an error message and the cost of allowing is a secret
  with a permalink.
- **A file with more than one name is refused.** Resolving symlinks catches a link
  pointing out of the root, but a hard link is not a link to anything — it is a
  second name for the same inode, so a path inside the root can be a key outside
  it with nothing to resolve. Nothing reports where the other names are, so a file
  with any is refused; copy it in and push the copy. In practice this rejects
  package-store files and nothing anyone wanted to publish.

The boundary constrains `krowk_push`; it is not a sandbox around the agent. An
agent that can also run a shell can read whatever it likes — this stops the tool
from being the thing that publishes it.

## Metadata

Flags win; everything else is detected so the agent never has to type it.

| Field | Source |
| --- | --- |
| `repo` | `GITHUB_REPOSITORY`, else `git remote get-url origin` |
| `commit` | `GITHUB_SHA`, else `git rev-parse HEAD` |
| `branch` | `git rev-parse --abbrev-ref HEAD` |
| `agent` | `KROWK_AGENT`, else `CLAUDECODE` → `claude-code`, `CURSOR_TRACE_ID` → `cursor`, `GITHUB_ACTIONS` → `github-actions` |
| `pull_request` | `--pull-request`, else derived from `GITHUB_REF` in a PR build |
| `reference`, `session`, `title` | Flags only |

## Wire contract

An upload is a three-step handshake: declare the files, PUT their bytes to the
presigned URLs the registry hands back, then finalize. Bytes never pass through
the API host, so the registry stays a control plane and storage does the heavy
lifting. The registry has to implement this for the CLI to work unchanged.

**1. Declare.** The client hashes each file first, so it can name the upload
before sending a byte.

```
POST {KROWK_API_URL}/artifacts
Authorization: Bearer <token>        # optional — anonymous uploads allowed
Idempotency-Key: <key>
Content-Type: application/json
```

```jsonc
{
  "idempotency_key": "<sha256 fold, see below>",
  "files": [
    { "filename": "foobar.jpg", "bytes": 421888, "content_type": "image/jpeg", "digest": "<sha256 of the bytes>" }
  ],
  "metadata": { }                    // the table above
}
```

```jsonc
// 201 Created — one target per declared file, in the order they were declared
{
  "id": "9f3c2e1",
  "uploads": [
    { "filename": "foobar.jpg", "method": "PUT", "url": "https://storage.../blob?sig=...",
      "headers": { "Content-Type": "image/jpeg" } }
  ],
  "finalize_url": "https://api.krowk.com/v1/artifacts/9f3c2e1/finalize"
}
```

```jsonc
// 200 OK — this key was already finalized. No bytes are sent; this is the retry path.
{ "id": "9f3c2e1", "complete": true, "artifact": { } }
```

**2. Send the bytes.** One `PUT` per target, streamed off disk, carrying the
headers the registry supplied. The API token is deliberately *not* attached —
a presigned URL carries its own authorisation and may point at any host.

**Auth.** A key is a bearer token; `--token` stores it, `KROWK_TOKEN` overrides
the file. Because a key can be revoked, expired or scoped read-only — none of
which is visible from the token string — the CLI can ask instead of guessing:

```
POST {KROWK_API_URL}/keys/verify
Authorization: Bearer <token>
```

```jsonc
// 200 OK
{ "valid": true, "key_id": "key_7f3a91c2", "workspace": "acme",
  "scopes": ["artifacts:read", "artifacts:write"] }
```

`401` with `no_key` when no token was sent, `invalid_key` when it was rejected.
A `200` carrying `valid: false` counts as a rejection too. Uploading needs
`artifacts:write`; a key without it is turned down at the manifest, before any
bytes move, with `403 insufficient_scope` naming the scope it lacked.

**Anonymous uploads.** No key is a supported path, not an error — the point is
that the upload step never fails for boring reasons. An anonymous upload is
ephemeral and unowned, so the finalize response marks it and hands back a link
that adopts it:

```jsonc
{ "anonymous": true, "expires_at": "…",           // 24h, not the workspace 48h
  "claim_url": "https://krowk.com/claim/2b7f91" }
```

Claiming happens in a browser, signed in — there is no API call for it. Which
side of the fence an upload lands on is settled when the handshake opens, so it
cannot change hands by finalizing with a different key.

The claim URL is a **capability**: anyone holding it can adopt the upload. So:

- it is printed for whoever ran the push, and kept out of both paste forms;
- `GET /v1/artifacts/{id}` never returns it — the ID is public, since it is in
  the shareable link, and knowing it must not be enough to claim the upload;
- it is handed back exactly once, to the call that created the artifact. Identity
  is derived from the bytes, so anyone holding a copy of the same file derives
  the same key; without this, pushing a file someone else had already shared
  anonymously would hand you their upload and its metadata. A retry that lost the
  original response gets the link but not the claim URL.

**3. Finalize.**

```
POST {finalize_url}
Authorization: Bearer <token>
Content-Type: application/json

{ "idempotency_key": "<key>" }
```

```jsonc
// 200 OK
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

- **Idempotency** — the key is a SHA-256 fold over each file's name, size and
  content digest, in order: `sha256(name \0 size \0 digest \0 …)`. It is
  derived from the bytes, so a retry, a crash-and-rerun, or the same push from
  another machine all converge on one artifact and one link. All three steps
  carry it; the registry verifies each blob against its declared digest on
  arrival, so agreeing on the key really does mean agreeing on the bytes.
- **Resumable** — declaring the same key again before finalizing returns the
  same ID and the same upload targets, so blobs already stored stay stored.
- **The ID authorises nothing.** It is the last segment of every link that gets
  pasted anywhere, so `finalize` requires the idempotency key and rejects a
  request without one. Only the caller that opened a handshake can complete it,
  or replay it to get the artifact back.
- **IDs lengthen on collision.** Seven hex characters is 28 bits, so two
  unrelated uploads collide after a few thousand — inside a real registry's
  first week. A collision extends the new ID rather than letting it take over an
  existing upload's link, so links stay short until they cannot be.
- **Rate limits** — `X-RateLimit-Limit`, `X-RateLimit-Remaining` on every
  response; `Retry-After` on the 429.
- **Retries** — each step retries up to 3 times on `retryable: true` (default
  for 429 and 5xx), honouring `Retry-After`.
- **Same-origin** — `finalize_url` must be on the API's own origin. The client
  refuses to send the token anywhere else, so a compromised registry response
  cannot redirect the key.
- **Upload targets are storage, not anything reachable.** A presigned URL names a
  foreign host by design, but the client requires `https`, ignores `method` and
  always sends `PUT`, and refuses loopback, link-local, private and carrier-grade
  NAT addresses. Otherwise a response body would choose the method, host, path,
  headers and body of a request the CLI makes from its own network position —
  which in CI reaches a great deal the registry cannot. A local registry is the
  one exception, since local targets are the whole point there.

  The check is on the connection, not only on the URL, because a URL check alone
  has two ways past it. An upload target that answers with a redirect is refused
  outright — a presigned URL is where the bytes belong, and a `302` would change
  the host and arrive as a `GET`, taking the method restriction with it. And the
  address is judged as it is dialled rather than by resolving the name first,
  since whoever returned the name can answer the check and the dial differently.

  "Inside the network" means more than the obvious four shapes: carrier-grade NAT
  (`100.64.0.0/10`, where Tailscale lives), `240.0.0.0/4`, multicast, and the
  NAT64 prefix `64:ff9b::/96`, which on an IPv6-only runner is how you reach the
  metadata service without naming a link-local address at all.

  **A configured proxy is exempt, and only at its own address.** Corporate proxies
  sit on private addresses, so refusing those outright would refuse every upload
  from the environments this tool is for — but the exemption is the proxy's
  address, resolved from `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`, not a switch
  that turns the boundary off. A request that skipped the proxy because `NO_PROXY`
  covers its host is judged like any other. One thing does stay out of reach:
  where a proxy sends the bytes is the proxy's business, so a name that resolves
  differently for it than for the URL check is not something this client can see.
- **Expiry** — an expired free link returns `410 Gone` carrying the original
  filename and upload time.

## Testing against a local registry

The CLI ships the registry it develops against, so trying it out needs neither
the network nor a key. In one terminal:

```bash
krowk registry serve                 # listens on :8787
```

And in another, `--dev` points the CLI at it:

```bash
krowk push screenshot.png --dev
krowk doctor --dev                   # says registry: local
```

`KROWK_DEV=1` does the same thing without the flag, which is how to point the MCP
server at it too. Precedence, most to least specific: `--dev`, then
`KROWK_API_URL`, then `KROWK_DEV`, then the public registry.

`registry serve` takes `--addr`, `--site` (the origin baked into returned links,
for demoing production-looking URLs) and `--limit-bytes` (to exercise the
too-large path).

It binds `127.0.0.1:8787` — **loopback by default**, because it accepts uploads
without a key and will answer a lookup for any artifact ID with the repo, branch,
commit and pull request behind it. Artifact IDs are seven characters and
enumerable, so on a café or office network a wider bind hands that to whoever is
nearby. `--addr` can still open it up, and the banner says so when you do.

Pending state is bounded, because declaring an upload needs no key and sends no
bytes — so it is the cheapest request here and the one worth abusing. A handshake
that never finalizes expires after an hour, swept on the next declaration; at most
256 may be in flight at once (`503`, retryable); and one manifest may declare at
most 64 files, since each mints a blob token and the body limit alone would allow
thousands. Finishing an upload releases its handshake too — only the artifact and
its ID reservation are kept, so a live link can never be handed to a later upload.

## Development

```bash
make check       # go vet + go test ./...
make build       # → bin/krowk and bin/krowk-mcp, versions stamped from git describe
make mock        # the same registry as `krowk registry serve`, from the checkout
make install     # → $GOPATH/bin/{krowk,krowk-mcp}
```

```
cmd/krowk               the binary
cmd/krowk-mcp           the MCP server
cmd/krowk-mock          the mock registry
internal/cli            flag parsing, routing, commands
internal/api            HTTP client, the upload handshake, retries, credentials
internal/runctx         git and CI metadata detection
internal/output         human / json / markdown rendering
internal/mcp            MCP over stdio, wrapping the same api client
internal/registry       the mock registry handler, shared with the tests
```

`internal/registry` is a working stand-in for `api.krowk.com`: the three-step
handshake, idempotency keys verified against the bytes that actually arrive,
the error envelope, rate-limit headers. It exists so the CLI can be developed
and demoed before the registry ships — and doubles as the spec the registry has
to satisfy. It serves the blob `PUT` itself rather than presigning out to object
storage, which keeps the mock one process while exercising exactly the same
client path. The CLI tests drive the real binary against it over a real socket
via `httptest`.

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

Non-blocking, in rough value order: a Claude Code `PostToolUse` hook so
screenshots upload with no prompting; a GitHub Action; publishing `krowk-mcp`
under the `@krowk/mcp` name the website advertises, which needs the same npm
wrapper as the CLI.

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
