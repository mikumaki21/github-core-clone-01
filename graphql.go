// GraphQL v4 surface for the github-core-clone-01 entry (Go port).
//
// Minimal hand-written GraphQL: a small tokenizer + recursive-descent parser
// (stdlib only) plus resolvers over the shared Store. It implements exactly
// the documents gh / github-mcp-server send (see the kit's
// checks-graphql.ts): viewer, repository (+ issue/issues/pullRequest/
// pullRequests), node(id:), __type/__schema introspection, the createIssue /
// closeIssue / createPullRequest / mergePullRequest / addComment mutations,
// and JSON-array operation batching. Unknown fields are GraphQL errors on
// HTTP 200, never HTTP 400s.
//
// Port of graphql.ts from the Node entry; observable behaviour is kept
// identical (same error strings, same node ids, same cursor format).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func HandleGraphql(ctx *Ctx) bool {
	if ctx.Path != "/api/graphql" {
		return false
	}
	headers := map[string]string{"X-GitHub-Request-Id": ctx.RequestID}
	if ctx.Method != "POST" {
		SendJSON(ctx.W, 404, map[string]any{"message": "Not Found"}, headers)
		return true
	}
	if ctx.BodyParseError {
		SendJSON(ctx.W, 400, map[string]any{"message": "Invalid JSON body"}, headers)
		return true
	}
	body := ctx.BodyJSON
	if arr, ok := body.([]any); ok {
		results := make([]any, len(arr))
		for i, entry := range arr {
			results[i] = runOperation(ctx.Store, entry)
		}
		SendJSON(ctx.W, 200, results, headers)
		return true
	}
	if _, ok := body.(map[string]any); !ok {
		SendJSON(ctx.W, 400, map[string]any{"message": "Expected a JSON object with query and variables"}, headers)
		return true
	}
	SendJSON(ctx.W, 200, runOperation(ctx.Store, body), headers)
	return true
}

// ---------------------------------------------------------------------------
// Node ids: base64("<Type>:<numeric id>"), so node(id:) round-trips.
// ---------------------------------------------------------------------------

func nodeID(typ string, num int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", typ, num)))
}

var nodeIDRe = regexp.MustCompile(`^([A-Za-z]+):(\d+)$`)

// decodeBase64Lenient mirrors Node's Buffer.from(s, 'base64'): characters
// outside the alphabet are ignored and missing padding is tolerated.
func decodeBase64Lenient(s string) ([]byte, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' {
			b.WriteByte(c)
		}
	}
	clean := b.String()
	if rem := len(clean) % 4; rem != 0 {
		clean += strings.Repeat("=", 4-rem)
	}
	return base64.StdEncoding.DecodeString(clean)
}

type decodedNodeID struct {
	typ string
	num int
}

func decodeNodeID(id any) *decodedNodeID {
	s, ok := id.(string)
	if !ok || s == "" {
		return nil
	}
	text, err := decodeBase64Lenient(s)
	if err != nil {
		return nil
	}
	m := nodeIDRe.FindStringSubmatch(string(text))
	if m == nil {
		return nil
	}
	num, err := strconv.Atoi(m[2])
	if err != nil {
		return nil
	}
	return &decodedNodeID{typ: m[1], num: num}
}

func encodeCursor(index int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:%d", index)))
}

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func decodeCursor(value any) int {
	s, ok := value.(string)
	if !ok || s == "" {
		return 0
	}
	text, err := decodeBase64Lenient(s)
	if err != nil {
		return 0
	}
	m := cursorRe.FindStringSubmatch(string(text))
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n + 1
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tkName tokenKind = iota
	tkString
	tkNumber
	tkPunct
	tkVar
)

func (k tokenKind) String() string {
	switch k {
	case tkName:
		return "name"
	case tkString:
		return "string"
	case tkNumber:
		return "number"
	case tkPunct:
		return "punct"
	case tkVar:
		return "var"
	}
	return "unknown"
}

type token struct {
	kind  tokenKind
	value string
}

// parseErr is a GraphQL document error (tokenizer or parser). It is raised
// with panic inside the tokenizer/parser and recovered in runOperation;
// any other panic keeps propagating to the server's recover.
type parseErr struct{ msg string }

func (e parseErr) Error() string { return e.msg }

func failParse(format string, args ...any) {
	panic(parseErr{msg: fmt.Sprintf(format, args...)})
}

var (
	numberRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?`)
	nameRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

func isNameStart(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// jsonStringifyChar mirrors JSON.stringify for a single character, as used
// in the Node tokenizer's "Unexpected character" error.
func jsonStringifyChar(c byte) string {
	b, _ := json.Marshal(string(c))
	return string(b)
}

func tokenize(src string) []token {
	var tokens []token
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && strings.HasPrefix(src[i:], "...") {
			tokens = append(tokens, token{tkPunct, "..."})
			i += 3
			continue
		}
		if strings.IndexByte("!&():=@[]{|}", ch) >= 0 {
			tokens = append(tokens, token{tkPunct, string(ch)})
			i++
			continue
		}
		if ch == '$' {
			m := nameRe.FindString(src[i+1:])
			if m == "" {
				failParse("Invalid variable name")
			}
			tokens = append(tokens, token{tkVar, m})
			i += 1 + len(m)
			continue
		}
		if ch == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					if esc == 'u' && j+5 < len(src) {
						code, _ := strconv.ParseInt(src[j+2:j+6], 16, 32)
						out.WriteRune(rune(code))
						j += 6
					} else {
						switch esc {
						case 'n':
							out.WriteByte('\n')
						case 't':
							out.WriteByte('\t')
						case 'r':
							out.WriteByte('\r')
						case '"':
							out.WriteByte('"')
						case '\\':
							out.WriteByte('\\')
						case '/':
							out.WriteByte('/')
						default:
							out.WriteByte(esc)
						}
						j += 2
					}
				} else {
					out.WriteByte(src[j])
					j++
				}
			}
			if j >= len(src) {
				failParse("Unterminated string")
			}
			tokens = append(tokens, token{tkString, out.String()})
			i = j + 1
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			m := numberRe.FindString(src[i:])
			if m == "" {
				failParse("Invalid number at offset %d", i)
			}
			tokens = append(tokens, token{tkNumber, m})
			i += len(m)
			continue
		}
		if isNameStart(ch) {
			m := nameRe.FindString(src[i:])
			if m == "" {
				failParse("Invalid name at offset %d", i)
			}
			tokens = append(tokens, token{tkName, m})
			i += len(m)
			continue
		}
		failParse("Unexpected character %s at offset %d", jsonStringifyChar(ch), i)
	}
	return tokens
}

// ---------------------------------------------------------------------------
// Parser (AST)
// ---------------------------------------------------------------------------

type valueKind int

const (
	vVar valueKind = iota
	vString
	vNumber
	vBool
	vNull
	vEnum
	vList
	vObject
)

type valueNode struct {
	kind   valueKind
	str    string // var name / string / enum
	num    float64
	b      bool
	items  []*valueNode
	fields []objField
}

type objField struct {
	name  string
	value *valueNode
}

type selKind int

const (
	selField selKind = iota
	selSpread
	selInline
)

type argNode struct {
	name  string
	value *valueNode
}

type selection struct {
	kind     selKind
	alias    string
	name     string
	args     []argNode
	sels     []*selection
	typeCond string
}

type operation struct {
	kind string // "query" | "mutation"
	name string
	sels []*selection
}

type fragmentDef struct {
	name     string
	typeCond string
	sels     []*selection
}

type document struct {
	ops   []*operation
	frags map[string]*fragmentDef
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

func (p *parser) next() token {
	t, ok := p.peek()
	if !ok {
		failParse("Unexpected end of query")
	}
	p.pos++
	return t
}

func (p *parser) expectPunct(value string) {
	t := p.next()
	if t.kind != tkPunct || t.value != value {
		failParse("Expected %q but got %s %s", value, t.kind, t.value)
	}
}

func (p *parser) expectName(want ...string) string {
	t := p.next()
	if t.kind != tkName {
		failParse("Expected a name but got %s %s", t.kind, t.value)
	}
	if len(want) > 0 && t.value != want[0] {
		failParse("Expected %q but got %q", want[0], t.value)
	}
	return t.value
}

// skipBalanced skips tokens until the matching close punct. The open punct
// was already consumed by the caller, so depth starts at 1.
func (p *parser) skipBalanced(open, close string) {
	depth := 1
	for {
		t := p.next()
		if t.kind == tkPunct && t.value == open {
			depth++
		} else if t.kind == tkPunct && t.value == close {
			depth--
			if depth == 0 {
				return
			}
		}
	}
}

func (p *parser) parseDocument() *document {
	doc := &document{frags: map[string]*fragmentDef{}}
	for p.pos < len(p.toks) {
		t := p.toks[p.pos]
		if t.kind == tkPunct && t.value == "{" {
			doc.ops = append(doc.ops, &operation{kind: "query", sels: p.parseSelectionSet()})
		} else if t.kind == tkName && (t.value == "query" || t.value == "mutation") {
			kind := p.next().value
			var name string
			if n, ok := p.peek(); ok && n.kind == tkName {
				name = p.next().value
			}
			if pr, ok := p.peek(); ok && pr.kind == tkPunct && pr.value == "(" {
				p.next()
				p.skipBalanced("(", ")")
			}
			doc.ops = append(doc.ops, &operation{kind: kind, name: name, sels: p.parseSelectionSet()})
		} else if t.kind == tkName && t.value == "fragment" {
			p.next()
			name := p.expectName()
			p.expectName("on")
			typeCond := p.expectName()
			doc.frags[name] = &fragmentDef{name: name, typeCond: typeCond, sels: p.parseSelectionSet()}
		} else {
			failParse("Unexpected token %s %s at top level", t.kind, t.value)
		}
	}
	if len(doc.ops) == 0 {
		failParse("Document contains no operation")
	}
	return doc
}

func (p *parser) parseSelectionSet() []*selection {
	p.expectPunct("{")
	var sels []*selection
	for {
		t, ok := p.peek()
		if !ok {
			failParse("Unterminated selection set")
		}
		if t.kind == tkPunct && t.value == "}" {
			p.next()
			return sels
		}
		sels = append(sels, p.parseSelection())
	}
}

func (p *parser) parseSelection() *selection {
	t, ok := p.peek()
	if !ok {
		failParse("Unexpected end of query")
	}
	if t.kind == tkPunct && t.value == "..." {
		p.next()
		n, ok := p.peek()
		if ok && n.kind == tkName && n.value == "on" {
			p.next()
			typeCond := p.expectName()
			return &selection{kind: selInline, typeCond: typeCond, sels: p.parseSelectionSet()}
		}
		if ok && n.kind == tkName {
			return &selection{kind: selSpread, name: p.next().value}
		}
		return &selection{kind: selInline, sels: p.parseSelectionSet()}
	}
	if t.kind != tkName {
		failParse("Expected a field name but got %s %s", t.kind, t.value)
	}
	name := p.next().value
	var alias string
	if colon, ok := p.peek(); ok && colon.kind == tkPunct && colon.value == ":" {
		p.next()
		alias = name
		name = p.expectName()
	}
	var args []argNode
	if pr, ok := p.peek(); ok && pr.kind == tkPunct && pr.value == "(" {
		p.next()
		for {
			q, ok := p.peek()
			if !ok {
				failParse("Unterminated argument list")
			}
			if q.kind == tkPunct && q.value == ")" {
				p.next()
				break
			}
			argName := p.expectName()
			p.expectPunct(":")
			args = append(args, argNode{name: argName, value: p.parseValue()})
		}
	}
	// Directives (@...) — skip name and optional (...) args.
	for {
		d, ok := p.peek()
		if ok && d.kind == tkPunct && d.value == "@" {
			p.next()
			p.expectName()
			if a, ok := p.peek(); ok && a.kind == tkPunct && a.value == "(" {
				p.next()
				p.skipBalanced("(", ")")
			}
		} else {
			break
		}
	}
	if s, ok := p.peek(); ok && s.kind == tkPunct && s.value == "{" {
		return &selection{kind: selField, alias: alias, name: name, args: args, sels: p.parseSelectionSet()}
	}
	return &selection{kind: selField, alias: alias, name: name, args: args}
}

func (p *parser) parseValue() *valueNode {
	t := p.next()
	switch t.kind {
	case tkVar:
		return &valueNode{kind: vVar, str: t.value}
	case tkString:
		return &valueNode{kind: vString, str: t.value}
	case tkNumber:
		num, _ := strconv.ParseFloat(t.value, 64)
		return &valueNode{kind: vNumber, num: num}
	case tkName:
		switch t.value {
		case "true":
			return &valueNode{kind: vBool, b: true}
		case "false":
			return &valueNode{kind: vBool, b: false}
		case "null":
			return &valueNode{kind: vNull}
		default:
			return &valueNode{kind: vEnum, str: t.value}
		}
	case tkPunct:
		if t.value == "[" {
			v := &valueNode{kind: vList}
			for {
				q, ok := p.peek()
				if !ok {
					failParse("Unterminated list")
				}
				if q.kind == tkPunct && q.value == "]" {
					p.next()
					return v
				}
				v.items = append(v.items, p.parseValue())
			}
		}
		if t.value == "{" {
			v := &valueNode{kind: vObject}
			for {
				q, ok := p.peek()
				if !ok {
					failParse("Unterminated input object")
				}
				if q.kind == tkPunct && q.value == "}" {
					p.next()
					return v
				}
				name := p.expectName()
				p.expectPunct(":")
				v.fields = append(v.fields, objField{name: name, value: p.parseValue()})
			}
		}
	}
	failParse("Unexpected value token %s %s", t.kind, t.value)
	return nil
}

// ---------------------------------------------------------------------------
// Schema table (introspection + field validation)
// ---------------------------------------------------------------------------

type typeDef struct {
	kind       string // "OBJECT" | "ENUM"
	fields     []string
	enumValues []string
}

var typeDefs = map[string]typeDef{
	"Query":                            {kind: "OBJECT", fields: []string{"viewer", "repository", "node", "__type", "__schema"}},
	"Mutation":                         {kind: "OBJECT", fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"}},
	"Repository":                       {kind: "OBJECT", fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests", "description", "url", "createdAt", "updatedAt", "isPrivate", "defaultBranch"}},
	"Issue":                            {kind: "OBJECT", fields: []string{"id", "databaseId", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "labels", "author", "comments", "closed", "locked"}},
	"PullRequest":                      {kind: "OBJECT", fields: []string{"id", "databaseId", "number", "title", "body", "state", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "mergedAt", "files", "createdAt", "updatedAt", "url", "author"}},
	"User":                             {kind: "OBJECT", fields: []string{"id", "login", "name", "url", "avatarUrl"}},
	"IssueConnection":                  {kind: "OBJECT", fields: []string{"nodes", "totalCount", "pageInfo", "edges"}},
	"PullRequestConnection":            {kind: "OBJECT", fields: []string{"nodes", "totalCount", "pageInfo", "edges"}},
	"PullRequestChangedFileConnection": {kind: "OBJECT", fields: []string{"nodes", "totalCount", "pageInfo"}},
	"PullRequestChangedFile":           {kind: "OBJECT", fields: []string{"path", "filename", "additions", "deletions", "changes"}},
	"LabelConnection":                  {kind: "OBJECT", fields: []string{"nodes", "totalCount", "pageInfo"}},
	"Label":                            {kind: "OBJECT", fields: []string{"id", "name", "color", "description"}},
	"Ref":                              {kind: "OBJECT", fields: []string{"id", "name", "prefix", "target"}},
	"PageInfo":                         {kind: "OBJECT", fields: []string{"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"}},
	"IssueCommentConnection":           {kind: "OBJECT", fields: []string{"nodes", "totalCount", "pageInfo"}},
	"IssueComment":                     {kind: "OBJECT", fields: []string{"id", "body", "author", "createdAt", "updatedAt", "url"}},
	"CommentEdge":                      {kind: "OBJECT", fields: []string{"node", "cursor"}},
	"CreateIssuePayload":               {kind: "OBJECT", fields: []string{"issue", "clientMutationId"}},
	"CloseIssuePayload":                {kind: "OBJECT", fields: []string{"issue", "clientMutationId"}},
	"CreatePullRequestPayload":         {kind: "OBJECT", fields: []string{"pullRequest", "clientMutationId"}},
	"MergePullRequestPayload":          {kind: "OBJECT", fields: []string{"pullRequest", "clientMutationId"}},
	"AddCommentPayload":                {kind: "OBJECT", fields: []string{"commentEdge", "comment", "clientMutationId"}},
	"SearchType":                       {kind: "ENUM", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE"}},
	"IssueState":                       {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED"}},
	"PullRequestState":                 {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	"MergeableState":                   {kind: "ENUM", enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"OrderDirection":                   {kind: "ENUM", enumValues: []string{"ASC", "DESC"}},
	"IssueOrderField":                  {kind: "ENUM", enumValues: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"RepositoryPermission":             {kind: "ENUM", enumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	"PullRequestMergeMethod":           {kind: "ENUM", enumValues: []string{"MERGE", "SQUASH", "REBASE"}},
	"__Type":                           {kind: "OBJECT", fields: []string{"name", "kind", "fields", "enumValues", "description"}},
	"__Field":                          {kind: "OBJECT", fields: []string{"name", "description", "args", "type"}},
	"__EnumValue":                      {kind: "OBJECT", fields: []string{"name", "description"}},
	"__Schema":                         {kind: "OBJECT", fields: []string{"queryType", "mutationType", "subscriptionType", "types", "directives"}},
}

// typeDefOrder is the fixed order of __schema.types (Go map iteration is
// random, so the Node insertion order is kept in this slice).
var typeDefOrder = []string{
	"Query", "Mutation", "Repository", "Issue", "PullRequest", "User",
	"IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection",
	"PullRequestChangedFile", "LabelConnection", "Label", "Ref", "PageInfo",
	"IssueCommentConnection", "IssueComment", "CommentEdge",
	"CreateIssuePayload", "CloseIssuePayload", "CreatePullRequestPayload",
	"MergePullRequestPayload", "AddCommentPayload",
	"SearchType", "IssueState", "PullRequestState", "MergeableState",
	"OrderDirection", "IssueOrderField", "RepositoryPermission", "PullRequestMergeMethod",
	"__Type", "__Field", "__EnumValue", "__Schema",
}

// ---------------------------------------------------------------------------
// Values: plain scalars, nil, or a tagged object (gobj, "__gql" = type).
// ---------------------------------------------------------------------------

type gobj map[string]any

func isGobj(v any) (gobj, bool) {
	m, ok := v.(gobj)
	return m, ok
}

func userVal(login string) gobj {
	return gobj{"__gql": "User", "login": login, "id": nodeID("User", 1)}
}

func repoVal(repo *Repo) gobj {
	return gobj{"__gql": "Repository", "repo": repo}
}

func issueVal(repo *Repo, issue *Issue) gobj {
	return gobj{"__gql": "Issue", "repo": repo, "issue": issue}
}

func pullVal(repo *Repo, pull *PullRequest) gobj {
	return gobj{"__gql": "PullRequest", "repo": repo, "pull": pull}
}

func findIssueByNum(store *Store, num int) (*Repo, *Issue) {
	for _, repo := range store.Repos {
		for _, issue := range repo.Issues {
			if issue.ID == num {
				return repo, issue
			}
		}
	}
	return nil, nil
}

func findPullByNum(store *Store, num int) (*Repo, *PullRequest) {
	for _, repo := range store.Repos {
		for _, pull := range repo.Pulls {
			if pull.ID == num {
				return repo, pull
			}
		}
	}
	return nil, nil
}

func repoOf(store *Store, decoded *decodedNodeID) *Repo {
	return store.ReposByID[decoded.num]
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

type execCtx struct {
	store     *Store
	variables map[string]any
	frags     map[string]*fragmentDef
	errors    []any
}

func (ec *execCtx) addError(msg string) {
	ec.errors = append(ec.errors, map[string]any{"message": msg})
}

func evalValue(node *valueNode, variables map[string]any) any {
	switch node.kind {
	case vVar:
		if v, ok := variables[node.str]; ok {
			return v
		}
		return nil
	case vString:
		return node.str
	case vNumber:
		return node.num
	case vBool:
		return node.b
	case vNull:
		return nil
	case vEnum:
		return node.str
	case vList:
		out := make([]any, len(node.items))
		for i, item := range node.items {
			out[i] = evalValue(item, variables)
		}
		return out
	case vObject:
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = evalValue(f.value, variables)
		}
		return out
	}
	return nil
}

func evalArgs(sel *selection, ec *execCtx) map[string]any {
	out := map[string]any{}
	for _, a := range sel.args {
		out[a.name] = evalValue(a.value, ec.variables)
	}
	return out
}

func asRecord(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func asInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

func issueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(pull *PullRequest) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func ownerLogin(repo *Repo, store *Store) string {
	if repo.OwnerLogin != "" {
		return repo.OwnerLogin
	}
	return store.ViewerLogin()
}

func pageInfoVal(hasNext, hasPrev bool, endCursor, startCursor any) gobj {
	return gobj{
		"__gql":           "PageInfo",
		"hasNextPage":     hasNext,
		"hasPreviousPage": hasPrev,
		"endCursor":       endCursor,
		"startCursor":     startCursor,
	}
}

func clampSliceLen(n, max int) int {
	if n < 0 {
		return 0
	}
	if n > max {
		return max
	}
	return n
}

func issuesConnection(repo *Repo, args map[string]any) gobj {
	list := append([]*Issue{}, repo.Issues...)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[strings.ToLower(asString(s))] = true
		}
		filtered := list[:0]
		// copy first: list shares backing with the fresh copy above, safe to filter in place
		for _, iss := range list {
			if wanted[iss.State] {
				filtered = append(filtered, iss)
			}
		}
		list = filtered
	}
	if orderBy := asRecord(args["orderBy"]); orderBy != nil {
		field := asString(orderBy["field"])
		if field == "" {
			field = "CREATED_AT"
		}
		dir := asString(orderBy["direction"])
		if dir == "" {
			dir = "ASC"
		}
		key := func(iss *Issue) string {
			if field == "UPDATED_AT" {
				return iss.UpdatedAt
			}
			return iss.CreatedAt
		}
		desc := dir == "DESC"
		sort.Slice(list, func(i, j int) bool {
			ka, kb := key(list[i]), key(list[j])
			if ka != kb {
				if desc {
					return ka > kb
				}
				return ka < kb
			}
			if desc {
				return list[i].Number > list[j].Number
			}
			return list[i].Number < list[j].Number
		})
	} else {
		sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	}
	offset := clampSliceLen(decodeCursor(args["after"]), len(list))
	first := asInt(args["first"], len(list))
	end := clampSliceLen(offset+first, len(list))
	page := list[offset:end]
	endIndex := offset + len(page) - 1
	nodes := make([]any, len(page))
	for i, iss := range page {
		nodes[i] = issueVal(repo, iss)
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(endIndex)
		startCursor = encodeCursor(offset)
	}
	return gobj{
		"__gql":      "IssueConnection",
		"nodes":      nodes,
		"totalCount": len(list),
		"pageInfo":   pageInfoVal(offset+len(page) < len(list), offset > 0, endCursor, startCursor),
	}
}

func pullsConnection(repo *Repo, args map[string]any) gobj {
	list := append([]*PullRequest{}, repo.Pulls...)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, s := range states {
			wanted[asString(s)] = true
		}
		filtered := []*PullRequest{}
		for _, p := range list {
			if wanted[pullState(p)] {
				filtered = append(filtered, p)
			}
		}
		list = filtered
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	first := asInt(args["first"], len(list))
	end := clampSliceLen(first, len(list))
	page := list[:end]
	nodes := make([]any, len(page))
	for i, p := range page {
		nodes[i] = pullVal(repo, p)
	}
	var endCursor, startCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(len(page) - 1)
		startCursor = encodeCursor(0)
	}
	return gobj{
		"__gql":      "PullRequestConnection",
		"nodes":      nodes,
		"totalCount": len(list),
		"pageInfo":   pageInfoVal(len(page) < len(list), false, endCursor, startCursor),
	}
}

func typeVal(name string) any {
	def, ok := typeDefs[name]
	if !ok {
		return nil
	}
	return gobj{"__gql": "__Type", "name": name, "kind": def.kind, "fields": def.fields, "enumValues": def.enumValues}
}

func resolveField(parent gobj, field string, args map[string]any, ec *execCtx) any {
	store := ec.store
	switch parent["__gql"] {
	case "Query":
		switch field {
		case "viewer":
			return userVal(store.ViewerLogin())
		case "repository":
			repo := store.GetRepo(asString(args["owner"]), asString(args["name"]))
			if repo == nil {
				return nil
			}
			return repoVal(repo)
		case "node":
			decoded := decodeNodeID(args["id"])
			if decoded == nil {
				return nil
			}
			switch decoded.typ {
			case "Repository":
				repo := repoOf(store, decoded)
				if repo == nil {
					return nil
				}
				return repoVal(repo)
			case "Issue":
				repo, issue := findIssueByNum(store, decoded.num)
				if issue == nil {
					return nil
				}
				return issueVal(repo, issue)
			case "PullRequest":
				repo, pull := findPullByNum(store, decoded.num)
				if pull == nil {
					return nil
				}
				return pullVal(repo, pull)
			case "User":
				return userVal(store.ViewerLogin())
			}
			return nil
		case "__type":
			return typeVal(asString(args["name"]))
		case "__schema":
			return gobj{"__gql": "__Schema"}
		}
		return nil
	case "Mutation":
		switch field {
		case "createIssue":
			input := asRecord(args["input"])
			decoded := decodeNodeID(input["repositoryId"])
			var repo *Repo
			if decoded != nil && decoded.typ == "Repository" {
				repo = repoOf(store, decoded)
			}
			if repo == nil {
				ec.addError("createIssue: repository not found")
				return gobj{"__gql": "CreateIssuePayload", "issue": nil}
			}
			created, errLabel := store.CreateIssue(repo, asString(input["title"]), asString(input["body"]), nil)
			if errLabel != "" {
				ec.addError("createIssue: unknown label " + errLabel)
				return gobj{"__gql": "CreateIssuePayload", "issue": nil}
			}
			return gobj{"__gql": "CreateIssuePayload", "issue": issueVal(repo, created)}
		case "closeIssue":
			input := asRecord(args["input"])
			decoded := decodeNodeID(input["issueId"])
			var repo *Repo
			var found *Issue
			if decoded != nil && decoded.typ == "Issue" {
				repo, found = findIssueByNum(store, decoded.num)
			}
			if found == nil {
				ec.addError("closeIssue: issue not found")
				return gobj{"__gql": "CloseIssuePayload", "issue": nil}
			}
			found.State = "closed"
			found.UpdatedAt = store.NowISO()
			return gobj{"__gql": "CloseIssuePayload", "issue": issueVal(repo, found)}
		case "createPullRequest":
			input := asRecord(args["input"])
			decoded := decodeNodeID(input["repositoryId"])
			var repo *Repo
			if decoded != nil && decoded.typ == "Repository" {
				repo = repoOf(store, decoded)
			}
			if repo == nil {
				ec.addError("createPullRequest: repository not found")
				return gobj{"__gql": "CreatePullRequestPayload", "pullRequest": nil}
			}
			created, errMsg := store.CreatePull(repo, asString(input["title"]), asString(input["body"]), asString(input["headRefName"]), asString(input["baseRefName"]))
			if errMsg != "" {
				ec.addError("createPullRequest: " + errMsg)
				return gobj{"__gql": "CreatePullRequestPayload", "pullRequest": nil}
			}
			return gobj{"__gql": "CreatePullRequestPayload", "pullRequest": pullVal(repo, created)}
		case "mergePullRequest":
			input := asRecord(args["input"])
			decoded := decodeNodeID(input["pullRequestId"])
			var repo *Repo
			var found *PullRequest
			if decoded != nil && decoded.typ == "PullRequest" {
				repo, found = findPullByNum(store, decoded.num)
			}
			if found == nil {
				ec.addError("mergePullRequest: pull request not found")
				return gobj{"__gql": "MergePullRequestPayload", "pullRequest": nil}
			}
			if _, errMsg := store.MergePull(repo, found); errMsg != "" {
				ec.addError("mergePullRequest: " + errMsg)
				return gobj{"__gql": "MergePullRequestPayload", "pullRequest": pullVal(repo, found)}
			}
			return gobj{"__gql": "MergePullRequestPayload", "pullRequest": pullVal(repo, found)}
		case "addComment":
			input := asRecord(args["input"])
			decoded := decodeNodeID(input["subjectId"])
			var repo *Repo
			var found *Issue
			if decoded != nil && decoded.typ == "Issue" {
				repo, found = findIssueByNum(store, decoded.num)
			}
			if found == nil {
				ec.addError("addComment: subject not found")
				return gobj{"__gql": "AddCommentPayload", "commentEdge": nil}
			}
			now := store.NowISO()
			comment := &IssueComment{
				ID:        repo.NextCommentID,
				Body:      asString(input["body"]),
				UserLogin: store.ViewerLogin(),
				CreatedAt: now,
				UpdatedAt: now,
			}
			repo.NextCommentID++
			found.Comments = append(found.Comments, comment)
			node := gobj{
				"__gql":     "IssueComment",
				"id":        nodeID("IssueComment", repo.ID*1000000+comment.ID),
				"body":      comment.Body,
				"createdAt": comment.CreatedAt,
				"updatedAt": comment.UpdatedAt,
				"author":    userVal(comment.UserLogin),
			}
			return gobj{"__gql": "AddCommentPayload", "commentEdge": gobj{"__gql": "CommentEdge", "node": node, "cursor": nil}}
		}
		return nil
	case "Repository":
		repo := parent["repo"].(*Repo)
		switch field {
		case "id":
			return nodeID("Repository", repo.ID)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return store.RepoFullName(repo)
		case "owner":
			return userVal(ownerLogin(repo, store))
		case "defaultBranchRef":
			return gobj{"__gql": "Ref", "name": repo.DefaultBranch, "prefix": "refs/heads/", "id": nodeID("Ref", repo.ID)}
		case "defaultBranch":
			return repo.DefaultBranch
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "isPrivate":
			return repo.Private
		case "description":
			return repo.Description
		case "createdAt":
			return repo.CreatedAt
		case "updatedAt":
			return repo.UpdatedAt
		case "url":
			return "https://github.com/" + ownerLogin(repo, store) + "/" + repo.Name
		case "issue":
			if num, ok := asFloat(args["number"]); ok {
				for _, iss := range repo.Issues {
					if float64(iss.Number) == num {
						return issueVal(repo, iss)
					}
				}
			}
			return nil
		case "issues":
			return issuesConnection(repo, args)
		case "pullRequest":
			if num, ok := asFloat(args["number"]); ok {
				for _, p := range repo.Pulls {
					if float64(p.Number) == num {
						return pullVal(repo, p)
					}
				}
			}
			return nil
		case "pullRequests":
			return pullsConnection(repo, args)
		}
		return nil
	case "Issue":
		repo := parent["repo"].(*Repo)
		issue := parent["issue"].(*Issue)
		switch field {
		case "id":
			return nodeID("Issue", issue.ID)
		case "databaseId":
			return issue.ID
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			return issueState(issue)
		case "closed":
			return issue.State == "closed"
		case "locked":
			return false
		case "url":
			return "https://github.com/" + ownerLogin(repo, store) + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number)
		case "createdAt":
			return issue.CreatedAt
		case "updatedAt":
			return issue.UpdatedAt
		case "author":
			return userVal(issue.UserLogin)
		case "labels":
			nodes := make([]any, len(issue.Labels))
			for i, name := range issue.Labels {
				nodes[i] = gobj{"__gql": "Label", "name": name, "id": nodeID("Label", issue.ID), "color": "", "description": ""}
			}
			return gobj{
				"__gql":      "LabelConnection",
				"nodes":      nodes,
				"totalCount": len(issue.Labels),
				"pageInfo":   pageInfoVal(false, false, nil, nil),
			}
		case "comments":
			nodes := make([]any, len(issue.Comments))
			for i, c := range issue.Comments {
				nodes[i] = gobj{
					"__gql":     "IssueComment",
					"id":        nodeID("IssueComment", repo.ID*1000000+c.ID),
					"body":      c.Body,
					"createdAt": c.CreatedAt,
					"updatedAt": c.UpdatedAt,
					"author":    userVal(c.UserLogin),
				}
			}
			return gobj{
				"__gql":      "IssueCommentConnection",
				"nodes":      nodes,
				"totalCount": len(issue.Comments),
				"pageInfo":   pageInfoVal(false, false, nil, nil),
			}
		}
		return nil
	case "PullRequest":
		repo := parent["repo"].(*Repo)
		pull := parent["pull"].(*PullRequest)
		switch field {
		case "id":
			return nodeID("PullRequest", pull.ID)
		case "databaseId":
			return pull.ID
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			return pullState(pull)
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "headRefOid":
			return pull.HeadSha
		case "baseRefOid":
			return pull.BaseSha
		case "mergeable":
			if pull.State == "open" {
				return "MERGEABLE"
			}
			return "UNKNOWN"
		case "merged":
			return pull.Merged
		case "mergedAt":
			return pull.MergedAt
		case "createdAt":
			return pull.CreatedAt
		case "updatedAt":
			return pull.UpdatedAt
		case "url":
			return "https://github.com/" + ownerLogin(repo, store) + "/" + repo.Name + "/pull/" + strconv.Itoa(pull.Number)
		case "author":
			return userVal(ownerLogin(repo, store))
		case "files":
			first := asInt(args["first"], len(pull.Files))
			end := clampSliceLen(first, len(pull.Files))
			nodes := make([]any, end)
			for i, f := range pull.Files[:end] {
				nodes[i] = gobj{"__gql": "PullRequestChangedFile", "path": f.Filename, "filename": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes}
			}
			return gobj{
				"__gql":      "PullRequestChangedFileConnection",
				"nodes":      nodes,
				"totalCount": len(pull.Files),
				"pageInfo":   pageInfoVal(first < len(pull.Files), false, nil, nil),
			}
		}
		return nil
	case "User":
		switch field {
		case "login":
			return asString(parent["login"])
		case "id":
			return asString(parent["id"])
		case "name":
			return asString(parent["login"])
		case "url":
			return "https://github.com/" + asString(parent["login"])
		case "avatarUrl":
			return ""
		}
		return nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection", "LabelConnection", "IssueCommentConnection":
		switch field {
		case "nodes":
			return parent["nodes"]
		case "totalCount":
			return parent["totalCount"]
		case "pageInfo":
			return parent["pageInfo"]
		case "edges":
			return []any{}
		}
		return nil
	case "PullRequestChangedFile":
		switch field {
		case "path", "filename":
			return asString(parent["path"])
		case "additions":
			return asInt(parent["additions"], 0)
		case "deletions":
			return asInt(parent["deletions"], 0)
		case "changes":
			return asInt(parent["changes"], 0)
		}
		return nil
	case "Label":
		switch field {
		case "name":
			return asString(parent["name"])
		case "id":
			return asString(parent["id"])
		case "color":
			return asString(parent["color"])
		case "description":
			return asString(parent["description"])
		}
		return nil
	case "Ref":
		switch field {
		case "name":
			return asString(parent["name"])
		case "id":
			return asString(parent["id"])
		case "prefix":
			if v, ok := parent["prefix"].(string); ok && v != "" {
				return v
			}
			return "refs/heads/"
		case "target":
			return nil
		}
		return nil
	case "PageInfo":
		switch field {
		case "hasNextPage":
			b, _ := parent["hasNextPage"].(bool)
			return b
		case "hasPreviousPage":
			b, _ := parent["hasPreviousPage"].(bool)
			return b
		case "endCursor":
			return parent["endCursor"]
		case "startCursor":
			return parent["startCursor"]
		}
		return nil
	case "__Type":
		switch field {
		case "name":
			return asString(parent["name"])
		case "kind":
			return asString(parent["kind"])
		case "description":
			return nil
		case "fields":
			fields, _ := parent["fields"].([]string)
			out := make([]any, len(fields))
			for i, name := range fields {
				out[i] = gobj{"__gql": "__Field", "name": name, "description": nil}
			}
			return out
		case "enumValues":
			values, _ := parent["enumValues"].([]string)
			out := make([]any, len(values))
			for i, name := range values {
				out[i] = gobj{"__gql": "__EnumValue", "name": name, "description": nil}
			}
			return out
		}
		return nil
	case "__Field":
		switch field {
		case "name":
			return asString(parent["name"])
		case "description":
			return nil
		case "args":
			return []any{}
		case "type":
			return nil
		}
		return nil
	case "__EnumValue":
		switch field {
		case "name":
			return asString(parent["name"])
		case "description":
			return nil
		}
		return nil
	case "__Schema":
		switch field {
		case "queryType":
			return typeVal("Query")
		case "mutationType":
			return typeVal("Mutation")
		case "subscriptionType":
			return nil
		case "types":
			out := make([]any, len(typeDefOrder))
			for i, name := range typeDefOrder {
				out[i] = typeVal(name)
			}
			return out
		case "directives":
			return []any{}
		}
		return nil
	case "CommentEdge":
		switch field {
		case "node":
			return parent["node"]
		case "cursor":
			return nil
		}
		return nil
	case "IssueComment":
		switch field {
		case "id":
			return asString(parent["id"])
		case "body":
			return asString(parent["body"])
		case "author":
			return parent["author"]
		case "createdAt":
			return asString(parent["createdAt"])
		case "updatedAt":
			return asString(parent["updatedAt"])
		case "url":
			return ""
		}
		return nil
	case "CreateIssuePayload", "CloseIssuePayload":
		switch field {
		case "issue":
			return parent["issue"]
		case "clientMutationId":
			return nil
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		switch field {
		case "pullRequest":
			return parent["pullRequest"]
		case "clientMutationId":
			return nil
		}
		return nil
	case "AddCommentPayload":
		switch field {
		case "commentEdge":
			return parent["commentEdge"]
		case "comment":
			if edge, ok := parent["commentEdge"].(gobj); ok && edge != nil {
				return edge["node"]
			}
			return nil
		case "clientMutationId":
			return nil
		}
		return nil
	}
	return nil
}

func executeSelections(sels []*selection, parent gobj, ec *execCtx) map[string]any {
	out := map[string]any{}
	parentType := asString(parent["__gql"])
	for _, sel := range sels {
		switch sel.kind {
		case selSpread:
			frag, ok := ec.frags[sel.name]
			if !ok {
				ec.addError(fmt.Sprintf("Unknown fragment %q", sel.name))
				continue
			}
			if frag.typeCond != parentType {
				continue
			}
			for k, v := range executeSelections(frag.sels, parent, ec) {
				out[k] = v
			}
			continue
		case selInline:
			if sel.typeCond != "" && sel.typeCond != parentType {
				continue
			}
			for k, v := range executeSelections(sel.sels, parent, ec) {
				out[k] = v
			}
			continue
		}
		key := sel.name
		if sel.alias != "" {
			key = sel.alias
		}
		if sel.name == "__typename" {
			out[key] = parentType
			continue
		}
		def, ok := typeDefs[parentType]
		if !ok || !containsString(def.fields, sel.name) {
			ec.addError(fmt.Sprintf("Cannot query field %q on type %q.", sel.name, parentType))
			continue
		}
		args := evalArgs(sel, ec)
		value := resolveField(parent, sel.name, args, ec)
		out[key] = materialize(value, sel.sels, ec)
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func materialize(value any, sels []*selection, ec *execCtx) any {
	if value == nil {
		return nil
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, len(arr))
		for i, item := range arr {
			out[i] = materialize(item, sels, ec)
		}
		return out
	}
	if obj, ok := isGobj(value); ok {
		if len(sels) == 0 {
			return nil
		}
		return executeSelections(sels, obj, ec)
	}
	return value
}

func runOperation(store *Store, op any) (result map[string]any) {
	rec := asRecord(op)
	if rec == nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Expected an operation with a query string"}}}
	}
	query, ok := rec["query"].(string)
	if !ok {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Expected an operation with a query string"}}}
	}
	variables := asRecord(rec["variables"])
	if variables == nil {
		variables = map[string]any{}
	}

	doc, err := func() (doc *document, err error) {
		defer func() {
			if r := recover(); r != nil {
				if pe, ok := r.(parseErr); ok {
					doc = nil
					err = pe
				} else {
					panic(r)
				}
			}
		}()
		return (&parser{toks: tokenize(query)}).parseDocument(), nil
	}()
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}

	var operation *operation
	if name, ok := rec["operationName"].(string); ok && name != "" {
		for _, o := range doc.ops {
			if o.name == name {
				operation = o
				break
			}
		}
		if operation == nil {
			return map[string]any{"data": nil, "errors": []any{map[string]any{"message": fmt.Sprintf("Unknown operation named %q", name)}}}
		}
	} else if len(doc.ops) == 1 {
		operation = doc.ops[0]
	} else {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Must provide operation name when the document has multiple operations"}}}
	}

	ec := &execCtx{store: store, variables: variables, frags: doc.frags}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	root := gobj{"__gql": rootType}
	data := executeSelections(operation.sels, root, ec)
	result = map[string]any{"data": data}
	if len(ec.errors) > 0 {
		result["errors"] = ec.errors
	}
	return result
}
