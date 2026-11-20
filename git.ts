// git.ts — git smart HTTP: git-upload-pack / git-receive-pack, protocol v2 and v0.
//
//   GET  /<owner>/<repo>.git/info/refs?service=git-upload-pack   (v2 when the
//        Git-Protocol: version=2 header is sent, v0 otherwise)
//   GET  /<owner>/<repo>.git/info/refs?service=git-receive-pack  (v0 only)
//   POST /<owner>/<repo>.git/git-upload-pack                     (v2 or v0)
//   POST /<owner>/<repo>.git/git-receive-pack                    (v0 only)
//
// Objects are stored verbatim in the shared store; a clone round-trips them
// byte-exactly. Packs sent to clients carry full objects only, never deltas;
// packs received from clients may carry OFS_DELTA / REF_DELTA (see gitpack.ts).
// A push cannot invent a repository, and a non-fast-forward update is refused
// per-ref while the other refs in the same command list still apply.

import { sendRaw } from './app.ts';
import type { Ctx } from './app.ts';
import { parseCommit, parseTree } from './store.ts';
import type { GitObject, Repo, Store } from './store.ts';
import {
  ZERO_OID,
  decodePackets,
  encodePktLine,
  encodePktLines,
  pktDelim,
  pktFlush,
  readPack,
  sideband,
  writePack,
} from './gitpack.ts';

const AGENT = 'makefun-git/1.0';

function headers(ctx: Ctx): Record<string, string> {
  return { 'x-github-request-id': ctx.requestId };
}

interface RefEntry {
  readonly name: string;
  readonly oid: string;
}

function sortedRefs(repo: Repo): RefEntry[] {
  return [...repo.refs.entries()]
    .map(([name, oid]) => ({ name, oid }))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

function headOid(repo: Repo): string | undefined {
  return repo.refs.get(headTarget(repo));
}

/** Follow a tag chain to the object it ultimately points at. Undefined when missing or cyclic. */
function peelOid(store: Store, oid: string): string | undefined {
  let current = oid;
  const seen = new Set<string>();
  for (;;) {
    const obj = store.getObject(current);
    if (!obj) return undefined;
    if (obj.type !== 'tag') return current;
    if (seen.has(current)) return undefined;
    seen.add(current);
    const match = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8'));
    if (!match) return undefined;
    current = match[1]!;
  }
}

// ---------------------------------------------------------------------------------------------
// ref advertisements (info/refs)
// ---------------------------------------------------------------------------------------------

function v0Advertisement(store: Store, repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? `side-band-64k thin-pack ofs-delta shallow agent=${AGENT} symref=HEAD:${headTarget(repo)}`
      : `report-status delete-refs side-band-64k ofs-delta agent=${AGENT}`;
  const parts: Buffer[] = [encodePktLine(`# service=${service}\n`), pktFlush()];
  const refs = sortedRefs(repo);
  if (refs.length === 0) {
    parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    parts.push(encodePktLine(`${headOid(repo) ?? ZERO_OID} HEAD\0${caps}\n`));
    for (const ref of refs) {
      parts.push(encodePktLine(`${ref.oid} ${ref.name}\n`));
      const obj = store.getObject(ref.oid);
      if (obj?.type === 'tag') {
        const peeled = peelOid(store, ref.oid);
        if (peeled && peeled !== ref.oid) parts.push(encodePktLine(`${peeled} ${ref.name}^{}\n`));
      }
    }
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

function v2Advertisement(): Buffer {
  return encodePktLines([
    '# service=git-upload-pack\n',
    'flush',
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow filter\n',
    'object-format=sha1\n',
    'flush',
  ]);
}

// ---------------------------------------------------------------------------------------------
// fetch planning: which objects to send, and the shallow boundary to report
// ---------------------------------------------------------------------------------------------

interface FetchPlan {
  /** Objects the client does not already hold, keyed by oid. */
  readonly objects: Map<string, GitObject>;
  /** New shallow boundary commits (commits at exactly `depth`, when deepening). */
  readonly shallow: string[];
  /** Old shallow boundaries that are now fully held. */
  readonly unshallow: string[];
  /** Haves that are ancestors of (or equal to) a want, for ACK lines. */
  readonly common: string[];
}

/** Add a tree and everything under it to `into`. */
function collectTree(store: Store, treeOid: string, into: Map<string, GitObject>, seen = new Set<string>()): void {
  if (seen.has(treeOid)) return;
  seen.add(treeOid);
  const tree = store.getObject(treeOid);
  if (!tree || tree.type !== 'tree') return;
  into.set(treeOid, tree);
  for (const entry of parseTree(tree.data)) {
    if (entry.mode === '40000') collectTree(store, entry.oid, into, seen);
    else if (entry.mode === '160000') continue; // gitlink: a commit the repo does not hold
    else {
      const blob = store.getObject(entry.oid);
      if (blob) into.set(entry.oid, blob);
    }
  }
}

function planFetch(
  store: Store,
  wants: readonly string[],
  haves: readonly string[],
  shallowArgs: readonly string[],
  depth: number | undefined,
  deepenRelative = false,
): FetchPlan {
  const shallowSet = new Set(shallowArgs);
  const objects = new Map<string, GitObject>();

  // `deepen N` is a total depth — except with `deepen-relative`, where it
  // means N generations deeper than the client's current shallow boundary.
  // The boundary sits `d` parent-steps below a want, i.e. at generation
  // d+1 from it, so the new total depth is d+1+N (deepest boundary wins).
  let effectiveDepth = depth;
  if (depth !== undefined && deepenRelative && shallowArgs.length > 0) {
    let deepest = -1;
    const seen = new Set<string>();
    const queue: Array<{ oid: string; dist: number }> = wants.map((oid) => ({ oid, dist: 0 }));
    let head = 0;
    while (head < queue.length) {
      const { oid, dist } = queue[head++]!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      if (shallowSet.has(oid)) {
        if (dist > deepest) deepest = dist;
        continue; // a boundary: do not walk past it
      }
      const obj = store.getObject(oid);
      if (!obj || obj.type !== 'commit') continue;
      const commit = parseCommit(obj.data);
      if (!commit) continue;
      for (const parent of commit.parents) queue.push({ oid: parent, dist: dist + 1 });
    }
    if (deepest >= 0) effectiveDepth = deepest + 1 + depth;
  }

  // Walk the wanted commits, depth-limited when deepening. A want that is a
  // tag peels to its target; a tag's own bytes travel with it.
  const included = new Set<string>();
  const depthOf = new Map<string, number>();
  const queue: Array<{ oid: string; depth: number }> = [];
  let queueHead = 0;
  const enqueueWant = (oid: string): void => {
    let current = oid;
    const seen = new Set<string>();
    for (;;) {
      const obj = store.getObject(current);
      if (!obj) return;
      if (obj.type === 'tag') {
        if (seen.has(current)) return;
        seen.add(current);
        objects.set(current, obj);
        const match = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8'));
        if (!match) return;
        current = match[1]!;
        continue;
      }
      if (obj.type === 'commit') {
        queue.push({ oid: current, depth: 1 });
        return;
      }
      objects.set(current, obj);
      if (obj.type === 'tree') collectTree(store, current, objects);
      return;
    }
  };
  for (const want of wants) enqueueWant(want);

  while (queueHead < queue.length) {
    const { oid, depth: d } = queue[queueHead++]!;
    if (included.has(oid)) continue;
    const obj = store.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    included.add(oid);
    depthOf.set(oid, d);
    objects.set(oid, obj);
    const commit = parseCommit(obj.data);
    if (!commit) continue;
    collectTree(store, commit.tree, objects);
    if (effectiveDepth !== undefined && d >= effectiveDepth) continue; // boundary: parents stay behind
    for (const parent of commit.parents) {
      // A shallow boundary stops the walk only when there is no depth
      // limit: with a limit (total depth, or deepen-relative) the limit
      // itself bounds the walk, and a deepen-relative fetch must walk
      // straight through the old boundary to reach deeper history.
      if (effectiveDepth === undefined && shallowSet.has(parent)) continue;
      if (!included.has(parent)) queue.push({ oid: parent, depth: d + 1 });
    }
  }

  // What the client already holds: walk the haves, but never past a shallow
  // boundary — the client's copy of a boundary commit has no parent history.
  const held = new Set<string>();
  const heldQueue: string[] = [];
  const heldSeen = new Set<string>();
  for (const have of haves) {
    let current = have;
    const seen = new Set<string>();
    for (;;) {
      const obj = store.getObject(current);
      if (!obj) break;
      if (obj.type === 'tag') {
        if (seen.has(current)) break;
        seen.add(current);
        held.add(current);
        const match = /^object ([0-9a-f]{40})$/m.exec(obj.data.toString('utf8'));
        if (!match) break;
        current = match[1]!;
        continue;
      }
      heldQueue.push(current);
      break;
    }
  }
  while (heldQueue.length > 0) {
    const oid = heldQueue.pop()!;
    if (heldSeen.has(oid)) continue;
    heldSeen.add(oid);
    const obj = store.getObject(oid);
    if (!obj) continue;
    if (obj.type === 'commit') {
      held.add(oid);
      const commit = parseCommit(obj.data);
      if (commit) {
        const treeObjects = new Map<string, GitObject>();
        collectTree(store, commit.tree, treeObjects);
        for (const key of treeObjects.keys()) held.add(key);
      }
      if (!shallowSet.has(oid) && commit) {
        for (const parent of commit.parents) heldQueue.push(parent);
      }
    } else {
      held.add(oid);
      if (obj.type === 'tree') {
        const treeObjects = new Map<string, GitObject>();
        collectTree(store, oid, treeObjects);
        for (const key of treeObjects.keys()) held.add(key);
      }
    }
  }

  for (const oid of held) objects.delete(oid);

  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (effectiveDepth !== undefined) {
    for (const [oid, d] of depthOf) if (d === effectiveDepth) shallow.push(oid);
    for (const boundary of shallowArgs) {
      const d = depthOf.get(boundary);
      if (d !== undefined && d < effectiveDepth) unshallow.push(boundary);
    }
    shallow.sort();
    unshallow.sort();
  }

  const reachable = store.reachableObjects(wants);
  const common = haves.filter((have) => reachable.has(have));

  return { objects, shallow, unshallow, common };
}

function packForPlan(plan: FetchPlan): Buffer {
  const ordered = [...plan.objects.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return writePack(ordered.map(([, object]) => object));
}

// ---------------------------------------------------------------------------------------------
// protocol v2: ls-refs and fetch
// ---------------------------------------------------------------------------------------------

function lsRefsResponse(ctx: Ctx, repo: Repo, lines: readonly string[]): Buffer {
  let peel = false;
  let symrefs = false;
  let unborn = false;
  const prefixes: string[] = [];
  for (const line of lines) {
    if (line === 'peel') peel = true;
    else if (line === 'symrefs') symrefs = true;
    else if (line === 'unborn') unborn = true;
    else if (line.startsWith('ref-prefix ')) prefixes.push(line.slice('ref-prefix '.length));
  }
  const keep = (name: string): boolean =>
    prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const out: string[] = [];
  const tip = headOid(repo);
  if (keep('HEAD')) {
    if (tip) out.push(`${tip} HEAD${symrefs ? ` symref-target:${headTarget(repo)}` : ''}`);
    else if (unborn) out.push(`unborn HEAD${symrefs ? ` symref-target:${headTarget(repo)}` : ''}`);
  }
  for (const ref of sortedRefs(repo)) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (peel) {
      const obj = ctx.store.getObject(ref.oid);
      if (obj?.type === 'tag') {
        const peeled = peelOid(ctx.store, ref.oid);
        if (peeled && peeled !== ref.oid) line += ` peeled:${peeled}`;
      }
    }
    out.push(line);
  }
  return Buffer.concat([...out.map((line) => encodePktLine(`${line}\n`)), pktFlush()]);
}

function fetchV2Response(ctx: Ctx, lines: readonly string[]): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowArgs: string[] = [];
  let depth: number | undefined;
  let deepenRelative = false;
  for (const line of lines) {
    if (line.startsWith('want ')) wants.push(line.slice(5, 45));
    else if (line.startsWith('have ')) haves.push(line.slice(5, 45));
    else if (line.startsWith('shallow ')) shallowArgs.push(line.slice(8, 48));
    else if (line === 'deepen-relative') deepenRelative = true;
    else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7), 10);
      if (Number.isFinite(n)) depth = n;
    }
    // thin-pack / no-progress / ofs-delta / filter / done: noted, no action
    // needed — full objects are always sent, so a filter changes nothing here.
  }
  if (wants.length === 0) throw new Error('fetch: no want lines');
  const plan = planFetch(ctx.store, wants, haves, shallowArgs, depth, deepenRelative);
  const parts: Buffer[] = [];
  // The acknowledgments section exists only when the client sent haves to
  // negotiate over: with no haves there is nothing to acknowledge, and a
  // real git client expects the packfile section first.
  if (haves.length > 0) {
    parts.push(encodePktLine('acknowledgments\n'));
    if (plan.common.length > 0) {
      for (const oid of plan.common) parts.push(encodePktLine(`ACK ${oid}\n`));
      parts.push(encodePktLine('ready\n'));
    } else {
      parts.push(encodePktLine('NAK\n'));
    }
    parts.push(pktDelim());
  }
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    parts.push(encodePktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
    parts.push(pktDelim());
  }
  parts.push(encodePktLine('packfile\n'));
  parts.push(sideband(packForPlan(plan)));
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

function handleUploadPackV2(ctx: Ctx, repo: Repo): Buffer {
  const { packets } = decodePackets(ctx.body);
  const lines = packets
    .filter((packet) => packet.kind === 'data')
    .map((packet) => packet.payload.toString('utf8').replace(/\n$/, ''));
  const command = lines[0] ?? '';
  if (command === 'command=ls-refs') return lsRefsResponse(ctx, repo, lines.slice(1));
  if (command === 'command=fetch') return fetchV2Response(ctx, lines.slice(1));
  throw new Error(`unsupported v2 command ${JSON.stringify(command)}`);
}

// ---------------------------------------------------------------------------------------------
// protocol v0: upload-pack
// ---------------------------------------------------------------------------------------------

function handleUploadPackV0(ctx: Ctx): Buffer {
  const { packets } = decodePackets(ctx.body);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowArgs: string[] = [];
  let depth: number | undefined;
  let deepenRelative = false;
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) wants.push(line.slice(5, 45)); // caps may follow the oid
    else if (line.startsWith('have ')) haves.push(line.slice(5, 45));
    else if (line.startsWith('shallow ')) shallowArgs.push(line.slice(8, 48));
    else if (line === 'deepen-relative') deepenRelative = true;
    else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7), 10);
      if (Number.isFinite(n)) depth = n;
    }
  }
  if (wants.length === 0) throw new Error('upload-pack: no want lines');
  const plan = planFetch(ctx.store, wants, haves, shallowArgs, depth, deepenRelative);
  const parts: Buffer[] = [];
  for (const oid of plan.shallow) parts.push(encodePktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(encodePktLine(`unshallow ${oid}\n`));
  if (plan.common.length > 0) {
    for (const oid of plan.common) parts.push(encodePktLine(`ACK ${oid}\n`));
  } else {
    parts.push(encodePktLine('NAK\n'));
  }
  parts.push(sideband(packForPlan(plan)));
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------------------------------------
// protocol v0: receive-pack
// ---------------------------------------------------------------------------------------------

interface PushCommand {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

function isFastForward(store: Store, oldOid: string, newOid: string): boolean {
  if (store.isAncestor(oldOid, newOid)) return true;
  // Tag updates compare the commits the tags peel to.
  const peeledOld = peelOid(store, oldOid);
  const peeledNew = peelOid(store, newOid);
  if (peeledOld && peeledNew && (peeledOld !== oldOid || peeledNew !== newOid)) {
    return store.isAncestor(peeledOld, peeledNew);
  }
  return false;
}

function evaluatePushCommand(
  store: Store,
  repo: Repo,
  command: PushCommand,
): { ref: string; ok: boolean; reason: string } {
  const { oldOid, newOid, ref } = command;
  const current = repo.refs.get(ref);
  if (newOid === ZERO_OID) {
    // Delete.
    if (current === undefined) return { ref, ok: false, reason: 'remote ref does not exist' };
    if (oldOid !== ZERO_OID && oldOid !== current) return { ref, ok: false, reason: 'non-fast-forward' };
    repo.refs.delete(ref);
    return { ref, ok: true, reason: '' };
  }
  if (!store.getObject(newOid)) return { ref, ok: false, reason: 'missing object' };
  if (oldOid === ZERO_OID) {
    // Create.
    if (current !== undefined) return { ref, ok: false, reason: 'already exists' };
    repo.refs.set(ref, newOid);
    return { ref, ok: true, reason: '' };
  }
  // Update.
  if (current === undefined) return { ref, ok: false, reason: 'remote ref does not exist' };
  if (oldOid !== current) return { ref, ok: false, reason: 'non-fast-forward' };
  if (newOid === oldOid) return { ref, ok: true, reason: '' };
  if (!isFastForward(store, oldOid, newOid)) return { ref, ok: false, reason: 'non-fast-forward' };
  repo.refs.set(ref, newOid);
  return { ref, ok: true, reason: '' };
}

function handleReceivePack(ctx: Ctx, repo: Repo): Buffer {
  const { packets, consumed } = decodePackets(ctx.body, { stopAtFlush: true });
  const commands: PushCommand[] = [];
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) line = line.slice(0, nul); // capabilities ride on the first command
    const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$/.exec(line);
    if (!match) throw new Error(`bad receive-pack command ${JSON.stringify(line)}`);
    commands.push({ oldOid: match[1]!, newOid: match[2]!, ref: match[3]! });
  }
  if (commands.length === 0) throw new Error('receive-pack: no commands');

  // Unpack first: the objects land in the store even when a ref update below
  // is refused, so a refused push still reports "unpack ok".
  const packBytes = ctx.body.subarray(consumed);
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (packBytes.length > 0) {
    try {
      const read = readPack(packBytes, (oid) => ctx.store.getObject(oid));
      for (const [oid, object] of read.objects) {
        if (!ctx.store.objects.has(oid)) ctx.store.objects.set(oid, object);
      }
    } catch (err) {
      unpackOk = false;
      unpackDetail = (err instanceof Error ? err.message : 'unpack failed').replace(/[\r\n]+/g, ' ');
    }
  } else if (commands.some((command) => command.newOid !== ZERO_OID)) {
    unpackOk = false;
    unpackDetail = 'missing packfile';
  }

  const inner: Buffer[] = [encodePktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`)];
  for (const command of commands) {
    const result = unpackOk
      ? evaluatePushCommand(ctx.store, repo, command)
      : { ref: command.ref, ok: false, reason: 'unpack failed' };
    inner.push(
      encodePktLine(result.ok ? `ok ${result.ref}\n` : `ng ${result.ref} ${result.reason}\n`),
    );
  }
  inner.push(pktFlush());
  // The report-status stream rides inside sideband channel 1.
  return Buffer.concat([sideband(Buffer.concat(inner)), pktFlush()]);
}

// ---------------------------------------------------------------------------------------------
// router
// ---------------------------------------------------------------------------------------------

export async function handleGit(ctx: Ctx): Promise<boolean> {
  const segments = ctx.segments;
  if (segments.length < 2 || !segments[1]!.endsWith('.git')) return false;
  const owner = segments[0]!;
  const repoName = segments[1]!.slice(0, -4);
  const rest = segments.slice(2);

  const repo = ctx.store.getRepo(owner, repoName);
  if (!repo) {
    sendRaw(ctx.res, 404, Buffer.from('Not Found\n', 'utf8'), 'text/plain; charset=utf-8', headers(ctx));
    return true;
  }

  if (ctx.method === 'GET' && rest.length === 2 && rest[0] === 'info' && rest[1] === 'refs') {
    const service = ctx.query.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return false;
    const isV2 = service === 'git-upload-pack' && ctx.req.headers['git-protocol'] === 'version=2';
    const body = isV2 ? v2Advertisement() : v0Advertisement(ctx.store, repo, service);
    sendRaw(ctx.res, 200, body, `application/x-${service}-advertisement`, headers(ctx));
    return true;
  }

  if (ctx.method === 'POST' && rest.length === 1 && rest[0] === 'git-upload-pack') {
    const isV2 = ctx.req.headers['git-protocol'] === 'version=2';
    const body = isV2 ? handleUploadPackV2(ctx, repo) : handleUploadPackV0(ctx);
    sendRaw(ctx.res, 200, body, 'application/x-git-upload-pack-result', headers(ctx));
    return true;
  }

  if (ctx.method === 'POST' && rest.length === 1 && rest[0] === 'git-receive-pack') {
    const body = handleReceivePack(ctx, repo);
    sendRaw(ctx.res, 200, body, 'application/x-git-receive-pack-result', headers(ctx));
    return true;
  }

  return false;
}
