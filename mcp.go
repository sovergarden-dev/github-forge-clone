package main

import (
	"encoding/json"
	"math"
	"slices"
)

var revisions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
var serverInfo = doc{"name": "git-forge", "version": "2.0.0"}
var capabilities = doc{"tools": doc{"listChanged": false}}

type tool struct {
	name, description  string
	required, optional []string
}

var catalog = []tool{
	{"create_repo", "Create a repository for the authenticated user.", []string{"name"}, []string{"description", "private"}},
	{"get_repo", "Read repository metadata by owner and name.", []string{"owner", "repo"}, nil},
	{"create_issue", "Open an issue in a repository.", []string{"owner", "repo", "title"}, []string{"body"}},
	{"list_issues", "List repository issues by state.", []string{"owner", "repo"}, []string{"state"}},
	{"create_pull_request", "Open a pull request between branches.", []string{"owner", "repo", "title", "head", "base"}, []string{"body"}},
	{"merge_pull_request", "Squash and merge an open pull request.", []string{"owner", "repo", "pull_number"}, nil},
}

func propertyType(key string) string {
	switch key {
	case "pull_number":
		return "integer"
	case "private":
		return "boolean"
	default:
		return "string"
	}
}

var toolList = func() []doc {
	out := []doc{}
	for _, t := range catalog {
		props := doc{}
		for _, group := range [][]string{t.required, t.optional} {
			for _, key := range group {
				spec := doc{"type": propertyType(key)}
				if key == "state" {
					spec["enum"] = []string{"open", "closed", "all"}
				}
				props[key] = spec
			}
		}
		out = append(out, doc{"name": t.name, "description": t.description, "inputSchema": doc{"type": "object", "properties": props, "required": t.required}})
	}
	return out
}()

func rpcError(id any, code int, msg string) doc {
	return doc{"jsonrpc": "2.0", "id": id, "error": doc{"code": code, "message": msg}}
}

func (f *forge) callTool(t tool, d doc) (result doc) {
	defer func() {
		if err := recover(); err != nil {
			if e, ok := err.(fault); ok {
				result = doc{"isError": true, "content": []doc{{"type": "text", "text": e.message}}}
				return
			}
			panic(err)
		}
	}()
	var value any
	if t.name == "create_repo" {
		value = f.describe(f.createRepo(d))
	} else {
		r := f.repo(required(d, "owner"), required(d, "repo"))
		switch t.name {
		case "get_repo":
			value = f.describe(r)
		case "create_issue":
			value = f.newIssue(r, d, "Issue").data
		case "list_issues":
			state := str(d["state"])
			if state == "" {
				state = "open"
			}
			rows := []doc{}
			for _, i := range r.list(false, false, state, false) {
				rows = append(rows, i.data)
			}
			value = rows
		case "create_pull_request":
			value = r.view(f.openPull(r, d))
		case "merge_pull_request":
			value = f.merge(r, r.pull(integer(d["pull_number"])), nil)
		}
	}
	structured := value
	if t.name == "list_issues" {
		structured = doc{"issues": value}
	}
	return doc{"content": []doc{{"type": "text", "text": string(marshal(value))}}, "structuredContent": structured, "isError": false}
}

func (f *forge) mcp(raw []byte) response {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return jsonReply(400, rpcError(nil, -32700, "Parse error"))
	}
	d, _ := v.(doc)
	method := str(d["method"])
	if d["jsonrpc"] != "2.0" || method == "" {
		return jsonReply(400, rpcError(nil, -32600, "Invalid Request"))
	}
	id, set := d["id"]
	if !set {
		return response{status: 202}
	}
	args, _ := d["params"].(doc)
	result := func(x any) response { return ok(doc{"jsonrpc": "2.0", "id": id, "result": x}) }
	fail := func(code int, msg string) response { return ok(rpcError(id, code, msg)) }
	switch method {
	case "server/discover":
		return result(doc{"supportedVersions": revisions, "capabilities": capabilities, "serverInfo": serverInfo, "_meta": doc{"io.modelcontextprotocol/serverInfo": serverInfo}})
	case "initialize":
		version := str(args["protocolVersion"])
		if !slices.Contains(revisions, version) {
			version = revisions[0]
		}
		return result(doc{"protocolVersion": version, "serverInfo": serverInfo, "capabilities": capabilities})
	case "ping":
		return result(doc{})
	case "tools/list":
		return result(doc{"tools": toolList})
	case "tools/call":
		name := str(args["name"])
		input, _ := args["arguments"].(doc)
		for _, t := range catalog {
			if t.name != name {
				continue
			}
			for _, k := range t.required {
				if _, exists := input[k]; !exists {
					return fail(-32602, "Missing required argument "+k)
				}
			}
			for k, val := range input {
				if !slices.Contains(t.required, k) && !slices.Contains(t.optional, k) {
					continue
				}
				valid := false
				switch propertyType(k) {
				case "integer":
					n, ok := val.(float64)
					valid = ok && n == math.Trunc(n)
				case "string":
					_, valid = val.(string)
				case "boolean":
					_, valid = val.(bool)
				}
				if !valid {
					return fail(-32602, "Invalid "+k)
				}
			}
			return result(f.callTool(t, input))
		}
		return fail(-32602, "Unknown tool "+name)
	default:
		return fail(-32601, "Method not found")
	}
}
