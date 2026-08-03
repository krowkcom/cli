// Stand-in for api.krowk.com until the real registry exists.
// Implements the contract the marketing site already publishes: multipart POST,
// digest-derived (idempotent) IDs, machine-readable errors, rate-limit headers.
import { createHash } from "node:crypto"

const DEFAULT_LIMIT_BYTES = 100 * 1024 * 1024
const DAILY_UPLOADS = 100

type Stored = {
  id: string
  url: string
  preview_url: string
  bytes: number
  expires_at: string
  files: { filename: string; bytes: number; content_type: string }[]
  metadata: unknown
}

const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body, null, 2), {
    status,
    headers: { "content-type": "application/json", ...headers },
  })

export function serve(port = Number(process.env.PORT ?? 8787), limitBytes = DEFAULT_LIMIT_BYTES) {
  const store = new Map<string, Stored>()

  return Bun.serve({
    port,
    async fetch(req) {
      const url = new URL(req.url)
      const site = process.env.KROWK_SITE_URL ?? url.origin
      const rateHeaders = {
        "x-ratelimit-limit": String(DAILY_UPLOADS),
        "x-ratelimit-remaining": String(Math.max(0, DAILY_UPLOADS - store.size)),
      }

      if (req.method === "POST" && url.pathname === "/v1/artifacts") {
        const form = await req.formData().catch(() => null)
        const files = (form?.getAll("file") ?? []).filter((f): f is File => f instanceof File)

        if (!files.length) {
          return json(422, {
            error: "no_file",
            fix: "attach at least one file as the multipart field `file`",
            retryable: false,
          })
        }

        const bytes = files.reduce((n, f) => n + f.size, 0)
        if (bytes > limitBytes) {
          return json(413, {
            error: "artifact_too_large",
            limit_bytes: limitBytes,
            got_bytes: bytes,
            fix: `re-encode below ${Math.round(limitBytes / 1024 / 1024)} MB or push frames separately`,
            retryable: false,
          })
        }

        // The URL is derived from the file digest, so a retrying agent gets one artifact.
        const digest = createHash("sha256")
        for (const f of files) digest.update(Buffer.from(await f.arrayBuffer()))
        const id = digest.digest("hex").slice(0, 7)

        const existing = store.get(id)
        if (existing) return json(200, existing, rateHeaders)

        const artifact: Stored = {
          id,
          url: `${site}/a/${id}`,
          preview_url: `${site}/a/${id}/preview.png`,
          bytes,
          expires_at: new Date(Date.now() + 48 * 3600 * 1000).toISOString(),
          files: files.map((f) => ({
            filename: f.name,
            bytes: f.size,
            content_type: f.type || "application/octet-stream",
          })),
          metadata: JSON.parse(String(form?.get("metadata") ?? "{}")),
        }
        store.set(id, artifact)
        return json(201, artifact, rateHeaders)
      }

      const id = url.pathname.match(/^\/v1\/artifacts\/([^/]+)$/)?.[1]
      if (req.method === "GET" && id) {
        const artifact = store.get(id)
        return artifact
          ? json(200, artifact, rateHeaders)
          : json(404, { error: "artifact_not_found", id, fix: "check the ID", retryable: false })
      }

      return json(404, {
        error: "not_found",
        fix: `POST ${url.origin}/v1/artifacts with a multipart \`file\` field`,
        retryable: false,
      })
    },
  })
}

if (import.meta.main) {
  const server = serve()
  console.log(`mock krowk registry on ${server.url}`)
  console.log(`  KROWK_API_URL=${server.url}v1 bun src/cli.ts uploads create <file>`)
}
