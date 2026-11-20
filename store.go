// Shared store: all state (repositories, git objects, refs, issues, labels,
// pull requests, comments) lives here. Every surface reads and writes
// through this file and nowhere else.
//
// Owner adoption: a repository is created with OwnerLogin == "", and the
// FIRST lookup by (owner, name) that finds it adopts that owner. Any later
// lookup under a different owner is a miss. Never invent an owner name.
//
// Determinism: timestamps come from a counter, not the clock. NowISO returns
// 2025-01-01T00:00:00Z plus seq seconds, incrementing seq on every call, so
// two runs that make the same requests in the same order produce the same
// state. Never use time.Now or math/rand for stored data.
//
// Concurrency: the HTTP server serializes whole requests under one mutex
// (like the single-threaded Node original), so no locking lives in this file.
// Git objects are stored verbatim; commits the server creates use
// BuildCommit in the canonical git format so a clone round-trips them
// byte-exactly.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var tagObjectRe = regexp.MustCompile(`(?m)^object ([0-9a-f]{40})$`)

type GitObjectType string

const (
	ObjCommit GitObjectType = "commit"
	ObjTree   GitObjectType = "tree"
	ObjBlob   GitObjectType = "blob"
	ObjTag    GitObjectType = "tag"
)

type GitObject struct {
	Type GitObjectType
	Data []byte
}

type Author struct {
	Name      string
	Email     string
	Timestamp int64
}

type IssueComment struct {
	ID        int
	Body      string
	UserLogin string
	CreatedAt string
	UpdatedAt string
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	UserLogin string
	CreatedAt string
	UpdatedAt string
	Comments  []*IssueComment
}

type Label struct {
	Name        string
	Color       string
	Description string
}

type PullFile struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
	Changes   int
	Sha       string
}

type PullRequest struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	Files          []*PullFile
	CreatedAt      string
	UpdatedAt      string
}

type Repo struct {
	ID              int
	Name            string
	OwnerLogin      string // "" until adopted on first lookup
	Description     string
	Private         bool
	DefaultBranch   string
	CreatedAt       string
	UpdatedAt       string
	Refs            map[string]string // refs/heads/<branch> -> commit oid; refs/tags/<tag> -> oid
	Labels          map[string]*Label
	Issues          []*Issue
	Pulls           []*PullRequest
	NextIssueNumber int
	NextPullNumber  int
	NextCommentID   int
}

func HashObject(typ GitObjectType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ParseCommit parses a commit object body into its fields. Returns nil when malformed.
type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func ParseCommit(data []byte) *CommitInfo {
	text := string(data)
	split := strings.Index(text, "\n\n")
	if split == -1 {
		return nil
	}
	head := text[:split]
	message := text[split+2:]
	out := &CommitInfo{Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			out.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			out.Parents = append(out.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			out.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			out.Committer = line[10:]
		}
	}
	if out.Tree == "" {
		return nil
	}
	return out
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree object body into entries.
func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	i := 0
	for i < len(data) {
		space := bytes.IndexByte(data[i:], ' ')
		if space == -1 {
			break
		}
		space += i
		mode := string(data[i:space])
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul == -1 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		i = nul + 21
	}
	return entries
}

type Store struct {
	Repos           map[string]*Repo // keyed by repo name (single token user)
	ReposByID       map[int]*Repo
	Objects         map[string]*GitObject
	nextRepoID      int
	nextObjectUserID int
	seq             int
}

func NewStore() *Store {
	return &Store{
		Repos:      map[string]*Repo{},
		ReposByID:  map[int]*Repo{},
		Objects:    map[string]*GitObject{},
		nextRepoID: 5000,
		nextObjectUserID: 1000,
	}
}

var baseTime = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// NowISO: deterministic timestamp, base epoch plus one second per call.
func (s *Store) NowISO() string {
	s.seq++
	return baseTime.Add(time.Duration(s.seq) * time.Second).Format("2006-01-02T15:04:05Z")
}

// NowUnix: deterministic unix timestamp for git objects.
func (s *Store) NowUnix() int64 {
	return 1735689600 + int64(s.seq)
}

// ViewerLogin: the login the token user is known by once an owner has been adopted anywhere.
// Deterministic: lowest repo id first (Node's Map preserves insertion order;
// Go map iteration is random, so sort by id).
func (s *Store) ViewerLogin() string {
	for _, repo := range s.ListRepos() {
		if repo.OwnerLogin != "" {
			return repo.OwnerLogin
		}
	}
	return "viewer"
}

// ------------------------------------------------------------ repositories

func (s *Store) CreateRepo(name string, private bool, description string) *Repo {
	if _, ok := s.Repos[name]; ok {
		return nil
	}
	now := s.NowISO()
	repo := &Repo{
		ID:              s.nextRepoID,
		Name:            name,
		OwnerLogin:      "",
		Description:     description,
		Private:         private,
		DefaultBranch:   "main",
		CreatedAt:       now,
		UpdatedAt:       now,
		Refs:            map[string]string{},
		Labels:          map[string]*Label{},
		Issues:          []*Issue{},
		Pulls:           []*PullRequest{},
		NextIssueNumber: 1,
		NextPullNumber:  1,
		NextCommentID:   1,
	}
	s.nextRepoID++
	s.nextObjectUserID++
	s.Repos[name] = repo
	s.ReposByID[repo.ID] = repo
	return repo
}

// GetRepo: lookup by (owner, name), adopting the owner on first successful lookup.
func (s *Store) GetRepo(owner, name string) *Repo {
	repo, ok := s.Repos[name]
	if !ok {
		return nil
	}
	if repo.OwnerLogin == "" {
		repo.OwnerLogin = owner
	}
	if repo.OwnerLogin != owner {
		return nil
	}
	return repo
}

func (s *Store) GetRepoByName(name string) *Repo {
	return s.Repos[name]
}

func (s *Store) ListRepos() []*Repo {
	out := make([]*Repo, 0, len(s.Repos))
	for _, r := range s.Repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) DeleteRepo(name string) bool {
	repo, ok := s.Repos[name]
	if !ok {
		return false
	}
	delete(s.Repos, name)
	delete(s.ReposByID, repo.ID)
	return true
}

func (s *Store) RepoFullName(repo *Repo) string {
	owner := repo.OwnerLogin
	if owner == "" {
		owner = "viewer"
	}
	return owner + "/" + repo.Name
}

// ------------------------------------------------------------ git objects

func (s *Store) PutObject(typ GitObjectType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := s.Objects[oid]; !ok {
		s.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

func (s *Store) GetObject(oid string) *GitObject {
	return s.Objects[oid]
}

func (s *Store) BlobOID(content []byte) string {
	return s.PutObject(ObjBlob, content)
}

type treeNode struct {
	files map[string][]byte
	dirs  map[string]*treeNode
}

func newTreeNode() *treeNode {
	return &treeNode{files: map[string][]byte{}, dirs: map[string]*treeNode{}}
}

// TreeOIDFromFiles builds (and stores) a tree from a flat path->content map.
func (s *Store) TreeOIDFromFiles(files map[string][]byte) string {
	root := newTreeNode()
	for path, content := range files {
		parts := []string{}
		for _, p := range strings.Split(path, "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) == 0 {
			continue
		}
		node := root
		for i := 0; i < len(parts)-1; i++ {
			part := parts[i]
			child, ok := node.dirs[part]
			if !ok {
				child = newTreeNode()
				node.dirs[part] = child
			}
			node = child
		}
		node.files[parts[len(parts)-1]] = content
	}
	var build func(node *treeNode) string
	build = func(node *treeNode) string {
		type entry struct {
			mode, name, oid string
		}
		var entries []entry
		for name, content := range node.files {
			entries = append(entries, entry{"100644", name, s.BlobOID(content)})
		}
		for name, child := range node.dirs {
			entries = append(entries, entry{"40000", name, build(child)})
		}
		sort.Slice(entries, func(i, j int) bool {
			an, bn := entries[i].name, entries[j].name
			if entries[i].mode == "40000" {
				an += "/"
			}
			if entries[j].mode == "40000" {
				bn += "/"
			}
			return an < bn
		})
		var buf bytes.Buffer
		for _, e := range entries {
			buf.WriteString(e.mode + " " + e.name + "\x00")
			raw, _ := hex.DecodeString(e.oid)
			buf.Write(raw)
		}
		return s.PutObject(ObjTree, buf.Bytes())
	}
	return build(root)
}

// FilesAtTree flattens a tree oid into path->content, following nested trees.
func (s *Store) FilesAtTree(treeOID string) map[string][]byte {
	out := map[string][]byte{}
	s.filesAtTree(treeOID, "", out)
	return out
}

func (s *Store) filesAtTree(treeOID, prefix string, out map[string][]byte) {
	obj := s.Objects[treeOID]
	if obj == nil || obj.Type != ObjTree {
		return
	}
	for _, e := range ParseTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			s.filesAtTree(e.OID, path, out)
		} else {
			if blob := s.Objects[e.OID]; blob != nil {
				out[path] = blob.Data
			}
		}
	}
}

// FilesAtRef: files at a ref (refs/heads/<branch>), or empty when the ref does not exist.
func (s *Store) FilesAtRef(repo *Repo, ref string) map[string][]byte {
	oid, ok := repo.Refs[ref]
	if !ok {
		return map[string][]byte{}
	}
	obj := s.Objects[oid]
	if obj == nil || obj.Type != ObjCommit {
		return map[string][]byte{}
	}
	commit := ParseCommit(obj.Data)
	if commit == nil {
		return map[string][]byte{}
	}
	return s.FilesAtTree(commit.Tree)
}

// BuildCommit builds and stores a commit object. Returns its oid.
func (s *Store) BuildCommit(tree string, parents []string, message, authorName, authorEmail string, timestamp int64) string {
	ts := timestamp
	if ts == 0 {
		ts = s.NowUnix()
	}
	ident := fmt.Sprintf("%s <%s> %d +0000", authorName, authorEmail, ts)
	lines := []string{"tree " + tree}
	for _, p := range parents {
		lines = append(lines, "parent "+p)
	}
	lines = append(lines, "author "+ident, "committer "+ident, "", message)
	return s.PutObject(ObjCommit, []byte(strings.Join(lines, "\n")))
}

// IsAncestor: true when `ancestor` is reachable from `descendant` (or equal), walking parents.
func (s *Store) IsAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := s.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commit := ParseCommit(obj.Data)
		if commit == nil {
			continue
		}
		for _, p := range commit.Parents {
			if p == ancestor {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

// ReachableObjects: all objects reachable from a set of tip oids.
func (s *Store) ReachableObjects(tips []string) map[string]*GitObject {
	out := map[string]*GitObject{}
	queue := append([]string{}, tips...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if _, ok := out[oid]; ok {
			continue
		}
		obj := s.Objects[oid]
		if obj == nil {
			continue
		}
		out[oid] = obj
		switch obj.Type {
		case ObjCommit:
			if c := ParseCommit(obj.Data); c != nil {
				queue = append(queue, c.Tree)
				queue = append(queue, c.Parents...)
			}
		case ObjTree:
			for _, e := range ParseTree(obj.Data) {
				queue = append(queue, e.OID)
			}
		case ObjTag:
			m := tagObjectRe.FindStringSubmatch(string(obj.Data))
			if m != nil {
				queue = append(queue, m[1])
			}
		}
	}
	return out
}

// ------------------------------------------------------------ contents / commits

// CommitFile commits one file on a branch (REST contents PUT). Creates the
// branch when it is unborn (no parent commit).
func (s *Store) CommitFile(repo *Repo, branch, path string, content []byte, message string) (commitSha, blobSha string) {
	ref := "refs/heads/" + branch
	files := s.FilesAtRef(repo, ref)
	blobSha = s.BlobOID(content)
	files[path] = content
	tree := s.TreeOIDFromFiles(files)
	var parents []string
	if tip, ok := repo.Refs[ref]; ok {
		parents = []string{tip}
	}
	login := repo.OwnerLogin
	if login == "" {
		login = "viewer"
	}
	commitSha = s.BuildCommit(tree, parents, message, login, login+"@example.invalid", 0)
	repo.Refs[ref] = commitSha
	repo.UpdatedAt = s.NowISO()
	return commitSha, blobSha
}

// DeleteFile deletes one file on a branch, committing the removal.
// Returns ("", false) when the file is absent.
func (s *Store) DeleteFile(repo *Repo, branch, path, message string) (string, bool) {
	ref := "refs/heads/" + branch
	files := s.FilesAtRef(repo, ref)
	if _, ok := files[path]; !ok {
		return "", false
	}
	delete(files, path)
	tree := s.TreeOIDFromFiles(files)
	var parents []string
	if tip, ok := repo.Refs[ref]; ok {
		parents = []string{tip}
	}
	login := repo.OwnerLogin
	if login == "" {
		login = "viewer"
	}
	commitSha := s.BuildCommit(tree, parents, message, login, login+"@example.invalid", 0)
	repo.Refs[ref] = commitSha
	repo.UpdatedAt = s.NowISO()
	return commitSha, true
}

// ------------------------------------------------------------ issues / pulls

// CreateIssue creates an issue. errLabel is non-empty when a label is unknown.
func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) (*Issue, string) {
	for _, l := range labels {
		if _, ok := repo.Labels[l]; !ok {
			return nil, l
		}
	}
	now := s.NowISO()
	issue := &Issue{
		ID:        repo.ID*100000 + repo.NextIssueNumber,
		Number:    repo.NextIssueNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string{}, labels...),
		UserLogin: firstNonEmpty(repo.OwnerLogin, "viewer"),
		CreatedAt: now,
		UpdatedAt: now,
		Comments:  []*IssueComment{},
	}
	repo.NextIssueNumber++
	repo.Issues = append(repo.Issues, issue)
	return issue, ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func lineCount(b []byte) int {
	return len(strings.Split(string(b), "\n"))
}

// CreatePull creates a pull request. errMsg is non-empty on failure.
func (s *Store) CreatePull(repo *Repo, title, body, head, base string) (*PullRequest, string) {
	headRef := "refs/heads/" + head
	baseRef := "refs/heads/" + base
	headSha, ok1 := repo.Refs[headRef]
	baseSha, ok2 := repo.Refs[baseRef]
	if !ok1 || !ok2 {
		return nil, "head or base branch not found"
	}
	now := s.NowISO()
	headFiles := s.FilesAtRef(repo, headRef)
	baseFiles := s.FilesAtRef(repo, baseRef)
	var files []*PullFile
	all := map[string]bool{}
	for k := range headFiles {
		all[k] = true
	}
	for k := range baseFiles {
		all[k] = true
	}
	paths := make([]string, 0, len(all))
	for k := range all {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	for _, path := range paths {
		h, hasH := headFiles[path]
		b, hasB := baseFiles[path]
		switch {
		case hasH && !hasB:
			n := lineCount(h)
			files = append(files, &PullFile{Filename: path, Status: "added", Additions: n, Deletions: 0, Changes: n, Sha: s.BlobOID(h)})
		case !hasH && hasB:
			n := lineCount(b)
			files = append(files, &PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: n, Changes: n, Sha: s.BlobOID(b)})
		case hasH && hasB && !bytes.Equal(h, b):
			a, d := lineCount(h), lineCount(b)
			files = append(files, &PullFile{Filename: path, Status: "modified", Additions: a, Deletions: d, Changes: a + d, Sha: s.BlobOID(h)})
		}
	}
	pull := &PullRequest{
		ID:        repo.ID*100000 + repo.NextPullNumber,
		Number:    repo.NextPullNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   head,
		BaseRef:   base,
		HeadSha:   headSha,
		BaseSha:   baseSha,
		Merged:    false,
		MergedAt:  "",
		Files:     files,
		CreatedAt: now,
		UpdatedAt: now,
	}
	repo.NextPullNumber++
	repo.Pulls = append(repo.Pulls, pull)
	return pull, ""
}

// MergePull squash-merges a pull: applies head files onto base, commits on
// base, closes the pull. Returns (sha, "") or ("", errMsg).
func (s *Store) MergePull(repo *Repo, pull *PullRequest) (string, string) {
	if pull.State != "open" {
		return "", "already closed"
	}
	baseRef := "refs/heads/" + pull.BaseRef
	headRef := "refs/heads/" + pull.HeadRef
	baseFiles := s.FilesAtRef(repo, baseRef)
	headFiles := s.FilesAtRef(repo, headRef)
	for path, content := range headFiles {
		baseFiles[path] = content
	}
	tree := s.TreeOIDFromFiles(baseFiles)
	var parents []string
	if baseTip, ok := repo.Refs[baseRef]; ok {
		parents = []string{baseTip}
	}
	login := firstNonEmpty(repo.OwnerLogin, "viewer")
	sha := s.BuildCommit(tree, parents, fmt.Sprintf("%s (#%d)", pull.Title, pull.Number), login, login+"@example.invalid", 0)
	repo.Refs[baseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = s.NowISO()
	pull.MergeCommitSha = sha
	pull.UpdatedAt = pull.MergedAt
	repo.UpdatedAt = pull.MergedAt
	return sha, ""
}
