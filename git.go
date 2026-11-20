// git.go — git smart HTTP: git-upload-pack / git-receive-pack, protocol v2 and v0.
//
//   GET  /<owner>/<repo>.git/info/refs?service=git-upload-pack   (v2 when the
//        Git-Protocol: version=2 header is sent, v0 otherwise)
//   GET  /<owner>/<repo>.git/info/refs?service=git-receive-pack  (v0 only)
//   POST /<owner>/<repo>.git/git-upload-pack                     (v2 or v0)
//   POST /<owner>/<repo>.git/git-receive-pack                    (v0 only)
//
// Objects are stored verbatim in the shared store; a clone round-trips them
// byte-exactly. Packs sent to clients carry full objects only, never deltas;
// packs received from clients may carry OFS_DELTA / REF_DELTA (see gitpack.go).
// A push cannot invent a repository, and a non-fast-forward update is refused
// per-ref while the other refs in the same command list still apply.
package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const agent = "makefun-git/1.0"

func gitHeaders(ctx *Ctx) map[string]string {
	return map[string]string{"X-GitHub-Request-Id": ctx.RequestID}
}

type refEntry struct {
	name string
	oid  string
}

func sortedRefs(repo *Repo) []refEntry {
	refs := make([]refEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, refEntry{name, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func headTarget(repo *Repo) string {
	return "refs/heads/" + repo.DefaultBranch
}

func headOid(repo *Repo) (string, bool) {
	oid, ok := repo.Refs[headTarget(repo)]
	return oid, ok
}

// peelOid follows a tag chain to the object it ultimately points at.
// "" when the target is missing or the chain is cyclic.
func peelOid(store *Store, oid string) string {
	current := oid
	seen := map[string]bool{}
	for {
		obj := store.GetObject(current)
		if obj == nil {
			return ""
		}
		if obj.Type != ObjTag {
			return current
		}
		if seen[current] {
			return ""
		}
		seen[current] = true
		m := tagObjectRe.FindStringSubmatch(string(obj.Data))
		if m == nil {
			return ""
		}
		current = m[1]
	}
}

// ---------------------------------------------------------------------------------------------
// ref advertisements (info/refs)
// ---------------------------------------------------------------------------------------------

func v0Advertisement(store *Store, repo *Repo, service string) []byte {
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k thin-pack ofs-delta shallow agent=" + agent + " symref=HEAD:" + headTarget(repo)
	} else {
		caps = "report-status delete-refs side-band-64k ofs-delta agent=" + agent
	}
	var parts [][]byte
	parts = append(parts, EncodePktLineString("# service="+service+"\n"), PktFlush())
	refs := sortedRefs(repo)
	if len(refs) == 0 {
		parts = append(parts, EncodePktLineString(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		head, _ := headOid(repo)
		if head == "" {
			head = ZeroOID
		}
		parts = append(parts, EncodePktLineString(head+" HEAD\x00"+caps+"\n"))
		for _, ref := range refs {
			parts = append(parts, EncodePktLineString(ref.oid+" "+ref.name+"\n"))
			if obj := store.GetObject(ref.oid); obj != nil && obj.Type == ObjTag {
				if peeled := peelOid(store, ref.oid); peeled != "" && peeled != ref.oid {
					parts = append(parts, EncodePktLineString(peeled+" "+ref.name+"^{}\n"))
				}
			}
		}
	}
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

func v2Advertisement() []byte {
	var parts [][]byte
	parts = append(parts, EncodePktLineString("# service=git-upload-pack\n"))
	parts = append(parts, PktFlush())
	parts = append(parts, EncodePktLineString("version 2\n"))
	parts = append(parts, EncodePktLineString("agent="+agent+"\n"))
	parts = append(parts, EncodePktLineString("ls-refs=unborn\n"))
	parts = append(parts, EncodePktLineString("fetch=shallow filter\n"))
	parts = append(parts, EncodePktLineString("object-format=sha1\n"))
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

// ---------------------------------------------------------------------------------------------
// fetch planning: which objects to send, and the shallow boundary to report
// ---------------------------------------------------------------------------------------------

type fetchPlan struct {
	// objects the client does not already hold, keyed by oid.
	objects map[string]*GitObject
	// new shallow boundary commits (commits at exactly `depth`, when deepening).
	shallow []string
	// old shallow boundaries that are now fully held.
	unshallow []string
	// haves that are ancestors of (or equal to) a want, for ACK lines.
	common []string
}

// collectTree adds a tree and everything under it to into.
func collectTree(store *Store, treeOid string, into map[string]*GitObject, seen map[string]bool) {
	if seen[treeOid] {
		return
	}
	seen[treeOid] = true
	tree := store.GetObject(treeOid)
	if tree == nil || tree.Type != ObjTree {
		return
	}
	into[treeOid] = tree
	for _, entry := range ParseTree(tree.Data) {
		switch entry.Mode {
		case "40000":
			collectTree(store, entry.OID, into, seen)
		case "160000":
			// gitlink: a commit the repo does not hold
		default:
			if blob := store.GetObject(entry.OID); blob != nil {
				into[entry.OID] = blob
			}
		}
	}
}

func planFetch(store *Store, wants, haves, shallowArgs []string, depth *int, deepenRelative bool) fetchPlan {
	shallowSet := map[string]bool{}
	for _, oid := range shallowArgs {
		shallowSet[oid] = true
	}
	objects := map[string]*GitObject{}

	// `deepen N` is a total depth — except with `deepen-relative`, where it
	// means N generations deeper than the client's current shallow boundary.
	// The boundary sits `d` parent-steps below a want, i.e. at generation
	// d+1 from it, so the new total depth is d+1+N (deepest boundary wins).
	var effectiveDepth *int
	if depth != nil {
		d := *depth
		effectiveDepth = &d
	}
	if depth != nil && deepenRelative && len(shallowArgs) > 0 {
		deepest := -1
		seen := map[string]bool{}
		type item struct {
			oid  string
			dist int
		}
		queue := make([]item, 0, len(wants))
		for _, oid := range wants {
			queue = append(queue, item{oid, 0})
		}
		for head := 0; head < len(queue); head++ {
			oid, dist := queue[head].oid, queue[head].dist
			if seen[oid] {
				continue
			}
			seen[oid] = true
			if shallowSet[oid] {
				if dist > deepest {
					deepest = dist
				}
				continue // a boundary: do not walk past it
			}
			obj := store.GetObject(oid)
			if obj == nil || obj.Type != ObjCommit {
				continue
			}
			commit := ParseCommit(obj.Data)
			if commit == nil {
				continue
			}
			for _, parent := range commit.Parents {
				queue = append(queue, item{parent, dist + 1})
			}
		}
		if deepest >= 0 {
			d := deepest + 1 + *depth
			effectiveDepth = &d
		}
	}

	// Walk the wanted commits, depth-limited when deepening. A want that is a
	// tag peels to its target; a tag's own bytes travel with it.
	included := map[string]bool{}
	depthOf := map[string]int{}
	type wantItem struct {
		oid   string
		depth int
	}
	queue := []wantItem{}
	enqueueWant := func(oid string) {
		current := oid
		seen := map[string]bool{}
		for {
			obj := store.GetObject(current)
			if obj == nil {
				return
			}
			if obj.Type == ObjTag {
				if seen[current] {
					return
				}
				seen[current] = true
				objects[current] = obj
				m := tagObjectRe.FindStringSubmatch(string(obj.Data))
				if m == nil {
					return
				}
				current = m[1]
				continue
			}
			if obj.Type == ObjCommit {
				queue = append(queue, wantItem{current, 1})
				return
			}
			objects[current] = obj
			if obj.Type == ObjTree {
				collectTree(store, current, objects, map[string]bool{})
			}
			return
		}
	}
	for _, want := range wants {
		enqueueWant(want)
	}

	for head := 0; head < len(queue); head++ {
		oid, d := queue[head].oid, queue[head].depth
		if included[oid] {
			continue
		}
		obj := store.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		included[oid] = true
		depthOf[oid] = d
		objects[oid] = obj
		commit := ParseCommit(obj.Data)
		if commit == nil {
			continue
		}
		collectTree(store, commit.Tree, objects, map[string]bool{})
		if effectiveDepth != nil && d >= *effectiveDepth {
			continue // boundary: parents stay behind
		}
		for _, parent := range commit.Parents {
			// A shallow boundary stops the walk only when there is no depth
			// limit: with a limit (total depth, or deepen-relative) the limit
			// itself bounds the walk, and a deepen-relative fetch must walk
			// straight through the old boundary to reach deeper history.
			if effectiveDepth == nil && shallowSet[parent] {
				continue
			}
			if !included[parent] {
				queue = append(queue, wantItem{parent, d + 1})
			}
		}
	}

	// What the client already holds: walk the haves, but never past a shallow
	// boundary — the client's copy of a boundary commit has no parent history.
	held := map[string]bool{}
	heldQueue := []string{}
	heldSeen := map[string]bool{}
	for _, have := range haves {
		current := have
		seen := map[string]bool{}
		for {
			obj := store.GetObject(current)
			if obj == nil {
				break
			}
			if obj.Type == ObjTag {
				if seen[current] {
					break
				}
				seen[current] = true
				held[current] = true
				m := tagObjectRe.FindStringSubmatch(string(obj.Data))
				if m == nil {
					break
				}
				current = m[1]
				continue
			}
			heldQueue = append(heldQueue, current)
			break
		}
	}
	for len(heldQueue) > 0 {
		oid := heldQueue[len(heldQueue)-1]
		heldQueue = heldQueue[:len(heldQueue)-1]
		if heldSeen[oid] {
			continue
		}
		heldSeen[oid] = true
		obj := store.GetObject(oid)
		if obj == nil {
			continue
		}
		if obj.Type == ObjCommit {
			held[oid] = true
			commit := ParseCommit(obj.Data)
			if commit != nil {
				treeObjects := map[string]*GitObject{}
				collectTree(store, commit.Tree, treeObjects, map[string]bool{})
				for key := range treeObjects {
					held[key] = true
				}
			}
			if !shallowSet[oid] && commit != nil {
				for _, parent := range commit.Parents {
					heldQueue = append(heldQueue, parent)
				}
			}
		} else {
			held[oid] = true
			if obj.Type == ObjTree {
				treeObjects := map[string]*GitObject{}
				collectTree(store, oid, treeObjects, map[string]bool{})
				for key := range treeObjects {
					held[key] = true
				}
			}
		}
	}

	for oid := range held {
		delete(objects, oid)
	}

	var shallow, unshallow []string
	if effectiveDepth != nil {
		for oid, d := range depthOf {
			if d == *effectiveDepth {
				shallow = append(shallow, oid)
			}
		}
		for _, boundary := range shallowArgs {
			if d, ok := depthOf[boundary]; ok && d < *effectiveDepth {
				unshallow = append(unshallow, boundary)
			}
		}
		sort.Strings(shallow)
		sort.Strings(unshallow)
	}

	// `common` is only ever read from `haves`: with no haves it is empty, so
	// skip the full-history reachableObjects walk (a depth-1 clone of a
	// 200-commit repo would otherwise re-parse every commit and tree here).
	var common []string
	if len(haves) > 0 {
		reachable := store.ReachableObjects(wants)
		for _, have := range haves {
			if _, ok := reachable[have]; ok {
				common = append(common, have)
			}
		}
	}

	return fetchPlan{objects: objects, shallow: shallow, unshallow: unshallow, common: common}
}

// packForPlan builds (or reuses a memoized) pack for a fetch plan's objects.
func packForPlan(plan fetchPlan) []byte {
	ordered := make([]string, 0, len(plan.objects))
	for oid := range plan.objects {
		ordered = append(ordered, oid)
	}
	sort.Strings(ordered)
	// A pack is a pure function of its object set (objects are immutable by
	// oid, ordered by oid, deflate is deterministic), so identical fetches —
	// e.g. 50 concurrent shallow clones of the same tip — share one pack's
	// bytes. The cache is therefore byte-identical to a freshly built pack.
	// Bounded (FIFO eviction) so a long trial cannot grow it without limit.
	key := strings.Join(ordered, ",")
	if hit, ok := packCache[key]; ok {
		return hit
	}
	packObjects := make([]PackObject, 0, len(ordered))
	for _, oid := range ordered {
		obj := plan.objects[oid]
		packObjects = append(packObjects, PackObject{Type: obj.Type, Data: obj.Data})
	}
	pack := WritePack(packObjects)
	for len(packCacheOrder) > 0 && (len(packCache) >= packCacheMaxEntries || packCacheBytes+len(pack) > packCacheMaxBytes) {
		oldest := packCacheOrder[0]
		packCacheOrder = packCacheOrder[1:]
		packCacheBytes -= len(packCache[oldest])
		delete(packCache, oldest)
	}
	packCache[key] = pack
	packCacheOrder = append(packCacheOrder, key)
	packCacheBytes += len(pack)
	return pack
}

// Memoized packs by sorted object-oid list; see packForPlan.
var packCache = map[string][]byte{}
var packCacheOrder []string
var packCacheBytes int

// At most this many packs are memoized at once.
const packCacheMaxEntries = 128

// At most this many memoized pack bytes are held at once.
const packCacheMaxBytes = 16 * 1024 * 1024

// ---------------------------------------------------------------------------------------------
// protocol v2: ls-refs and fetch
// ---------------------------------------------------------------------------------------------

func lsRefsResponse(ctx *Ctx, repo *Repo, lines []string) []byte {
	peel := false
	symrefs := false
	unborn := false
	var prefixes []string
	for _, line := range lines {
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	var out []string
	tip, hasTip := headOid(repo)
	if keep("HEAD") {
		symrefSuffix := ""
		if symrefs {
			symrefSuffix = " symref-target:" + headTarget(repo)
		}
		if hasTip {
			out = append(out, tip+" HEAD"+symrefSuffix)
		} else if unborn {
			out = append(out, "unborn HEAD"+symrefSuffix)
		}
	}
	for _, ref := range sortedRefs(repo) {
		if !keep(ref.name) {
			continue
		}
		line := ref.oid + " " + ref.name
		if peel {
			if obj := ctx.Store.GetObject(ref.oid); obj != nil && obj.Type == ObjTag {
				if peeled := peelOid(ctx.Store, ref.oid); peeled != "" && peeled != ref.oid {
					line += " peeled:" + peeled
				}
			}
		}
		out = append(out, line)
	}
	var parts [][]byte
	for _, line := range out {
		parts = append(parts, EncodePktLineString(line+"\n"))
	}
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

// safeSlice mirrors JS String.slice(start, end): indices clamp to the string.
func safeSlice(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(s) {
		end = len(s)
	}
	if start > len(s) {
		return ""
	}
	if end < start {
		end = start
	}
	return s[start:end]
}

// parseLeadingInt mirrors JS Number.parseInt(text, 10): leading whitespace
// and an optional sign are skipped, then the leading digit run is parsed.
// ok is false when there are no digits.
func parseLeadingInt(text string) (n int, ok bool) {
	i := 0
	for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\n' || text[i] == '\r') {
		i++
	}
	j := i
	if j < len(text) && (text[j] == '+' || text[j] == '-') {
		j++
	}
	k := j
	for k < len(text) && text[k] >= '0' && text[k] <= '9' {
		k++
	}
	if k == j {
		return 0, false
	}
	v, err := strconv.Atoi(text[i:k])
	if err != nil {
		return 0, false
	}
	return v, true
}

func fetchV2Response(ctx *Ctx, lines []string) []byte {
	var wants, haves, shallowArgs []string
	var depth *int
	deepenRelative := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, safeSlice(line, 5, 45))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, safeSlice(line, 5, 45))
		case strings.HasPrefix(line, "shallow "):
			shallowArgs = append(shallowArgs, safeSlice(line, 8, 48))
		case line == "deepen-relative":
			deepenRelative = true
		case strings.HasPrefix(line, "deepen "):
			if n, ok := parseLeadingInt(line[7:]); ok {
				depth = &n
			}
		}
		// thin-pack / no-progress / ofs-delta / filter / done: noted, no action
		// needed — full objects are always sent, so a filter changes nothing here.
	}
	if len(wants) == 0 {
		panic("fetch: no want lines")
	}
	plan := planFetch(ctx.Store, wants, haves, shallowArgs, depth, deepenRelative)
	var parts [][]byte
	// The acknowledgments section exists only when the client sent haves to
	// negotiate over: with no haves there is nothing to acknowledge, and a
	// real git client expects the packfile section first.
	if len(haves) > 0 {
		parts = append(parts, EncodePktLineString("acknowledgments\n"))
		if len(plan.common) > 0 {
			for _, oid := range plan.common {
				parts = append(parts, EncodePktLineString("ACK "+oid+"\n"))
			}
			parts = append(parts, EncodePktLineString("ready\n"))
		} else {
			parts = append(parts, EncodePktLineString("NAK\n"))
		}
		parts = append(parts, PktDelim())
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		parts = append(parts, EncodePktLineString("shallow-info\n"))
		for _, oid := range plan.shallow {
			parts = append(parts, EncodePktLineString("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			parts = append(parts, EncodePktLineString("unshallow "+oid+"\n"))
		}
		parts = append(parts, PktDelim())
	}
	parts = append(parts, EncodePktLineString("packfile\n"))
	parts = append(parts, Sideband(packForPlan(plan)))
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

func handleUploadPackV2(ctx *Ctx, repo *Repo) []byte {
	packets, _ := DecodePackets(ctx.Body, false)
	var lines []string
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		lines = append(lines, strings.TrimSuffix(string(packet.Payload), "\n"))
	}
	command := ""
	if len(lines) > 0 {
		command = lines[0]
	}
	switch command {
	case "command=ls-refs":
		return lsRefsResponse(ctx, repo, lines[1:])
	case "command=fetch":
		return fetchV2Response(ctx, lines[1:])
	default:
		panic(fmt.Sprintf("unsupported v2 command %q", command))
	}
}

// ---------------------------------------------------------------------------------------------
// protocol v0: upload-pack
// ---------------------------------------------------------------------------------------------

func handleUploadPackV0(ctx *Ctx) []byte {
	packets, _ := DecodePackets(ctx.Body, false)
	var wants, haves, shallowArgs []string
	var depth *int
	deepenRelative := false
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, safeSlice(line, 5, 45)) // caps may follow the oid
		case strings.HasPrefix(line, "have "):
			haves = append(haves, safeSlice(line, 5, 45))
		case strings.HasPrefix(line, "shallow "):
			shallowArgs = append(shallowArgs, safeSlice(line, 8, 48))
		case line == "deepen-relative":
			deepenRelative = true
		case strings.HasPrefix(line, "deepen "):
			if n, ok := parseLeadingInt(line[7:]); ok {
				depth = &n
			}
		}
	}
	if len(wants) == 0 {
		panic("upload-pack: no want lines")
	}
	plan := planFetch(ctx.Store, wants, haves, shallowArgs, depth, deepenRelative)
	var parts [][]byte
	for _, oid := range plan.shallow {
		parts = append(parts, EncodePktLineString("shallow "+oid+"\n"))
	}
	for _, oid := range plan.unshallow {
		parts = append(parts, EncodePktLineString("unshallow "+oid+"\n"))
	}
	if len(plan.common) > 0 {
		for _, oid := range plan.common {
			parts = append(parts, EncodePktLineString("ACK "+oid+"\n"))
		}
	} else {
		parts = append(parts, EncodePktLineString("NAK\n"))
	}
	parts = append(parts, Sideband(packForPlan(plan)))
	parts = append(parts, PktFlush())
	return bytes.Join(parts, nil)
}

// ---------------------------------------------------------------------------------------------
// protocol v0: receive-pack
// ---------------------------------------------------------------------------------------------

type pushCommand struct {
	oldOid string
	newOid string
	ref    string
}

var receivePackCmdRe = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$`)

func isFastForward(store *Store, oldOid, newOid string) bool {
	if store.IsAncestor(oldOid, newOid) {
		return true
	}
	// Tag updates compare the commits the tags peel to.
	peeledOld := peelOid(store, oldOid)
	peeledNew := peelOid(store, newOid)
	if peeledOld != "" && peeledNew != "" && (peeledOld != oldOid || peeledNew != newOid) {
		return store.IsAncestor(peeledOld, peeledNew)
	}
	return false
}

type pushResult struct {
	ref    string
	ok     bool
	reason string
}

func evaluatePushCommand(store *Store, repo *Repo, command pushCommand) pushResult {
	current, exists := repo.Refs[command.ref]
	if command.newOid == ZeroOID {
		// Delete.
		if !exists {
			return pushResult{command.ref, false, "remote ref does not exist"}
		}
		if command.oldOid != ZeroOID && command.oldOid != current {
			return pushResult{command.ref, false, "non-fast-forward"}
		}
		delete(repo.Refs, command.ref)
		return pushResult{command.ref, true, ""}
	}
	if store.GetObject(command.newOid) == nil {
		return pushResult{command.ref, false, "missing object"}
	}
	if command.oldOid == ZeroOID {
		// Create.
		if exists {
			return pushResult{command.ref, false, "already exists"}
		}
		repo.Refs[command.ref] = command.newOid
		return pushResult{command.ref, true, ""}
	}
	// Update.
	if !exists {
		return pushResult{command.ref, false, "remote ref does not exist"}
	}
	if command.oldOid != current {
		return pushResult{command.ref, false, "non-fast-forward"}
	}
	if command.newOid == command.oldOid {
		return pushResult{command.ref, true, ""}
	}
	if !isFastForward(store, command.oldOid, command.newOid) {
		return pushResult{command.ref, false, "non-fast-forward"}
	}
	repo.Refs[command.ref] = command.newOid
	return pushResult{command.ref, true, ""}
}

var crlfRe = regexp.MustCompile(`[\r\n]+`)

// readPackSafe mirrors Node's try/catch around readPack: a corrupt pack
// reports "unpack failed" instead of crashing the request.
func readPackSafe(pack []byte, resolveBase func(oid string) *PackObject) (result ReadPackResult, ok bool, detail string) {
	defer func() {
		if rec := recover(); rec != nil {
			result = ReadPackResult{}
			ok = false
			detail = crlfRe.ReplaceAllString(fmt.Sprintf("%v", rec), " ")
		}
	}()
	result = ReadPack(pack, resolveBase)
	return result, true, "ok"
}

func handleReceivePack(ctx *Ctx, repo *Repo) []byte {
	packets, consumed := DecodePackets(ctx.Body, true)
	var commands []pushCommand
	for _, packet := range packets {
		if packet.Kind != PktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul] // capabilities ride on the first command
		}
		m := receivePackCmdRe.FindStringSubmatch(line)
		if m == nil {
			panic(fmt.Sprintf("bad receive-pack command %q", line))
		}
		commands = append(commands, pushCommand{oldOid: m[1], newOid: m[2], ref: m[3]})
	}
	if len(commands) == 0 {
		panic("receive-pack: no commands")
	}

	// Unpack first: the objects land in the store even when a ref update below
	// is refused, so a refused push still reports "unpack ok".
	packBytes := ctx.Body[consumed:]
	unpackOk := true
	unpackDetail := "ok"
	if len(packBytes) > 0 {
		read, ok, detail := readPackSafe(packBytes, func(oid string) *PackObject {
			if obj := ctx.Store.GetObject(oid); obj != nil {
				return &PackObject{Type: obj.Type, Data: obj.Data}
			}
			return nil
		})
		if !ok {
			unpackOk = false
			unpackDetail = detail
		} else {
			for oid, object := range read.Objects {
				if _, exists := ctx.Store.Objects[oid]; !exists {
					ctx.Store.Objects[oid] = &GitObject{Type: object.Type, Data: object.Data}
				}
			}
		}
	} else if func() bool {
		for _, command := range commands {
			if command.newOid != ZeroOID {
				return true
			}
		}
		return false
	}() {
		unpackOk = false
		unpackDetail = "missing packfile"
	}

	var inner [][]byte
	if unpackOk {
		inner = append(inner, EncodePktLineString("unpack ok\n"))
	} else {
		inner = append(inner, EncodePktLineString("unpack "+unpackDetail+"\n"))
	}
	for _, command := range commands {
		var result pushResult
		if unpackOk {
			result = evaluatePushCommand(ctx.Store, repo, command)
		} else {
			result = pushResult{command.ref, false, "unpack failed"}
		}
		if result.ok {
			inner = append(inner, EncodePktLineString("ok "+result.ref+"\n"))
		} else {
			inner = append(inner, EncodePktLineString("ng "+result.ref+" "+result.reason+"\n"))
		}
	}
	inner = append(inner, PktFlush())
	// The report-status stream rides inside sideband channel 1.
	return bytes.Join([][]byte{Sideband(bytes.Join(inner, nil)), PktFlush()}, nil)
}

// ---------------------------------------------------------------------------------------------
// router
// ---------------------------------------------------------------------------------------------

func HandleGit(ctx *Ctx) bool {
	segments := ctx.Segments
	if len(segments) < 2 || !strings.HasSuffix(segments[1], ".git") {
		return false
	}
	owner := segments[0]
	repoName := segments[1][:len(segments[1])-4]
	rest := segments[2:]

	repo := ctx.Store.GetRepo(owner, repoName)
	if repo == nil {
		SendRaw(ctx.W, 404, []byte("Not Found\n"), "text/plain; charset=utf-8", gitHeaders(ctx))
		return true
	}

	if ctx.Method == "GET" && len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" {
		service := ctx.Query.Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return false
		}
		isV2 := service == "git-upload-pack" && ctx.R.Header.Get("Git-Protocol") == "version=2"
		var body []byte
		if isV2 {
			body = v2Advertisement()
		} else {
			body = v0Advertisement(ctx.Store, repo, service)
		}
		SendRaw(ctx.W, 200, body, "application/x-"+service+"-advertisement", gitHeaders(ctx))
		return true
	}

	if ctx.Method == "POST" && len(rest) == 1 && rest[0] == "git-upload-pack" {
		isV2 := ctx.R.Header.Get("Git-Protocol") == "version=2"
		var body []byte
		if isV2 {
			body = handleUploadPackV2(ctx, repo)
		} else {
			body = handleUploadPackV0(ctx)
		}
		SendRaw(ctx.W, 200, body, "application/x-git-upload-pack-result", gitHeaders(ctx))
		return true
	}

	if ctx.Method == "POST" && len(rest) == 1 && rest[0] == "git-receive-pack" {
		body := handleReceivePack(ctx, repo)
		SendRaw(ctx.W, 200, body, "application/x-git-receive-pack-result", gitHeaders(ctx))
		return true
	}

	return false
}
