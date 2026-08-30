# Changelog

What changed in each release of the krowk CLI, for the people upgrading rather
than for the people who wrote it. Newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the versions are the `v*` tags a release is cut from. Entries land under
`[Unreleased]` as the work merges, and move under a version when it is tagged.

## [Unreleased]

### Added

- `krowk push --private` uploads where only your workspace can read it, and
  `krowk_push` takes `private: true` for the same thing. The image still embeds
  anywhere: a private artifact's bytes sit on the CDN under a key whose secret
  segment is the whole of the authorization, which is what lets GitHub, Jira or
  Slack — fetching an embed server-side and anonymously, carrying nobody's
  session — render it at all. What changes is the card. `krowk.com/a/{slug}`
  opens only for a signed-in workspace member and answers everyone else exactly
  as it answers a slug that was never minted, and the API read is gated the same
  way, so nothing unfurls a private link.

  It needs an API key and is refused rather than published without one: a
  keyless upload lands in the shared anonymous workspace, which nobody is a
  member of, so there is nothing for it to be private to. The refusal comes
  before anything is sent — an agent told afterwards that its `--private` was
  dropped would have already published the file.

- Every artifact now reports its own `visibility`, on every read, as a name
  rather than a flag — so a client branching on the word keeps working when the
  third one arrives.

### Changed

- The paste labels stop promising what a private card cannot do. `Paste into
  Slack, Basecamp — they unfurl the link themselves` is true of a public
  artifact and false of a private one, so a private push is labelled for the
  audience that can actually open it, and the breadcrumb that used to say "hand
  this link on — it is public and needs no key to read" says who it opens for
  instead. The markdown label still promises the image, because the image still
  renders. Human output names the visibility beside the size when it is not the
  public default, and `krowk uploads list` names it per row.

- `--format url`, and any `--destination` the registry's table says wants the
  bare link, warn on stderr when what they printed is a private card. That form
  exists to be unfurled and a private card unfurls nowhere, so
  printing one silently would be the same broken promise in a different place.
  The warning is on stderr rather than in the output, because the output is
  about to be pasted.

- The bundled stand-in registry (`go run ./internal/devregistry`) enforces the
  same contract, so a client developed against it behaves the same in
  production: visibility is declared, validated and served; a private artifact's
  metadata answers its own workspace and answers everyone else `404`; its card
  page is indistinguishable from a slug that never existed; its byte URL names
  neither the workspace nor the artifact; and `PUT /v1/artifacts/{slug}/visibility`
  moves an artifact between public and private, re-keying the bytes and killing
  the old URL in both directions.

  One behaviour it had wrong is fixed with it: a **public** artifact's metadata
  read is now scoped to no workspace, keyed or not — matching the registry,
  where a scope a reader escapes by dropping the `Authorization` header would
  protect nothing. Its `Idempotency-Key` digest also now covers the payload as
  declared rather than as the stand-in resolved it, so a key replayed with a
  different visibility, different metadata or a differently-cased checksum is
  the refusal the registry gives rather than a replay of the first answer.

## [0.8.2] - 2026-08-29

### Added

- A spinner on the one thing krowk does that takes long enough to look hung. An
  upload of a few hundred kilobytes over a slow link is four seconds of a
  terminal that has printed nothing, which is indistinguishable from one that has
  stopped, so a single line on stderr says what is being sent and keeps moving —
  naming each file in turn when there are several. It is not progress and does
  not pretend to be: krowk hands the file to object storage in one request and is
  never told how much of it has landed, so a percentage would be a number krowk
  invented. It erases itself before the durable line is printed, so nothing about
  it reaches the scrollback and a transcript reads as though the wait never
  happened. Shown only when stderr is a terminal and the answer is prose:
  `--json`, `--quiet`, `--destination`, the paste formats and any piped stream
  are read rather than watched, and escape codes in a captured file help nobody.

### Changed

- Human output now reads as a person would say it, while the JSON envelope keeps
  every code and every fix string exactly as it was — agents parse the envelope,
  and none of this reaches them.
  - A failure leads with the fix as a sentence rather than with the wire code:
    `✗ Re-encode below 100 MB or push frames separately.` where it used to open
    on `artifact_too_large`. The code is still there, dimmed, one line down and
    in the envelope; a command the fix names is pulled onto its own line so it
    can be copied rather than picked out of prose, and a fix that names two
    things to do says both, one per line.
  - A success reads as a confirmation rather than as the record read back at
    somebody who already knows what they pushed:
    `✓ Uploaded shot.png → https://krowk.com/a/art_2e1d`, with the size, the run
    and the expiry dimmed on the line under it. `claim`, `uploads delete` and
    `runs start` / `runs finish` got the same treatment — `✓ Took art_2e1d down`,
    `✓ Finished run run_7f`, and no wire timestamp read out at a person.
  - An expiry is said the way somebody would say it out loud: `expires tomorrow`
    rather than `expires in 24h`, counted in midnights and in the reader's own
    zone, so an upload at eleven at night expires tomorrow however few hours that
    is. The MCP server still prints the exact duration, since an agent does
    better with a number.
- `krowk help` is now laid out for reading rather than for completeness: the
  commands are grouped under `PUSH & PASTE`, `RUNS`, `UPLOADS` and
  `ACCOUNT & SYSTEM` in two aligned columns, under a `USAGE` block that leads
  with the one command that matters, and it closes on what to type next. The
  groups are written down once and filled from the same catalog `krowk help
  --json` is rendered from, so a command cannot exist in one and not the other.
  The machine surface is unchanged.

## [0.8.1] - 2026-08-28

### Added

- The Krowk mark at the top of `krowk` and `krowk help`: the four-by-four grid
  of squares from the logo, drawn in half block characters so that two rows of
  the grid share one row of text — a character cell is twice as tall as it is
  wide, so a square of the grid drawn as a whole character would stretch the
  mark, and half a character keeps it square at the smallest size it can be
  drawn at. A blank line above and below so it is not jammed against the chrome
  or the words, and no colour, so it takes the foreground of whatever theme is
  running. Under it, `Krowk` and the version on one line and what krowk is on
  the next. It opens what a person reads and nothing else — the JSON surface
  stays a data structure, and one command's help stays an answer to the
  narrower question.
- A moving major tag for the GitHub Action: `uses: krowkcom/cli@v0` follows the
  0.x line rather than freezing a workflow on one patch release, and each
  release moves it once the archives and the npm packages are up. A release tag
  still pins the action and the CLI together, and an explicit `version` input
  still wins over both. The tag has no release of its own, so it installs the
  latest — and the action now refuses to install across a major line rather
  than hand a workflow pinned to `@v0` a 1.x binary with a changed command
  surface.

### Changed

- `krowk` on its own now greets rather than printing the manual. Typing the name
  to see what happens used to answer with every flag, every exit code and every
  paragraph of prose — about 150 lines, and neither "what is this" nor "what do
  I type" was any easier to find for it. It is now the mark, what krowk is, and
  three lines: the first upload, the key that makes uploads keep, and
  `krowk help` for the rest. Those three are the ones the installer signs off
  with, so a first run says what the install said. `krowk help` and
  `krowk --help` are unchanged and still answer in full, and a program reading
  `krowk` — piped, `--json`, or with a `--jq` expression — still gets the whole
  surface, since prose is no use to it and the surface is what it came for.

### Fixed

- The release workflow no longer triggers on every `v*` tag, only on a
  three-component version. The moving major tag is a `v*` tag too, and a
  release run for `v0` would have tried to cut a release of version "0" and
  publish it to npm.

## [0.8.0] - 2026-08-27

### Added

- `--link`, for the links a piece of work is about — the issue it fixes, the
  spec it implements, the discussion behind it. Repeat it for more than one, up
  to twenty, and label each with `--link-title` and classify it with
  `--link-rel` (`tracks`, `fixes`, `spec`, `discussion`, `source`,
  `supersedes`, or a word of your own); both describe the `--link` before them.
  They land on the run as `krowk.links`, an array of `{url, title, rel}`
  objects, so a reader can name a link instead of showing a raw URL. A link
  that is not an absolute `http(s)` URL, one with a space in it, a title over
  140 characters or a rel over 64, either of them carrying a tab, a newline or
  another control character, a twenty-first link, or a set of links
  large enough to crowd out the detected metadata is refused rather than
  trimmed — metadata is stored verbatim and nothing downstream validates it
  again, so a shortened URL would be a link to somewhere else for as long as
  the record lives. `--reference` is
  unchanged and is now the place for identifiers that are not URLs, such as a
  bare ticket key.
- The same links on the MCP `krowk_push` tool, as a `links` array whose schema
  names the suggested `rel` values, so an agent picks from the vocabulary
  rather than inventing one.
- A GitHub Action, `uses: krowkcom/cli@<tag>`, wrapping the CLI for CI: give it
  files or globs, it installs the binary, pushes them, and hands back `urls`,
  a `markdown` paste block ready for a PR comment, the `run-slug` and the
  `json` envelope with its claim tokens stripped — with the links also written
  to the job's step summary. The pull request, repo and commit are detected
  from the runner's environment, the same way they are locally. Pinning the
  action to a release tag pins the binary to that release, an explicit
  `version` input wins over the tag, a directory a glob swept up is named
  rather than handed to krowk, and a `**` glob on a bash too old for one
  (macOS ships 3.2) fails saying exactly that while plain globs keep working.

### Fixed

- Run metadata passed to a push that names an existing run is now reported as
  dropped instead of vanishing. `krowk push shot.png --run run_… --link …`
  records nothing on that run — a run carries the metadata it was opened with —
  and both the CLI and the MCP tool now say so in `notes`, naming the flags and
  the run. `--caption` and `--metadata` are unaffected: they land on the
  artifact. The keyless note gained `--title` for the same reason: it was
  dropped and unmentioned.
- The branch a run records in CI. GitHub checks out a detached HEAD, where
  git can only answer the literal `HEAD` — the branch is read from the
  runner's environment instead, preferring a pull request's source branch
  over the synthetic `412/merge` ref, and taking nothing from a tag push. A
  local detached HEAD records no branch at all now, which is the truth of it.

## [0.7.0] - 2026-08-26

### Removed

- The `registry serve` command. It was a development tool, but it sat in the
  public help and the surface JSON, where an agent reading `krowk --help` would
  take it for a way to host uploads — and host them on a process whose links
  die with it. The stand-in still exists for developing krowk itself: run it
  with `make mock` (`go run ./internal/devregistry`) and point commands at it
  with `--dev` as before.

## [0.6.0] - 2026-08-24

Upgrading from 0.4.1 over npm? This carries everything 0.5.0 did as well —
`--jq`, pasted links where a slug is asked for, and the rest — because 0.5.0
was released on GitHub but never published to npm.

### Changed

- Every paste form krowk prints now comes from the registry verbatim — the
  block, the bare link, and the `destinations` table beside them in the JSON
  envelope. Nothing is assembled in the CLI any more, which is what lets the
  look of a krowk reference change in a single registry deploy, including for
  the installs that already exist. `--format markdown` therefore prints the
  whole block rather than a one-line embed, and several files come back as
  several blocks separated by a blank line rather than one line each.
- The bundled agent skill now says plainly what it only implied: never paste a
  bare artifact link anywhere, use `paste.markdown` / `paste.url` and pick
  between them with the served `paste.destinations` table, and reach for
  `--destination` where the destination is known. It also nudges: an unclaimed
  artifact pasted into a pull request, an issue or a doc becomes a broken image
  once it expires, so the claim step is surfaced to the person before the paste
  rather than after. A test holds the skill to those lines.
- `--title` no longer relabels a pasted link. It is the title of the work and
  lands on the run, as it always did; what a pasted link says about a file is
  now that file's `--caption`, which is recorded on the artifact and read back
  by whatever renders it. `krowk_push` over MCP follows the same rule.

### Added

- `--destination <tool>` on `push` prints what that tool wants pasted into it —
  the krowk block for `github`, `linear` and the other markdown surfaces, the
  bare card link for `slack`, `basecamp` and the others that unfurl one
  themselves. A tool krowk has not been told about gets the block, and the push
  still succeeds: a block where it does not render is informative text, a bare
  link is a link nobody can tell anything about. Which tool wants which form is
  the registry's table, served with the artifact and readable at
  `paste.destinations`, so a tool proving out reaches installs that predate it —
  there is no list of tools inside the CLI. It cannot be combined with
  `--format`, `--json` or `--jq`, which ask for a different rendering of the same
  result; that is refused as `bad_flag` rather than silently ranked.
- `--caption '<text>'` on `push` records what a file shows on the artifact
  itself, as `krowk.caption`, so whatever renders the link later — a card page,
  a pull request comment, an integration — reads the caption off the record
  instead of being told it again at every destination. It is per file and
  repeatable: `krowk push before.png after.png --caption 'Cart before the fix'
  --caption 'Cart after the fix'` captions each one, a single caption covers a
  whole set, and a count that matches neither is refused as `bad_flag` rather
  than guessed at. Distinct from `--title`, which stays a label for the work and
  lands on the run. A keyless push drops it as it drops all metadata, and says
  so in `notes`.
- Ordinary `krowk push` output now ends with the ready-to-paste krowk block, so
  the last thing on screen is the thing worth copying rather than a bare link.
  `--quiet` still prints the record and nothing suggested.
- The paste envelope `krowk registry serve` answers with now carries the krowk
  block itself — the image, the caption from `krowk.caption`, the link through
  to the card, and the expiry of an unclaimed upload — beside the bare link and
  the destination table. It is what the production registry serves, so a paste
  built against the local stand-in looks like a paste built against production.

## [0.5.0] - 2026-08-21

### Added

- `--jq '<expression>'` filters a result inside krowk, with jq compiled in — no
  jq binary to install and no pipe to build. It works on every command, implies
  `--json`, and reads what the command rendered: the envelope normally, the bare
  record under `--quiet`. A string result prints without its quotes, so
  `URL=$(krowk push shot.png --jq '.data.artifacts[0].url')` is the whole
  ceremony; anything else prints as JSON, one value per line. `krowk help --json`
  is filterable too, which is the shortest way for an agent to read the surface.
  A failure is filtered like any other result, in whatever shape the command
  rendered it: `--jq '.error.error'` reads the code out of an envelope, and
  `--quiet --jq '.error'` reads it out of the bare body. An expression that does
  not parse is refused as `bad_jq` before the command sends anything, and one
  that does not fit the result it was pointed at answers `jq_failed` afterwards,
  saying that the command itself succeeded so that a wrapper retrying on a
  non-zero exit does not repeat the work. A failure `--jq` caused is always
  reported whole, since filtering the complaint with the expression behind it
  would bury it. `auth token`, `registry serve` and `--version` print no JSON,
  and refuse the flag rather than ignore it — the surface says which commands
  those are, under `no_json`. Neither can it be combined with `--format human`,
  `markdown` or `url`, since one of the two would have to be discarded.
  `doctor` and `upgrade` answer with a bare record and no envelope, as they
  always have, so filter those as `--jq '.token_source'`.
- Every command that names a record now takes the link as readily as the slug.
  Paste the card page, `https://krowk.com/a/art_…`, the CDN URL under it, or the
  markdown line carrying both, into `uploads show`, `uploads attach`,
  `uploads delete`, `claim`, `runs show`, `runs finish` or `--run` — the slug is
  read out of it, and the MCP tools take one the same way. Anything that is not
  link-shaped is passed on untouched, so slugs behave exactly as they did.
- A link carrying no slug of the kind the command wants now fails as
  `bad_artifact` or `bad_run` (exit 1) before anything is sent, instead of going
  out and coming back as a record that does not exist. A card link handed to
  `runs show` names the artifact it carries, and a link carrying two different
  artifacts is refused rather than acted on — the takedown has no undo.

### Changed

- A `--format` nobody has heard of is now refused even when `--json` or `--jq`
  was passed as well. It used to be accepted and ignored, so a caller who meant
  `--format markdown` and mistyped it was told nothing.
- A claim token is trimmed before it is sent, on `claim` and on
  `uploads delete`, so one copied with a trailing space or newline works instead
  of failing as an unauthorised claim.
- A blank record where one is required is now the command's own missing-argument
  failure rather than a request. `--run "  "` — a shell expanding an unset
  variable — no longer reads as "no run at all", where it used to open a fresh
  run on a push and widen `uploads list` to the whole workspace. A blank
  artifact answers `no_artifact` (exit 1) on both the CLI and the MCP server,
  which previously answered `missing_claim` (exit 3, a credential to fix).
- Failures about a pasted value no longer quote it back. A URL is where
  credentials travel, and a refusal is written to stderr and into the JSON
  envelope.

[Unreleased]: https://github.com/krowkcom/cli/compare/v0.8.2...HEAD
[0.8.2]: https://github.com/krowkcom/cli/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/krowkcom/cli/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/krowkcom/cli/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/krowkcom/cli/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/krowkcom/cli/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/krowkcom/cli/compare/v0.4.1...v0.5.0
