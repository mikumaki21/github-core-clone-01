// rest.go — REST v3 surface.
//
// Port of rest.ts from the Node entry: repos, contents, branches, commits,
// issues, labels, pulls (+ squash merge with conflict detection). All state
// lives in store.go; this file only shapes JSON and routes.
//
// One deliberate quirk is preserved from the Node original: pageParams calls
// num(query.get('per_page'), 30), and URLSearchParams.get returns null when
// the parameter is absent — Number(null) is 0, so an absent per_page clamps
// to 1, not 30. A present but non-numeric per_page falls back to 30.
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- helpers

func restHeaders(ctx *Ctx, extra map[string]string) map[string]string {
	h := map[string]string{"X-GitHub-Request-Id": ctx.RequestID}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func originOf(ctx *Ctx) string {
	host := ctx.R.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return "http://" + host
}

func ownerOrViewer(repo *Repo) string {
	if repo.OwnerLogin != "" {
		return repo.OwnerLogin
	}
	return "viewer"
}

func fullName(repo *Repo) string {
	return ownerOrViewer(repo) + "/" + repo.Name
}

func bodyObj(ctx *Ctx) map[string]any {
	if m, ok := ctx.BodyJSON.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// bodyStr mirrors the Node str(v, d): only a JSON string counts, else d.
func bodyStr(b map[string]any, key, def string) string {
	if v, ok := b[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// jsString mirrors String(v) for label values coming from JSON.
func jsString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return formatJSNumber(t)
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// formatJSNumber mirrors String(n) for ordinary finite numbers.
func formatJSNumber(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// jsNumber mirrors Number(s) for query-string values: "" -> 0, "abc" -> NaN.
func jsNumber(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	if t == "Infinity" || t == "+Infinity" {
		return math.Inf(1)
	}
	if t == "-Infinity" {
		return math.Inf(-1)
	}
	if len(t) > 2 && (t[:2] == "0x" || t[:2] == "0X") {
		if v, err := strconv.ParseInt(t[2:], 16, 64); err == nil {
			return float64(v)
		}
		return math.NaN()
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f
	}
	return math.NaN()
}

// numQuery mirrors num(ctx.query.get(key), def). An absent key is null in
// Node, and Number(null) is 0 — not the default.
func numQuery(ctx *Ctx, key string, def float64) float64 {
	vals, ok := ctx.Query[key]
	if !ok {
		return 0
	}
	n := jsNumber(vals[0])
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return def
	}
	return n
}

func notFound(ctx *Ctx) {
	SendJSON(ctx.W, 404, map[string]any{"message": "Not Found"}, restHeaders(ctx, nil))
}

func badJSON(ctx *Ctx) bool {
	if ctx.BodyParseError {
		SendJSON(ctx.W, 400, map[string]any{"message": "Invalid JSON body"}, restHeaders(ctx, nil))
		return true
	}
	return false
}

// queryParam mirrors `ctx.query.get(k) ?? def`: missing -> def, present -> value.
func queryParam(ctx *Ctx, key, def string) string {
	if vals, ok := ctx.Query[key]; ok {
		return vals[0]
	}
	return def
}

// ---------------------------------------------------------------- shapes

func restRepoJSON(ctx *Ctx, repo *Repo) map[string]any {
	origin := originOf(ctx)
	full := fullName(repo)
	owner := ownerOrViewer(repo)
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      full,
		"owner":          map[string]any{"login": owner, "id": 1, "type": "User"},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     repo.CreatedAt,
		"updated_at":     repo.UpdatedAt,
		"html_url":       origin + "/" + full,
		"url":            origin + "/api/v3/repos/" + full,
		"clone_url":      origin + "/" + full + ".git",
		"git_url":        origin + "/" + full + ".git",
		"ssh_url":        "git@example.invalid:" + full + ".git",
	}
}

func restIssueJSON(ctx *Ctx, repo *Repo, issue *Issue) map[string]any {
	origin := originOf(ctx)
	full := fullName(repo)
	labels := make([]any, 0, len(issue.Labels))
	for _, n := range issue.Labels {
		color := "ededed"
		desc := ""
		if l, ok := repo.Labels[n]; ok {
			color = l.Color
			desc = l.Description
		}
		labels = append(labels, map[string]any{"name": n, "color": color, "description": desc})
	}
	return map[string]any{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       map[string]any{"login": issue.UserLogin},
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
		"html_url":   origin + "/" + full + "/issues/" + strconv.Itoa(issue.Number),
		"url":        origin + "/api/v3/repos/" + full + "/issues/" + strconv.Itoa(issue.Number),
	}
}

func restPullJSON(ctx *Ctx, repo *Repo, pull *PullRequest) map[string]any {
	origin := originOf(ctx)
	full := fullName(repo)
	owner := ownerOrViewer(repo)
	var mergedAt any
	if pull.MergedAt != "" {
		mergedAt = pull.MergedAt
	}
	var mergeSha any
	if pull.MergeCommitSha != "" {
		mergeSha = pull.MergeCommitSha
	}
	return map[string]any{
		"id":               pull.ID,
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"head":             map[string]any{"ref": pull.HeadRef, "sha": pull.HeadSha, "label": owner + ":" + pull.HeadRef},
		"base":             map[string]any{"ref": pull.BaseRef, "sha": pull.BaseSha, "label": owner + ":" + pull.BaseRef},
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
		"created_at":       pull.CreatedAt,
		"updated_at":       pull.UpdatedAt,
		"html_url":         origin + "/" + full + "/pull/" + strconv.Itoa(pull.Number),
		"url":              origin + "/api/v3/repos/" + full + "/pulls/" + strconv.Itoa(pull.Number),
	}
}

func restCommentJSON(ctx *Ctx, repo *Repo, issue *Issue, c *IssueComment) map[string]any {
	origin := originOf(ctx)
	full := fullName(repo)
	return map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       map[string]any{"login": c.UserLogin},
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
		"html_url":   origin + "/" + full + "/issues/" + strconv.Itoa(issue.Number) + "#issuecomment-" + strconv.Itoa(c.ID),
		"url":        origin + "/api/v3/repos/" + full + "/issues/comments/" + strconv.Itoa(c.ID),
	}
}

// ---------------------------------------------------------------- pagination

func pageParams(ctx *Ctx) (page, perPage float64) {
	perPage = math.Min(100, math.Max(1, numQuery(ctx, "per_page", 30)))
	page = math.Max(1, numQuery(ctx, "page", 1))
	return page, perPage
}

// paginate slices items for the requested page and builds a Link header
// value ("" when there is no next page).
func paginate[T any](ctx *Ctx, items []T) ([]T, string) {
	page, perPage := pageParams(ctx)
	startF := (page - 1) * perPage
	endF := startF + perPage
	start := int(startF)
	end := int(endF)
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	if end < start {
		end = start
	}
	if end > len(items) {
		end = len(items)
	}
	link := ""
	if endF < float64(len(items)) {
		host := ctx.R.Host
		if host == "" {
			host = "127.0.0.1"
		}
		u := &url.URL{Scheme: "http", Host: host, Path: ctx.Path}
		q := u.Query()
		q.Set("page", formatJSNumber(page+1))
		q.Set("per_page", formatJSNumber(perPage))
		u.RawQuery = q.Encode()
		link = "<" + u.String() + ">; rel=\"next\""
	}
	return items[start:end], link
}

func linkHeaders(ctx *Ctx, link string) map[string]string {
	if link == "" {
		return restHeaders(ctx, nil)
	}
	return restHeaders(ctx, map[string]string{"Link": link})
}

// ---------------------------------------------------------------- git helpers

// resolveCommit resolves a ref-ish (branch name, full ref, or commit oid)
// to a commit oid.
func resolveCommit(ctx *Ctx, repo *Repo, refish string) (string, bool) {
	if refish == "" {
		sha, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		return sha, ok
	}
	if strings.HasPrefix(refish, "refs/") {
		sha, ok := repo.Refs[refish]
		return sha, ok
	}
	if sha, ok := repo.Refs["refs/heads/"+refish]; ok {
		return sha, true
	}
	if obj := ctx.Store.GetObject(refish); obj != nil && obj.Type == ObjCommit {
		return refish, true
	}
	return "", false
}

type walkedCommit struct {
	sha     string
	message string
}

// walkCommits walks commit parents (first-parent order via a stack),
// newest first.
func walkCommits(ctx *Ctx, tip string) []walkedCommit {
	var out []walkedCommit
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := ctx.Store.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		parsed := ParseCommit(obj.Data)
		if parsed == nil {
			continue
		}
		out = append(out, walkedCommit{sha: oid, message: parsed.Message})
		for i := len(parsed.Parents) - 1; i >= 0; i-- {
			stack = append(stack, parsed.Parents[i])
		}
	}
	return out
}

// ancestors returns all ancestors of a commit (including itself).
func ancestors(ctx *Ctx, tip string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := ctx.Store.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		parsed := ParseCommit(obj.Data)
		if parsed == nil {
			continue
		}
		stack = append(stack, parsed.Parents...)
	}
	return seen
}

// mergeBase is the nearest common ancestor of two tips (walk head's history
// breadth-first, first hit in base's ancestor set).
func restMergeBase(ctx *Ctx, headTip, baseTip string) (string, bool) {
	baseAnc := ancestors(ctx, baseTip)
	seen := map[string]bool{}
	queue := []string{headTip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if baseAnc[oid] {
			return oid, true
		}
		obj := ctx.Store.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		parsed := ParseCommit(obj.Data)
		if parsed == nil {
			continue
		}
		queue = append(queue, parsed.Parents...)
	}
	return "", false
}

// bufEq compares two possibly-absent blobs: absent equals absent only.
func bufEq(a []byte, aOk bool, b []byte, bOk bool) bool {
	if !aOk && !bOk {
		return true
	}
	if !aOk || !bOk {
		return false
	}
	return bytes.Equal(a, b)
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// decodeBase64Lenient lives in graphql.go (same Node Buffer.from semantics:
// characters outside the alphabet are ignored, missing padding tolerated).


func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ---------------------------------------------------------------- handler

func HandleRest(ctx *Ctx) bool {
	rest := ctx.Segments[2:] // segments are ['api','v3', ...rest]
	method := ctx.Method
	origin := originOf(ctx)

	// GET /api/v3/ (root)
	if len(rest) == 0 {
		if method != "GET" {
			notFound(ctx)
			return true
		}
		SendJSON(ctx.W, 200, map[string]any{
			"current_user_url": origin + "/api/v3/user",
			"repository_url":   origin + "/api/v3/repos/{owner}/{repo}",
		}, restHeaders(ctx, map[string]string{"X-Oauth-Scopes": "repo, read:org"}))
		return true
	}

	// GET /api/v3/meta
	if len(rest) == 1 && rest[0] == "meta" {
		if method != "GET" {
			notFound(ctx)
			return true
		}
		SendJSON(ctx.W, 200, map[string]any{
			"installed_version":                 "1.0.0",
			"verifiable_password_authentication": false,
		}, restHeaders(ctx, nil))
		return true
	}

	// POST /api/v3/user/repos + GET /api/v3/user/repos
	if len(rest) == 2 && rest[0] == "user" && rest[1] == "repos" {
		if method == "POST" {
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			name := strings.TrimSpace(bodyStr(b, "name", ""))
			if name == "" {
				SendJSON(ctx.W, 422, map[string]any{"message": "name is required"}, restHeaders(ctx, nil))
				return true
			}
			repo := ctx.Store.CreateRepo(name, bodyBool(b, "private", false), bodyStr(b, "description", ""))
			if repo == nil {
				SendJSON(ctx.W, 422, map[string]any{"message": "Repository already exists"}, restHeaders(ctx, nil))
				return true
			}
			SendJSON(ctx.W, 201, restRepoJSON(ctx, repo), restHeaders(ctx, nil))
			return true
		}
		if method == "GET" {
			page, link := paginate(ctx, ctx.Store.ListRepos())
			out := make([]any, 0, len(page))
			for _, r := range page {
				out = append(out, restRepoJSON(ctx, r))
			}
			SendJSON(ctx.W, 200, out, linkHeaders(ctx, link))
			return true
		}
		notFound(ctx)
		return true
	}

	// /api/v3/repos/{owner}/{repo}[/...]
	if len(rest) >= 3 && rest[0] == "repos" {
		owner := rest[1]
		name := rest[2]
		tail := rest[3:]
		repo := ctx.Store.GetRepo(owner, name)
		if repo == nil {
			notFound(ctx)
			return true
		}
		full := fullName(repo)

		// GET/PATCH/DELETE /repos/{o}/{r}
		if len(tail) == 0 {
			if method == "GET" {
				SendJSON(ctx.W, 200, restRepoJSON(ctx, repo), restHeaders(ctx, nil))
				return true
			}
			if method == "PATCH" {
				if badJSON(ctx) {
					return true
				}
				b := bodyObj(ctx)
				if s, ok := b["description"].(string); ok {
					repo.Description = s
				}
				if v, ok := b["private"].(bool); ok {
					repo.Private = v
				}
				repo.UpdatedAt = ctx.Store.NowISO()
				SendJSON(ctx.W, 200, restRepoJSON(ctx, repo), restHeaders(ctx, nil))
				return true
			}
			if method == "DELETE" {
				ctx.Store.DeleteRepo(repo.Name)
				SendRaw(ctx.W, 204, []byte{}, "application/json", restHeaders(ctx, nil))
				return true
			}
			notFound(ctx)
			return true
		}

		// POST /repos/{o}/{r}/git/refs
		if len(tail) == 2 && tail[0] == "git" && tail[1] == "refs" {
			if method != "POST" {
				notFound(ctx)
				return true
			}
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			ref := bodyStr(b, "ref", "")
			sha := bodyStr(b, "sha", "")
			if !strings.HasPrefix(ref, "refs/") || !isHex40(sha) {
				SendJSON(ctx.W, 422, map[string]any{"message": "Invalid ref or sha"}, restHeaders(ctx, nil))
				return true
			}
			if _, ok := repo.Refs[ref]; ok {
				SendJSON(ctx.W, 422, map[string]any{"message": "Reference already exists"}, restHeaders(ctx, nil))
				return true
			}
			repo.Refs[ref] = sha
			SendJSON(ctx.W, 201, map[string]any{
				"ref":    ref,
				"object": map[string]any{"sha": sha, "type": "commit", "url": origin + "/api/v3/repos/" + full + "/git/commits/" + sha},
			}, restHeaders(ctx, nil))
			return true
		}

		// GET /repos/{o}/{r}/branches
		if len(tail) == 1 && tail[0] == "branches" {
			if method != "GET" {
				notFound(ctx)
				return true
			}
			refs := make([]string, 0, len(repo.Refs))
			for ref := range repo.Refs {
				refs = append(refs, ref)
			}
			sort.Strings(refs)
			branches := make([]any, 0)
			for _, ref := range refs {
				if !strings.HasPrefix(ref, "refs/heads/") {
					continue
				}
				sha := repo.Refs[ref]
				branches = append(branches, map[string]any{
					"name":      ref[len("refs/heads/"):],
					"commit":    map[string]any{"sha": sha, "url": origin + "/api/v3/repos/" + full + "/commits/" + sha},
					"protected": false,
				})
			}
			SendJSON(ctx.W, 200, branches, restHeaders(ctx, nil))
			return true
		}

		// GET /repos/{o}/{r}/commits
		if len(tail) == 1 && tail[0] == "commits" {
			if method != "GET" {
				notFound(ctx)
				return true
			}
			tip, ok := resolveCommit(ctx, repo, ctx.Query.Get("sha"))
			if !ok {
				notFound(ctx)
				return true
			}
			ownerLogin := ownerOrViewer(repo)
			list := make([]any, 0)
			for _, c := range walkCommits(ctx, tip) {
				list = append(list, map[string]any{
					"sha": c.sha,
					"commit": map[string]any{
						"message":   c.message,
						"author":    map[string]any{"name": ownerLogin, "email": ownerLogin + "@example.invalid"},
						"committer": map[string]any{"name": ownerLogin, "email": ownerLogin + "@example.invalid"},
					},
					"html_url": origin + "/" + full + "/commit/" + c.sha,
					"url":      origin + "/api/v3/repos/" + full + "/commits/" + c.sha,
				})
			}
			SendJSON(ctx.W, 200, list, restHeaders(ctx, nil))
			return true
		}

		// /repos/{o}/{r}/contents[/{path...}]
		if len(tail) >= 1 && tail[0] == "contents" {
			return handleContents(ctx, repo, full, tail[1:])
		}

		// POST /repos/{o}/{r}/labels
		if len(tail) == 1 && tail[0] == "labels" {
			if method != "POST" {
				notFound(ctx)
				return true
			}
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			name := strings.TrimSpace(bodyStr(b, "name", ""))
			if name == "" {
				SendJSON(ctx.W, 422, map[string]any{"message": "name is required"}, restHeaders(ctx, nil))
				return true
			}
			color := strings.TrimPrefix(bodyStr(b, "color", "ededed"), "#")
			description := bodyStr(b, "description", "")
			repo.Labels[name] = &Label{Name: name, Color: color, Description: description}
			SendJSON(ctx.W, 201, map[string]any{
				"name":        name,
				"color":       color,
				"description": description,
				"url":         origin + "/api/v3/repos/" + full + "/labels/" + url.PathEscape(name),
			}, restHeaders(ctx, nil))
			return true
		}

		// /repos/{o}/{r}/issues[/{n}[/comments]]
		if len(tail) >= 1 && tail[0] == "issues" {
			return handleIssues(ctx, repo, full, tail[1:])
		}

		// /repos/{o}/{r}/pulls[/{n}[/files|merge]]
		if len(tail) >= 1 && tail[0] == "pulls" {
			return handlePulls(ctx, repo, full, tail[1:])
		}

		notFound(ctx)
		return true
	}

	// Anything else under /api/v3: unknown route.
	notFound(ctx)
	return true
}

// bodyBool mirrors the Node bool(v, d): only a JSON boolean counts, else d.
func bodyBool(b map[string]any, key string, def bool) bool {
	if v, ok := b[key]; ok {
		if bv, ok := v.(bool); ok {
			return bv
		}
	}
	return def
}

func handleContents(ctx *Ctx, repo *Repo, full string, tail []string) bool {
	origin := originOf(ctx)
	method := ctx.Method
	filePath := strings.Join(tail, "/")

	if method == "PUT" {
		if badJSON(ctx) {
			return true
		}
		b := bodyObj(ctx)
		message := bodyStr(b, "message", "Update file")
		contentB64 := bodyStr(b, "content", "")
		if contentB64 == "" {
			SendJSON(ctx.W, 422, map[string]any{"message": "content is required"}, restHeaders(ctx, nil))
			return true
		}
		branch := bodyStr(b, "branch", repo.DefaultBranch)
		if branch == "" {
			branch = repo.DefaultBranch
		}
		ref := "refs/heads/" + branch
		files := ctx.Store.FilesAtRef(repo, ref)
		_, exists := files[filePath]
		if exists {
			currentSha := ctx.Store.BlobOID(files[filePath])
			if bodyStr(b, "sha", "") != currentSha {
				SendJSON(ctx.W, 409, map[string]any{"message": "sha does not match the current blob sha"}, restHeaders(ctx, nil))
				return true
			}
		}
		content, _ := decodeBase64Lenient(contentB64)
		commitSha, blobSha := ctx.Store.CommitFile(repo, branch, filePath, content, message)
		status := 201
		if exists {
			status = 200
		}
		SendJSON(ctx.W, status, map[string]any{
			"content": map[string]any{
				"name":     baseName(filePath),
				"path":     filePath,
				"sha":      blobSha,
				"size":     len(content),
				"encoding": "base64",
				"type":     "file",
				"url":      origin + "/api/v3/repos/" + full + "/contents/" + filePath,
			},
			"commit": map[string]any{"sha": commitSha, "message": message},
		}, restHeaders(ctx, nil))
		return true
	}

	if method == "GET" {
		tip, ok := resolveCommit(ctx, repo, ctx.Query.Get("ref"))
		if !ok {
			notFound(ctx)
			return true
		}
		obj := ctx.Store.GetObject(tip)
		var parsed *CommitInfo
		if obj != nil && obj.Type == ObjCommit {
			parsed = ParseCommit(obj.Data)
		}
		if parsed == nil {
			notFound(ctx)
			return true
		}
		files := ctx.Store.FilesAtTree(parsed.Tree)
		// Exact file?
		if filePath != "" {
			if content, ok := files[filePath]; ok {
				SendJSON(ctx.W, 200, map[string]any{
					"type":     "file",
					"encoding": "base64",
					"size":     len(content),
					"name":     baseName(filePath),
					"path":     filePath,
					"content":  base64.StdEncoding.EncodeToString(content),
					"sha":      ctx.Store.BlobOID(content),
					"url":      origin + "/api/v3/repos/" + full + "/contents/" + filePath,
					"html_url": origin + "/" + full + "/blob/" + repo.DefaultBranch + "/" + filePath,
				}, restHeaders(ctx, nil))
				return true
			}
		}
		// Directory listing (root when filePath == '').
		prefix := ""
		if filePath != "" {
			prefix = filePath + "/"
		}
		type dirEntry struct {
			typ     string
			name    string
			path    string
			sha     string
			size    int
			hasSize bool
		}
		entries := map[string]*dirEntry{}
		for p, content := range files {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rel := p[len(prefix):]
			if rel == "" {
				continue
			}
			if i := strings.Index(rel, "/"); i == -1 {
				entries[rel] = &dirEntry{typ: "file", name: rel, path: p, sha: ctx.Store.BlobOID(content), size: len(content), hasSize: true}
			} else {
				dirName := rel[:i]
				if _, ok := entries[dirName]; !ok {
					sub := map[string][]byte{}
					for p2, c2 := range files {
						if strings.HasPrefix(p2, prefix+dirName+"/") {
							sub[p2[len(prefix+dirName+"/"):]] = c2
						}
					}
					entries[dirName] = &dirEntry{typ: "dir", name: dirName, path: prefix + dirName, sha: ctx.Store.TreeOIDFromFiles(sub)}
				}
			}
		}
		if filePath != "" && len(entries) == 0 {
			notFound(ctx)
			return true
		}
		names := make([]string, 0, len(entries))
		for n := range entries {
			names = append(names, n)
		}
		sort.Strings(names)
		list := make([]any, 0, len(names))
		for _, n := range names {
			e := entries[n]
			m := map[string]any{
				"type":     e.typ,
				"name":     e.name,
				"path":     e.path,
				"sha":      e.sha,
				"url":      origin + "/api/v3/repos/" + full + "/contents/" + e.path,
				"html_url": origin + "/" + full + "/tree/" + repo.DefaultBranch + "/" + e.path,
			}
			if e.hasSize {
				m["size"] = e.size
			}
			list = append(list, m)
		}
		SendJSON(ctx.W, 200, list, restHeaders(ctx, nil))
		return true
	}

	if method == "DELETE" {
		if badJSON(ctx) {
			return true
		}
		b := bodyObj(ctx)
		branch := bodyStr(b, "branch", repo.DefaultBranch)
		if branch == "" {
			branch = repo.DefaultBranch
		}
		message := bodyStr(b, "message", "Delete file")
		commitSha, ok := ctx.Store.DeleteFile(repo, branch, filePath, message)
		if !ok {
			notFound(ctx)
			return true
		}
		SendJSON(ctx.W, 200, map[string]any{
			"content": nil,
			"commit":  map[string]any{"sha": commitSha, "message": message},
		}, restHeaders(ctx, nil))
		return true
	}

	notFound(ctx)
	return true
}

func labelsFromBody(b map[string]any) []string {
	arr, ok := b["labels"].([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, l := range arr {
		out = append(out, jsString(l))
	}
	return out
}

func handleIssues(ctx *Ctx, repo *Repo, full string, tail []string) bool {
	method := ctx.Method

	// POST/GET /issues
	if len(tail) == 0 {
		if method == "POST" {
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			issue, unknown := ctx.Store.CreateIssue(repo, bodyStr(b, "title", ""), bodyStr(b, "body", ""), labelsFromBody(b))
			if issue == nil {
				SendJSON(ctx.W, 422, map[string]any{"message": "Unknown label: " + unknown}, restHeaders(ctx, nil))
				return true
			}
			SendJSON(ctx.W, 201, restIssueJSON(ctx, repo, issue), restHeaders(ctx, nil))
			return true
		}
		if method == "GET" {
			state := strings.ToLower(queryParam(ctx, "state", "open"))
			list := make([]*Issue, 0, len(repo.Issues))
			for _, i := range repo.Issues {
				if state == "open" || state == "closed" {
					if i.State != state {
						continue
					}
				}
				list = append(list, i)
			}
			direction := strings.ToLower(queryParam(ctx, "direction", "desc"))
			if direction == "desc" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			page, link := paginate(ctx, list)
			out := make([]any, 0, len(page))
			for _, i := range page {
				out = append(out, restIssueJSON(ctx, repo, i))
			}
			SendJSON(ctx.W, 200, out, linkHeaders(ctx, link))
			return true
		}
		notFound(ctx)
		return true
	}

	// /issues/{n} and /issues/{n}/comments
	num, err := strconv.Atoi(strings.TrimSpace(tail[0]))
	if err != nil {
		notFound(ctx)
		return true
	}
	var issue *Issue
	for _, i := range repo.Issues {
		if i.Number == num {
			issue = i
			break
		}
	}
	if issue == nil {
		notFound(ctx)
		return true
	}

	if len(tail) == 1 {
		if method == "GET" {
			SendJSON(ctx.W, 200, restIssueJSON(ctx, repo, issue), restHeaders(ctx, nil))
			return true
		}
		if method == "PATCH" {
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			if s, ok := b["state"].(string); ok && (s == "open" || s == "closed") {
				issue.State = s
			}
			if s, ok := b["title"].(string); ok {
				issue.Title = s
			}
			if s, ok := b["body"].(string); ok {
				issue.Body = s
			}
			if _, ok := b["labels"].([]any); ok {
				labels := labelsFromBody(b)
				for _, l := range labels {
					if _, ok := repo.Labels[l]; !ok {
						SendJSON(ctx.W, 422, map[string]any{"message": "Unknown label: " + l}, restHeaders(ctx, nil))
						return true
					}
				}
				issue.Labels = labels
			}
			issue.UpdatedAt = ctx.Store.NowISO()
			SendJSON(ctx.W, 200, restIssueJSON(ctx, repo, issue), restHeaders(ctx, nil))
			return true
		}
		notFound(ctx)
		return true
	}

	if len(tail) == 2 && tail[1] == "comments" {
		if method == "POST" {
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			id := repo.NextCommentID
			repo.NextCommentID++
			now := ctx.Store.NowISO()
			comment := &IssueComment{
				ID:        id,
				Body:      bodyStr(b, "body", ""),
				UserLogin: ownerOrViewer(repo),
				CreatedAt: now,
				UpdatedAt: now,
			}
			issue.Comments = append(issue.Comments, comment)
			SendJSON(ctx.W, 201, restCommentJSON(ctx, repo, issue, comment), restHeaders(ctx, nil))
			return true
		}
		if method == "GET" {
			out := make([]any, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				out = append(out, restCommentJSON(ctx, repo, issue, c))
			}
			SendJSON(ctx.W, 200, out, restHeaders(ctx, nil))
			return true
		}
		notFound(ctx)
		return true
	}

	notFound(ctx)
	return true
}

func handlePulls(ctx *Ctx, repo *Repo, full string, tail []string) bool {
	method := ctx.Method

	if len(tail) == 0 {
		if method == "POST" {
			if badJSON(ctx) {
				return true
			}
			b := bodyObj(ctx)
			headRaw := bodyStr(b, "head", "")
			baseRaw := bodyStr(b, "base", "")
			head := headRaw
			if i := strings.LastIndex(headRaw, ":"); i >= 0 {
				head = headRaw[i+1:]
			}
			base := baseRaw
			if i := strings.LastIndex(baseRaw, ":"); i >= 0 {
				base = baseRaw[i+1:]
			}
			pull, errMsg := ctx.Store.CreatePull(repo, bodyStr(b, "title", ""), bodyStr(b, "body", ""), head, base)
			if pull == nil {
				SendJSON(ctx.W, 422, map[string]any{"message": errMsg}, restHeaders(ctx, nil))
				return true
			}
			SendJSON(ctx.W, 201, restPullJSON(ctx, repo, pull), restHeaders(ctx, nil))
			return true
		}
		if method == "GET" {
			state := strings.ToLower(queryParam(ctx, "state", "open"))
			list := make([]*PullRequest, 0, len(repo.Pulls))
			for _, p := range repo.Pulls {
				if state == "open" || state == "closed" {
					if p.State != state {
						continue
					}
				}
				list = append(list, p)
			}
			page, link := paginate(ctx, list)
			out := make([]any, 0, len(page))
			for _, p := range page {
				out = append(out, restPullJSON(ctx, repo, p))
			}
			SendJSON(ctx.W, 200, out, linkHeaders(ctx, link))
			return true
		}
		notFound(ctx)
		return true
	}

	num, err := strconv.Atoi(strings.TrimSpace(tail[0]))
	if err != nil {
		notFound(ctx)
		return true
	}
	var pull *PullRequest
	for _, p := range repo.Pulls {
		if p.Number == num {
			pull = p
			break
		}
	}

	if len(tail) == 1 {
		if method != "GET" {
			notFound(ctx)
			return true
		}
		if pull == nil {
			notFound(ctx)
			return true
		}
		SendJSON(ctx.W, 200, restPullJSON(ctx, repo, pull), restHeaders(ctx, nil))
		return true
	}

	if len(tail) == 2 && tail[1] == "files" {
		if method != "GET" {
			notFound(ctx)
			return true
		}
		if pull == nil {
			notFound(ctx)
			return true
		}
		out := make([]any, 0, len(pull.Files))
		for _, f := range pull.Files {
			out = append(out, map[string]any{
				"filename":  f.Filename,
				"status":    f.Status,
				"additions": f.Additions,
				"deletions": f.Deletions,
				"changes":   f.Changes,
				"sha":       f.Sha,
			})
		}
		SendJSON(ctx.W, 200, out, restHeaders(ctx, nil))
		return true
	}

	if len(tail) == 2 && tail[1] == "merge" {
		if method != "PUT" {
			notFound(ctx)
			return true
		}
		if badJSON(ctx) {
			return true
		}
		if pull == nil {
			notFound(ctx)
			return true
		}
		if pull.State != "open" {
			SendJSON(ctx.W, 405, map[string]any{"message": "Pull request is already closed"}, restHeaders(ctx, nil))
			return true
		}
		// Conflict check: merge-base of current head/base tips; a path
		// changed on BOTH sides since the merge-base with different content
		// conflicts.
		headTip, ok1 := repo.Refs["refs/heads/"+pull.HeadRef]
		baseTip, ok2 := repo.Refs["refs/heads/"+pull.BaseRef]
		if !ok1 || !ok2 {
			notFound(ctx)
			return true
		}
		if base, found := restMergeBase(ctx, headTip, baseTip); found {
			var baseFiles map[string][]byte
			if obj := ctx.Store.GetObject(base); obj != nil && obj.Type == ObjCommit {
				if pc := ParseCommit(obj.Data); pc != nil {
					baseFiles = ctx.Store.FilesAtTree(pc.Tree)
				}
			}
			if baseFiles == nil {
				baseFiles = map[string][]byte{}
			}
			headFiles := ctx.Store.FilesAtRef(repo, "refs/heads/"+pull.HeadRef)
			curBaseFiles := ctx.Store.FilesAtRef(repo, "refs/heads/"+pull.BaseRef)
			allPaths := map[string]bool{}
			for p := range baseFiles {
				allPaths[p] = true
			}
			for p := range headFiles {
				allPaths[p] = true
			}
			for p := range curBaseFiles {
				allPaths[p] = true
			}
			for path := range allPaths {
				hb, hOk := headFiles[path]
				bb, bOk := baseFiles[path]
				cb, cOk := curBaseFiles[path]
				changedHead := !bufEq(hb, hOk, bb, bOk)
				changedBase := !bufEq(cb, cOk, bb, bOk)
				if changedHead && changedBase && !bufEq(hb, hOk, cb, cOk) {
					SendJSON(ctx.W, 405, map[string]any{"message": "Merge conflict"}, restHeaders(ctx, nil))
					return true
				}
			}
		}
		sha, errMsg := ctx.Store.MergePull(repo, pull)
		if errMsg != "" {
			SendJSON(ctx.W, 405, map[string]any{"message": errMsg}, restHeaders(ctx, nil))
			return true
		}
		SendJSON(ctx.W, 200, map[string]any{
			"sha":     sha,
			"merged":  true,
			"message": pull.Title + " (#" + strconv.Itoa(pull.Number) + ")",
		}, restHeaders(ctx, nil))
		return true
	}

	notFound(ctx)
	return true
}
