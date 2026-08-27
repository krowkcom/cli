# Changelog

What changed in each release of the krowk CLI, for the people upgrading rather
than for the people who wrote it. Newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the versions are the `v*` tags a release is cut from. Entries land under
`[Unreleased]` as the work merges, and move under a version when it is tagged.

## [Unreleased]

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

[Unreleased]: https://github.com/krowkcom/cli/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/krowkcom/cli/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/krowkcom/cli/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/krowkcom/cli/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/krowkcom/cli/compare/v0.4.1...v0.5.0
