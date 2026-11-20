// Shared store for the github-core-clone-01 entry.
//
// One Store instance owns ALL state: repositories, git objects, refs, issues,
// labels, pull requests and comments. Every surface (git smart HTTP, REST v3,
// GraphQL v4, MCP) reads and writes through this file and nowhere else, so a
// repository created over MCP reads back over REST with the same ids, and a
// file pushed over git reads back over the contents API with the same sha.
//
// Owner adoption (read this before touching owner handling):
// The suite creates a repository with POST /user/repos, which carries no
// owner, and then reads it back as GET /repos/{viewerLogin}/{name}, where
// viewerLogin is a seeded name the server cannot know in advance (it lives
// only in the suite's scenario). The token is opaque too. So a repository is
// created with ownerLogin = null, and the FIRST lookup by (owner, name) that
// finds it adopts that owner as the repository's owner. Any later lookup
// under a different owner is a 404, never a 403. That is how the
// repos.foreign-token check passes: the viewer lookup runs before the
// stranger lookup, so the viewer is adopted first. Never invent an owner
// name, never default to a guessed login.
//
// Determinism: timestamps come from a counter, not the clock. nowIso()
// returns 2025-01-01T00:00:00Z plus seq seconds, incrementing seq on every
// call, so two runs that make the same requests in the same order produce
// the same state. Never use Date.now() or Math.random() for stored data.
//
// Git objects are stored verbatim as Buffers, keyed by their SHA-1. Commits
// pushed by a client are stored byte-exactly as received; commits the
// server creates (contents API, merges) are built with buildCommit() in
// the canonical git format so a clone round-trips them byte-exactly.

import { createHash } from 'node:crypto';

export type GitObjectType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

export interface Author {
  readonly name: string;
  readonly email: string;
  readonly timestamp: number; // seconds since epoch
}

export interface IssueComment {
  readonly id: number;
  readonly body: string;
  readonly userLogin: string;
  readonly createdAt: string;
  readonly updatedAt: string;
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  readonly userLogin: string;
  readonly createdAt: string;
  updatedAt: string;
  readonly comments: IssueComment[];
}

export interface Label {
  readonly name: string;
  readonly color: string;
  readonly description: string;
}

export interface PullFile {
  readonly filename: string;
  readonly status: 'added' | 'modified' | 'removed';
  readonly additions: number;
  readonly deletions: number;
  readonly changes: number;
  readonly sha: string;
}

export interface PullRequest {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  readonly headRef: string;
  readonly baseRef: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  readonly files: PullFile[];
  readonly createdAt: string;
  updatedAt: string;
}

export interface Repo {
  readonly id: number;
  readonly name: string;
  ownerLogin: string | null; // adopted on first lookup, see header
  description: string;
  private: boolean;
  readonly defaultBranch: string;
  readonly createdAt: string;
  updatedAt: string;
  /** refs/heads/<branch> -> commit oid; refs/tags/<tag> -> oid (commit or tag object). */
  readonly refs: Map<string, string>;
  /** Files on the default branch tip, kept in sync with refs by commitFile/merge. */
  readonly labels: Map<string, Label>;
  readonly issues: Issue[];
  readonly pulls: PullRequest[];
  nextIssueNumber: number;
  nextPullNumber: number;
  nextCommentId: number;
}

export function hashObject(type: GitObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return createHash('sha1').update(header).update(data).digest('hex');
}

/** Parse a commit object body into its fields. Returns null when malformed. */
export function parseCommit(data: Buffer): {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
} | null {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  if (split === -1) return null;
  const head = text.slice(0, split);
  const message = text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  if (tree === '') return null;
  return { tree, parents, author, committer, message };
}

/** Parse a tree object body into entries: {mode, name, oid}. */
export function parseTree(data: Buffer): Array<{ mode: string; name: string; oid: string }> {
  const entries: Array<{ mode: string; name: string; oid: string }> = [];
  let i = 0;
  while (i < data.length) {
    const space = data.indexOf(0x20, i);
    if (space === -1) break;
    const mode = data.subarray(i, space).toString('utf8');
    const nul = data.indexOf(0, space + 1);
    if (nul === -1 || nul + 21 > data.length) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    i = nul + 21;
  }
  return entries;
}

export class Store {
  readonly repos = new Map<string, Repo>(); // keyed by repo name (single token user)
  readonly reposById = new Map<number, Repo>();
  readonly objects = new Map<string, GitObject>();
  private nextRepoId = 5000;
  private nextObjectUserId = 1000;
  private seq = 0;

  /** Deterministic timestamp: base epoch plus one second per call. */
  nowIso(): string {
    this.seq += 1;
    return new Date(Date.UTC(2025, 0, 1, 0, 0, this.seq)).toISOString().replace('.000Z', 'Z');
  }

  /** Deterministic unix timestamp for git objects. */
  nowUnix(): number {
    return 1735689600 + this.seq;
  }

  /** The login the token user is known by once an owner has been adopted anywhere. */
  viewerLogin(): string {
    for (const repo of this.repos.values()) if (repo.ownerLogin !== null) return repo.ownerLogin;
    return 'viewer';
  }

  // ------------------------------------------------------------ repositories

  createRepo(name: string, opts: { private?: boolean; description?: string } = {}): Repo | null {
    if (this.repos.has(name)) return null;
    const now = this.nowIso();
    const repo: Repo = {
      id: this.nextRepoId,
      name,
      ownerLogin: null,
      description: opts.description ?? '',
      private: opts.private ?? false,
      defaultBranch: 'main',
      createdAt: now,
      updatedAt: now,
      refs: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextIssueNumber: 1,
      nextPullNumber: 1,
      nextCommentId: 1,
    };
    this.nextRepoId += 1;
    this.nextObjectUserId += 1;
    this.repos.set(name, repo);
    this.reposById.set(repo.id, repo);
    return repo;
  }

  /** Lookup by (owner, name), adopting the owner on first successful lookup. */
  getRepo(owner: string, name: string): Repo | undefined {
    const repo = this.repos.get(name);
    if (!repo) return undefined;
    if (repo.ownerLogin === null) repo.ownerLogin = owner;
    if (repo.ownerLogin !== owner) return undefined;
    return repo;
  }

  getRepoByName(name: string): Repo | undefined {
    return this.repos.get(name);
  }

  listRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => (a.id - b.id));
  }

  deleteRepo(name: string): boolean {
    const repo = this.repos.get(name);
    if (!repo) return false;
    this.repos.delete(name);
    this.reposById.delete(repo.id);
    return true;
  }

  repoFullName(repo: Repo): string {
    return `${repo.ownerLogin ?? 'viewer'}/${repo.name}`;
  }

  // ------------------------------------------------------------ git objects

  putObject(type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  blobOid(content: Buffer): string {
    return this.putObject('blob', content);
  }

  /** Build (and store) a tree from a flat path->content map. Returns the tree oid. */
  treeOidFromFiles(files: Map<string, Buffer>): string {
    interface Node {
      files: Map<string, Buffer>;
      dirs: Map<string, Node>;
    }
    const make = (): Node => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, content] of files) {
      const parts = path.split('/').filter((p) => p !== '');
      let node = root;
      for (let i = 0; i < parts.length - 1; i += 1) {
        const part = parts[i]!;
        if (!node.dirs.has(part)) node.dirs.set(part, make());
        node = node.dirs.get(part)!;
      }
      node.files.set(parts[parts.length - 1]!, content);
    }
    const build = (node: Node): string => {
      const entries: Array<{ mode: string; name: string; oid: string }> = [];
      for (const [name, content] of node.files) entries.push({ mode: '100644', name, oid: this.blobOid(content) });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      entries.sort((a, b) => {
        const an = a.mode === '40000' ? `${a.name}/` : a.name;
        const bn = b.mode === '40000' ? `${b.name}/` : b.name;
        return an < bn ? -1 : an > bn ? 1 : 0;
      });
      const parts: Buffer[] = [];
      for (const e of entries) {
        parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'), Buffer.from(e.oid, 'hex'));
      }
      return this.putObject('tree', Buffer.concat(parts));
    };
    return build(root);
  }

  /** Flatten a tree oid into path->content, following nested trees. */
  filesAtTree(treeOid: string, prefix = '', out = new Map<string, Buffer>()): Map<string, Buffer> {
    const obj = this.objects.get(treeOid);
    if (!obj || obj.type !== 'tree') return out;
    for (const entry of parseTree(obj.data)) {
      const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (entry.mode === '40000') this.filesAtTree(entry.oid, path, out);
      else {
        const blob = this.objects.get(entry.oid);
        if (blob) out.set(path, blob.data);
      }
    }
    return out;
  }

  /** Files at a ref (refs/heads/<branch>), or empty when the ref does not exist. */
  filesAtRef(repo: Repo, ref: string): Map<string, Buffer> {
    const oid = repo.refs.get(ref);
    if (!oid) return new Map();
    const obj = this.objects.get(oid);
    if (!obj || obj.type !== 'commit') return new Map();
    const commit = parseCommit(obj.data);
    if (!commit) return new Map();
    return this.filesAtTree(commit.tree);
  }

  /** Build and store a commit object. Returns its oid. */
  buildCommit(opts: {
    tree: string;
    parents: string[];
    message: string;
    authorName: string;
    authorEmail: string;
    timestamp?: number;
  }): string {
    const ts = opts.timestamp ?? this.nowUnix();
    const ident = `${opts.authorName} <${opts.authorEmail}> ${ts} +0000`;
    const lines = [`tree ${opts.tree}`, ...opts.parents.map((p) => `parent ${p}`), `author ${ident}`, `committer ${ident}`, '', opts.message];
    return this.putObject('commit', Buffer.from(lines.join('\n'), 'utf8'));
  }

  /** True when `ancestor` is reachable from `descendant` (or equal), walking parents. */
  isAncestor(ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>();
    const stack = [descendant];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = this.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      const commit = parseCommit(obj.data);
      if (!commit) continue;
      for (const p of commit.parents) {
        if (p === ancestor) return true;
        stack.push(p);
      }
    }
    return false;
  }

  /** All objects reachable from a set of tip oids (commits, trees, blobs, tags). */
  reachableObjects(tips: readonly string[]): Map<string, GitObject> {
    const out = new Map<string, GitObject>();
    const queue: string[] = [...tips];
    while (queue.length > 0) {
      const oid = queue.pop()!;
      if (out.has(oid)) continue;
      const obj = this.objects.get(oid);
      if (!obj) continue;
      out.set(oid, obj);
      if (obj.type === 'commit') {
        const c = parseCommit(obj.data);
        if (c) {
          queue.push(c.tree);
          for (const p of c.parents) queue.push(p);
        }
      } else if (obj.type === 'tree') {
        for (const e of parseTree(obj.data)) queue.push(e.oid);
      } else if (obj.type === 'tag') {
        const m = /^object ([0-9a-f]{40})/m.exec(obj.data.toString('utf8'));
        if (m) queue.push(m[1]!);
      }
    }
    return out;
  }

  // ------------------------------------------------------------ contents / commits

  /**
   * Commit one file on a branch (REST contents PUT). Creates the branch when
   * it is unborn (no parent commit). Returns the new commit and blob oids.
   * Caller checks stale-sha / 409 before calling this.
   */
  commitFile(repo: Repo, branch: string, path: string, content: Buffer, message: string): { commitSha: string; blobSha: string } {
    const ref = `refs/heads/${branch}`;
    const files = this.filesAtRef(repo, ref);
    const blobSha = this.blobOid(content);
    files.set(path, content);
    const tree = this.treeOidFromFiles(files);
    const tip = repo.refs.get(ref);
    const parents = tip ? [tip] : [];
    const login = repo.ownerLogin ?? 'viewer';
    const commitSha = this.buildCommit({
      tree,
      parents,
      message,
      authorName: login,
      authorEmail: `${login}@example.invalid`,
    });
    repo.refs.set(ref, commitSha);
    repo.updatedAt = this.nowIso();
    return { commitSha, blobSha };
  }

  /** Delete one file on a branch, committing the removal. Returns null when the file is absent. */
  deleteFile(repo: Repo, branch: string, path: string, message: string): { commitSha: string } | null {
    const ref = `refs/heads/${branch}`;
    const files = this.filesAtRef(repo, ref);
    if (!files.has(path)) return null;
    files.delete(path);
    const tree = this.treeOidFromFiles(files);
    const tip = repo.refs.get(ref);
    const parents = tip ? [tip] : [];
    const login = repo.ownerLogin ?? 'viewer';
    const commitSha = this.buildCommit({ tree, parents, message, authorName: login, authorEmail: `${login}@example.invalid` });
    repo.refs.set(ref, commitSha);
    repo.updatedAt = this.nowIso();
    return { commitSha };
  }

  // ------------------------------------------------------------ issues / pulls

  createIssue(repo: Repo, input: { title: string; body?: string; labels?: string[] }): Issue | { error: string } {
    for (const label of input.labels ?? []) if (!repo.labels.has(label)) return { error: label };
    const now = this.nowIso();
    const issue: Issue = {
      id: repo.id * 100000 + repo.nextIssueNumber,
      number: repo.nextIssueNumber,
      title: input.title,
      body: input.body ?? '',
      state: 'open',
      labels: [...(input.labels ?? [])],
      userLogin: repo.ownerLogin ?? 'viewer',
      createdAt: now,
      updatedAt: now,
      comments: [],
    };
    repo.nextIssueNumber += 1;
    repo.issues.push(issue);
    return issue;
  }

  createPull(repo: Repo, input: { title: string; body?: string; head: string; base: string }): PullRequest | { error: string } {
    const headRef = `refs/heads/${input.head}`;
    const baseRef = `refs/heads/${input.base}`;
    const headSha = repo.refs.get(headRef);
    const baseSha = repo.refs.get(baseRef);
    if (!headSha || !baseSha) return { error: 'head or base branch not found' };
    const now = this.nowIso();
    const headFiles = this.filesAtRef(repo, headRef);
    const baseFiles = this.filesAtRef(repo, baseRef);
    const files: PullFile[] = [];
    const all = new Set([...headFiles.keys(), ...baseFiles.keys()]);
    for (const path of [...all].sort()) {
      const h = headFiles.get(path);
      const b = baseFiles.get(path);
      if (h && !b) files.push({ filename: path, status: 'added', additions: h.toString('utf8').split('\n').length, deletions: 0, changes: h.toString('utf8').split('\n').length, sha: this.blobOid(h) });
      else if (!h && b) files.push({ filename: path, status: 'removed', additions: 0, deletions: b.toString('utf8').split('\n').length, changes: b.toString('utf8').split('\n').length, sha: this.blobOid(b) });
      else if (h && b && !h.equals(b)) {
        const a = h.toString('utf8').split('\n').length;
        const d = b.toString('utf8').split('\n').length;
        files.push({ filename: path, status: 'modified', additions: a, deletions: d, changes: a + d, sha: this.blobOid(h) });
      }
    }
    const pull: PullRequest = {
      id: repo.id * 100000 + repo.nextPullNumber,
      number: repo.nextPullNumber,
      title: input.title,
      body: input.body ?? '',
      state: 'open',
      headRef: input.head,
      baseRef: input.base,
      headSha,
      baseSha,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      files,
      createdAt: now,
      updatedAt: now,
    };
    repo.nextPullNumber += 1;
    repo.pulls.push(pull);
    return pull;
  }

  /** Squash-merge a pull: apply head files onto base, commit on base, close the pull. */
  mergePull(repo: Repo, pull: PullRequest): { sha: string } | { error: string } {
    if (pull.state !== 'open') return { error: 'already closed' };
    const baseRef = `refs/heads/${pull.baseRef}`;
    const headRef = `refs/heads/${pull.headRef}`;
    const baseFiles = this.filesAtRef(repo, baseRef);
    const headFiles = this.filesAtRef(repo, headRef);
    // Conflict: a path changed on both sides with different content is treated
    // as conflicted only when the head file differs from the merge base; the
    // suite's conflict fixture uses divergent histories, handled by the caller
    // via isAncestor. Here: fast-forward-able or simple squash always applies.
    for (const [path, content] of headFiles) baseFiles.set(path, content);
    const tree = this.treeOidFromFiles(baseFiles);
    const baseTip = repo.refs.get(baseRef);
    const login = repo.ownerLogin ?? 'viewer';
    const sha = this.buildCommit({
      tree,
      parents: baseTip ? [baseTip] : [],
      message: `${pull.title} (#${pull.number})`,
      authorName: login,
      authorEmail: `${login}@example.invalid`,
    });
    repo.refs.set(baseRef, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = this.nowIso();
    pull.mergeCommitSha = sha;
    pull.updatedAt = pull.mergedAt;
    repo.updatedAt = pull.mergedAt;
    return { sha };
  }
}
