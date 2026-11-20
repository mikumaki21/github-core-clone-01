# Build log — github-core-clone-01

## 2026-11-20 20:43 WIB — start
- Coordinator: entry dir created at ~/workspace/makefun/github-core-clone-01-entry/, starter copied (main.ts, package.json, tsconfig.json, LICENSE).
- Skill v1.5.230 read; requirements: Node 22 only — using Node v22.21.1 at ~/workspace/makefun/tools/node-v22.21.1-linux-x64/bin/node.
- Account confirmed by Joy: @mikuma17 (primary wallet DK7JAQSeJRPAwjFkrQFxJfS1Btia2rFgFfGmJFFPZa3Q). Do NOT submit until parent confirms — build only.
- Deadline: entries close 2026-11-21 06:30 UTC / 13:30 WIB.
- Shared store written (store.ts): repos, git objects, trees, commits, issues, pulls, owner-adoption, deterministic timestamps.
- Router written (app.ts): auth gate, request-id, JSON helpers, dispatch to git/rest/graphql/mcp.
- Fanned out to 4 builders (git, REST, GraphQL, MCP) in parallel, minimal end-to-end first.
- Update 20:46 WIB from Joy via parent: "gas kerjain sekarang" — speed up, minimal runnable forge first, then practice trials. Account confirmed @mikuma17. No submit until parent confirms.
- Key design decision recorded in DESIGN.md: owner adoption (repo created owner-less, first lookup adopts owner; other owner = 404), auth gated in app.ts for all surfaces except /healthz, deterministic timestamps from store counter.
- 20:50 WIB — REST builder DONE: rest.ts 598 lines, 48/48 in its own smoke test (repos/issues/pulls/contents/pagination/merge incl. conflict 405). Not yet verified against the kit runner. graphql.ts (1042 lines) and mcp.ts (529 lines) already written by sibling builders, awaiting their reports. git.ts/gitpack.ts not yet present.
- 20:51 WIB — GraphQL builder DONE: graphql.ts 1045 lines, 25/25 in its own test (all 20 GraphQL check slugs covered + extras). Known gaps: mergeable always MERGEABLE for open PRs (no conflict signal), viewer user id fixed base64("User:1"), defaultBranchRef returned even for empty repo, not tested through live createApp() server. Still waiting: git builder (git.ts/gitpack.ts missing) and MCP builder report (mcp.ts 529 lines on disk).
- 20:52 WIB — MCP builder DONE: mcp.ts 548 lines, 31/31 in its own test (discover, initialize, tools/list byte-identical, 6 tools, create/get repo, issues, pulls, merge incl. conflict refusal, unknown tool -32602, parse error 400/-32700, no-auth 401, concurrent calls, session-id echo, DELETE 204). Tested via a harness replicating app.ts routing because git.ts was missing at test time — NOT yet run against real app.ts or the kit runner. Only git builder remains running.
- 20:58 WIB — Git builder DONE: git.ts 585 lines + gitpack.ts 406 lines. Claims 30/30 git on official runner and 120/120 full suite, plus 30/30 real-git 2.43 test.
- 21:00 WIB — COORDINATOR QA, verified independently (fresh server, Node 22, token test-token-qa):
  - sdk/run.ts --json: 120/120 passed (git 30, repos 15, issues 15, pulls 20, graphql 20, mcp 15, protocol 5), feature 600, agent 200, working 1.
  - sdk/run.ts --perf on a FRESH server: 120/120, feature 600, agent 200, perf 165 (push 46, clone 30, read 50, write 39), total 965/1000. (A second perf run on a reused server scored perf 171 but conformance dropped to 36/120 — stale-state artifact of reusing a server, not a real failure; the trial always starts fresh.)
  - main.ts lifecycle vs fake env: binds ephemeral port, registers, polls done, exits 0.
  - One coordinator fix after QA: malformed git POST now returns 400 instead of 500 (app.ts catch). Re-ran suite after the fix: still 120/120.
  - Entry size: 168,501 bytes total (~165 KiB), largest file graphql.ts 41 KiB — well under the 16 MiB / 8 MiB limits.
  - NOT done (parent's job): no GitHub repo, no push, no bundle/check/submit, no keypair, API key not used. Account confirmed @mikuma17.
