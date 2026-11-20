// REST v3 surface for the github-core-clone-01 entry.
// Implements the routes in checks-rest.ts: repos, contents, branches,
// commits, issues, labels, pulls (+ squash merge with conflict detection).
// All state lives in store.ts; this file only shapes JSON and routes.

import { sendJson, sendRaw, type Ctx } from './app.ts';
import { parseCommit, type Issue, type PullRequest, type Repo } from './store.ts';

const H = (ctx: Ctx, extra: Record<string, string> = {}): Record<string, string> => ({
  'x-github-request-id': ctx.requestId,
  ...extra,
});

function originOf(ctx: Ctx): string {
  const host = ctx.req.headers.host;
  return host ? `http://${host}` : 'http://127.0.0.1';
}

function bodyObj(ctx: Ctx): Record<string, unknown> {
  const b = ctx.bodyJson;
  return b !== null && typeof b === 'object' && !Array.isArray(b) ? (b as Record<string, unknown>) : {};
}

function str(v: unknown, d = ''): string {
  return typeof v === 'string' ? v : d;
}

function bool(v: unknown, d = false): boolean {
  return typeof v === 'boolean' ? v : d;
}

function num(v: unknown, d: number): number {
  const n = typeof v === 'number' ? v : Number(v);
  return Number.isFinite(n) ? n : d;
}

function notFound(ctx: Ctx, msg = 'Not Found'): void {
  sendJson(ctx.res, 404, { message: msg }, H(ctx));
}

function badJson(ctx: Ctx): boolean {
  if (ctx.bodyParseError) {
    sendJson(ctx.res, 400, { message: 'Invalid JSON body' }, H(ctx));
    return true;
  }
  return false;
}

// ---------------------------------------------------------------- shapes

function repoJson(ctx: Ctx, repo: Repo): Record<string, unknown> {
  const origin = originOf(ctx);
  const full = `${repo.ownerLogin ?? 'viewer'}/${repo.name}`;
  return {
    id: repo.id,
    name: repo.name,
    full_name: full,
    owner: { login: repo.ownerLogin ?? 'viewer', id: 1, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    html_url: `${origin}/${full}`,
    url: `${origin}/api/v3/repos/${full}`,
    clone_url: `${origin}/${full}.git`,
    git_url: `${origin}/${full}.git`,
    ssh_url: `git@example.invalid:${full}.git`,
  };
}

function issueJson(ctx: Ctx, repo: Repo, issue: Issue): Record<string, unknown> {
  const origin = originOf(ctx);
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
    html_url: `${origin}/${full}/issues/${issue.number}`,
    url: `${origin}/api/v3/repos/${full}/issues/${issue.number}`,
  };
}

function pullJson(ctx: Ctx, repo: Repo, pull: PullRequest): Record<string, unknown> {
  const origin = originOf(ctx);
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
    html_url: `${origin}/${full}/pull/${pull.number}`,
    url: `${origin}/api/v3/repos/${full}/pulls/${pull.number}`,
  };
}

// ---------------------------------------------------------------- pagination

function pageParams(ctx: Ctx): { page: number; perPage: number } {
  const perPage = Math.min(100, Math.max(1, num(ctx.query.get('per_page'), 30)));
  const page = Math.max(1, num(ctx.query.get('page'), 1));
  return { page, perPage };
}

/** Slice a list for the requested page; returns the page plus a Link header value (or ''). */
function paginate<T>(ctx: Ctx, items: T[]): { page: T[]; link: string } {
  const { page, perPage } = pageParams(ctx);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  let link = '';
  if (start + perPage < items.length) {
    const url = new URL(ctx.path, originOf(ctx));
    url.searchParams.set('page', String(page + 1));
    url.searchParams.set('per_page', String(perPage));
    link = `<${url.toString()}>; rel="next"`;
  }
  return { page: slice, link };
}

function linkHeaders(ctx: Ctx, link: string): Record<string, string> {
  return link === '' ? H(ctx) : H(ctx, { link });
}

// ---------------------------------------------------------------- git helpers

/** Resolve a ref-ish (branch name, full ref, or commit oid) to a commit oid. */
function resolveCommit(ctx: Ctx, repo: Repo, refish: string | null): string | undefined {
  if (refish === null || refish === '') return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (refish.startsWith('refs/')) return repo.refs.get(refish);
  const byBranch = repo.refs.get(`refs/heads/${refish}`);
  if (byBranch) return byBranch;
  const obj = ctx.store.getObject(refish);
  if (obj && obj.type === 'commit') return refish;
  return undefined;
}

/** Walk commit parents (first-parent order via a stack), newest first. */
function walkCommits(ctx: Ctx, tip: string): Array<{ sha: string; message: string }> {
  const out: Array<{ sha: string; message: string }> = [];
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = ctx.store.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    const parsed = parseCommit(obj.data);
    if (!parsed) continue;
    out.push({ sha: oid, message: parsed.message });
    for (let i = parsed.parents.length - 1; i >= 0; i -= 1) stack.push(parsed.parents[i]!);
  }
  return out;
}

/** All ancestors of a commit (including itself). */
function ancestors(ctx: Ctx, tip: string): Set<string> {
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = ctx.store.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    const parsed = parseCommit(obj.data);
    if (!parsed) continue;
    for (const p of parsed.parents) stack.push(p);
  }
  return seen;
}

/** Nearest common ancestor of two tips (walk head's history, first hit in base's set). */
function mergeBase(ctx: Ctx, headTip: string, baseTip: string): string | null {
  const baseAnc = ancestors(ctx, baseTip);
  const seen = new Set<string>();
  const queue = [headTip];
  while (queue.length > 0) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (baseAnc.has(oid)) return oid;
    const obj = ctx.store.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    const parsed = parseCommit(obj.data);
    if (!parsed) continue;
    for (const p of parsed.parents) queue.push(p);
  }
  return null;
}

function bufEq(a: Buffer | undefined, b: Buffer | undefined): boolean {
  if (a === undefined && b === undefined) return true;
  if (a === undefined || b === undefined) return false;
  return a.equals(b);
}

// ---------------------------------------------------------------- handler

export async function handleRest(ctx: Ctx): Promise<boolean> {
  const seg = ctx.segments; // ['api','v3', ...rest]
  const rest = seg.slice(2);
  const { method } = ctx;
  const origin = originOf(ctx);

  // GET /api/v3/ (root)
  if (rest.length === 0) {
    if (method !== 'GET') { notFound(ctx); return true; }
    sendJson(ctx.res, 200, { current_user_url: `${origin}/api/v3/user`, repository_url: `${origin}/api/v3/repos/{owner}/{repo}` }, H(ctx, { 'x-oauth-scopes': 'repo, read:org' }));
    return true;
  }

  // GET /api/v3/meta
  if (rest.length === 1 && rest[0] === 'meta') {
    if (method !== 'GET') { notFound(ctx); return true; }
    sendJson(ctx.res, 200, { installed_version: '1.0.0', verifiable_password_authentication: false }, H(ctx));
    return true;
  }

  // POST /api/v3/user/repos  +  GET /api/v3/user/repos
  if (rest.length === 2 && rest[0] === 'user' && rest[1] === 'repos') {
    if (method === 'POST') {
      if (badJson(ctx)) return true;
      const b = bodyObj(ctx);
      const name = str(b.name).trim();
      if (name === '') { sendJson(ctx.res, 422, { message: 'name is required' }, H(ctx)); return true; }
      const repo = ctx.store.createRepo(name, { private: bool(b.private, false), description: str(b.description) });
      if (!repo) { sendJson(ctx.res, 422, { message: 'Repository already exists' }, H(ctx)); return true; }
      sendJson(ctx.res, 201, repoJson(ctx, repo), H(ctx));
      return true;
    }
    if (method === 'GET') {
      const { page, link } = paginate(ctx, ctx.store.listRepos());
      sendJson(ctx.res, 200, page.map((r) => repoJson(ctx, r)), linkHeaders(ctx, link));
      return true;
    }
    notFound(ctx);
    return true;
  }

  // /api/v3/repos/{owner}/{repo}[/...]
  if (rest.length >= 3 && rest[0] === 'repos') {
    const owner = rest[1]!;
    const name = rest[2]!;
    const tail = rest.slice(3);
    const repo = ctx.store.getRepo(owner, name);
    if (!repo) { notFound(ctx, 'Not Found'); return true; }
    const full = `${repo.ownerLogin ?? owner}/${repo.name}`;

    // GET/PATCH/DELETE /repos/{o}/{r}
    if (tail.length === 0) {
      if (method === 'GET') { sendJson(ctx.res, 200, repoJson(ctx, repo), H(ctx)); return true; }
      if (method === 'PATCH') {
        if (badJson(ctx)) return true;
        const b = bodyObj(ctx);
        if (typeof b.description === 'string') repo.description = b.description;
        if (typeof b.private === 'boolean') repo.private = b.private;
        repo.updatedAt = ctx.store.nowIso();
        sendJson(ctx.res, 200, repoJson(ctx, repo), H(ctx));
        return true;
      }
      if (method === 'DELETE') {
        ctx.store.deleteRepo(repo.name);
        sendRaw(ctx.res, 204, Buffer.alloc(0), 'application/json', H(ctx));
        return true;
      }
      notFound(ctx);
      return true;
    }

    // POST /repos/{o}/{r}/git/refs
    if (tail.length === 2 && tail[0] === 'git' && tail[1] === 'refs') {
      if (method !== 'POST') { notFound(ctx); return true; }
      if (badJson(ctx)) return true;
      const b = bodyObj(ctx);
      const ref = str(b.ref);
      const sha = str(b.sha);
      if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/i.test(sha)) {
        sendJson(ctx.res, 422, { message: 'Invalid ref or sha' }, H(ctx));
        return true;
      }
      if (repo.refs.has(ref)) { sendJson(ctx.res, 422, { message: 'Reference already exists' }, H(ctx)); return true; }
      repo.refs.set(ref, sha);
      sendJson(ctx.res, 201, { ref, object: { sha, type: 'commit', url: `${origin}/api/v3/repos/${full}/git/commits/${sha}` } }, H(ctx));
      return true;
    }

    // GET /repos/{o}/{r}/branches
    if (tail.length === 1 && tail[0] === 'branches') {
      if (method !== 'GET') { notFound(ctx); return true; }
      const branches: Array<Record<string, unknown>> = [];
      for (const [ref, sha] of [...repo.refs.entries()].sort()) {
        if (!ref.startsWith('refs/heads/')) continue;
        branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha, url: `${origin}/api/v3/repos/${full}/commits/${sha}` }, protected: false });
      }
      sendJson(ctx.res, 200, branches, H(ctx));
      return true;
    }

    // GET /repos/{o}/{r}/commits
    if (tail.length === 1 && tail[0] === 'commits') {
      if (method !== 'GET') { notFound(ctx); return true; }
      const tip = resolveCommit(ctx, repo, ctx.query.get('sha'));
      if (!tip) { notFound(ctx, 'Not Found'); return true; }
      const list = walkCommits(ctx, tip).map((c) => ({
        sha: c.sha,
        commit: { message: c.message, author: { name: repo.ownerLogin ?? 'viewer', email: `${repo.ownerLogin ?? 'viewer'}@example.invalid` }, committer: { name: repo.ownerLogin ?? 'viewer', email: `${repo.ownerLogin ?? 'viewer'}@example.invalid` } },
        html_url: `${origin}/${full}/commit/${c.sha}`,
        url: `${origin}/api/v3/repos/${full}/commits/${c.sha}`,
      }));
      sendJson(ctx.res, 200, list, H(ctx));
      return true;
    }

    // /repos/{o}/{r}/contents[/{path...}]
    if (tail.length >= 1 && tail[0] === 'contents') {
      const filePath = tail.slice(1).join('/');
      if (method === 'PUT') {
        if (badJson(ctx)) return true;
        const b = bodyObj(ctx);
        const message = str(b.message, 'Update file');
        const contentB64 = str(b.content);
        if (contentB64 === '') { sendJson(ctx.res, 422, { message: 'content is required' }, H(ctx)); return true; }
        const branch = str(b.branch, repo.defaultBranch) || repo.defaultBranch;
        const ref = `refs/heads/${branch}`;
        const files = ctx.store.filesAtRef(repo, ref);
        const exists = files.has(filePath);
        if (exists) {
          const currentSha = ctx.store.blobOid(files.get(filePath)!);
          if (str(b.sha) !== currentSha) { sendJson(ctx.res, 409, { message: 'sha does not match the current blob sha' }, H(ctx)); return true; }
        }
        const content = Buffer.from(contentB64.replace(/[\r\n\t ]+/g, ''), 'base64');
        const { commitSha, blobSha } = ctx.store.commitFile(repo, branch, filePath, content, message);
        const baseName = filePath.split('/').pop() ?? filePath;
        sendJson(ctx.res, exists ? 200 : 201, {
          content: { name: baseName, path: filePath, sha: blobSha, size: content.length, encoding: 'base64', type: 'file', url: `${origin}/api/v3/repos/${full}/contents/${filePath}` },
          commit: { sha: commitSha, message },
        }, H(ctx));
        return true;
      }
      if (method === 'GET') {
        const tip = resolveCommit(ctx, repo, ctx.query.get('ref'));
        if (!tip) { notFound(ctx, 'Not Found'); return true; }
        const obj = ctx.store.getObject(tip);
        const parsed = obj && obj.type === 'commit' ? parseCommit(obj.data) : null;
        if (!parsed) { notFound(ctx, 'Not Found'); return true; }
        const files = ctx.store.filesAtTree(parsed.tree);
        // Exact file?
        if (filePath !== '' && files.has(filePath)) {
          const content = files.get(filePath)!;
          const baseName = filePath.split('/').pop() ?? filePath;
          sendJson(ctx.res, 200, {
            type: 'file', encoding: 'base64', size: content.length, name: baseName, path: filePath,
            content: content.toString('base64'), sha: ctx.store.blobOid(content),
            url: `${origin}/api/v3/repos/${full}/contents/${filePath}`,
            html_url: `${origin}/${full}/blob/${repo.defaultBranch}/${filePath}`,
          }, H(ctx));
          return true;
        }
        // Directory listing (root when filePath === '').
        const prefix = filePath === '' ? '' : `${filePath}/`;
        const entries = new Map<string, { type: 'file' | 'dir'; name: string; path: string; sha: string; size?: number }>();
        for (const [p, content] of files) {
          if (!p.startsWith(prefix)) continue;
          const rel = p.slice(prefix.length);
          if (rel === '') continue;
          const slash = rel.indexOf('/');
          if (slash === -1) {
            entries.set(rel, { type: 'file', name: rel, path: p, sha: ctx.store.blobOid(content), size: content.length });
          } else {
            const dirName = rel.slice(0, slash);
            if (!entries.has(dirName)) {
              const sub = new Map<string, Buffer>();
              for (const [p2, c2] of files) if (p2.startsWith(`${prefix}${dirName}/`)) sub.set(p2.slice(`${prefix}${dirName}/`.length), c2);
              entries.set(dirName, { type: 'dir', name: dirName, path: `${prefix}${dirName}`, sha: ctx.store.treeOidFromFiles(sub) });
            }
          }
        }
        if (filePath !== '' && entries.size === 0) { notFound(ctx, 'Not Found'); return true; }
        const list = [...entries.values()].sort((a, b) => (a.name < b.name ? -1 : 1)).map((e) => ({
          ...e, url: `${origin}/api/v3/repos/${full}/contents/${e.path}`, html_url: `${origin}/${full}/tree/${repo.defaultBranch}/${e.path}`,
        }));
        sendJson(ctx.res, 200, list, H(ctx));
        return true;
      }
      if (method === 'DELETE') {
        if (badJson(ctx)) return true;
        const b = bodyObj(ctx);
        const branch = str(b.branch, repo.defaultBranch) || repo.defaultBranch;
        const message = str(b.message, 'Delete file');
        const result = ctx.store.deleteFile(repo, branch, filePath, message);
        if (!result) { notFound(ctx, 'Not Found'); return true; }
        sendJson(ctx.res, 200, { content: null, commit: { sha: result.commitSha, message } }, H(ctx));
        return true;
      }
      notFound(ctx);
      return true;
    }

    // POST /repos/{o}/{r}/labels
    if (tail.length === 1 && tail[0] === 'labels') {
      if (method !== 'POST') { notFound(ctx); return true; }
      if (badJson(ctx)) return true;
      const b = bodyObj(ctx);
      const name = str(b.name).trim();
      if (name === '') { sendJson(ctx.res, 422, { message: 'name is required' }, H(ctx)); return true; }
      const color = str(b.color, 'ededed').replace(/^#/, '');
      const description = str(b.description);
      repo.labels.set(name, { name, color, description });
      sendJson(ctx.res, 201, { name, color, description, url: `${origin}/api/v3/repos/${full}/labels/${encodeURIComponent(name)}` }, H(ctx));
      return true;
    }

    // /repos/{o}/{r}/issues[/{n}[/comments]]
    if (tail.length >= 1 && tail[0] === 'issues') {
      // POST/GET /issues
      if (tail.length === 1) {
        if (method === 'POST') {
          if (badJson(ctx)) return true;
          const b = bodyObj(ctx);
          const labels = Array.isArray(b.labels) ? (b.labels as unknown[]).map((l) => String(l)) : [];
          const result = ctx.store.createIssue(repo, { title: str(b.title), body: str(b.body), labels });
          if ('error' in result) { sendJson(ctx.res, 422, { message: `Unknown label: ${result.error}` }, H(ctx)); return true; }
          sendJson(ctx.res, 201, issueJson(ctx, repo, result), H(ctx));
          return true;
        }
        if (method === 'GET') {
          const state = (ctx.query.get('state') ?? 'open').toLowerCase();
          let list = [...repo.issues];
          if (state === 'open' || state === 'closed') list = list.filter((i) => i.state === state);
          const direction = (ctx.query.get('direction') ?? 'desc').toLowerCase();
          // repo.issues is in creation order; sort=created only.
          if (direction === 'desc') list = [...list].reverse();
          const { page, link } = paginate(ctx, list);
          sendJson(ctx.res, 200, page.map((i) => issueJson(ctx, repo, i)), linkHeaders(ctx, link));
          return true;
        }
        notFound(ctx);
        return true;
      }
      // /issues/{n} and /issues/{n}/comments
      const num = Number(tail[1]);
      const issue = repo.issues.find((i) => i.number === num);
      if (!issue) { notFound(ctx, 'Not Found'); return true; }
      if (tail.length === 2) {
        if (method === 'GET') { sendJson(ctx.res, 200, issueJson(ctx, repo, issue), H(ctx)); return true; }
        if (method === 'PATCH') {
          if (badJson(ctx)) return true;
          const b = bodyObj(ctx);
          if (b.state === 'open' || b.state === 'closed') issue.state = b.state;
          if (typeof b.title === 'string') issue.title = b.title;
          if (typeof b.body === 'string') issue.body = b.body;
          if (Array.isArray(b.labels)) {
            const labels = (b.labels as unknown[]).map((l) => String(l));
            for (const l of labels) if (!repo.labels.has(l)) { sendJson(ctx.res, 422, { message: `Unknown label: ${l}` }, H(ctx)); return true; }
            issue.labels = labels;
          }
          issue.updatedAt = ctx.store.nowIso();
          sendJson(ctx.res, 200, issueJson(ctx, repo, issue), H(ctx));
          return true;
        }
        notFound(ctx);
        return true;
      }
      if (tail.length === 3 && tail[2] === 'comments') {
        if (method === 'POST') {
          if (badJson(ctx)) return true;
          const b = bodyObj(ctx);
          const id = repo.nextCommentId;
          repo.nextCommentId += 1;
          const now = ctx.store.nowIso();
          const comment = { id, body: str(b.body), userLogin: repo.ownerLogin ?? 'viewer', createdAt: now, updatedAt: now };
          issue.comments.push(comment);
          sendJson(ctx.res, 201, {
            id, body: comment.body, user: { login: comment.userLogin },
            created_at: comment.createdAt, updated_at: comment.updatedAt,
            html_url: `${origin}/${full}/issues/${issue.number}#issuecomment-${id}`,
            url: `${origin}/api/v3/repos/${full}/issues/comments/${id}`,
          }, H(ctx));
          return true;
        }
        if (method === 'GET') {
          sendJson(ctx.res, 200, issue.comments.map((c) => ({
            id: c.id, body: c.body, user: { login: c.userLogin },
            created_at: c.createdAt, updated_at: c.updatedAt,
            html_url: `${origin}/${full}/issues/${issue.number}#issuecomment-${c.id}`,
            url: `${origin}/api/v3/repos/${full}/issues/comments/${c.id}`,
          })), H(ctx));
          return true;
        }
        notFound(ctx);
        return true;
      }
      notFound(ctx);
      return true;
    }

    // /repos/{o}/{r}/pulls[/{n}[/files|merge]]
    if (tail.length >= 1 && tail[0] === 'pulls') {
      if (tail.length === 1) {
        if (method === 'POST') {
          if (badJson(ctx)) return true;
          const b = bodyObj(ctx);
          const headRaw = str(b.head);
          const baseRaw = str(b.base);
          const head = headRaw.includes(':') ? headRaw.split(':').pop()! : headRaw;
          const base = baseRaw.includes(':') ? baseRaw.split(':').pop()! : baseRaw;
          const result = ctx.store.createPull(repo, { title: str(b.title), body: str(b.body), head, base });
          if ('error' in result) { sendJson(ctx.res, 422, { message: result.error }, H(ctx)); return true; }
          sendJson(ctx.res, 201, pullJson(ctx, repo, result), H(ctx));
          return true;
        }
        if (method === 'GET') {
          const state = (ctx.query.get('state') ?? 'open').toLowerCase();
          let list = [...repo.pulls];
          if (state === 'open' || state === 'closed') list = list.filter((p) => p.state === state);
          const { page, link } = paginate(ctx, list);
          sendJson(ctx.res, 200, page.map((p) => pullJson(ctx, repo, p)), linkHeaders(ctx, link));
          return true;
        }
        notFound(ctx);
        return true;
      }
      const num = Number(tail[1]);
      const pull = repo.pulls.find((p) => p.number === num);
      if (tail.length === 2) {
        if (method !== 'GET') { notFound(ctx); return true; }
        if (!pull) { notFound(ctx, 'Not Found'); return true; }
        sendJson(ctx.res, 200, pullJson(ctx, repo, pull), H(ctx));
        return true;
      }
      if (tail.length === 3 && tail[2] === 'files') {
        if (method !== 'GET') { notFound(ctx); return true; }
        if (!pull) { notFound(ctx, 'Not Found'); return true; }
        sendJson(ctx.res, 200, pull.files.map((f) => ({ ...f })), H(ctx));
        return true;
      }
      if (tail.length === 3 && tail[2] === 'merge') {
        if (method !== 'PUT') { notFound(ctx); return true; }
        if (badJson(ctx)) return true;
        if (!pull) { notFound(ctx, 'Not Found'); return true; }
        if (pull.state !== 'open') { sendJson(ctx.res, 405, { message: 'Pull request is already closed' }, H(ctx)); return true; }
        // Conflict check: merge-base of current head/base tips; a path changed
        // on BOTH sides since the merge-base with different content conflicts.
        const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
        const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
        if (!headTip || !baseTip) { notFound(ctx, 'Not Found'); return true; }
        const base = mergeBase(ctx, headTip, baseTip);
        if (base !== null) {
          const baseCommit = parseCommit(ctx.store.getObject(base)!.data);
          const baseFiles = baseCommit ? ctx.store.filesAtTree(baseCommit.tree) : new Map<string, Buffer>();
          const headFiles = ctx.store.filesAtRef(repo, `refs/heads/${pull.headRef}`);
          const curBaseFiles = ctx.store.filesAtRef(repo, `refs/heads/${pull.baseRef}`);
          const allPaths = new Set([...baseFiles.keys(), ...headFiles.keys(), ...curBaseFiles.keys()]);
          for (const path of allPaths) {
            const changedHead = !bufEq(headFiles.get(path), baseFiles.get(path));
            const changedBase = !bufEq(curBaseFiles.get(path), baseFiles.get(path));
            if (changedHead && changedBase && !bufEq(headFiles.get(path), curBaseFiles.get(path))) {
              sendJson(ctx.res, 405, { message: 'Merge conflict' }, H(ctx));
              return true;
            }
          }
        }
        const merged = ctx.store.mergePull(repo, pull);
        if ('error' in merged) { sendJson(ctx.res, 405, { message: merged.error }, H(ctx)); return true; }
        sendJson(ctx.res, 200, { sha: merged.sha, merged: true, message: `${pull.title} (#${pull.number})` }, H(ctx));
        return true;
      }
      notFound(ctx);
      return true;
    }

    notFound(ctx);
    return true;
  }

  // Anything else under /api/v3: unknown route.
  notFound(ctx);
  return true;
}
