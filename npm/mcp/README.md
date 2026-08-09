# @krowk/mcp

The krowk MCP server. It gives an agent `krowk_push`, `krowk_list_artifacts`,
`krowk_get_artifact`, `krowk_claim_artifact`, `krowk_get_run` and
`krowk_verify_key` over stdio, so output from a task becomes a permalink without
anybody shelling out.

```json
{
  "mcpServers": {
    "krowk": {
      "command": "npx",
      "args": ["-y", "@krowk/mcp"],
      "env": { "KROWK_TOKEN": "krowk_sk_..." }
    }
  }
}
```

Uploads are confined to a root directory — the working directory by default,
`--root` or `KROWK_MCP_ROOT` to say otherwise. The model picks the paths, so the
boundary is what keeps an instruction hidden in a repository file from
publishing something else entirely.

## What this package is

A launcher. The server itself is a single static Go binary, `krowk-mcp`.
Installing this package pulls one more package — `@krowk/linux-x64` or whichever
matches your machine — through the same npm registry as everything else. There
is no postinstall script and no download from a second host, so it works behind
a proxy, under `npm ci --ignore-scripts`, and off a private mirror.

If Node is not already in the picture, point the client at the binary instead:

```bash
go install github.com/krowkcom/cli/cmd/krowk-mcp@latest
# or grab an archive from https://github.com/krowkcom/cli/releases/latest
```

```json
{ "mcpServers": { "krowk": { "command": "krowk-mcp" } } }
```

## Documentation

Tools, arguments, and the errors they return: <https://github.com/krowkcom/cli>.

MIT.
