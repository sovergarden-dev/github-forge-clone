package main

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

func page[T any](rows []T, req *http.Request, w http.ResponseWriter) []T {
	q := req.URL.Query()
	size, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || size < 1 {
		size = 30
	}
	size = min(size, 100)
	number, err := strconv.Atoi(q.Get("page"))
	if err != nil || number < 1 {
		number = 1
	}
	last := (len(rows) + size - 1) / size
	var links []string
	link := func(n int, rel string) {
		u := *req.URL
		u.Scheme, u.Host = "http", req.Host
		q.Set("page", strconv.Itoa(n))
		u.RawQuery = q.Encode()
		links = append(links, "<"+u.String()+">; rel=\""+rel+"\"")
	}
	if number < last {
		link(number+1, "next")
		link(last, "last")
	}
	if number > 1 {
		link(number-1, "prev")
		link(1, "first")
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	if number > last {
		return []T{}
	}
	start := (number - 1) * size
	return rows[start:min(start+size, len(rows))]
}

func queryDefault(q url.Values, key, fallback string) string {
	if v := q.Get(key); v != "" {
		return v
	}
	return fallback
}

func (f *forge) rest(w http.ResponseWriter, req *http.Request, raw []byte) response {
	method := req.Method
	path := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/v3"), "/")
	var d doc
	if len(raw) > 0 {
		d = jsonObject(raw)
	}
	switch path {
	case "":
		if method == "GET" {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			return ok(doc{"current_user_url": "/api/v3/user", "user_repositories_url": "/api/v3/user/repos"})
		}
	case "/user":
		if method == "GET" {
			return ok(f.user)
		}
	case "/meta":
		if method == "GET" {
			return ok(doc{"installed_version": "3.17.0"})
		}
	case "/user/repos":
		if method == "POST" {
			return jsonReply(201, f.describe(f.createRepo(d)))
		}
		if method == "GET" {
			repos := make([]*repository, 0, len(f.repos))
			for _, r := range f.repos {
				repos = append(repos, r)
			}
			sort.Slice(repos, func(i, j int) bool { return repos[i].id < repos[j].id })
			list := []doc{}
			for _, r := range page(repos, req, w) {
				list = append(list, f.describe(r))
			}
			return ok(list)
		}
	}
	remaining, matched := strings.CutPrefix(path, "/repos/")
	if !matched {
		missing()
	}
	owner, tail, hasRepo := strings.Cut(remaining, "/")
	if !hasRepo {
		missing()
	}
	name, sub, _ := strings.Cut(tail, "/")
	r := f.repo(owner, name)
	resource, identifier, _ := strings.Cut(sub, "/")
	q := req.URL.Query()
	switch resource {
	case "":
		switch method {
		case "DELETE":
			delete(f.repos, strings.ToLower(r.name))
			for key, v := range f.byID {
				if v.repo == r {
					delete(f.byID, key)
				}
			}
			return response{status: 204}
		case "PATCH":
			if v, has := d["default_branch"]; has {
				branch := str(v)
				if r.refs[heads+branch] == zero {
					invalid("Unknown default branch")
				}
				r.branch = branch
				r.lastRefs, r.lastFetch = wireCache{}, wireCache{}
				r.ads = [2][]byte{}
			}
			if s, ok := d["description"].(string); ok {
				r.description = s
			}
			if p, ok := d["private"].(bool); ok {
				r.private = p
			}
			return ok(f.describe(r))
		case "GET":
			return ok(f.describe(r))
		}
	case "git":
		part, rest, _ := strings.Cut(identifier, "/")
		if part != "refs" && part != "ref" {
			missing()
		}
		format := func(ref string, id oid) doc {
			return doc{"ref": ref, "object": doc{"type": kinds[r.object(id).kind], "sha": id.String()}}
		}
		if method == "POST" && rest == "" {
			ref := required(d, "ref")
			id, ok := parseID(required(d, "sha"))
			if !ok || !refOK(ref) || r.refs[ref] != zero || r.objects[id] == nil || strings.HasPrefix(ref, heads) && r.object(id).kind != commitKind {
				invalid("Invalid reference")
			}
			r.updateRef(ref, id)
			return jsonReply(201, format(ref, id))
		}
		if method == "GET" && rest != "" {
			ref := "refs/" + rest
			id := r.refs[ref]
			if id == zero {
				missing()
			}
			return ok(format(ref, id))
		}
	case "branches":
		if method == "GET" {
			branch := func(ref string) doc {
				id := r.refs[ref]
				if id == zero {
					missing()
				}
				return doc{"name": strings.TrimPrefix(ref, heads), "commit": doc{"sha": id.String()}, "protected": false}
			}
			if identifier != "" {
				return ok(branch(heads + identifier))
			}
			rows := []doc{}
			for _, ref := range sortedKeys(r.refs) {
				if strings.HasPrefix(ref, heads) {
					rows = append(rows, branch(ref))
				}
			}
			return ok(page(rows, req, w))
		}
	case "commits":
		if method == "GET" {
			ref := identifier
			if ref == "" {
				ref = queryDefault(q, "sha", r.branch)
			}
			id := r.resolve(ref)
			if id == zero {
				missing()
			}
			id = r.peel(id)
			if identifier != "" {
				return ok(r.commitInfo(id))
			}
			var ids []oid
			r.walkHistory(id, func(id oid) bool { ids = append(ids, id); return true })
			rows := []doc{}
			for _, id := range page(ids, req, w) {
				rows = append(rows, r.commitInfo(id))
			}
			return ok(rows)
		}
	case "contents":
		if method == "GET" {
			return ok(r.readContent(identifier, queryDefault(q, "ref", r.branch)))
		}
		if method == "PUT" || method == "DELETE" {
			return f.contentWrite(r, identifier, d, method == "DELETE")
		}
	case "labels":
		if method == "GET" {
			if identifier != "" {
				label := r.labels[strings.ToLower(identifier)]
				if label == nil {
					missing()
				}
				return ok(label)
			}
			rows := []doc{}
			for _, key := range sortedKeys(r.labels) {
				rows = append(rows, r.labels[key])
			}
			return ok(page(rows, req, w))
		}
		if method == "POST" && identifier == "" {
			name := required(d, "name")
			color := str(d["color"])
			if _, set := d["color"]; !set {
				color = "ededed"
			}
			key := strings.ToLower(name)
			if !hexColor.MatchString(color) || r.labels[key] != nil {
				invalid("Invalid or duplicate label")
			}
			id := f.allocate()
			label := doc{"id": id, "node_id": globalID("Label", id), "name": name, "color": color, "description": str(d["description"])}
			r.labels[key] = label
			return jsonReply(201, label)
		}
	case "issues", "pulls":
		pulls := resource == "pulls"
		if identifier == "" {
			if method == "POST" {
				if pulls {
					return jsonReply(201, r.view(f.openPull(r, d)))
				}
				return response{201, jsonType, f.newIssue(r, d, "Issue").wire}
			}
			if method == "GET" {
				items := r.list(pulls, true, queryDefault(q, "state", "open"), q.Get("direction") == "asc")
				rows := []doc{}
				for _, i := range page(items, req, w) {
					rows = append(rows, r.view(i))
				}
				return ok(rows)
			}
			break
		}
		num, action, _ := strings.Cut(identifier, "/")
		n, err := strconv.Atoi(num)
		if err != nil {
			missing()
		}
		var item *issue
		if pulls {
			item = r.pull(n)
		} else {
			item = r.item(n)
		}
		switch {
		case action == "" && method == "GET":
			if item.head == "" {
				return response{200, jsonType, item.wire}
			}
			return ok(r.view(item))
		case !pulls && action == "" && method == "PATCH":
			f.edit(r, item, d)
			return ok(r.view(item))
		case !pulls && action == "comments" && method == "GET":
			return ok(page(item.comments, req, w))
		case !pulls && action == "comments" && method == "POST":
			return jsonReply(201, f.comment(item, required(d, "body")))
		case pulls && action == "files" && method == "GET":
			return ok(page(r.fileDiff(item), req, w))
		case pulls && action == "merge" && method == "PUT":
			return ok(f.merge(r, item, d))
		}
	}
	missing()
	return response{}
}
