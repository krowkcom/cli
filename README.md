# krowk

Permalinks for agent output. Push a screenshot, get a URL that unfurls in
GitHub, Slack, Basecamp and Linear with the run metadata attached.

Proof of concept. The CLI speaks the registry's real protocol and is tested
against both a local stand-in and a running
[krowk-registry](../krowk-registry); what is still missing is the unfurl layer
and distribution (see [What the POC still needs](#what-the-poc-still-needs)).

```bash
krowk push ../../tmp/screenshots/foobar.jpg \
  --pull-request="https://github.com/acme/storefront/pull/412" \
  --reference="https://linear.app/acme/issue/ENG-9" \
  --session="3fe6808d-088d-4a6f-a04c-cc9690bcf852"
```

```
✓ uploaded  foobar.jpg  412 KB
  https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg
  run run_8Kd2wq · expires in 24h
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
| `krowk push <file...>` | Upload files, get a link for each |
| `krowk uploads create <file...>` | The same thing, spelled out |
| `krowk uploads list` | List the workspace's uploads, newest first (`--limit`, `--before`) |
| `krowk uploads show <artifact>` | Read one artifact back |
| `krowk runs start` | Open a run to group later uploads under |
| `krowk runs finish <run>` | Close a run |
| `krowk claim <artifact> <claim-token>` | Keep an anonymous upload past its expiry |
| `krowk auth login --token <token>` | Store an API token in `~/.config/krowk/credentials.json` (0600) |
| `krowk auth token` | Print the stored token, for scripts |
| `krowk doctor` | Report version, API reachability, auth and detected run context |

Upload flags: `--run`, `--pull-request`, `--reference` (repeatable), `--session`,
`--title`, plus `--repo` / `--commit` / `--agent` to override detection. Flags
may follow the filenames.

Global flags: `--format human|json|markdown`, `--json`, `--quiet`, `--help`,
`--version`. Output is human on a TTY and JSON when piped, so an agent that
captures stdout gets structured data without asking for it.

**One file, one artifact, one link.** Pushing three files creates three
artifacts with three URLs. What groups them is a run, not an artifact.

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

Metadata is recorded on the **run**, because the registry keeps none on an
artifact. A run belongs to a workspace, so it needs an API key:

- **With a key** — `push` opens a run carrying the metadata, attaches every
  artifact to it, and closes it on the way out. `--run` attaches to a run you
  opened yourself instead, and leaves closing it to you.
- **Without a key** — the upload still works. It lands in the shared anonymous
  workspace, expires in 24 hours, and comes back with a claim token. There is no
  run, so metadata named by flag is *not* recorded, and the result says so in
  `notes` rather than dropping it silently.

## Wire contract

What the registry implements, and what this client talks to. Bytes never pass
through the registry: it hands out a presigned upload URL, the client uploads
straight to object storage, and a later call verifies what landed.

The API is resourceful all the way down, so what would be a verb hanging off an
artifact is a nested resource instead. The method follows from whether the call
can be repeated — finalizing and completing are idempotent, so they are `PUT`s;
claiming spends a one-shot token, so it is a `POST`:

```
GET        /                                  service descriptor
GET        /v1/artifacts                      list, newest first (needs a key)
POST       /v1/artifacts                      declare an upload
GET        /v1/artifacts/:slug                read one back
PUT|PATCH  /v1/artifacts/:slug/finalization   confirm the bytes landed
POST       /v1/artifacts/:slug/claim          spend a claim token (needs a key)
POST       /v1/runs                           open a run (needs a key)
PUT|PATCH  /v1/runs/:slug/completion          close a run (needs a key)
```

Everything but listing, claiming and the run endpoints works without a key: for a
keyless request the slug *is* the capability, since slugs are 21 random base58
characters and the bytes are public on the CDN regardless.

```
POST {KROWK_API_URL}/artifacts
Authorization: Bearer krowk_sk_...      # optional — anonymous uploads allowed
Content-Type: application/json

{ "artifact": {
    "filename": "foobar.jpg", "content_type": "image/jpeg",
    "byte_size": 421888, "checksum": "<sha-256 hex>", "run": "run_..." } }
```

```jsonc
// 201 Created
{
  "slug": "art_2e1d…", "state": "pending",
  "url": "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
  "markdown": "![foobar.jpg](https://cdn.krowk.com/…)",
  "expires_at": "2026-08-05T14:32:00Z",       // anonymous uploads only
  "upload": {
    "method": "PUT", "url": "https://…?X-Amz-Signature=…",
    "headers": { "Content-Type": "image/jpeg", "Content-Length": "421888" }
  },
  "next_step": "PUT the file to upload.url …, then PUT /v1/artifacts/art_2e1d/finalization",
  "claim_token": "krowk_claim_…"              // anonymous uploads only, shown once
}
```

Then the two calls `next_step` names:

```
PUT <upload.url>                                  # exactly the headers above, nothing more
PUT {KROWK_API_URL}/artifacts/{slug}/finalization
```

Every failure carries the same shape, so an agent can branch on the code:

```json
{ "error": { "code": "invalid",
             "message": "Byte size must be at most 104857600 bytes",
             "details": { "byte_size": ["must be at most 104857600 bytes"] } } }
```

The client flattens that into one map and adds a `fix` line naming the next
thing to actually do, so `krowk`'s own errors and the registry's read alike.

- **Size and digest are declared up front** because they are signed into the
  upload URL. That is what lets object storage refuse an oversized or corrupted
  body at the edge instead of storing it and leaving us to notice later. It is
  also why the client reads the whole file to digest it before the first call.
- **Finalizing and completing are idempotent** — an agent that retries either gets
  the same success, and the record keeps the moment it first reached that state.
- **Listing is cursor-paged** — `next` carries the slug to pass back as `before`,
  and is null on the last page. It is present whenever a page came back full, so a
  total that is an exact multiple of `--limit` costs one extra empty page.
- **Retries** — up to 3 attempts on a retryable failure (429, 5xx,
  `upload_missing`, `storage_unavailable`), honouring `Retry-After`.
- **Expiry** — an anonymous artifact past its expiry answers `410 Gone`.

### Not idempotent across pushes

Pushing the same bytes twice creates **two artifacts with two links**. An
artifact's identity is a random slug, not a digest of its contents, so the
registry cannot tell a retry from a second upload. The earlier draft of this CLI
promised digest-derived IDs and one stable link per file; the registry does not
work that way, and this client no longer claims it does. An agent that retries a
whole `push` after a partial failure leaves the first attempt's artifacts behind
to expire.

## Development

```bash
make check       # go vet + go test ./...
make build       # → bin/krowk, version stamped from git describe
make mock        # stand-in registry on :8787
make install     # → $GOPATH/bin/krowk
```

Against the stand-in, which needs nothing running:

```bash
KROWK_API_URL=http://localhost:8787/v1 ./bin/krowk push screenshot.png
```

Against the real registry, with [krowk-registry](../krowk-registry) up on port
3000 — note the hostname, because that app routes by it:

```bash
export KROWK_API_URL=http://api.krowk.localhost:3000/v1
./bin/krowk doctor
./bin/krowk push screenshot.png                      # anonymous, expires in 24h

# For the keyed flow, mint a key in the registry checkout:
#   bin/rails runner 'puts Workspace.find_or_create_by!(name: "Local Dev") \
#     .api_keys.create!(name: "krowk-cli local").token'
export KROWK_TOKEN=krowk_sk_...
./bin/krowk push screenshot.png --pull-request=https://github.com/acme/x/pull/1
```

```
cmd/krowk               the binary
cmd/krowk-mock          the stand-in registry
internal/cli            flag parsing, routing, commands
internal/api            the three-step upload, runs, retries, credentials
internal/runctx         git and CI metadata detection
internal/output         human / json / markdown rendering
internal/registry       the stand-in registry handler, shared with the tests
```

`internal/registry` is an in-memory stand-in for `api.krowk.com` that keeps the
test suite hermetic — no Postgres, no object storage, no Rails. It implements the
same sequence, the same error envelope and the same refusals the real registry
makes, including the ones that exist to catch a broken client: it will not
finalize bytes that never arrived, and it rejects a body whose length or digest
is not what was declared. The one thing it fakes is signing — object storage is
the same process on a `/_storage` path, with an opaque token in place of SigV4.

A stand-in can drift with the client and keep the suite green while the real
registry answers 404, so `TestWireShapeMatchesTheRegistrysRoutes` pins the method
and path of every call the CLI makes. Check it against `bin/rails routes` in the
registry checkout when either side moves.

Environment: `KROWK_TOKEN`, `KROWK_API_URL`, `KROWK_AGENT`.

## What the POC still needs

Blocking, in order:

1. **The unfurl layer.** The whole pitch is the preview card. Today `url` points
   straight at the CDN object, so pasting it gets you an image or a file
   download, not a card with the run metadata on it. That means a
   `krowk.com/a/{slug}` page serving OpenGraph tags plus a rendered preview, and
   a Slack app for Slack's own unfurl path.
2. **Distribution.** `krowk` and `@krowk/mcp` are unclaimed on npm. The site
   tells people to run `npx krowk push`, so the first release needs GoReleaser
   for the binaries plus a thin npm wrapper that pulls the right one through
   per-platform `optionalDependencies` — the pattern esbuild uses. A Homebrew
   tap and `curl | bash` come off the same build.
3. **Getting a token.** `auth login --token` stores whatever it is handed. Keys
   exist in the registry and the dashboard can issue them, but there is no
   device flow, so an agent in a container still needs a human to paste one in.

Non-blocking, in rough value order: the `@krowk/mcp` server wrapping `push` as
`krowk_push`; a Claude Code `PostToolUse` hook so screenshots upload with no
prompting; a GitHub Action.

## Open questions

- **Nothing dedupes.** See [above](#not-idempotent-across-pushes). Should the
  registry key artifacts by digest within a workspace, or is a link per push the
  intended behaviour?
- **A run per push.** With a key, every `push` that is not given `--run` opens
  and closes its own run, so ten screenshots from one agent session become ten
  runs. Should the CLI persist a run for the session — keyed on the agent's
  session ID — so they group without the caller threading `--run` through?
- **Oversized files are read before they are refused.** The upload cap lives in
  the registry, so the client digests a 2 GB file and only then hears it is too
  large. Should the API root publish `max_upload_bytes` so the client can refuse
  it locally?
- **Anonymous uploads have no key to rate-limit against.** IP, or a
  machine-scoped anonymous token minted on first push?

MIT.
