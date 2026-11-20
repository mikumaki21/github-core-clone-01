# github-core-clone-01 entry — a GitHub-compatible git forge

A standalone GitHub-compatible git forge in Node 22, standard library only.
One process, no dependencies, no git binary, no outside service.

It serves git smart HTTP (upload-pack protocol v2, receive-pack v0), the
REST v3 API, the GraphQL v4 API, and a Model Context Protocol endpoint,
all from one shared in-memory store.

## Run it

```sh
node --experimental-strip-types main.ts
```

`MAKEFUN_ENV_URL` must be set: the program binds an ephemeral port,
registers it, then polls until the env says done and exits 0.
`MAKEFUN_SHOP_TOKEN` is the bearer token every API call must carry.

## Layout

- `main.ts` — the entry lifecycle (bind, register, poll done, exit 0).
- `app.ts` — router, auth, JSON helpers.
- `store.ts` — the shared store: repositories, git objects, issues, pulls.
- `git.ts` / `gitpack.ts` — git smart HTTP and packfiles.
- `rest.ts` — REST v3.
- `graphql.ts` — GraphQL v4.
- `mcp.ts` — MCP over streamable HTTP.
