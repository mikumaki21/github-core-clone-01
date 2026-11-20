// mcp.go — MCP surface (JSON-RPC 2.0 over Streamable HTTP).
//
// POST /mcp. Two protocol eras are served:
//   modern 2026-07-28: server/discover, no handshake, no session required.
//   legacy 2025-11-25 and earlier: initialize + notifications/initialized,
//     with an optional Mcp-Session-Id header (accepted and echoed, never required).
// Every tool call acts on the shared store, so a repo/issue/pull created over
// MCP is the same object (same id) over REST, and vice versa.
//
// Port of mcp.ts from the Node entry. Observable behaviour is identical;
// field order in JSON views is preserved with ordered structs, never maps.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

const mcpServerName = "github-forge"
const mcpServerVersion = "1.0.0"
const mcpCurrentRevision = "2026-07-28"

var mcpSupportedVersions = []string{
	"2026-07-28",
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// The tool list is a constant, so two tools/list calls answer byte for byte
// identically. Property names are the github-mcp-server ones the suite maps
// its documented arguments onto.
const mcpToolsJSON = `[{"name":"create_repo","description":"Create a new repository for the authenticated user.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Name of the repository to create."},"description":{"type":"string","description":"A short description of the repository."},"private":{"type":"boolean","description":"Whether the repository is private."}},"required":["name"]}},{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The login of the repository owner."},"repo":{"type":"string","description":"The name of the repository."}},"required":["owner","repo"]}},{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The login of the repository owner."},"repo":{"type":"string","description":"The name of the repository."},"title":{"type":"string","description":"Title of the issue."},"body":{"type":"string","description":"Body of the issue."},"labels":{"type":"array","items":{"type":"string"},"description":"Labels to apply to the issue."}},"required":["owner","repo","title"]}},{"name":"list_issues","description":"List issues in a repository, optionally filtered by state.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The login of the repository owner."},"repo":{"type":"string","description":"The name of the repository."},"state":{"type":"string","description":"Issue state to list: open, closed, or all."}},"required":["owner","repo"]}},{"name":"create_pull_request","description":"Create a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The login of the repository owner."},"repo":{"type":"string","description":"The name of the repository."},"title":{"type":"string","description":"Title of the pull request."},"body":{"type":"string","description":"Body of the pull request."},"head":{"type":"string","description":"The branch with the changes (head)."},"base":{"type":"string","description":"The branch the changes are pulled into (base)."}},"required":["owner","repo","title","head","base"]}},{"name":"merge_pull_request","description":"Merge a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"The login of the repository owner."},"repo":{"type":"string","description":"The name of the repository."},"pull_number":{"type":"integer","description":"Number of the pull request to merge."}},"required":["owner","repo","pull_number"]}}]`

var mcpCanonicalTools = []string{
	"create_repo",
	"get_repo",
	"create_issue",
	"list_issues",
	"create_pull_request",
	"merge_pull_request",
}

// ------------------------------------------------------------ JSON-RPC types

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

func rpcOK(id any, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErr(id any, code int, message string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpCapabilities struct {
	Tools struct{} `json:"tools"`
}

func mcpInfo() mcpServerInfo { return mcpServerInfo{Name: mcpServerName, Version: mcpServerVersion} }

func isRecord(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func normalizeToolName(name string) string {
	lower := strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// matchTool matches a tool name by suffix, case-insensitively, allowing a
// server prefix.
func matchTool(name string) (string, bool) {
	wanted := normalizeToolName(name)
	for _, canonical := range mcpCanonicalTools {
		norm := normalizeToolName(canonical)
		if wanted == norm || strings.HasSuffix(wanted, norm) {
			return canonical, true
		}
	}
	return "", false
}

// ------------------------------------------------------------ argument mapping

func pickString(args map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if v, ok := args[key].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

func pickNumber(args map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		v, ok := args[key]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				continue
			}
			return int(math.Trunc(n)), true
		case string:
			t := strings.TrimSpace(n)
			if t == "" {
				continue
			}
			digits := true
			for i := 0; i < len(t); i++ {
				if t[i] < '0' || t[i] > '9' {
					digits = false
					break
				}
			}
			if !digits {
				continue
			}
			if parsed, err := strconv.Atoi(t); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// resolveOwnerRepo resolves owner/repo, accepting a full_name ("owner/repo")
// when either is absent.
func resolveOwnerRepo(args map[string]any) (owner, repo string, hasOwner, hasRepo bool) {
	owner, hasOwner = pickString(args, "owner", "owner_login", "org", "organization", "user", "user_login", "login", "owner_name")
	repo, hasRepo = pickString(args, "repo", "repository", "repository_name", "repo_name")
	if !hasOwner || !hasRepo {
		if full, ok := pickString(args, "full_name", "repo_full_name", "repository_full_name", "fullName"); ok && strings.Contains(full, "/") {
			parts := strings.SplitN(full, "/", 2)
			if !hasOwner && parts[0] != "" {
				owner, hasOwner = parts[0], true
			}
			if !hasRepo && parts[1] != "" {
				repo, hasRepo = parts[1], true
			}
		}
	}
	return owner, repo, hasOwner, hasRepo
}

// ------------------------------------------------------------ REST-shaped views

func mcpOrigin(ctx *Ctx) string {
	if host := ctx.R.Host; host != "" {
		return "http://" + host
	}
	return "http://127.0.0.1"
}

func mcpOwnerLogin(repo *Repo) string {
	if repo.OwnerLogin != "" {
		return repo.OwnerLogin
	}
	return "viewer"
}

type mcpRepoOwner struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type mcpRepoJSON struct {
	ID            int          `json:"id"`
	Name          string       `json:"name"`
	FullName      string       `json:"full_name"`
	Owner         mcpRepoOwner `json:"owner"`
	Private       bool         `json:"private"`
	Description   string       `json:"description"`
	DefaultBranch string       `json:"default_branch"`
	CreatedAt     string       `json:"created_at"`
	UpdatedAt     string       `json:"updated_at"`
	HTMLURL       string       `json:"html_url"`
	URL           string       `json:"url"`
	CloneURL      string       `json:"clone_url"`
	GitURL        string       `json:"git_url"`
	SSHURL        string       `json:"ssh_url"`
}

func repoJSON(ctx *Ctx, repo *Repo) mcpRepoJSON {
	owner := mcpOwnerLogin(repo)
	full := owner + "/" + repo.Name
	origin := mcpOrigin(ctx)
	return mcpRepoJSON{
		ID:            repo.ID,
		Name:          repo.Name,
		FullName:      full,
		Owner:         mcpRepoOwner{Login: owner, ID: 1, Type: "User"},
		Private:       repo.Private,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
		CreatedAt:     repo.CreatedAt,
		UpdatedAt:     repo.UpdatedAt,
		HTMLURL:       origin + "/" + full,
		URL:           origin + "/api/v3/repos/" + full,
		CloneURL:      origin + "/" + full + ".git",
		GitURL:        origin + "/" + full + ".git",
		SSHURL:        "git@example.invalid:" + full + ".git",
	}
}

type mcpIssueLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type mcpIssueUser struct {
	Login string `json:"login"`
}

type mcpIssueJSON struct {
	ID        int             `json:"id"`
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	State     string          `json:"state"`
	Labels    []mcpIssueLabel `json:"labels"`
	User      mcpIssueUser    `json:"user"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	HTMLURL   string          `json:"html_url"`
	URL       string          `json:"url"`
}

func issueJSON(ctx *Ctx, repo *Repo, issue *Issue) mcpIssueJSON {
	full := mcpOwnerLogin(repo) + "/" + repo.Name
	origin := mcpOrigin(ctx)
	labels := make([]mcpIssueLabel, 0, len(issue.Labels))
	for _, n := range issue.Labels {
		l := mcpIssueLabel{Name: n, Color: "ededed", Description: ""}
		if lab, ok := repo.Labels[n]; ok && lab != nil {
			l.Color = lab.Color
			l.Description = lab.Description
		}
		labels = append(labels, l)
	}
	return mcpIssueJSON{
		ID:        issue.ID,
		Number:    issue.Number,
		Title:     issue.Title,
		Body:      issue.Body,
		State:     issue.State,
		Labels:    labels,
		User:      mcpIssueUser{Login: issue.UserLogin},
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.UpdatedAt,
		HTMLURL:   origin + "/" + full + "/issues/" + strconv.Itoa(issue.Number),
		URL:       origin + "/api/v3/repos/" + full + "/issues/" + strconv.Itoa(issue.Number),
	}
}

type mcpPullRef struct {
	Ref   string `json:"ref"`
	Sha   string `json:"sha"`
	Label string `json:"label"`
}

type mcpPullJSON struct {
	ID             int        `json:"id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Head           mcpPullRef `json:"head"`
	Base           mcpPullRef `json:"base"`
	Merged         bool       `json:"merged"`
	MergedAt       string     `json:"merged_at"`
	MergeCommitSha string     `json:"merge_commit_sha"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
	HTMLURL        string     `json:"html_url"`
	URL            string     `json:"url"`
}

func pullJSON(ctx *Ctx, repo *Repo, pull *PullRequest) mcpPullJSON {
	full := mcpOwnerLogin(repo) + "/" + repo.Name
	owner := mcpOwnerLogin(repo)
	origin := mcpOrigin(ctx)
	return mcpPullJSON{
		ID:             pull.ID,
		Number:         pull.Number,
		Title:          pull.Title,
		Body:           pull.Body,
		State:          pull.State,
		Head:           mcpPullRef{Ref: pull.HeadRef, Sha: pull.HeadSha, Label: owner + ":" + pull.HeadRef},
		Base:           mcpPullRef{Ref: pull.BaseRef, Sha: pull.BaseSha, Label: owner + ":" + pull.BaseRef},
		Merged:         pull.Merged,
		MergedAt:       pull.MergedAt,
		MergeCommitSha: pull.MergeCommitSha,
		CreatedAt:      pull.CreatedAt,
		UpdatedAt:      pull.UpdatedAt,
		HTMLURL:        origin + "/" + full + "/pull/" + strconv.Itoa(pull.Number),
		URL:            origin + "/api/v3/repos/" + full + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

// ------------------------------------------------------------ merge conflicts

// filesAtCommit returns the files at a commit oid (empty when the commit
// does not exist).
func filesAtCommit(ctx *Ctx, oid string) map[string][]byte {
	obj := ctx.Store.GetObject(oid)
	if obj == nil || obj.Type != ObjCommit {
		return map[string][]byte{}
	}
	commit := ParseCommit(obj.Data)
	if commit == nil {
		return map[string][]byte{}
	}
	return ctx.Store.FilesAtTree(commit.Tree)
}

// mergeBase returns the first common ancestor of two commits, or "" when the
// histories are disjoint.
func mergeBase(ctx *Ctx, a, b string) string {
	ancestors := map[string]bool{}
	stack := []string{a}
	guard := 0
	for len(stack) > 0 && guard < 100000 {
		guard++
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancestors[oid] {
			continue
		}
		ancestors[oid] = true
		obj := ctx.Store.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commit := ParseCommit(obj.Data)
		if commit == nil {
			continue
		}
		stack = append(stack, commit.Parents...)
	}
	queue := []string{b}
	guard = 0
	for len(queue) > 0 && guard < 100000 {
		guard++
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if ancestors[oid] {
			return oid
		}
		obj := ctx.Store.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		commit := ParseCommit(obj.Data)
		if commit == nil {
			continue
		}
		queue = append(queue, commit.Parents...)
	}
	return ""
}

// mergeConflict is the merge-conflict check for one pull, or "" when the
// merge may proceed.
func mergeConflict(ctx *Ctx, repo *Repo, pull *PullRequest) string {
	baseTip, okBase := repo.Refs["refs/heads/"+pull.BaseRef]
	headTip, okHead := repo.Refs["refs/heads/"+pull.HeadRef]
	if !okBase || !okHead {
		return "head or base branch not found"
	}
	if baseTip == headTip {
		return ""
	}
	if ctx.Store.IsAncestor(baseTip, headTip) || ctx.Store.IsAncestor(headTip, baseTip) {
		return ""
	}
	base := mergeBase(ctx, baseTip, headTip)
	baseFiles := map[string][]byte{}
	if base != "" {
		baseFiles = filesAtCommit(ctx, base)
	}
	headFiles := filesAtCommit(ctx, headTip)
	baseNow := filesAtCommit(ctx, baseTip)
	changedOn := func(now map[string][]byte) map[string]bool {
		changed := map[string]bool{}
		for path, content := range now {
			old, ok := baseFiles[path]
			if !ok || !bytes.Equal(old, content) {
				changed[path] = true
			}
		}
		for path := range baseFiles {
			if _, ok := now[path]; !ok {
				changed[path] = true
			}
		}
		return changed
	}
	headChanged := changedOn(headFiles)
	baseChanged := changedOn(baseNow)
	// Sorted for a deterministic answer when several paths conflict; the
	// Node original reports the first in its own (insertion) order.
	paths := make([]string, 0, len(headChanged))
	for path := range headChanged {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if !baseChanged[path] {
			continue
		}
		h, hasH := headFiles[path]
		b, hasB := baseNow[path]
		if hasH != hasB {
			return "Merge conflict: " + path + " changed on both head and base"
		}
		if hasH && hasB && !bytes.Equal(h, b) {
			return "Merge conflict: " + path + " changed on both head and base"
		}
	}
	return ""
}

// ------------------------------------------------------------ tool calls

type toolOutcome struct {
	ok      bool
	result  any
	message string
}

func toolError(message string) toolOutcome { return toolOutcome{ok: false, message: message} }

func callTool(ctx *Ctx, canonical string, args map[string]any) toolOutcome {
	store := ctx.Store
	switch canonical {
	case "create_repo":
		name, ok := pickString(args, "name", "repo", "repository", "repository_name", "repo_name")
		if !ok {
			return toolError("Missing required argument: name")
		}
		description, _ := pickString(args, "description", "about", "summary", "body")
		privRaw, hasPriv := args["private"]
		if !hasPriv {
			privRaw = args["is_private"]
		}
		isPrivate := privRaw == true || privRaw == "true"
		repo := store.CreateRepo(name, isPrivate, description)
		if repo == nil {
			return toolError("Repository already exists: " + name)
		}
		return toolOutcome{ok: true, result: repoJSON(ctx, repo)}
	case "get_repo":
		owner, name, hasOwner, hasRepo := resolveOwnerRepo(args)
		if !hasOwner {
			return toolError("Missing required argument: owner")
		}
		if !hasRepo {
			return toolError("Missing required argument: repo")
		}
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return toolError("Not Found: repository " + owner + "/" + name + " does not exist")
		}
		return toolOutcome{ok: true, result: repoJSON(ctx, repo)}
	case "create_issue":
		owner, name, hasOwner, hasRepo := resolveOwnerRepo(args)
		if !hasOwner {
			return toolError("Missing required argument: owner")
		}
		if !hasRepo {
			return toolError("Missing required argument: repo")
		}
		title, ok := pickString(args, "title", "issue_title", "subject")
		if !ok {
			return toolError("Missing required argument: title")
		}
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return toolError("Not Found: repository " + owner + "/" + name + " does not exist")
		}
		body, _ := pickString(args, "body", "issue_body", "description", "content", "text")
		var labels []string
		if raw, ok := args["labels"].([]any); ok {
			for _, l := range raw {
				if s, ok := l.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		created, errLabel := store.CreateIssue(repo, title, body, labels)
		if errLabel != "" {
			return toolError("Label does not exist: " + errLabel)
		}
		return toolOutcome{ok: true, result: issueJSON(ctx, repo, created)}
	case "list_issues":
		owner, name, hasOwner, hasRepo := resolveOwnerRepo(args)
		if !hasOwner {
			return toolError("Missing required argument: owner")
		}
		if !hasRepo {
			return toolError("Missing required argument: repo")
		}
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return toolError("Not Found: repository " + owner + "/" + name + " does not exist")
		}
		state, _ := pickString(args, "state", "status")
		if state == "" {
			state = "open"
		}
		issues := make([]mcpIssueJSON, 0, len(repo.Issues))
		for _, issue := range repo.Issues {
			if state == "all" || issue.State == state {
				issues = append(issues, issueJSON(ctx, repo, issue))
			}
		}
		return toolOutcome{ok: true, result: issues}
	case "create_pull_request":
		owner, name, hasOwner, hasRepo := resolveOwnerRepo(args)
		if !hasOwner {
			return toolError("Missing required argument: owner")
		}
		if !hasRepo {
			return toolError("Missing required argument: repo")
		}
		title, ok := pickString(args, "title", "pr_title", "pull_request_title", "subject")
		if !ok {
			return toolError("Missing required argument: title")
		}
		head, ok := pickString(args, "head", "head_ref", "head_branch", "source", "source_branch", "from_branch")
		if !ok {
			return toolError("Missing required argument: head")
		}
		base, ok := pickString(args, "base", "base_ref", "base_branch", "target", "target_branch", "into_branch")
		if !ok {
			return toolError("Missing required argument: base")
		}
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return toolError("Not Found: repository " + owner + "/" + name + " does not exist")
		}
		body, _ := pickString(args, "body", "description", "content", "text")
		created, errMsg := store.CreatePull(repo, title, body, head, base)
		if errMsg != "" {
			return toolError(errMsg)
		}
		return toolOutcome{ok: true, result: pullJSON(ctx, repo, created)}
	case "merge_pull_request":
		owner, name, hasOwner, hasRepo := resolveOwnerRepo(args)
		if !hasOwner {
			return toolError("Missing required argument: owner")
		}
		if !hasRepo {
			return toolError("Missing required argument: repo")
		}
		number, ok := pickNumber(args, "pull_number", "pull_request_number", "pr_number", "number", "issue_number", "index")
		if !ok {
			return toolError("Missing required argument: pull_number")
		}
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return toolError("Not Found: repository " + owner + "/" + name + " does not exist")
		}
		var pull *PullRequest
		for _, p := range repo.Pulls {
			if p.Number == number {
				pull = p
				break
			}
		}
		if pull == nil {
			return toolError("Not Found: pull request #" + strconv.Itoa(number) + " does not exist")
		}
		if pull.State != "open" {
			return toolError("Pull request #" + strconv.Itoa(number) + " is already closed")
		}
		if conflict := mergeConflict(ctx, repo, pull); conflict != "" {
			return toolError(conflict)
		}
		if _, errMsg := store.MergePull(repo, pull); errMsg != "" {
			return toolError(errMsg)
		}
		return toolOutcome{ok: true, result: pullJSON(ctx, repo, pull)}
	}
	return toolError("Unknown tool: " + canonical)
}

// ------------------------------------------------------------ JSON-RPC dispatch

type mcpContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content           []mcpContentPart `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
}

type mcpDiscoverResult struct {
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      mcpCapabilities `json:"capabilities"`
	ServerInfo        mcpServerInfo   `json:"serverInfo"`
	Meta              mcpDiscoverMeta `json:"_meta"`
}

type mcpDiscoverMeta struct {
	ServerInfo mcpServerInfo `json:"io.modelcontextprotocol/serverInfo"`
}

type mcpInitializeResult struct {
	ProtocolVersion string           `json:"protocolVersion"`
	Capabilities    mcpCapabilities  `json:"capabilities"`
	ServerInfo      mcpServerInfo    `json:"serverInfo"`
}

type mcpToolsListResult struct {
	Tools json.RawMessage `json:"tools"`
}

// handleRequest maps one JSON-RPC request object to its response, or nil for
// a notification.
func handleRequest(ctx *Ctx, req any) (resp *rpcResponse) {
	rec, ok := isRecord(req)
	if !ok {
		return rpcErr(nil, -32600, "Invalid Request")
	}
	var id any
	if raw, present := rec["id"]; present {
		switch raw.(type) {
		case string, float64:
			id = raw
		default:
			id = nil
		}
	} else {
		id = nil
	}
	_, hasID := rec["id"]
	defer func() {
		if rec := recover(); rec != nil {
			resp = rpcErr(id, -32603, fmt.Sprint(rec))
		}
	}()
	method, _ := rec["method"].(string)
	if method == "" {
		// A notification with no method is dropped silently; a request gets -32600.
		if hasID {
			return rpcErr(id, -32600, "Invalid Request: missing method")
		}
		return nil
	}
	if !hasID {
		// Notification: no response, ever (202 with no body at the HTTP layer).
		return nil
	}
	params, _ := isRecord(rec["params"])
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "server/discover":
		info := mcpInfo()
		return rpcOK(id, mcpDiscoverResult{
			SupportedVersions: append([]string{}, mcpSupportedVersions...),
			Capabilities:      mcpCapabilities{},
			ServerInfo:        info,
			Meta:              mcpDiscoverMeta{ServerInfo: info},
		})
	case "initialize":
		offered := mcpCurrentRevision
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			offered = v
		} else if meta, ok := isRecord(params["_meta"]); ok {
			if v, ok := meta["protocolVersion"].(string); ok {
				offered = v
			}
		}
		negotiated := mcpCurrentRevision
		for _, supported := range mcpSupportedVersions {
			if offered == supported {
				negotiated = offered
				break
			}
		}
		return rpcOK(id, mcpInitializeResult{
			ProtocolVersion: negotiated,
			Capabilities:    mcpCapabilities{},
			ServerInfo:      mcpInfo(),
		})
	case "ping":
		return rpcOK(id, struct{}{})
	case "tools/list":
		return rpcOK(id, mcpToolsListResult{Tools: json.RawMessage(mcpToolsJSON)})
	case "tools/call":
		name, _ := params["name"].(string)
		if name == "" {
			return rpcErr(id, -32602, "Invalid params: missing tool name")
		}
		canonical, matched := matchTool(name)
		if !matched {
			return rpcErr(id, -32602, "Unknown tool: "+name)
		}
		args, _ := isRecord(params["arguments"])
		if args == nil {
			args = map[string]any{}
		}
		outcome := callTool(ctx, canonical, args)
		if !outcome.ok {
			return rpcOK(id, mcpCallResult{
				Content: []mcpContentPart{{Type: "text", Text: outcome.message}},
				IsError: true,
			})
		}
		text, err := json.Marshal(outcome.result)
		if err != nil {
			return rpcErr(id, -32603, err.Error())
		}
		return rpcOK(id, mcpCallResult{
			Content:           []mcpContentPart{{Type: "text", Text: string(text)}},
			StructuredContent: outcome.result,
		})
	default:
		if strings.HasPrefix(method, "notifications/") {
			return nil
		}
		return rpcErr(id, -32601, "Method not found: "+method)
	}
}

func HandleMcp(ctx *Ctx) bool {
	if ctx.Method == "DELETE" {
		// Legacy session teardown (client.close()): nothing is held, just ack.
		SendRaw(ctx.W, 204, []byte{}, "application/json", map[string]string{"X-GitHub-Request-Id": ctx.RequestID})
		return true
	}
	if ctx.Method != "POST" {
		return false
	}

	// Defense in depth: main.go already 401s when a token is configured, but
	// enforce it here too so a bare /mcp POST never slips past.
	if token := os.Getenv("MAKEFUN_SHOP_TOKEN"); token != "" {
		if ctx.R.Header.Get("Authorization") != "Bearer "+token {
			SendJSON(ctx.W, 401, map[string]any{"message": "Bad credentials"}, map[string]string{"X-GitHub-Request-Id": ctx.RequestID})
			return true
		}
	}

	headers := map[string]string{"X-GitHub-Request-Id": ctx.RequestID}
	if sessionID := ctx.R.Header.Get("Mcp-Session-Id"); sessionID != "" {
		headers["Mcp-Session-Id"] = sessionID
	}

	// A body that is not JSON: HTTP 400 with JSON-RPC -32700.
	if ctx.BodyParseError || strings.TrimSpace(ctx.BodyText) == "" {
		SendJSON(ctx.W, 400, rpcErr(nil, -32700, "Parse error"), headers)
		return true
	}

	body := ctx.BodyJSON
	if arr, ok := body.([]any); ok {
		responses := make([]*rpcResponse, 0, len(arr))
		for _, item := range arr {
			if response := handleRequest(ctx, item); response != nil {
				responses = append(responses, response)
			}
		}
		if len(responses) == 0 {
			SendRaw(ctx.W, 202, []byte{}, "application/json", headers)
			return true
		}
		SendJSON(ctx.W, 200, responses, headers)
		return true
	}
	if _, ok := isRecord(body); !ok {
		SendJSON(ctx.W, 200, rpcErr(nil, -32600, "Invalid Request"), headers)
		return true
	}
	response := handleRequest(ctx, body)
	if response == nil {
		// Notification (e.g. notifications/initialized): 202, no body.
		SendRaw(ctx.W, 202, []byte{}, "application/json", headers)
		return true
	}
	SendJSON(ctx.W, 200, response, headers)
	return true
}
