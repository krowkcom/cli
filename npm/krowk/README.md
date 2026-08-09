# krowk

Permalinks for agent output. Push a screenshot, get a URL that unfurls in
GitHub, Slack, Basecamp and Linear with the run metadata attached.

```bash
npx krowk push screenshot.png \
  --pull-request="https://github.com/acme/storefront/pull/412"
```

```
✓ uploaded  screenshot.png  412 KB
  https://cdn.krowk.com/ws_9f3c/art_2e1d/screenshot.png
  run run_8Kd2wq · expires in 24h
```

## What this package is

A launcher. krowk itself is a single static Go binary with no runtime and no
dependencies; this package exists because the website says `npx krowk push` and
some people are already in Node.

Installing it pulls one more package — `@krowk/linux-x64` or whichever matches
your machine — through the same npm registry as everything else. There is no
postinstall script and no download from a second host, so it works behind a
proxy, under `npm ci --ignore-scripts`, and off a private mirror.

If Node is not already in the picture, skip it. The binary is the primary
channel:

```bash
go install github.com/krowkcom/cli/cmd/krowk@latest
# or grab an archive from https://github.com/krowkcom/cli/releases/latest
```

## Documentation

Commands, flags, output formats, MCP, and what the CLI refuses to upload and
why: <https://github.com/krowkcom/cli>.

MIT.
