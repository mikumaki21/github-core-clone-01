// MCP surface for the github-core-clone-01 entry.
//
// POST /mcp, JSON-RPC 2.0 over Streamable HTTP. Two protocol eras are served:
//   modern 2026-07-28: server/discover, no handshake, no session required.
//   legacy 2025-11-25 and earlier: initialize + notifications/initialized,
//     with an optional Mcp-Session-Id header (accepted and echoed, never required).
// Every tool call acts on the shared store, so a repo/issue/pull created over
// MCP is the same object (same id) over REST, and vice versa.

import { sendJson, sendRaw, type Ctx } from './app.ts';
import { parseCommit, type Issue, type PullRequest, type Repo } from './store.ts';

const SERVER_NAME = 'github-forge';
const SERVER_VERSION = '1.0.0';
const CURRENT_REVISION = '2026-07-28';

const SUPPORTED_VERSIONS: readonly string[] = [
  '2026-07-28',
  '2025-11-25',
  '2025-06-18',
  '2025-03-26',
  '2024-11-05',
];

const CAPABILITIES = { tools: {} } as const;

// The tool list is built once, as a constant, so two tools/list calls answer
// byte for byte identically. Property names are the github-mcp-server ones
// the suite maps its documented arguments onto.
const TOOLS = Object.freeze([
  Object.freeze({
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        name: Object.freeze({ type: 'string', description: 'Name of the repository to create.' }),
        description: Object.freeze({ type: 'string', description: 'A short description of the repository.' }),
        private: Object.freeze({ type: 'boolean', description: 'Whether the repository is private.' }),
      }),
      required: Object.freeze(['name']),
    }),
  }),
  Object.freeze({
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The login of the repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The name of the repository.' }),
      }),
      required: Object.freeze(['owner', 'repo']),
    }),
  }),
  Object.freeze({
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The login of the repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The name of the repository.' }),
        title: Object.freeze({ type: 'string', description: 'Title of the issue.' }),
        body: Object.freeze({ type: 'string', description: 'Body of the issue.' }),
        labels: Object.freeze({ type: 'array', items: Object.freeze({ type: 'string' }), description: 'Labels to apply to the issue.' }),
      }),
      required: Object.freeze(['owner', 'repo', 'title']),
    }),
  }),
  Object.freeze({
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The login of the repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The name of the repository.' }),
        state: Object.freeze({ type: 'string', description: 'Issue state to list: open, closed, or all.' }),
      }),
      required: Object.freeze(['owner', 'repo']),
    }),
  }),
  Object.freeze({
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The login of the repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The name of the repository.' }),
        title: Object.freeze({ type: 'string', description: 'Title of the pull request.' }),
        body: Object.freeze({ type: 'string', description: 'Body of the pull request.' }),
        head: Object.freeze({ type: 'string', description: 'The branch with the changes (head).' }),
        base: Object.freeze({ type: 'string', description: 'The branch the changes are pulled into (base).' }),
      }),
      required: Object.freeze(['owner', 'repo', 'title', 'head', 'base']),
    }),
  }),
  Object.freeze({
    name: 'merge_pull_request',
    description: 'Merge a pull request in a repository.',
    inputSchema: Object.freeze({
      type: 'object',
      properties: Object.freeze({
        owner: Object.freeze({ type: 'string', description: 'The login of the repository owner.' }),
        repo: Object.freeze({ type: 'string', description: 'The name of the repository.' }),
        pull_number: Object.freeze({ type: 'integer', description: 'Number of the pull request to merge.' }),
      }),
      required: Object.freeze(['owner', 'repo', 'pull_number']),
    }),
  }),
]);

// ------------------------------------------------------------ JSON-RPC helpers

type JsonRpcId = string | number | null;

interface RpcResponse {
  jsonrpc: '2.0';
  id: JsonRpcId;
  result?: unknown;
  error?: { code: number; message: string; data?: unknown };
}

function ok(id: JsonRpcId, result: unknown): RpcResponse {
  return { jsonrpc: '2.0', id, result };
}

function err(id: JsonRpcId, code: number, message: string): RpcResponse {
  return { jsonrpc: '2.0', id, error: { code, message } };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function normalizeName(name: string): string {
  return name.toLowerCase().replace(/[^a-z0-9]/g, '');
}

const CANONICAL_TOOLS = ['create_repo', 'get_repo', 'create_issue', 'list_issues', 'create_pull_request', 'merge_pull_request'] as const;
type CanonicalTool = (typeof CANONICAL_TOOLS)[number];

/** Match a tool name by suffix, case-insensitively, allowing a server prefix. */
function matchTool(name: string): CanonicalTool | null {
  const wanted = normalizeName(name);
  for (const canonical of CANONICAL_TOOLS) {
    const norm = normalizeName(canonical);
    if (wanted === norm || wanted.endsWith(norm)) return canonical;
  }
  return null;
}

// ------------------------------------------------------------ argument mapping

function pickString(args: Record<string, unknown>, ...keys: string[]): string | undefined {
  for (const key of keys) {
    const value = args[key];
    if (typeof value === 'string' && value !== '') return value;
  }
  return undefined;
}

function pickNumber(args: Record<string, unknown>, ...keys: string[]): number | undefined {
  for (const key of keys) {
    const value = args[key];
    if (typeof value === 'number' && Number.isFinite(value)) return Math.trunc(value);
    if (typeof value === 'string' && /^\d+$/.test(value.trim())) return Number(value.trim());
  }
  return undefined;
}

/** Resolve owner/repo, accepting a full_name ("owner/repo") when either is absent. */
function resolveOwnerRepo(args: Record<string, unknown>): { owner?: string; repo?: string } {
  let owner = pickString(args, 'owner', 'owner_login', 'org', 'organization', 'user', 'user_login', 'login', 'owner_name');
  let repo = pickString(args, 'repo', 'repository', 'repository_name', 'repo_name');
  if ((owner === undefined || repo === undefined)) {
    const full = pickString(args, 'full_name', 'repo_full_name', 'repository_full_name', 'fullName');
    if (full !== undefined && full.includes('/')) {
      const [o, r] = full.split('/', 2);
      if (owner === undefined && o !== '') owner = o;
      if (repo === undefined && r !== '') repo = r;
    }
  }
  return { owner, repo };
}

// ------------------------------------------------------------ REST-shaped views

function originOf(ctx: Ctx): string {
  const host = ctx.req.headers.host;
  return host ? `http://${host}` : 'http://127.0.0.1';
}

function repoJson(ctx: Ctx, repo: Repo): Record<string, unknown> {
  const owner = repo.ownerLogin ?? 'viewer';
  const full = `${owner}/${repo.name}`;
  return {
    id: repo.id,
    name: repo.name,
    full_name: full,
    owner: { login: owner, id: 1, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    html_url: `${originOf(ctx)}/${full}`,
    url: `${originOf(ctx)}/api/v3/repos/${full}`,
    clone_url: `${originOf(ctx)}/${full}.git`,
    git_url: `${originOf(ctx)}/${full}.git`,
    ssh_url: `git@example.invalid:${full}.git`,
  };
}

function issueJson(ctx: Ctx, repo: Repo, issue: Issue): Record<string, unknown> {
  const full = `${repo.ownerLogin ?? 'viewer'}/${repo.name}`;
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((n) => {
      const l = repo.labels.get(n);
      return { name: n, color: l?.color ?? 'ededed', description: l?.description ?? '' };
    }),
    user: { login: issue.userLogin },
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    html_url: `${originOf(ctx)}/${full}/issues/${issue.number}`,
    url: `${originOf(ctx)}/api/v3/repos/${full}/issues/${issue.number}`,
  };
}

function pullJson(ctx: Ctx, repo: Repo, pull: PullRequest): Record<string, unknown> {
  const full = `${repo.ownerLogin ?? 'viewer'}/${repo.name}`;
  const owner = repo.ownerLogin ?? 'viewer';
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: { ref: pull.headRef, sha: pull.headSha, label: `${owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    html_url: `${originOf(ctx)}/${full}/pull/${pull.number}`,
    url: `${originOf(ctx)}/api/v3/repos/${full}/pulls/${pull.number}`,
  };
}

// ------------------------------------------------------------ merge conflicts

/** Files at a commit oid (empty when the commit does not exist). */
function filesAtCommit(ctx: Ctx, oid: string): Map<string, Buffer> {
  const obj = ctx.store.getObject(oid);
  if (!obj || obj.type !== 'commit') return new Map();
  const commit = parseCommit(obj.data);
  if (!commit) return new Map();
  return ctx.store.filesAtTree(commit.tree);
}

/** First common ancestor of two commits, or null when the histories are disjoint. */
function mergeBase(ctx: Ctx, a: string, b: string): string | null {
  const ancestors = new Set<string>();
  const stack = [a];
  let guard = 0;
  while (stack.length > 0 && guard < 100000) {
    guard += 1;
    const oid = stack.pop()!;
    if (ancestors.has(oid)) continue;
    ancestors.add(oid);
    const obj = ctx.store.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    const commit = parseCommit(obj.data);
    if (!commit) continue;
    for (const parent of commit.parents) stack.push(parent);
  }
  const queue = [b];
  guard = 0;
  while (queue.length > 0 && guard < 100000) {
    guard += 1;
    const oid = queue.pop()!;
    if (ancestors.has(oid)) return oid;
    const obj = ctx.store.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    const commit = parseCommit(obj.data);
    if (!commit) continue;
    for (const parent of commit.parents) queue.push(parent);
  }
  return null;
}

function toolError(message: string): ToolOutcome {
  return { ok: false, message };
}

function callTool(ctx: Ctx, canonical: CanonicalTool, args: Record<string, unknown>): ToolOutcome {
  const store = ctx.store;
  switch (canonical) {
    case 'create_repo': {
      const name = pickString(args, 'name', 'repo', 'repository', 'repository_name', 'repo_name');
      if (name === undefined) return toolError('Missing required argument: name');
      const description = pickString(args, 'description', 'about', 'summary', 'body') ?? '';
      const privRaw = args['private'] ?? args['is_private'];
      const isPrivate = privRaw === true || privRaw === 'true';
      const repo = store.createRepo(name, { description, private: isPrivate });
      if (repo === null) return toolError(`Repository already exists: ${name}`);
      return { ok: true, result: repoJson(ctx, repo) };
    }
    case 'get_repo': {
      const { owner, repo: name } = resolveOwnerRepo(args);
      if (owner === undefined) return toolError('Missing required argument: owner');
      if (name === undefined) return toolError('Missing required argument: repo');
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Not Found: repository ${owner}/${name} does not exist`);
      return { ok: true, result: repoJson(ctx, repo) };
    }
    case 'create_issue': {
      const { owner, repo: name } = resolveOwnerRepo(args);
      if (owner === undefined) return toolError('Missing required argument: owner');
      if (name === undefined) return toolError('Missing required argument: repo');
      const title = pickString(args, 'title', 'issue_title', 'subject');
      if (title === undefined) return toolError('Missing required argument: title');
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Not Found: repository ${owner}/${name} does not exist`);
      const body = pickString(args, 'body', 'issue_body', 'description', 'content', 'text') ?? '';
      const labelsRaw = args['labels'];
      const labels = Array.isArray(labelsRaw) ? labelsRaw.filter((l): l is string => typeof l === 'string') : undefined;
      const created = store.createIssue(repo, { title, body, labels });
      if ('error' in created) return toolError(`Label does not exist: ${created.error}`);
      return { ok: true, result: issueJson(ctx, repo, created) };
    }
    case 'list_issues': {
      const { owner, repo: name } = resolveOwnerRepo(args);
      if (owner === undefined) return toolError('Missing required argument: owner');
      if (name === undefined) return toolError('Missing required argument: repo');
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Not Found: repository ${owner}/${name} does not exist`);
      const state = pickString(args, 'state', 'status') ?? 'open';
      const issues = repo.issues.filter((issue) => state === 'all' || issue.state === state);
      return { ok: true, result: issues.map((issue) => issueJson(ctx, repo, issue)) };
    }
    case 'create_pull_request': {
      const { owner, repo: name } = resolveOwnerRepo(args);
      if (owner === undefined) return toolError('Missing required argument: owner');
      if (name === undefined) return toolError('Missing required argument: repo');
      const title = pickString(args, 'title', 'pr_title', 'pull_request_title', 'subject');
      if (title === undefined) return toolError('Missing required argument: title');
      const head = pickString(args, 'head', 'head_ref', 'head_branch', 'source', 'source_branch', 'from_branch');
      if (head === undefined) return toolError('Missing required argument: head');
      const base = pickString(args, 'base', 'base_ref', 'base_branch', 'target', 'target_branch', 'into_branch');
      if (base === undefined) return toolError('Missing required argument: base');
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Not Found: repository ${owner}/${name} does not exist`);
      const body = pickString(args, 'body', 'description', 'content', 'text') ?? '';
      const created = store.createPull(repo, { title, body, head, base });
      if ('error' in created) return toolError(created.error);
      return { ok: true, result: pullJson(ctx, repo, created) };
    }
    case 'merge_pull_request': {
      const { owner, repo: name } = resolveOwnerRepo(args);
      if (owner === undefined) return toolError('Missing required argument: owner');
      if (name === undefined) return toolError('Missing required argument: repo');
      const number = pickNumber(args, 'pull_number', 'pull_request_number', 'pr_number', 'number', 'issue_number', 'index');
      if (number === undefined) return toolError('Missing required argument: pull_number');
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Not Found: repository ${owner}/${name} does not exist`);
      const pull = repo.pulls.find((p) => p.number === number);
      if (pull === undefined) return toolError(`Not Found: pull request #${number} does not exist`);
      if (pull.state !== 'open') return toolError(`Pull request #${number} is already closed`);
      const conflict = mergeConflict(ctx, repo, pull);
      if (conflict !== null) return toolError(conflict);
      const merged = store.mergePull(repo, pull);
      if ('error' in merged) return toolError(merged.error);
      return { ok: true, result: pullJson(ctx, repo, pull) };
    }
  }
}

/** Merge-conflict check for one pull, or null when the merge may proceed. */
function mergeConflict(ctx: Ctx, repo: Repo, pull: PullRequest): string | null {
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  if (!baseTip || !headTip) return 'head or base branch not found';
  if (baseTip === headTip) return null;
  if (ctx.store.isAncestor(baseTip, headTip) || ctx.store.isAncestor(headTip, baseTip)) return null;
  const base = mergeBase(ctx, baseTip, headTip);
  const baseFiles = base === null ? new Map<string, Buffer>() : filesAtCommit(ctx, base);
  const headFiles = filesAtCommit(ctx, headTip);
  const baseNow = filesAtCommit(ctx, baseTip);
  const changedOn = (now: Map<string, Buffer>): Set<string> => {
    const changed = new Set<string>();
    for (const [path, content] of now) {
      const old = baseFiles.get(path);
      if (old === undefined || !old.equals(content)) changed.add(path);
    }
    for (const path of baseFiles.keys()) if (!now.has(path)) changed.add(path);
    return changed;
  };
  const headChanged = changedOn(headFiles);
  const baseChanged = changedOn(baseNow);
  for (const path of headChanged) {
    if (!baseChanged.has(path)) continue;
    const h = headFiles.get(path);
    const b = baseNow.get(path);
    if ((h === undefined) !== (b === undefined)) return `Merge conflict: ${path} changed on both head and base`;
    if (h !== undefined && b !== undefined && !h.equals(b)) return `Merge conflict: ${path} changed on both head and base`;
  }
  return null;
}

// ------------------------------------------------------------ JSON-RPC dispatch

function serverInfo(): { name: string; version: string } {
  return { name: SERVER_NAME, version: SERVER_VERSION };
}

/** One JSON-RPC request object -> its response, or null for a notification. */
function handleRequest(ctx: Ctx, req: unknown): RpcResponse | null {
  if (!isRecord(req)) return err(null, -32600, 'Invalid Request');
  const rawId = req['id'];
  const id: JsonRpcId = typeof rawId === 'string' || typeof rawId === 'number' ? rawId : null;
  const hasId = req['id'] !== undefined;
  const method = req['method'];
  if (typeof method !== 'string' || method === '') {
    // A notification with no method is dropped silently; a request gets -32600.
    return hasId ? err(id, -32600, 'Invalid Request: missing method') : null;
  }
  if (!hasId) {
    // Notification: no response, ever (202 with no body at the HTTP layer).
    return null;
  }
  const params = isRecord(req['params']) ? req['params'] : {};
  try {
    switch (method) {
      case 'server/discover': {
        const info = serverInfo();
        return ok(id, {
          supportedVersions: [...SUPPORTED_VERSIONS],
          capabilities: { tools: {} },
          serverInfo: info,
          _meta: { 'io.modelcontextprotocol/serverInfo': info },
        });
      }
      case 'initialize': {
        const offered =
          typeof params['protocolVersion'] === 'string' && params['protocolVersion'] !== ''
            ? params['protocolVersion']
            : typeof params['_meta'] === 'object' && params['_meta'] !== null && typeof (params['_meta'] as Record<string, unknown>)['protocolVersion'] === 'string'
              ? ((params['_meta'] as Record<string, unknown>)['protocolVersion'] as string)
              : CURRENT_REVISION;
        // Echo the client's revision (legacy handshake or modern); the suite
        // only ever offers a revision this server lists.
        const negotiated = SUPPORTED_VERSIONS.includes(offered) ? offered : CURRENT_REVISION;
        return ok(id, { protocolVersion: negotiated, capabilities: { tools: {} }, serverInfo: serverInfo() });
      }
      case 'ping': {
        return ok(id, {});
      }
      case 'tools/list': {
        return ok(id, { tools: TOOLS });
      }
      case 'tools/call': {
        const name = params['name'];
        if (typeof name !== 'string' || name === '') return err(id, -32602, 'Invalid params: missing tool name');
        const canonical = matchTool(name);
        if (canonical === null) return err(id, -32602, `Unknown tool: ${name}`);
        const args = isRecord(params['arguments']) ? params['arguments'] : {};
        const outcome = callTool(ctx, canonical, args);
        if (!outcome.ok) {
          return ok(id, { content: [{ type: 'text', text: outcome.message }], isError: true });
        }
        const text = JSON.stringify(outcome.result);
        return ok(id, { content: [{ type: 'text', text }], structuredContent: outcome.result });
      }
      default: {
        if (method.startsWith('notifications/')) return null;
        return err(id, -32601, `Method not found: ${method}`);
      }
    }
  } catch (e) {
    return err(id, -32603, e instanceof Error ? e.message : 'Internal error');
  }
}

export async function handleMcp(ctx: Ctx): Promise<boolean> {
  if (ctx.method === 'DELETE') {
    // Legacy session teardown (client.close()): nothing is held, just ack.
    sendRaw(ctx.res, 204, Buffer.alloc(0), 'application/json', { 'x-github-request-id': ctx.requestId });
    return true;
  }
  if (ctx.method !== 'POST') return false;

  // Defense in depth: app.ts already 401s when a token is configured, but
  // enforce it here too so a bare /mcp POST never slips past.
  const token = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  if (token !== '') {
    const auth = ctx.req.headers.authorization ?? '';
    if (auth !== `Bearer ${token}`) {
      sendJson(ctx.res, 401, { message: 'Bad credentials' }, { 'x-github-request-id': ctx.requestId });
      return true;
    }
  }

  const headers: Record<string, string> = { 'x-github-request-id': ctx.requestId };
  const sessionId = ctx.req.headers['mcp-session-id'];
  if (typeof sessionId === 'string' && sessionId !== '') headers['mcp-session-id'] = sessionId;

  // A body that is not JSON: HTTP 400 with JSON-RPC -32700.
  if (ctx.bodyParseError || ctx.bodyText.trim() === '') {
    sendJson(ctx.res, 400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }, headers);
    return true;
  }

  const body = ctx.bodyJson;
  if (Array.isArray(body)) {
    const responses: RpcResponse[] = [];
    for (const item of body) {
      const response = handleRequest(ctx, item);
      if (response !== null) responses.push(response);
    }
    if (responses.length === 0) {
      sendRaw(ctx.res, 202, Buffer.alloc(0), 'application/json', headers);
      return true;
    }
    sendJson(ctx.res, 200, responses, headers);
    return true;
  }
  if (!isRecord(body)) {
    sendJson(ctx.res, 200, err(null, -32600, 'Invalid Request'), headers);
    return true;
  }
  const response = handleRequest(ctx, body);
  if (response === null) {
    // Notification (e.g. notifications/initialized): 202, no body.
    sendRaw(ctx.res, 202, Buffer.alloc(0), 'application/json', headers);
    return true;
  }
  sendJson(ctx.res, 200, response, headers);
  return true;
}
