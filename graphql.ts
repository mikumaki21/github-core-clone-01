// GraphQL v4 surface for the github-core-clone-01 entry.
//
// Minimal hand-written GraphQL: a small tokenizer + recursive-descent parser
// (no dependencies) plus resolvers over the shared Store. It implements
// exactly the documents gh / github-mcp-server send (see the kit's
// checks-graphql.ts): viewer, repository (+ issue/issues/pullRequest/
// pullRequests), node(id:), __type/__schema introspection, the createIssue /
// closeIssue / createPullRequest / mergePullRequest / addComment mutations,
// and JSON-array operation batching. Unknown fields are GraphQL errors on
// HTTP 200, never HTTP 400s.

import { sendJson, type Ctx } from './app.ts';
import type { Issue, PullRequest, Repo, Store } from './store.ts';

export async function handleGraphql(ctx: Ctx): Promise<boolean> {
  if (ctx.path !== '/api/graphql') return false;
  const headers = { 'x-github-request-id': ctx.requestId };
  if (ctx.method !== 'POST') {
    sendJson(ctx.res, 404, { message: 'Not Found' }, headers);
    return true;
  }
  if (ctx.bodyParseError) {
    sendJson(ctx.res, 400, { message: 'Invalid JSON body' }, headers);
    return true;
  }
  const body = ctx.bodyJson;
  if (Array.isArray(body)) {
    const results = body.map((entry) => runOperation(ctx.store, entry));
    sendJson(ctx.res, 200, results, headers);
    return true;
  }
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    sendJson(ctx.res, 400, { message: 'Expected a JSON object with query and variables' }, headers);
    return true;
  }
  sendJson(ctx.res, 200, runOperation(ctx.store, body as Record<string, unknown>), headers);
  return true;
}

// ---------------------------------------------------------------------------
// Node ids: base64("<Type>:<numeric id>"), so node(id:) round-trips.
// ---------------------------------------------------------------------------

function nodeId(type: string, num: number): string {
  return Buffer.from(`${type}:${num}`, 'utf8').toString('base64');
}

function decodeNodeId(id: unknown): { type: string; num: number } | null {
  if (typeof id !== 'string' || id === '') return null;
  let text: string;
  try {
    text = Buffer.from(id, 'base64').toString('utf8');
  } catch {
    return null;
  }
  const m = /^([A-Za-z]+):(\d+)$/.exec(text);
  if (!m) return null;
  return { type: m[1]!, num: Number(m[2]) };
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'number'; value: string }
  | { kind: 'punct'; value: string }
  | { kind: 'var'; value: string };

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && src.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('!&():=@[]{|}'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '$') {
      const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(src.slice(i + 1));
      if (!m) throw new Error('Invalid variable name');
      tokens.push({ kind: 'var', value: m[0] });
      i += 1 + m[0].length;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1]!;
          if (esc === 'u' && j + 5 < src.length) {
            out += String.fromCharCode(parseInt(src.slice(j + 2, j + 6), 16));
            j += 6;
          } else {
            out += { n: '\n', t: '\t', r: '\r', '"': '"', '\\': '\\', '/': '/' }[esc] ?? esc;
            j += 2;
          }
        } else {
          out += src[j];
          j += 1;
        }
      }
      if (j >= src.length) throw new Error('Unterminated string');
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      const m = /^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(src.slice(i));
      if (!m) throw new Error(`Invalid number at offset ${i}`);
      tokens.push({ kind: 'number', value: m[0] });
      i += m[0].length;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(src.slice(i));
      if (!m) throw new Error(`Invalid name at offset ${i}`);
      tokens.push({ kind: 'name', value: m[0] });
      i += m[0].length;
      continue;
    }
    throw new Error(`Unexpected character ${JSON.stringify(ch)} at offset ${i}`);
  }
  return tokens;
}

// ---------------------------------------------------------------------------
// Parser (AST)
// ---------------------------------------------------------------------------

type ValueNode =
  | { kind: 'var'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'number'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; items: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

type SelectionNode =
  | { kind: 'field'; alias?: string; name: string; args: Array<{ name: string; value: ValueNode }>; selections?: SelectionNode[] }
  | { kind: 'spread'; name: string }
  | { kind: 'inline'; typeCondition?: string; selections: SelectionNode[] };

interface Operation {
  kind: 'query' | 'mutation';
  name?: string;
  selections: SelectionNode[];
}

interface FragmentDef {
  name: string;
  typeCondition: string;
  selections: SelectionNode[];
}

interface Document {
  operations: Operation[];
  fragments: Map<string, FragmentDef>;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const t = this.tokens[this.pos];
    if (!t) throw new Error('Unexpected end of query');
    this.pos += 1;
    return t;
  }

  private expectPunct(value: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== value) throw new Error(`Expected "${value}" but got ${t.kind} ${t.value}`);
  }

  private expectName(value?: string): string {
    const t = this.next();
    if (t.kind !== 'name') throw new Error(`Expected a name but got ${t.kind} ${t.value}`);
    if (value !== undefined && t.value !== value) throw new Error(`Expected "${value}" but got "${t.value}"`);
    return t.value;
  }

  private skipBalanced(open: string, close: string): void {
    // The open punct was already consumed by the caller, so depth starts at 1.
    let depth = 1;
    for (;;) {
      const t = this.next();
      if (t.kind === 'punct' && t.value === open) depth += 1;
      else if (t.kind === 'punct' && t.value === close) {
        depth -= 1;
        if (depth === 0) return;
      }
    }
  }

  parseDocument(): Document {
    const operations: Operation[] = [];
    const fragments = new Map<string, FragmentDef>();
    while (this.pos < this.tokens.length) {
      const t = this.peek()!;
      if (t.kind === 'punct' && t.value === '{') {
        operations.push({ kind: 'query', selections: this.parseSelectionSet() });
      } else if (t.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
        const kind = this.next().value as 'query' | 'mutation';
        let name: string | undefined;
        const n = this.peek();
        if (n && n.kind === 'name') name = this.next().value;
        const p = this.peek();
        if (p && p.kind === 'punct' && p.value === '(') {
          this.next();
          this.skipBalanced('(', ')');
        }
        operations.push({ kind, name, selections: this.parseSelectionSet() });
      } else if (t.kind === 'name' && t.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName('on');
        const typeCondition = this.expectName();
        fragments.set(name, { name, typeCondition, selections: this.parseSelectionSet() });
      } else {
        throw new Error(`Unexpected token ${t.kind} ${t.value} at top level`);
      }
    }
    if (operations.length === 0) throw new Error('Document contains no operation');
    return { operations, fragments };
  }

  parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    for (;;) {
      const t = this.peek();
      if (!t) throw new Error('Unterminated selection set');
      if (t.kind === 'punct' && t.value === '}') {
        this.next();
        return selections;
      }
      selections.push(this.parseSelection());
    }
  }

  private parseSelection(): SelectionNode {
    const t = this.peek()!;
    if (t.kind === 'punct' && t.value === '...') {
      this.next();
      const n = this.peek();
      if (n && n.kind === 'name' && n.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        return { kind: 'inline', typeCondition, selections: this.parseSelectionSet() };
      }
      if (n && n.kind === 'name') {
        return { kind: 'spread', name: this.next().value };
      }
      return { kind: 'inline', selections: this.parseSelectionSet() };
    }
    if (t.kind !== 'name') throw new Error(`Expected a field name but got ${t.kind} ${t.value}`);
    let name = this.next().value;
    let alias: string | undefined;
    const colon = this.peek();
    if (colon && colon.kind === 'punct' && colon.value === ':') {
      this.next();
      alias = name;
      name = this.expectName();
    }
    const args: Array<{ name: string; value: ValueNode }> = [];
    const p = this.peek();
    if (p && p.kind === 'punct' && p.value === '(') {
      this.next();
      for (;;) {
        const q = this.peek();
        if (!q) throw new Error('Unterminated argument list');
        if (q.kind === 'punct' && q.value === ')') {
          this.next();
          break;
        }
        const argName = this.expectName();
        this.expectPunct(':');
        args.push({ name: argName, value: this.parseValue() });
      }
    }
    // Directives (@...) — skip name and optional (...) args.
    for (;;) {
      const d = this.peek();
      if (d && d.kind === 'punct' && d.value === '@') {
        this.next();
        this.expectName();
        const a = this.peek();
        if (a && a.kind === 'punct' && a.value === '(') {
          this.next();
          this.skipBalanced('(', ')');
        }
      } else break;
    }
    const s = this.peek();
    if (s && s.kind === 'punct' && s.value === '{') {
      return { kind: 'field', alias, name, args, selections: this.parseSelectionSet() };
    }
    return { kind: 'field', alias, name, args };
  }

  private parseValue(): ValueNode {
    const t = this.next();
    if (t.kind === 'var') return { kind: 'var', name: t.value };
    if (t.kind === 'string') return { kind: 'string', value: t.value };
    if (t.kind === 'number') return { kind: 'number', value: Number(t.value) };
    if (t.kind === 'name') {
      if (t.value === 'true') return { kind: 'boolean', value: true };
      if (t.value === 'false') return { kind: 'boolean', value: false };
      if (t.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: t.value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const items: ValueNode[] = [];
      for (;;) {
        const q = this.peek();
        if (!q) throw new Error('Unterminated list');
        if (q.kind === 'punct' && q.value === ']') {
          this.next();
          return { kind: 'list', items };
        }
        items.push(this.parseValue());
      }
    }
    if (t.kind === 'punct' && t.value === '{') {
      const fields: Array<{ name: string; value: ValueNode }> = [];
      for (;;) {
        const q = this.peek();
        if (!q) throw new Error('Unterminated input object');
        if (q.kind === 'punct' && q.value === '}') {
          this.next();
          return { kind: 'object', fields };
        }
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue() });
      }
    }
    throw new Error(`Unexpected value token ${t.kind} ${t.value}`);
  }
}

// ---------------------------------------------------------------------------
// Schema table (introspection + field validation)
// ---------------------------------------------------------------------------

interface TypeDef {
  kind: 'OBJECT' | 'ENUM';
  fields: string[];
  enumValues: string[];
}

const TYPE_DEFS: Record<string, TypeDef> = {
  Query: { kind: 'OBJECT', fields: ['viewer', 'repository', 'node', '__type', '__schema'], enumValues: [] },
  Mutation: { kind: 'OBJECT', fields: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'], enumValues: [] },
  Repository: {
    kind: 'OBJECT',
    fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'description', 'url', 'createdAt', 'updatedAt', 'isPrivate', 'defaultBranch'],
    enumValues: [],
  },
  Issue: {
    kind: 'OBJECT',
    fields: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'url', 'createdAt', 'updatedAt', 'labels', 'author', 'comments', 'closed', 'locked'],
    enumValues: [],
  },
  PullRequest: {
    kind: 'OBJECT',
    fields: ['id', 'databaseId', 'number', 'title', 'body', 'state', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'mergeable', 'merged', 'mergedAt', 'files', 'createdAt', 'updatedAt', 'url', 'author'],
    enumValues: [],
  },
  User: { kind: 'OBJECT', fields: ['id', 'login', 'name', 'url', 'avatarUrl'], enumValues: [] },
  IssueConnection: { kind: 'OBJECT', fields: ['nodes', 'totalCount', 'pageInfo', 'edges'], enumValues: [] },
  PullRequestConnection: { kind: 'OBJECT', fields: ['nodes', 'totalCount', 'pageInfo', 'edges'], enumValues: [] },
  PullRequestChangedFileConnection: { kind: 'OBJECT', fields: ['nodes', 'totalCount', 'pageInfo'], enumValues: [] },
  PullRequestChangedFile: { kind: 'OBJECT', fields: ['path', 'filename', 'additions', 'deletions', 'changes'], enumValues: [] },
  LabelConnection: { kind: 'OBJECT', fields: ['nodes', 'totalCount', 'pageInfo'], enumValues: [] },
  Label: { kind: 'OBJECT', fields: ['id', 'name', 'color', 'description'], enumValues: [] },
  Ref: { kind: 'OBJECT', fields: ['id', 'name', 'prefix', 'target'], enumValues: [] },
  PageInfo: { kind: 'OBJECT', fields: ['hasNextPage', 'hasPreviousPage', 'endCursor', 'startCursor'], enumValues: [] },
  IssueCommentConnection: { kind: 'OBJECT', fields: ['nodes', 'totalCount', 'pageInfo'], enumValues: [] },
  IssueComment: { kind: 'OBJECT', fields: ['id', 'body', 'author', 'createdAt', 'updatedAt', 'url'], enumValues: [] },
  CommentEdge: { kind: 'OBJECT', fields: ['node', 'cursor'], enumValues: [] },
  CreateIssuePayload: { kind: 'OBJECT', fields: ['issue', 'clientMutationId'], enumValues: [] },
  CloseIssuePayload: { kind: 'OBJECT', fields: ['issue', 'clientMutationId'], enumValues: [] },
  CreatePullRequestPayload: { kind: 'OBJECT', fields: ['pullRequest', 'clientMutationId'], enumValues: [] },
  MergePullRequestPayload: { kind: 'OBJECT', fields: ['pullRequest', 'clientMutationId'], enumValues: [] },
  AddCommentPayload: { kind: 'OBJECT', fields: ['commentEdge', 'comment', 'clientMutationId'], enumValues: [] },
  SearchType: { kind: 'ENUM', fields: [], enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE'] },
  IssueState: { kind: 'ENUM', fields: [], enumValues: ['OPEN', 'CLOSED'] },
  PullRequestState: { kind: 'ENUM', fields: [], enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  MergeableState: { kind: 'ENUM', fields: [], enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  OrderDirection: { kind: 'ENUM', fields: [], enumValues: ['ASC', 'DESC'] },
  IssueOrderField: { kind: 'ENUM', fields: [], enumValues: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  RepositoryPermission: { kind: 'ENUM', fields: [], enumValues: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  PullRequestMergeMethod: { kind: 'ENUM', fields: [], enumValues: ['MERGE', 'SQUASH', 'REBASE'] },
  __Type: { kind: 'OBJECT', fields: ['name', 'kind', 'fields', 'enumValues', 'description'], enumValues: [] },
  __Field: { kind: 'OBJECT', fields: ['name', 'description', 'args', 'type'], enumValues: [] },
  __EnumValue: { kind: 'OBJECT', fields: ['name', 'description'], enumValues: [] },
  __Schema: { kind: 'OBJECT', fields: ['queryType', 'mutationType', 'subscriptionType', 'types', 'directives'], enumValues: [] },
};

// ---------------------------------------------------------------------------
// Values: plain scalars, null, or a tagged object { __gql: <type>, ... }.
// ---------------------------------------------------------------------------

interface GObj {
  __gql: string;
  [key: string]: unknown;
}

function isGObj(v: unknown): v is GObj {
  return v !== null && typeof v === 'object' && !Array.isArray(v) && typeof (v as GObj).__gql === 'string';
}

function userVal(login: string): GObj {
  return { __gql: 'User', login, id: nodeId('User', 1) };
}

function repoVal(repo: Repo): GObj {
  return { __gql: 'Repository', repo };
}

function issueVal(repo: Repo, issue: Issue): GObj {
  return { __gql: 'Issue', repo, issue };
}

function pullVal(repo: Repo, pull: PullRequest): GObj {
  return { __gql: 'PullRequest', repo, pull };
}

function findIssueByNum(store: Store, num: number): { repo: Repo; issue: Issue } | null {
  for (const repo of store.repos.values()) {
    const issue = repo.issues.find((x) => x.id === num);
    if (issue) return { repo, issue };
  }
  return null;
}

function findPullByNum(store: Store, num: number): { repo: Repo; pull: PullRequest } | null {
  for (const repo of store.repos.values()) {
    const pull = repo.pulls.find((x) => x.id === num);
    if (pull) return { repo, pull };
  }
  return null;
}

function repoOf(store: Store, decoded: { type: string; num: number }): Repo | undefined {
  return store.reposById.get(decoded.num);
}

function encodeCursor(index: number): string {
  return Buffer.from(`cursor:${index}`, 'utf8').toString('base64');
}

function decodeCursor(value: unknown): number {
  if (typeof value !== 'string' || value === '') return 0;
  try {
    const text = Buffer.from(value, 'base64').toString('utf8');
    const m = /^cursor:(\d+)$/.exec(text);
    if (m) return Number(m[1]) + 1;
  } catch {
    // fall through
  }
  return 0;
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

interface ExecCtx {
  store: Store;
  variables: Record<string, unknown>;
  fragments: Map<string, FragmentDef>;
  errors: Array<{ message: string }>;
}

function evalValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'var':
      return variables[node.name] ?? null;
    case 'string':
    case 'number':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.items.map((item) => evalValue(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = evalValue(f.value, variables);
      return out;
    }
  }
}

function evalArgs(field: { args: Array<{ name: string; value: ValueNode }> }, ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const a of field.args) out[a.name] = evalValue(a.value, ctx.variables);
  return out;
}

function asRecord(v: unknown): Record<string, unknown> | null {
  return v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : null;
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: PullRequest): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function ownerLogin(repo: Repo, store: Store): string {
  return repo.ownerLogin ?? store.viewerLogin();
}

function issuesConnection(repo: Repo, args: Record<string, unknown>): GObj {
  let list = [...repo.issues];
  const states = args.states;
  if (Array.isArray(states) && states.length > 0) {
    const wanted = new Set(states.map((s) => String(s).toLowerCase()));
    list = list.filter((i) => wanted.has(i.state));
  }
  const orderBy = asRecord(args.orderBy);
  if (orderBy) {
    const field = String(orderBy.field ?? 'CREATED_AT');
    const dir = String(orderBy.direction ?? 'ASC');
    const key = (i: Issue): string => (field === 'UPDATED_AT' ? i.updatedAt : i.createdAt);
    list.sort((a, b) => {
      const ka = key(a);
      const kb = key(b);
      if (ka !== kb) return dir === 'DESC' ? (ka < kb ? 1 : -1) : ka < kb ? -1 : 1;
      return dir === 'DESC' ? b.number - a.number : a.number - b.number;
    });
  } else {
    list.sort((a, b) => a.number - b.number);
  }
  const offset = decodeCursor(args.after);
  const first = typeof args.first === 'number' ? args.first : list.length;
  const page = list.slice(offset, offset + first);
  const endIndex = offset + page.length - 1;
  return {
    __gql: 'IssueConnection',
    nodes: page.map((i) => issueVal(repo, i)),
    totalCount: list.length,
    pageInfo: {
      __gql: 'PageInfo',
      hasNextPage: offset + page.length < list.length,
      hasPreviousPage: offset > 0,
      endCursor: page.length > 0 ? encodeCursor(endIndex) : null,
      startCursor: page.length > 0 ? encodeCursor(offset) : null,
    },
  };
}

function pullsConnection(repo: Repo, args: Record<string, unknown>): GObj {
  let list = [...repo.pulls];
  const states = args.states;
  if (Array.isArray(states) && states.length > 0) {
    const wanted = new Set(states.map((s) => String(s)));
    list = list.filter((p) => {
      const st = pullState(p);
      return wanted.has(st);
    });
  }
  list.sort((a, b) => b.number - a.number);
  const first = typeof args.first === 'number' ? args.first : list.length;
  const page = list.slice(0, first);
  return {
    __gql: 'PullRequestConnection',
    nodes: page.map((p) => pullVal(repo, p)),
    totalCount: list.length,
    pageInfo: {
      __gql: 'PageInfo',
      hasNextPage: page.length < list.length,
      hasPreviousPage: false,
      endCursor: page.length > 0 ? encodeCursor(page.length - 1) : null,
      startCursor: page.length > 0 ? encodeCursor(0) : null,
    },
  };
}

function typeVal(name: string): GObj | null {
  const def = TYPE_DEFS[name];
  if (!def) return null;
  return { __gql: '__Type', name, kind: def.kind, fields: def.fields, enumValues: def.enumValues };
}

function resolveField(parent: GObj, field: string, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  const store = ctx.store;
  switch (parent.__gql) {
    case 'Query': {
      if (field === 'viewer') return userVal(store.viewerLogin());
      if (field === 'repository') {
        const repo = store.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
        return repo ? repoVal(repo) : null;
      }
      if (field === 'node') {
        const decoded = decodeNodeId(args.id);
        if (!decoded) return null;
        if (decoded.type === 'Repository') {
          const repo = repoOf(store, decoded);
          return repo ? repoVal(repo) : null;
        }
        if (decoded.type === 'Issue') {
          const found = findIssueByNum(store, decoded.num);
          return found ? issueVal(found.repo, found.issue) : null;
        }
        if (decoded.type === 'PullRequest') {
          const found = findPullByNum(store, decoded.num);
          return found ? pullVal(found.repo, found.pull) : null;
        }
        if (decoded.type === 'User') return userVal(store.viewerLogin());
        return null;
      }
      if (field === '__type') return typeVal(String(args.name ?? ''));
      if (field === '__schema') return { __gql: '__Schema' };
      return null;
    }
    case 'Mutation': {
      if (field === 'createIssue') {
        const input = asRecord(args.input) ?? {};
        const decoded = decodeNodeId(input.repositoryId);
        const repo = decoded && decoded.type === 'Repository' ? repoOf(store, decoded) : undefined;
        if (!repo) {
          ctx.errors.push({ message: 'createIssue: repository not found' });
          return { __gql: 'CreateIssuePayload', issue: null };
        }
        const created = store.createIssue(repo, { title: String(input.title ?? ''), body: typeof input.body === 'string' ? input.body : '' });
        if ('error' in created) {
          ctx.errors.push({ message: `createIssue: unknown label ${created.error}` });
          return { __gql: 'CreateIssuePayload', issue: null };
        }
        return { __gql: 'CreateIssuePayload', issue: issueVal(repo, created) };
      }
      if (field === 'closeIssue') {
        const input = asRecord(args.input) ?? {};
        const decoded = decodeNodeId(input.issueId);
        const found = decoded && decoded.type === 'Issue' ? findIssueByNum(store, decoded.num) : null;
        if (!found) {
          ctx.errors.push({ message: 'closeIssue: issue not found' });
          return { __gql: 'CloseIssuePayload', issue: null };
        }
        found.issue.state = 'closed';
        found.issue.updatedAt = store.nowIso();
        return { __gql: 'CloseIssuePayload', issue: issueVal(found.repo, found.issue) };
      }
      if (field === 'createPullRequest') {
        const input = asRecord(args.input) ?? {};
        const decoded = decodeNodeId(input.repositoryId);
        const repo = decoded && decoded.type === 'Repository' ? repoOf(store, decoded) : undefined;
        if (!repo) {
          ctx.errors.push({ message: 'createPullRequest: repository not found' });
          return { __gql: 'CreatePullRequestPayload', pullRequest: null };
        }
        const created = store.createPull(repo, {
          title: String(input.title ?? ''),
          body: typeof input.body === 'string' ? input.body : '',
          head: String(input.headRefName ?? ''),
          base: String(input.baseRefName ?? ''),
        });
        if ('error' in created) {
          ctx.errors.push({ message: `createPullRequest: ${created.error}` });
          return { __gql: 'CreatePullRequestPayload', pullRequest: null };
        }
        return { __gql: 'CreatePullRequestPayload', pullRequest: pullVal(repo, created) };
      }
      if (field === 'mergePullRequest') {
        const input = asRecord(args.input) ?? {};
        const decoded = decodeNodeId(input.pullRequestId);
        const found = decoded && decoded.type === 'PullRequest' ? findPullByNum(store, decoded.num) : null;
        if (!found) {
          ctx.errors.push({ message: 'mergePullRequest: pull request not found' });
          return { __gql: 'MergePullRequestPayload', pullRequest: null };
        }
        const merged = store.mergePull(found.repo, found.pull);
        if ('error' in merged) {
          ctx.errors.push({ message: `mergePullRequest: ${merged.error}` });
          return { __gql: 'MergePullRequestPayload', pullRequest: pullVal(found.repo, found.pull) };
        }
        return { __gql: 'MergePullRequestPayload', pullRequest: pullVal(found.repo, found.pull) };
      }
      if (field === 'addComment') {
        const input = asRecord(args.input) ?? {};
        const decoded = decodeNodeId(input.subjectId);
        const found = decoded && decoded.type === 'Issue' ? findIssueByNum(store, decoded.num) : null;
        if (!found) {
          ctx.errors.push({ message: 'addComment: subject not found' });
          return { __gql: 'AddCommentPayload', commentEdge: null };
        }
        const now = store.nowIso();
        const comment = {
          id: found.repo.nextCommentId,
          body: String(input.body ?? ''),
          userLogin: store.viewerLogin(),
          createdAt: now,
          updatedAt: now,
        };
        found.repo.nextCommentId += 1;
        found.issue.comments.push(comment);
        const node: GObj = {
          __gql: 'IssueComment',
          id: nodeId('IssueComment', found.repo.id * 1000000 + comment.id),
          body: comment.body,
          createdAt: comment.createdAt,
          updatedAt: comment.updatedAt,
          author: userVal(comment.userLogin),
        };
        return { __gql: 'AddCommentPayload', commentEdge: { __gql: 'CommentEdge', node, cursor: null } };
      }
      return null;
    }
    case 'Repository': {
      const repo = parent.repo as Repo;
      switch (field) {
        case 'id': return nodeId('Repository', repo.id);
        case 'databaseId': return repo.id;
        case 'name': return repo.name;
        case 'nameWithOwner': return store.repoFullName(repo);
        case 'owner': return userVal(ownerLogin(repo, store));
        case 'defaultBranchRef': return { __gql: 'Ref', name: repo.defaultBranch, prefix: 'refs/heads/', id: nodeId('Ref', repo.id) };
        case 'defaultBranch': return repo.defaultBranch;
        case 'viewerPermission': return 'ADMIN';
        case 'hasIssuesEnabled': return true;
        case 'isPrivate': return repo.private;
        case 'description': return repo.description;
        case 'createdAt': return repo.createdAt;
        case 'updatedAt': return repo.updatedAt;
        case 'url': return `https://github.com/${ownerLogin(repo, store)}/${repo.name}`;
        case 'issue': {
          const issue = repo.issues.find((i) => i.number === args.number);
          return issue ? issueVal(repo, issue) : null;
        }
        case 'issues': return issuesConnection(repo, args);
        case 'pullRequest': {
          const pull = repo.pulls.find((p) => p.number === args.number);
          return pull ? pullVal(repo, pull) : null;
        }
        case 'pullRequests': return pullsConnection(repo, args);
        default: return null;
      }
    }
    case 'Issue': {
      const repo = parent.repo as Repo;
      const issue = parent.issue as Issue;
      switch (field) {
        case 'id': return nodeId('Issue', issue.id);
        case 'databaseId': return issue.id;
        case 'number': return issue.number;
        case 'title': return issue.title;
        case 'body': return issue.body;
        case 'state': return issueState(issue);
        case 'closed': return issue.state === 'closed';
        case 'locked': return false;
        case 'url': return `https://github.com/${ownerLogin(repo, store)}/${repo.name}/issues/${issue.number}`;
        case 'createdAt': return issue.createdAt;
        case 'updatedAt': return issue.updatedAt;
        case 'author': return userVal(issue.userLogin);
        case 'labels':
          return {
            __gql: 'LabelConnection',
            nodes: issue.labels.map((name) => ({ __gql: 'Label', name, id: nodeId('Label', issue.id), color: '', description: '' })),
            totalCount: issue.labels.length,
            pageInfo: { __gql: 'PageInfo', hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null },
          };
        case 'comments':
          return {
            __gql: 'IssueCommentConnection',
            nodes: issue.comments.map((c) => ({
              __gql: 'IssueComment',
              id: nodeId('IssueComment', repo.id * 1000000 + c.id),
              body: c.body,
              createdAt: c.createdAt,
              updatedAt: c.updatedAt,
              author: userVal(c.userLogin),
            })),
            totalCount: issue.comments.length,
            pageInfo: { __gql: 'PageInfo', hasNextPage: false, hasPreviousPage: false, endCursor: null, startCursor: null },
          };
        default: return null;
      }
    }
    case 'PullRequest': {
      const repo = parent.repo as Repo;
      const pull = parent.pull as PullRequest;
      switch (field) {
        case 'id': return nodeId('PullRequest', pull.id);
        case 'databaseId': return pull.id;
        case 'number': return pull.number;
        case 'title': return pull.title;
        case 'body': return pull.body;
        case 'state': return pullState(pull);
        case 'headRefName': return pull.headRef;
        case 'baseRefName': return pull.baseRef;
        case 'headRefOid': return pull.headSha;
        case 'baseRefOid': return pull.baseSha;
        case 'mergeable': return pull.state === 'open' ? 'MERGEABLE' : 'UNKNOWN';
        case 'merged': return pull.merged;
        case 'mergedAt': return pull.mergedAt;
        case 'createdAt': return pull.createdAt;
        case 'updatedAt': return pull.updatedAt;
        case 'url': return `https://github.com/${ownerLogin(repo, store)}/${repo.name}/pull/${pull.number}`;
        case 'author': return userVal(ownerLogin(repo, store));
        case 'files': {
          const first = typeof args.first === 'number' ? args.first : pull.files.length;
          return {
            __gql: 'PullRequestChangedFileConnection',
            nodes: pull.files.slice(0, first).map((f) => ({ __gql: 'PullRequestChangedFile', path: f.filename, filename: f.filename, additions: f.additions, deletions: f.deletions, changes: f.changes })),
            totalCount: pull.files.length,
            pageInfo: { __gql: 'PageInfo', hasNextPage: first < pull.files.length, hasPreviousPage: false, endCursor: null, startCursor: null },
          };
        }
        default: return null;
      }
    }
    case 'User': {
      if (field === 'login') return parent.login as string;
      if (field === 'id') return parent.id as string;
      if (field === 'name') return parent.login as string;
      if (field === 'url') return `https://github.com/${parent.login as string}`;
      if (field === 'avatarUrl') return '';
      return null;
    }
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection':
    case 'LabelConnection':
    case 'IssueCommentConnection': {
      if (field === 'nodes') return parent.nodes as unknown[];
      if (field === 'totalCount') return parent.totalCount as number;
      if (field === 'pageInfo') return parent.pageInfo as GObj;
      if (field === 'edges') return [];
      return null;
    }
    case 'PullRequestChangedFile': {
      if (field === 'path' || field === 'filename') return parent.path as string;
      if (field === 'additions') return (parent.additions as number) ?? 0;
      if (field === 'deletions') return (parent.deletions as number) ?? 0;
      if (field === 'changes') return (parent.changes as number) ?? 0;
      return null;
    }
    case 'Label': {
      if (field === 'name') return parent.name as string;
      if (field === 'id') return parent.id as string;
      if (field === 'color') return (parent.color as string) ?? '';
      if (field === 'description') return (parent.description as string) ?? '';
      return null;
    }
    case 'Ref': {
      if (field === 'name') return parent.name as string;
      if (field === 'id') return parent.id as string;
      if (field === 'prefix') return (parent.prefix as string) ?? 'refs/heads/';
      if (field === 'target') return null;
      return null;
    }
    case 'PageInfo': {
      if (field === 'hasNextPage') return parent.hasNextPage as boolean;
      if (field === 'hasPreviousPage') return parent.hasPreviousPage as boolean;
      if (field === 'endCursor') return (parent.endCursor as string | null) ?? null;
      if (field === 'startCursor') return (parent.startCursor as string | null) ?? null;
      return null;
    }
    case '__Type': {
      if (field === 'name') return parent.name as string;
      if (field === 'kind') return parent.kind as string;
      if (field === 'description') return null;
      if (field === 'fields') {
        return (parent.fields as string[]).map((name) => ({ __gql: '__Field', name, description: null }));
      }
      if (field === 'enumValues') {
        return (parent.enumValues as string[]).map((name) => ({ __gql: '__EnumValue', name, description: null }));
      }
      return null;
    }
    case '__Field': {
      if (field === 'name') return parent.name as string;
      if (field === 'description') return null;
      if (field === 'args') return [];
      if (field === 'type') return null;
      return null;
    }
    case '__EnumValue': {
      if (field === 'name') return parent.name as string;
      if (field === 'description') return null;
      return null;
    }
    case '__Schema': {
      if (field === 'queryType') return typeVal('Query');
      if (field === 'mutationType') return typeVal('Mutation');
      if (field === 'subscriptionType') return null;
      if (field === 'types') return Object.keys(TYPE_DEFS).map((name) => typeVal(name));
      if (field === 'directives') return [];
      return null;
    }
    case 'CommentEdge': {
      if (field === 'node') return parent.node as GObj;
      if (field === 'cursor') return null;
      return null;
    }
    case 'IssueComment': {
      if (field === 'id') return parent.id as string;
      if (field === 'body') return parent.body as string;
      if (field === 'author') return parent.author as GObj;
      if (field === 'createdAt') return parent.createdAt as string;
      if (field === 'updatedAt') return parent.updatedAt as string;
      if (field === 'url') return '';
      return null;
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      if (field === 'issue') return (parent.issue as GObj | null) ?? null;
      if (field === 'clientMutationId') return null;
      return null;
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field === 'pullRequest') return (parent.pullRequest as GObj | null) ?? null;
      if (field === 'clientMutationId') return null;
      return null;
    }
    case 'AddCommentPayload': {
      if (field === 'commentEdge') return (parent.commentEdge as GObj | null) ?? null;
      if (field === 'comment') {
        const edge = parent.commentEdge as GObj | null;
        return edge ? (edge.node as GObj) : null;
      }
      if (field === 'clientMutationId') return null;
      return null;
    }
    default:
      return null;
  }
}

function executeSelections(selections: SelectionNode[], parent: GObj, ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const parentType = parent.__gql;
  for (const sel of selections) {
    if (sel.kind === 'spread') {
      const frag = ctx.fragments.get(sel.name);
      if (!frag) {
        ctx.errors.push({ message: `Unknown fragment "${sel.name}"` });
        continue;
      }
      if (frag.typeCondition !== parentType) continue;
      Object.assign(out, executeSelections(frag.selections, parent, ctx));
      continue;
    }
    if (sel.kind === 'inline') {
      if (sel.typeCondition && sel.typeCondition !== parentType) continue;
      Object.assign(out, executeSelections(sel.selections, parent, ctx));
      continue;
    }
    const key = sel.alias ?? sel.name;
    if (sel.name === '__typename') {
      out[key] = parentType;
      continue;
    }
    const def = TYPE_DEFS[parentType];
    if (!def || !def.fields.includes(sel.name)) {
      ctx.errors.push({ message: `Cannot query field "${sel.name}" on type "${parentType}".` });
      continue;
    }
    const args = evalArgs(sel, ctx);
    const value = resolveField(parent, sel.name, args, ctx);
    out[key] = materialize(value, sel.selections, ctx);
  }
  return out;
}

function materialize(value: unknown, selections: SelectionNode[] | undefined, ctx: ExecCtx): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) {
    return value.map((item) => materialize(item, selections, ctx));
  }
  if (isGObj(value)) {
    if (!selections || selections.length === 0) return null;
    return executeSelections(selections, value, ctx);
  }
  return value;
}

function runOperation(store: Store, op: unknown): Record<string, unknown> {
  const rec = asRecord(op);
  if (!rec || typeof rec.query !== 'string') {
    return { data: null, errors: [{ message: 'Expected an operation with a query string' }] };
  }
  const variables = asRecord(rec.variables) ?? {};
  let doc: Document;
  try {
    doc = new Parser(tokenize(rec.query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : String(err) }] };
  }
  let operation: Operation | undefined;
  if (typeof rec.operationName === 'string' && rec.operationName !== '') {
    operation = doc.operations.find((o) => o.name === rec.operationName);
    if (!operation) {
      return { data: null, errors: [{ message: `Unknown operation named "${rec.operationName}"` }] };
    }
  } else if (doc.operations.length === 1) {
    operation = doc.operations[0];
  } else {
    return { data: null, errors: [{ message: 'Must provide operation name when the document has multiple operations' }] };
  }
  const ctx: ExecCtx = { store, variables, fragments: doc.fragments, errors: [] };
  const root: GObj = { __gql: operation.kind === 'mutation' ? 'Mutation' : 'Query' };
  const data = executeSelections(operation.selections, root, ctx);
  const result: Record<string, unknown> = { data };
  if (ctx.errors.length > 0) result.errors = ctx.errors;
  return result;
}
