import { afterAll, beforeAll, expect, test } from "bun:test"
import pkg from "../package.json"
import { serve } from "../mock/server"
import { main, VERSION } from "../src/cli"
import { ciPullRequest, detectAgent, slug } from "../src/context"
import { humanBytes, relativeExpiry } from "../src/output"

const FIXTURE = new URL("../package.json", import.meta.url).pathname

let registry: ReturnType<typeof serve>
let tiny: ReturnType<typeof serve>

beforeAll(() => {
  registry = serve(0)
  tiny = serve(0, 10) // 10-byte limit, to exercise the 413 path without a huge file
  process.env.KROWK_API_URL = `${registry.url}v1`
  process.env.KROWK_TOKEN = "krk_test"
})

afterAll(() => {
  registry.stop(true)
  tiny.stop(true)
})

async function run(...argv: string[]) {
  const lines: string[] = []
  const { log, error } = console
  console.log = (s: unknown) => lines.push(String(s))
  console.error = (s: unknown) => lines.push(String(s))
  try {
    return { code: await main(argv), out: lines.join("\n") }
  } finally {
    Object.assign(console, { log, error })
  }
}

test("the advertised version is the packaged version", () => {
  expect(VERSION).toBe(pkg.version)
})

test("uploads create returns a canonical URL and round-trips the metadata", async () => {
  const { code, out } = await run(
    "uploads",
    "create",
    FIXTURE,
    "--pull-request=https://github.com/acme/storefront/pull/412",
    "--reference=https://linear.app/acme/issue/ENG-9",
    "--reference=https://sentry.io/issues/1",
    "--session=sess_abc123",
    "--title=Checkout — mobile",
    "--json",
  )

  expect(code).toBe(0)
  const { ok, data } = JSON.parse(out)
  expect(ok).toBe(true)
  expect(data.url).toBe(`${registry.url}a/${data.id}`)
  expect(data.metadata.pull_request).toBe("https://github.com/acme/storefront/pull/412")
  expect(data.metadata.reference).toEqual(["https://linear.app/acme/issue/ENG-9", "https://sentry.io/issues/1"])
  expect(data.metadata.session).toBe("sess_abc123")
  expect(data.metadata.client).toBe(`krowk-cli/${VERSION}`)
  // Auto-detected without a flag.
  expect(data.metadata.commit).toMatch(/^[0-9a-f]{40}$/)
})

test("the same bytes upload to the same URL, however many times an agent retries", async () => {
  const first = await run("push", FIXTURE, "--json")
  const second = await run("push", FIXTURE, "--json")
  expect(JSON.parse(first.out).data.url).toBe(JSON.parse(second.out).data.url)
})

test("markdown format emits a paste-ready preview link", async () => {
  const { out } = await run("uploads", "create", FIXTURE, "--title=Checkout", "--format=markdown")
  expect(out).toMatch(/^\[!\[Checkout]\(http.+preview\.png\)]\(http.+\)$/)
})

test("a missing file fails locally with a fix, before any upload", async () => {
  const { code, out } = await run("uploads", "create", "nope.png", "--json")
  expect(code).toBe(1)
  expect(JSON.parse(out).error).toMatchObject({ error: "file_unreadable", retryable: false })
})

test("a rejected upload surfaces the server's limit, actual value and fix", async () => {
  const previous = process.env.KROWK_API_URL
  process.env.KROWK_API_URL = `${tiny.url}v1`
  try {
    const { code, out } = await run("uploads", "create", FIXTURE, "--json")
    expect(code).toBe(1)
    expect(JSON.parse(out).error).toMatchObject({
      status: 413,
      error: "artifact_too_large",
      limit_bytes: 10,
      retryable: false,
    })
  } finally {
    process.env.KROWK_API_URL = previous
  }
})

test("an unknown command is an error an agent can read", async () => {
  const { code, out } = await run("uploads", "yeet", "--json")
  expect(code).toBe(1)
  expect(JSON.parse(out).error.error).toBe("unknown_command")
})

test("context detection", () => {
  expect(slug("git@github.com:acme/storefront.git")).toBe("acme/storefront")
  expect(slug("https://github.com/acme/storefront")).toBe("acme/storefront")
  expect(detectAgent({ CLAUDECODE: "1" })).toBe("claude-code")
  expect(detectAgent({ KROWK_AGENT: "custom", CLAUDECODE: "1" })).toBe("custom")
  expect(ciPullRequest({ GITHUB_REF: "refs/pull/412/merge", GITHUB_REPOSITORY: "acme/storefront" })).toBe(
    "https://github.com/acme/storefront/pull/412",
  )
})

test("formatting", () => {
  expect(humanBytes(412 * 1024)).toBe("412 KB")
  expect(relativeExpiry(new Date(Date.now() + 47 * 3600_000).toISOString())).toBe("expires in 47h")
  expect(relativeExpiry(new Date(Date.now() - 1000).toISOString())).toBe("expired")
})
