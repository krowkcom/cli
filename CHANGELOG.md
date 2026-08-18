# Changelog

What changed in each release of the krowk CLI, for the people upgrading rather
than for the people who wrote it. Newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the versions are the `v*` tags a release is cut from. Entries land under
`[Unreleased]` as the work merges, and move under a version when it is tagged.

## [Unreleased]

### Added

- Every command that names a record now takes the link as readily as the slug.
  Paste `https://krowk.com/a/art_…`, the CDN URL under it, or any other link
  krowk printed into `uploads show`, `uploads attach`, `uploads delete`,
  `claim`, `runs show`, `runs finish` or `--run`, and the slug is read out of
  it — the MCP tools take one the same way. Anything that is not link-shaped is
  passed on untouched, so slugs behave exactly as they did. A link carrying no
  slug of the kind the command wants now fails locally as `bad_artifact` or
  `bad_run` (exit 1), instead of going out and coming back as a record that does
  not exist; a card link handed to `runs show` names the artifact it carries.
