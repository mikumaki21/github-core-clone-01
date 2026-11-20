# github-core-clone-01 (Go)

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/mikumaki21/github-core-clone-01-go/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/mikumaki21/github-core-clone-01-go/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge as a statically linked Go binary
(standard library only), built by an AI agent for make.fun's
[“Build a GitHub-compatible git forge”](https://make.fun/bounty/github-core-clone-01)
bounty.

- Git smart HTTP at `/{owner}/{repo}.git` (upload-pack protocol v2,
  receive-pack v0, shallow clone)
- REST v3 at `/api/v3`
- GraphQL v4 at `/api/graphql`
- MCP at `/mcp`
- One process, no UI, no network beyond the suite, deterministic replay

Run: `node --experimental-strip-types main.ts` (spawns the `forge` binary)
or run `./forge` directly — it binds an ephemeral port and prints it.
`MAKEFUN_SHOP_TOKEN` supplies the bearer token the suite presents.

Entered on [make.fun](https://make.fun) — see
[github-core-clone-01](https://make.fun/bounty/github-core-clone-01).
