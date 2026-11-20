package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type selection struct {
	name, alias, on, fragment string
	args                      doc
	fields                    []selection
}

type operation struct {
	name, kind string
	defaults   doc
	fields     []selection
}

type parser struct {
	tokens []string
	pos    int
}

type graphFault string
type variable string
type resolver func(doc) any

func gqlError(s string) { panic(graphFault(s)) }

func tokenize(source string) []string {
	tokens := []string{}
	nameByte := func(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	for i := 0; i < len(source); {
		start := i
		c := source[i]
		i++
		switch {
		case c == ' ' || c == '\r' || c == '\n' || c == '\t' || c == ',':
			continue
		case c == '#':
			for i < len(source) && source[i] != '\n' && source[i] != '\r' {
				i++
			}
			continue
		case nameByte(c):
			for i < len(source) && (nameByte(source[i]) || digit(source[i])) {
				i++
			}
		case c == '"':
			closed := false
			for i < len(source) {
				v := source[i]
				i++
				if v == '\\' && i < len(source) {
					i++
					continue
				}
				if v == '"' {
					closed = true
					break
				}
			}
			if !closed {
				gqlError("Unterminated string")
			}
		case c == '-' || digit(c):
			for i < len(source) && (digit(source[i]) || strings.ContainsRune(".+-eE", rune(source[i]))) {
				i++
			}
		case c == '.' && strings.HasPrefix(source[start:], "..."):
			i = start + 3
		case strings.ContainsRune("!$():=@[]{}|", rune(c)):
		default:
			gqlError("Invalid token at " + strconv.Itoa(start))
		}
		tokens = append(tokens, source[start:i])
	}
	return tokens
}

func (p *parser) peek() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.pos]
}

func (p *parser) take() string {
	s := p.peek()
	if s == "" {
		gqlError("Unexpected end of document")
	}
	p.pos++
	return s
}

func (p *parser) accept(s string) bool {
	if p.peek() != s {
		return false
	}
	p.pos++
	return true
}

func (p *parser) need(s string) {
	if !p.accept(s) {
		gqlError("Expected " + s)
	}
}

func (p *parser) name() string {
	s := p.take()
	c := s[0]
	if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
		gqlError("Expected a name")
	}
	return s
}

func (p *parser) value() any {
	if p.accept("$") {
		return variable(p.name())
	}
	if p.accept("[") {
		var a []any
		for !p.accept("]") {
			a = append(a, p.value())
		}
		return a
	}
	if p.accept("{") {
		d := doc{}
		for !p.accept("}") {
			key := p.name()
			p.need(":")
			d[key] = p.value()
		}
		return d
	}
	s := p.take()
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if s[0] == '"' {
		var v string
		if json.Unmarshal([]byte(s), &v) != nil {
			gqlError("Invalid string")
		}
		return v
	}
	if s[0] == '-' || s[0] >= '0' && s[0] <= '9' {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			gqlError("Invalid number")
		}
		return n
	}
	return s
}

func (p *parser) arguments() doc {
	d := doc{}
	if p.accept("(") {
		for !p.accept(")") {
			key := p.name()
			p.need(":")
			d[key] = p.value()
		}
	}
	return d
}

func (p *parser) directives(d doc) {
	for p.accept("@") {
		name := p.name()
		d["@"+name] = p.arguments()["if"]
	}
}

func (p *parser) fields(depth int) []selection {
	if depth > 128 {
		gqlError("Selection is too deep")
	}
	p.need("{")
	var out []selection
	for !p.accept("}") {
		f := selection{}
		if p.accept("...") {
			if p.accept("on") {
				f.on = p.name()
			} else {
				f.fragment = p.name()
			}
			f.args = doc{}
			p.directives(f.args)
			if f.on != "" {
				f.fields = p.fields(depth + 1)
			}
		} else {
			f.alias = p.name()
			f.name = f.alias
			if p.accept(":") {
				f.name = p.name()
			}
			f.args = p.arguments()
			p.directives(f.args)
			if p.peek() == "{" {
				f.fields = p.fields(depth + 1)
			}
		}
		out = append(out, f)
	}
	return out
}

func (p *parser) typeName() {
	if p.accept("[") {
		p.typeName()
		p.need("]")
	} else {
		p.name()
	}
	p.accept("!")
}

func (p *parser) document() ([]operation, map[string]selection) {
	var ops []operation
	fragments := map[string]selection{}
	for p.peek() != "" {
		if p.accept("fragment") {
			key := p.name()
			p.need("on")
			on := p.name()
			fragments[key] = selection{on: on, fields: p.fields(0)}
			continue
		}
		op := operation{kind: "query", defaults: doc{}}
		if p.peek() != "{" {
			op.kind = p.name()
			if op.kind != "query" && op.kind != "mutation" {
				gqlError("Unsupported operation " + op.kind)
			}
			if p.peek() != "(" && p.peek() != "{" {
				op.name = p.name()
			}
			if p.accept("(") {
				for !p.accept(")") {
					p.need("$")
					key := p.name()
					p.need(":")
					p.typeName()
					if p.accept("=") {
						op.defaults[key] = p.value()
					}
				}
			}
		}
		op.fields = p.fields(0)
		ops = append(ops, op)
	}
	return ops, fragments
}

func resolve(v any, vars doc) any {
	switch val := v.(type) {
	case variable:
		return vars[string(val)]
	case doc:
		d := make(doc, len(val))
		for k, x := range val {
			d[k] = resolve(x, vars)
		}
		return d
	case []any:
		out := make([]any, len(val))
		for i, x := range val {
			out[i] = resolve(x, vars)
		}
		return out
	}
	return v
}

type gqlContext struct {
	vars      doc
	fragments map[string]selection
}

func (g gqlContext) project(v any, fields []selection, active map[string]bool) any {
	if v == nil || len(fields) == 0 {
		return v
	}
	if a, yes := v.([]any); yes {
		out := make([]any, len(a))
		for j, x := range a {
			out[j] = g.project(x, fields, active)
		}
		return out
	}
	d, yes := v.(doc)
	if !yes {
		gqlError("Selection on a scalar field")
	}
	out := make(doc, len(fields))
	for _, f := range fields {
		args := resolve(f.args, g.vars).(doc)
		if args["@skip"] == true || args["@include"] == false {
			continue
		}
		if f.on != "" || f.fragment != "" {
			spec := f
			if f.fragment != "" {
				var exists bool
				spec, exists = g.fragments[f.fragment]
				if !exists {
					gqlError("Unknown fragment " + f.fragment)
				}
				if active[f.fragment] {
					gqlError("Cyclic fragment " + f.fragment)
				}
				active[f.fragment] = true
			}
			if spec.on == d["__typename"] || spec.on == "Node" {
				sub := g.project(d, spec.fields, active).(doc)
				for k, x := range sub {
					out[k] = x
				}
			}
			delete(active, f.fragment)
			continue
		}
		field, exists := d[f.name]
		if !exists {
			gqlError("Cannot query field \"" + f.name + "\" on type \"" + str(d["__typename"]) + "\"")
		}
		if fn, yes := field.(resolver); yes {
			field = fn(args)
		}
		out[f.alias] = g.project(field, f.fields, active)
	}
	return out
}

func connection(all []any, args doc) doc {
	start := 0
	if cursor := str(args["after"]); cursor != "" {
		b, err := base64.StdEncoding.DecodeString(cursor)
		n, errN := strconv.Atoi(strings.TrimPrefix(string(b), "cursor:"))
		if err != nil || errN != nil || n < 0 || n >= len(all) {
			gqlError("Invalid cursor")
		}
		start = n + 1
	}
	count := 30
	if n, set := args["first"]; set {
		count = max(0, min(100, integer(n)))
	}
	end := min(start+count, len(all))
	nodes := make([]any, end-start)
	copy(nodes, all[start:end])
	edges := make([]any, 0, len(nodes))
	var first, last any
	for j, n := range nodes {
		cursor := base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(start+j)))
		if j == 0 {
			first = cursor
		}
		last = cursor
		edges = append(edges, doc{"node": n, "cursor": cursor})
	}
	return doc{"nodes": nodes, "edges": edges, "totalCount": len(all), "pageInfo": doc{"hasPreviousPage": start > 0, "hasNextPage": end < len(all), "startCursor": first, "endCursor": last}}
}

var graphTypes = map[string]string{
	"User":         "id databaseId login",
	"Repository":   "id databaseId name nameWithOwner description owner defaultBranchRef viewerPermission hasIssuesEnabled isPrivate issue issues pullRequest pullRequests",
	"Issue":        "id databaseId number title body state author createdAt updatedAt closedAt comments labels",
	"PullRequest":  "id databaseId number title body state author createdAt updatedAt closedAt headRefName baseRefName headRefOid baseRefOid merged mergeable files comments labels",
	"IssueComment": "id databaseId body author createdAt updatedAt",
}
var graphEnums = map[string]string{"SearchType": "ISSUE REPOSITORY USER", "IssueState": "OPEN CLOSED", "PullRequestState": "OPEN CLOSED MERGED"}

func typeInfo(args doc) any {
	name := str(args["name"])
	kind, names, key := "OBJECT", graphTypes[name], "fields"
	if names == "" {
		kind, names, key = "ENUM", graphEnums[name], "enumValues"
	}
	if names == "" {
		return nil
	}
	fields := []any{}
	for _, n := range strings.Fields(names) {
		fields = append(fields, doc{"name": n})
	}
	d := doc{"name": name, "kind": kind, "fields": nil, "enumValues": nil}
	d[key] = fields
	return d
}

func (f *forge) graphUser() doc {
	return doc{"__typename": "User", "id": f.user["node_id"], "databaseId": 1, "login": f.owner}
}
func (f *forge) graphComment(c doc) doc {
	return doc{"__typename": "IssueComment", "id": c["node_id"], "databaseId": c["id"], "body": c["body"], "author": f.graphUser(), "createdAt": c["created_at"], "updatedAt": c["updated_at"]}
}

func (f *forge) graphItem(r *repository, i *issue) doc {
	d := i.data
	out := doc{"__typename": "Issue", "id": d["node_id"], "databaseId": d["id"], "number": i.number, "title": d["title"], "body": d["body"], "state": strings.ToUpper(str(d["state"])), "author": f.graphUser(), "createdAt": d["created_at"], "updatedAt": d["updated_at"], "closedAt": d["closed_at"]}
	out["comments"] = resolver(func(a doc) any {
		rows := []any{}
		for _, c := range i.comments {
			rows = append(rows, f.graphComment(c))
		}
		return connection(rows, a)
	})
	out["labels"] = resolver(func(a doc) any {
		rows := []any{}
		for _, l := range d["labels"].([]doc) {
			rows = append(rows, doc{"name": l["name"], "id": l["node_id"], "color": l["color"], "description": l["description"]})
		}
		return connection(rows, a)
	})
	if i.head == "" {
		return out
	}
	h, b := r.tips(i)
	out["__typename"] = "PullRequest"
	out["headRefName"], out["baseRefName"], out["headRefOid"], out["baseRefOid"] = i.head, i.base, h.String(), b.String()
	out["merged"] = d["merged"]
	if d["merged"] == true {
		out["state"] = "MERGED"
	}
	out["mergeable"] = resolver(func(a doc) any {
		if d["merged"] == true {
			return "UNKNOWN"
		}
		if r.mergeTree(i) == nil {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	})
	out["files"] = resolver(func(a doc) any {
		rows := []any{}
		for _, file := range r.fileDiff(i) {
			rows = append(rows, doc{"path": file["filename"], "additions": file["additions"], "deletions": file["deletions"], "changeType": strings.ToUpper(str(file["status"]))})
		}
		return connection(rows, a)
	})
	return out
}

func (f *forge) graphRepo(r *repository) doc {
	var head any
	if id := r.refs[heads+r.branch]; id != zero {
		head = doc{"name": r.branch, "target": doc{"oid": id.String()}}
	}
	out := doc{"__typename": "Repository", "id": r.gid, "databaseId": r.id, "name": r.name, "nameWithOwner": f.owner + "/" + r.name, "owner": f.graphUser(), "description": r.description, "defaultBranchRef": head, "viewerPermission": "ADMIN", "hasIssuesEnabled": true, "isPrivate": r.private}
	one := func(pulls bool) resolver {
		return func(a doc) any {
			m := r.issues
			if pulls {
				m = r.pulls
			}
			i := m[integer(a["number"])]
			if i == nil {
				return nil
			}
			return f.graphItem(r, i)
		}
	}
	many := func(pulls bool) resolver {
		return func(a doc) any {
			order, _ := a["orderBy"].(doc)
			items := r.list(pulls, false, "all", order["direction"] == "ASC")
			states := map[string]bool{}
			filter := a["states"] != nil
			switch v := a["states"].(type) {
			case []any:
				for _, s := range v {
					states[str(s)] = true
				}
			case string:
				states[v] = true
			}
			rows := []any{}
			for _, i := range items {
				node := f.graphItem(r, i)
				if !filter || states[str(node["state"])] {
					rows = append(rows, node)
				}
			}
			return connection(rows, a)
		}
	}
	out["issue"], out["pullRequest"] = one(false), one(true)
	out["issues"], out["pullRequests"] = many(false), many(true)
	return out
}

func (f *forge) queryRoot() doc {
	return doc{"__typename": "Query", "viewer": f.graphUser(), "__type": resolver(typeInfo),
		"repository": resolver(func(a doc) any {
			if !strings.EqualFold(str(a["owner"]), f.owner) {
				return nil
			}
			r := f.repos[strings.ToLower(str(a["name"]))]
			if r == nil {
				return nil
			}
			return f.graphRepo(r)
		}),
		"node": resolver(func(a doc) any {
			id := str(a["id"])
			if id == f.user["node_id"] {
				return f.graphUser()
			}
			n := f.byID[id]
			if n.repo == nil {
				return nil
			}
			if n.item != nil {
				return f.graphItem(n.repo, n.item)
			}
			return f.graphRepo(n.repo)
		}),
	}
}

func (f *forge) mutation(key string, args doc) any {
	d, _ := args["input"].(doc)
	var node nodeRef
	for _, k := range []string{"repositoryId", "issueId", "pullRequestId", "subjectId"} {
		if id := str(d[k]); id != "" {
			node = f.byID[id]
			break
		}
	}
	r, i := node.repo, node.item
	if r == nil {
		gqlError("Unknown node ID")
	}
	payload := doc{"clientMutationId": d["clientMutationId"]}
	switch key {
	case "createIssue":
		payload["issue"] = f.graphItem(r, f.newIssue(r, d, "Issue"))
	case "closeIssue", "reopenIssue":
		if i == nil {
			gqlError("Expected issue ID")
		}
		state := "closed"
		if key == "reopenIssue" {
			state = "open"
		}
		f.edit(r, i, doc{"state": state})
		payload["issue"] = f.graphItem(r, i)
	case "addComment":
		if i == nil {
			gqlError("Expected issue or pull request ID")
		}
		payload["commentEdge"] = doc{"node": f.graphComment(f.comment(i, required(d, "body")))}
	case "createPullRequest":
		data := doc{"head": d["headRefName"], "base": d["baseRefName"], "title": d["title"], "body": d["body"]}
		payload["pullRequest"] = f.graphItem(r, f.openPull(r, data))
	case "mergePullRequest":
		if i == nil || i.head == "" {
			gqlError("Expected pull request ID")
		}
		f.merge(r, i, doc{"merge_method": strings.ToLower(str(d["mergeMethod"])), "sha": d["expectedHeadOid"]})
		payload["pullRequest"] = f.graphItem(r, i)
	default:
		gqlError("Unknown mutation " + key)
	}
	return payload
}

func (f *forge) graphOne(request any) (answer doc) {
	defer func() {
		if err := recover(); err != nil {
			message := fmt.Sprint(err)
			if e, ok := err.(fault); ok {
				message = e.message
			}
			answer = doc{"errors": []doc{{"message": message}}}
		}
	}()
	d, _ := request.(doc)
	p := parser{tokens: tokenize(str(d["query"]))}
	ops, fragments := p.document()
	var op *operation
	for i := range ops {
		if ops[i].name == str(d["operationName"]) || len(ops) == 1 && d["operationName"] == nil {
			op = &ops[i]
			break
		}
	}
	if op == nil {
		gqlError("An operation name is required for this document")
	}
	vars := op.defaults
	if input, ok := d["variables"].(doc); ok {
		for k, v := range input {
			vars[k] = v
		}
	}
	root := f.queryRoot()
	if op.kind == "mutation" {
		root = doc{"__typename": "Mutation"}
		for _, key := range []string{"createIssue", "closeIssue", "reopenIssue", "addComment", "createPullRequest", "mergePullRequest"} {
			root[key] = resolver(func(a doc) any { return f.mutation(key, a) })
		}
	}
	g := gqlContext{vars, fragments}
	return doc{"data": g.project(root, op.fields, map[string]bool{})}
}

func (f *forge) graphQL(raw []byte) response {
	var v any
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &v) != nil {
		reject(400, "Problems parsing JSON")
	}
	if batch, batched := v.([]any); batched {
		results := make([]doc, len(batch))
		for j, document := range batch {
			results[j] = f.graphOne(document)
		}
		return ok(results)
	}
	return ok(f.graphOne(v))
}
