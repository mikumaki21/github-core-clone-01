# Design — github-core-clone-01 entry (minimal, end-to-end first)

## Files (owner = one file each, do not edit another surface's file)
- `main.ts` — lifecycle from the starter, keep as-is except an authored header note. Never bind 8080, listen(0), register, poll /done.
- `app.ts` — router + auth + JSON helpers ONLY. Do not add surface logic here.
- `store.ts` — THE shared store. All state lives here. Read it before writing any handler.
- `gitpack.ts` — pkt-line + packfile encode/decode (git surface owns it).
- `git.ts` — smart HTTP. `rest.ts` — REST v3. `graphql.ts` — GraphQL v4. `mcp.ts` — MCP.
- Handler interface: each surface exports `handleX(ctx: Ctx): Promise<boolean>` from app.ts. `true` = handled and response sent, `false` = fall through to 404. `Ctx` has method, path, segments (decoded), query, body/bodyText/bodyJson/bodyParseError, store, requestId. Use `sendJson` / `sendRaw` from app.ts. Never write headers/body by hand in a surface file.

## Shared rules (all surfaces)
- Auth is done in app.ts: Bearer MAKEFUN_SHOP_TOKEN, 401 {"message":"Bad credentials"} when missing/wrong, except GET /healthz. Git sends the token too.
- Every reply: application/json, X-GitHub-Request-Id (app.ts adds it via sendJson/sendRaw headers — pass `{ 'x-github-request-id': ctx.requestId }` on every send). Unknown route: 404 {"message":"Not Found"}. Malformed JSON body: 400 {"message": ...}.
- Owner adoption (store.ts header): repos are created with ownerLogin null; the FIRST lookup by (owner, name) adopts that owner; later lookups under another owner are 404. Never guess a login.
- Determinism: timestamps only from store.nowIso()/nowUnix(). Never Date.now() or Math.random() for stored data.
- Node 22 stdlib only. No deps, no git binary, no network. Run with the Node 22 binary at ~/workspace/makefun/tools/node-v22.21.1-linux-x64/bin/node, never system node v24.
- Git objects are stored verbatim; commits the server creates use store.buildCommit().

## Check counts (120 total, 600 feature pts = 5/check)
git 30, repos 15, issues 15, pulls 20, graphql 20, mcp 15, protocol 5. Priority: REST + git first, then GraphQL, then MCP, then performance (fast in-memory store, no per-request work beyond the request itself).

## Conformance sources (read the ones for your surface before coding)
Kit: ~/workspace/makefun/kit-github-core-clone-01/program/env/src/
- git: checks-git.ts, git-client.ts
- rest: checks-rest.ts, rest-client.ts
- graphql: checks-graphql.ts, graphql-client.ts
- mcp: checks-mcp.ts, mcp-client.ts
- all: wire.ts, manifest.ts, bench.ts
Local runner: node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token TOK [--perf] [--json]  (run from the kit dir, with Node 22)
