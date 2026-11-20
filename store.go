package main

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type issue struct {
	data           doc
	number         int
	comments       []doc
	head, base     string
	headID, baseID oid
	wire           []byte
	wireLength     string
}

type repository struct {
	id                             int
	gid, name, description, branch string
	private                        bool
	nextNumber                     int
	objects                        map[oid]*object
	refs                           map[string]oid
	labels                         map[string]doc
	issues, pulls                  map[int]*issue
	wireMu                         sync.RWMutex
	lastRefs, lastFetch            wireCache
	ads                            [2][]byte
}

func globalID(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(kind + ":" + strconv.Itoa(id)))
}

func (f *forge) allocate() int {
	id := f.next
	f.next++
	return id
}

func (f *forge) repo(owner, name string) *repository {
	if !strings.EqualFold(owner, f.owner) {
		missing()
	}
	r := f.repos[strings.ToLower(name)]
	if r == nil {
		missing()
	}
	return r
}

var repoName = regexp.MustCompile("^[A-Za-z0-9_.-]{1,100}$")
var hexColor = regexp.MustCompile("^[a-fA-F0-9]{6}$")
var cleanIdentity = strings.NewReplacer("\r", "", "\n", "", "<", "", ">", "")

func (f *forge) createRepo(d doc) *repository {
	name := required(d, "name")
	key := strings.ToLower(name)
	if !repoName.MatchString(name) || name == "." || name == ".." {
		invalid("Invalid repository name")
	}
	if f.repos[key] != nil {
		invalid("Repository already exists")
	}
	id := f.allocate()
	private, _ := d["private"].(bool)
	r := &repository{
		id: id, gid: globalID("Repository", id), name: name, branch: "main", private: private,
		description: str(d["description"]), nextNumber: 1,
		objects: make(map[oid]*object), refs: make(map[string]oid), labels: make(map[string]doc),
		issues: make(map[int]*issue), pulls: make(map[int]*issue),
	}
	f.repos[key] = r
	f.byID[r.gid] = nodeRef{repo: r}
	if d["auto_init"] == true {
		contents := base64.StdEncoding.EncodeToString([]byte("# " + name + "\n"))
		f.contentWrite(r, "README.md", doc{"message": "Initial commit", "content": contents}, false)
	}
	return r
}

func (f *forge) describe(r *repository) doc {
	return doc{"id": r.id, "node_id": r.gid, "name": r.name, "full_name": f.owner + "/" + r.name,
		"owner": f.user, "private": r.private, "description": r.description, "default_branch": r.branch, "has_issues": true}
}

func (r *repository) commitInfo(id oid) doc {
	obj := r.object(id)
	if obj.kind != commitKind {
		missing()
	}
	_, msg, _ := bytes.Cut(obj.data, []byte("\n\n"))
	message := strings.TrimSuffix(string(msg), "\n")
	tree := doc{"sha": obj.links[0].String()}
	parents := make([]doc, 0, len(obj.links)-1)
	for _, p := range obj.links[1:] {
		parents = append(parents, doc{"sha": p.String()})
	}
	return doc{"sha": id.String(), "commit": doc{"message": message, "tree": tree}, "message": message, "tree": tree, "parents": parents}
}

func (f *forge) commit(r *repository, branch string, files map[string]treeEntry, message string, parents []oid, d doc) oid {
	identity := func(v any) string {
		user, _ := v.(doc)
		name, email := str(user["name"]), str(user["email"])
		if name == "" {
			name = f.owner
		}
		if email == "" {
			email = f.owner + "@users.noreply.github.com"
		}
		return cleanIdentity.Replace(name) + " <" + cleanIdentity.Replace(email) + "> " + f.stamp
	}
	var b strings.Builder
	b.WriteString("tree " + r.makeTree(files).String() + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p.String() + "\n")
	}
	committer := d["committer"]
	if committer == nil {
		committer = d["author"]
	}
	b.WriteString("author " + identity(d["author"]) + "\ncommitter " + identity(committer) + "\n\n")
	b.WriteString(strings.TrimRight(message, "\n") + "\n")
	id := r.put(commitKind, []byte(b.String()))
	r.updateRef(heads+branch, id)
	return id
}

func pathOK(path string) bool {
	for _, s := range strings.Split(path, "/") {
		if s == "" || s == "." || s == ".." || strings.EqualFold(s, ".git") || strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}

func pathsFit(files map[string]treeEntry) bool {
	for path := range files {
		for i := strings.IndexByte(path, '/'); i >= 0; {
			if _, exists := files[path[:i]]; exists {
				return false
			}
			next := strings.IndexByte(path[i+1:], '/')
			if next < 0 {
				break
			}
			i += next + 1
		}
	}
	return true
}

func (r *repository) fileInfo(e treeEntry, path string, withData bool) doc {
	kind := "file"
	var data []byte
	switch e.mode {
	case "40000":
		kind = "dir"
	case "160000":
		kind = "submodule"
	default:
		if e.mode == "120000" {
			kind = "symlink"
		}
		data = r.object(e.id).data
	}
	d := doc{"type": kind, "name": e.name, "path": path, "sha": e.id.String(), "size": len(data)}
	if withData && (kind == "file" || kind == "symlink") {
		d["encoding"] = "base64"
		d["content"] = base64.StdEncoding.EncodeToString(data)
	}
	return d
}

func (r *repository) readContent(path, ref string) any {
	id := r.resolve(ref)
	if id == zero {
		missing()
	}
	e := treeEntry{mode: "40000", id: r.root(id)}
	if path != "" {
		for _, part := range strings.Split(path, "/") {
			if e.mode != "40000" {
				missing()
			}
			matched := false
			for _, child := range r.object(e.id).entries {
				if child.name == part {
					e = child
					matched = true
					break
				}
			}
			if !matched {
				missing()
			}
		}
	}
	if e.mode != "40000" {
		return r.fileInfo(e, path, true)
	}
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	rows := []doc{}
	for _, child := range r.object(e.id).entries {
		rows = append(rows, r.fileInfo(child, prefix+child.name, false))
	}
	return rows
}

func (f *forge) contentWrite(r *repository, path string, d doc, remove bool) response {
	if !pathOK(path) {
		invalid("Invalid path")
	}
	branch := r.branch
	if _, set := d["branch"]; set {
		branch = required(d, "branch")
	}
	parent := r.refs[heads+branch]
	if parent == zero && (len(r.refs) > 0 || branch != r.branch) {
		missing()
	}
	files := r.flatten(parent)
	prev, exists := files[path]
	message := required(d, "message")
	if remove && !exists {
		missing()
	}
	if exists && str(d["sha"]) == "" {
		invalid("A sha is required to replace this file")
	}
	if sha, has := d["sha"]; has && (!exists || str(sha) != prev.id.String()) {
		reject(409, "File SHA does not match")
	}
	var content any
	if remove {
		delete(files, path)
	} else {
		s, ok := d["content"].(string)
		if !ok {
			invalid("Invalid base64 content")
		}
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			invalid("Invalid base64 content")
		}
		mode := prev.mode
		if !exists {
			mode = "100644"
		}
		e := treeEntry{name: path[strings.LastIndexByte(path, '/')+1:], mode: mode, id: r.put(blobKind, data)}
		files[path] = e
		if !pathsFit(files) {
			invalid("File and directory names overlap")
		}
		content = r.fileInfo(e, path, true)
	}
	var parents []oid
	if parent != zero {
		parents = []oid{parent}
	}
	sha := f.commit(r, branch, files, message, parents, d)
	status := 201
	if exists {
		status = 200
	}
	return jsonReply(status, doc{"content": content, "commit": r.commitInfo(sha)})
}

func (r *repository) labelsFor(input any) []doc {
	labels := []doc{}
	if input == nil {
		return labels
	}
	values, ok := input.([]any)
	if !ok {
		invalid("Invalid labels")
	}
	seen := map[string]bool{}
	for _, v := range values {
		if d, ok := v.(doc); ok {
			v = d["name"]
		}
		key := strings.ToLower(str(v))
		l := r.labels[key]
		if l == nil {
			invalid("Unknown label")
		}
		if !seen[key] {
			labels = append(labels, l)
			seen[key] = true
		}
	}
	return labels
}

func (f *forge) newIssue(r *repository, d doc, kind string) *issue {
	title := required(d, "title")
	return f.addIssue(r, title, str(d["body"]), r.labelsFor(d["labels"]), kind)
}

func (i *issue) encode() {
	i.wire = marshal(i.data)
	i.wireLength = strconv.Itoa(len(i.wire))
}

func (f *forge) addIssue(r *repository, title, body string, labels []doc, kind string) *issue {
	id := f.allocate()
	gid := globalID(kind, id)
	i := &issue{number: r.nextNumber}
	r.nextNumber++
	i.data = doc{"id": id, "node_id": gid, "number": i.number, "title": title, "body": body, "state": "open", "user": f.user, "labels": labels, "comments": 0, "created_at": f.iso, "updated_at": f.iso, "closed_at": nil}
	f.byID[gid] = nodeRef{r, i}
	if kind == "Issue" {
		r.issues[i.number] = i
	}
	i.encode()
	return i
}

func (r *repository) item(n int) *issue {
	if i := r.issues[n]; i != nil {
		return i
	}
	if p := r.pulls[n]; p != nil {
		return r.pull(n)
	}
	missing()
	return nil
}

func (f *forge) edit(r *repository, i *issue, d doc) {
	title := str(i.data["title"])
	if _, has := d["title"]; has {
		title = required(d, "title")
	}
	labels := i.data["labels"]
	if v, has := d["labels"]; has {
		labels = r.labelsFor(v)
	}
	if v, has := d["state"]; has {
		state := str(v)
		if state != "open" && state != "closed" {
			invalid("Invalid state")
		}
		i.data["state"] = state
		i.data["closed_at"] = nil
		if state == "closed" {
			i.data["closed_at"] = f.iso
		}
	}
	if body, ok := d["body"].(string); ok {
		i.data["body"] = body
	}
	i.data["title"], i.data["labels"], i.data["updated_at"] = title, labels, f.iso
	i.encode()
}

func (f *forge) comment(i *issue, body string) doc {
	if body == "" {
		invalid("Missing or invalid body")
	}
	id := f.allocate()
	c := doc{"id": id, "node_id": globalID("IssueComment", id), "body": body, "user": f.user, "created_at": f.iso, "updated_at": f.iso}
	i.comments = append(i.comments, c)
	i.data["comments"] = len(i.comments)
	i.encode()
	return c
}

func (f *forge) openPull(r *repository, d doc) *issue {
	branch := func(key string) (string, oid) {
		ref := required(d, key)
		if owner, name, ok := strings.Cut(ref, ":"); ok {
			if !strings.EqualFold(owner, f.owner) {
				invalid("Unknown owner")
			}
			ref = name
		}
		id := r.refs[heads+ref]
		if id == zero {
			invalid("Unknown branch")
		}
		return ref, id
	}
	head, h := branch("head")
	base, b := branch("base")
	if r.ancestor(h, b) {
		invalid("No commits between branches")
	}
	for _, p := range r.pulls {
		if p.data["state"] == "open" && p.head == head && p.base == base {
			invalid("A pull request already exists")
		}
	}
	p := f.newIssue(r, d, "PullRequest")
	p.head, p.base, p.headID, p.baseID = head, base, h, b
	p.data["merged"], p.data["merged_at"], p.data["merge_commit_sha"] = false, nil, nil
	r.pulls[p.number] = p
	return p
}

func (r *repository) pull(n int) *issue {
	p := r.pulls[n]
	if p == nil {
		missing()
	}
	return p
}

func (r *repository) tips(p *issue) (oid, oid) {
	h, b := p.headID, p.baseID
	if p.data["merged"] != true {
		if id := r.refs[heads+p.head]; id != zero {
			h = id
		}
		if id := r.refs[heads+p.base]; id != zero {
			b = id
		}
	}
	return h, b
}

func (r *repository) view(i *issue) doc {
	if i.head == "" {
		return i.data
	}
	h, b := r.tips(i)
	d := make(doc, len(i.data)+2)
	for key, value := range i.data {
		d[key] = value
	}
	d["head"] = doc{"ref": i.head, "sha": h.String()}
	d["base"] = doc{"ref": i.base, "sha": b.String()}
	return d
}

type change struct {
	path          string
	before, after treeEntry
}

func (r *repository) changes(p *issue) []change {
	h, b := r.tips(p)
	seen := map[oid]bool{}
	r.walkHistory(b, func(id oid) bool { seen[id] = true; return true })
	var common oid
	r.walkHistory(h, func(id oid) bool {
		if seen[id] {
			common = id
			return false
		}
		return true
	})
	base, head := r.flatten(common), r.flatten(h)
	var diff []change
	for path, entry := range base {
		if next := head[path]; entry != next {
			diff = append(diff, change{path, entry, next})
		}
	}
	for path, entry := range head {
		if _, ok := base[path]; !ok {
			diff = append(diff, change{path, treeEntry{}, entry})
		}
	}
	sort.Slice(diff, func(i, j int) bool { return diff[i].path < diff[j].path })
	return diff
}

func (f *forge) merge(r *repository, p *issue, d doc) doc {
	if p.data["state"] != "open" {
		reject(405, "Pull request is closed")
	}
	h, b := r.tips(p)
	if sha := str(d["sha"]); sha != "" && sha != h.String() {
		reject(409, "Head branch was modified")
	}
	files := r.mergeTree(p)
	if files == nil {
		reject(405, "Pull request is not mergeable")
	}
	parents := []oid{b}
	if d["merge_method"] == "merge" {
		parents = append(parents, h)
	}
	title, body := str(p.data["title"]), str(p.data["body"])
	if t, ok := d["commit_title"].(string); ok {
		title = t
	}
	if t, ok := d["commit_message"].(string); ok {
		body = t
	}
	sha := f.commit(r, p.base, files, title+"\n\n"+body, parents, nil)
	p.headID, p.baseID = h, b
	p.data["merged"], p.data["state"], p.data["merged_at"], p.data["closed_at"], p.data["merge_commit_sha"] = true, "closed", f.iso, f.iso, sha.String()
	return doc{"sha": sha.String(), "merged": true, "message": "Pull Request successfully merged"}
}

func (r *repository) list(pulls, withPulls bool, state string, asc bool) []*issue {
	list := make([]*issue, 0)
	add := func(m map[int]*issue) {
		for _, i := range m {
			if state == "all" || i.data["state"] == state {
				list = append(list, i)
			}
		}
	}
	if !pulls {
		add(r.issues)
	}
	if pulls || withPulls {
		add(r.pulls)
	}
	sort.Slice(list, func(i, j int) bool {
		if asc {
			return list[i].number < list[j].number
		}
		return list[i].number > list[j].number
	})
	return list
}
