package main

import (
	"bytes"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const upload = "git-upload-pack"
const receive = "git-receive-pack"
const heads = "refs/heads/"
const tags = "refs/tags/"
const flush = "0000"
const delim = "0001"
const digits = "0123456789abcdef"

func packetHeader(dst []byte, length int) []byte {
	n := length + 4
	return append(dst, digits[n>>12&15], digits[n>>8&15], digits[n>>4&15], digits[n&15])
}

func line(dst []byte, s string) []byte {
	dst = packetHeader(dst, len(s)+1)
	dst = append(dst, s...)
	return append(dst, '\n')
}

func packetList(lines ...string) []byte {
	var out []byte
	for _, s := range lines {
		out = line(out, s)
	}
	return append(out, flush...)
}

func readPackets(data []byte, stop bool) ([]string, []byte) {
	var lines []string
	for len(data) > 0 {
		if len(data) < 4 {
			badGit()
		}
		n, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil {
			badGit()
		}
		data = data[4:]
		if n == 0 && stop {
			break
		}
		if n < 3 {
			continue
		}
		if n < 4 || int(n)-4 > len(data) {
			badGit()
		}
		size := int(n) - 4
		lines = append(lines, string(bytes.TrimSuffix(data[:size], []byte{'\n'})))
		data = data[size:]
	}
	return lines, data
}

func sideband(out, data []byte) []byte {
	for len(data) > 0 {
		n := min(65515, len(data))
		out = append(packetHeader(out, n+1), 1)
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return append(out, flush...)
}

func refOK(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.Count(ref, "/") < 2 || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.ContainsAny(ref, "~^:?*[\\") {
		return false
	}
	for _, s := range []string{"..", "@{", "//", "/."} {
		if strings.Contains(ref, s) {
			return false
		}
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, ch := range ref {
		if ch <= 32 || ch == 127 {
			return false
		}
	}
	return true
}

func (r *repository) updateRef(ref string, id oid) {
	if id == zero {
		delete(r.refs, ref)
	} else {
		r.refs[ref] = id
	}
	r.lastFetch, r.lastRefs = wireCache{}, wireCache{}
	r.ads = [2][]byte{}
}

func (r *repository) resolve(name string) oid {
	if name == "HEAD" {
		return r.refs[heads+r.branch]
	}
	for _, ref := range []string{name, heads + name, tags + name} {
		if id, ok := r.refs[ref]; ok {
			return id
		}
	}
	if id, ok := parseID(name); ok && r.objects[id] != nil {
		return id
	}
	return zero
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var advertisementV2 = packetList("version 2", "agent=forge/2", "ls-refs=unborn", "fetch=shallow", "object-format=sha1")

func (r *repository) advertisement(service string, v2 bool) []byte {
	if v2 {
		return advertisementV2
	}
	slot := 0
	if service == receive {
		slot = 1
	}
	r.wireMu.Lock()
	defer r.wireMu.Unlock()
	if r.ads[slot] != nil {
		return r.ads[slot]
	}
	head := heads + r.branch
	caps := "report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=forge/2"
	var refs []string
	if service == upload {
		caps = "side-band-64k ofs-delta shallow symref=HEAD:" + head + " agent=forge/2"
		if id, ok := r.refs[head]; ok {
			refs = append(refs, id.String()+" HEAD")
		}
	}
	for _, ref := range sortedKeys(r.refs) {
		id := r.refs[ref]
		refs = append(refs, id.String()+" "+ref)
		if service == upload && r.object(id).kind == tagKind {
			refs = append(refs, r.peel(id).String()+" "+ref+"^{}")
		}
	}
	if len(refs) == 0 {
		refs = append(refs, zero.String()+" capabilities^{}")
	}
	refs[0] += "\x00" + caps
	r.ads[slot] = append(packetList("# service="+service), packetList(refs...)...)
	return r.ads[slot]
}

func (r *repository) listRefs(args []string) []byte {
	peel, sym, unborn := false, false, false
	var prefixes []string
	for _, arg := range args {
		switch arg {
		case "peel":
			peel = true
		case "symrefs":
			sym = true
		case "unborn":
			unborn = true
		default:
			if p, ok := strings.CutPrefix(arg, "ref-prefix "); ok {
				prefixes = append(prefixes, p)
			}
		}
	}
	include := func(ref string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(ref, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	head := heads + r.branch
	tip := r.refs[head]
	if include("HEAD") && (tip != zero || unborn) {
		id := "unborn"
		if tip != zero {
			id = tip.String()
		}
		s := id + " HEAD"
		if sym {
			s += " symref-target:" + head
		}
		out = line(out, s)
	}
	for _, ref := range sortedKeys(r.refs) {
		if !include(ref) {
			continue
		}
		id := r.refs[ref]
		s := id.String() + " " + ref
		if peel {
			if p := r.peel(id); p != id {
				s += " peeled:" + p.String()
			}
		}
		out = line(out, s)
	}
	return append(out, flush...)
}

type reach struct {
	order          []oid
	seen, boundary map[oid]bool
}

func (r *repository) closure(tips []oid, depth int, stop map[oid]bool) reach {
	v := reach{seen: make(map[oid]bool), boundary: make(map[oid]bool)}
	type hop struct {
		id    oid
		level int
	}
	var commits []hop
	var visit func(oid, int)
	visit = func(id oid, level int) {
		if v.seen[id] {
			return
		}
		obj := r.object(id)
		if obj.kind == commitKind {
			commits = append(commits, hop{id, level})
			return
		}
		v.seen[id] = true
		v.order = append(v.order, id)
		for _, link := range obj.links {
			visit(link, level)
		}
	}
	for _, id := range tips {
		visit(id, 1)
	}
	for i := 0; i < len(commits); i++ {
		x := commits[i]
		if v.seen[x.id] {
			continue
		}
		v.seen[x.id] = true
		v.order = append(v.order, x.id)
		obj := r.object(x.id)
		visit(obj.links[0], x.level)
		if x.level >= depth || stop[x.id] {
			if len(obj.links) > 1 {
				v.boundary[x.id] = true
			}
		} else {
			for _, id := range obj.links[1:] {
				commits = append(commits, hop{id, x.level + 1})
			}
		}
	}
	return v
}

type wireCache struct{ key, value []byte }

func (r *repository) uploadPack(data []byte, v2 bool) []byte {
	cached := func() []byte {
		if !v2 {
			return nil
		}
		if bytes.Equal(data, r.lastRefs.key) && r.lastRefs.value != nil {
			return r.lastRefs.value
		}
		if bytes.Equal(data, r.lastFetch.key) {
			return r.lastFetch.value
		}
		return nil
	}
	r.wireMu.RLock()
	response := cached()
	r.wireMu.RUnlock()
	if response != nil {
		return response
	}
	r.wireMu.Lock()
	defer r.wireMu.Unlock()
	if response = cached(); response != nil {
		return response
	}
	args, _ := readPackets(data, false)
	var wants, haves, shallows []oid
	shallow := map[oid]bool{}
	depth := fullDepth
	command := ""
	done, relative, band, offset := false, false, v2, false
	for _, arg := range args {
		key, val, _ := strings.Cut(arg, " ")
		switch key {
		case "command=ls-refs", "command=fetch":
			command = key
		case "want", "have", "shallow":
			text, capabilities, _ := strings.Cut(val, " ")
			id, ok := parseID(text)
			if !ok {
				badGit()
			}
			switch key {
			case "want":
				wants = append(wants, id)
				for _, c := range strings.Fields(capabilities) {
					if c == "side-band-64k" {
						band = true
					}
					if c == "ofs-delta" {
						offset = true
					}
				}
			case "have":
				if r.objects[id] != nil {
					haves = append(haves, id)
				}
			case "shallow":
				shallows = append(shallows, id)
				shallow[id] = true
			}
		case "done":
			done = true
		case "deepen-relative":
			relative = true
		case "ofs-delta":
			offset = true
		case "deepen":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				badGit()
			}
			depth = n
		}
	}
	if v2 && command == "command=ls-refs" {
		out := r.listRefs(args)
		if len(data) <= 8192 && len(out) <= 65536 {
			r.lastRefs = wireCache{data, out}
		}
		return out
	}
	if v2 && command != "command=fetch" {
		badGit()
	}
	if len(wants) == 0 {
		return []byte(flush)
	}
	if v2 && !done {
		out := line(nil, "acknowledgments")
		for _, id := range haves {
			out = line(out, "ACK "+id.String())
		}
		if len(haves) == 0 {
			out = line(out, "NAK")
		}
		return append(out, flush...)
	}
	relative = relative && depth != fullDepth && len(shallow) > 0
	limit := depth
	var stops map[oid]bool
	if relative || depth == fullDepth {
		stops = shallow
	}
	if relative {
		limit = fullDepth
	}
	want := r.closure(wants, limit, stops)
	var changes []string
	for _, id := range shallows {
		if !want.seen[id] {
			continue
		}
		if relative && want.boundary[id] {
			delete(want.boundary, id)
			ext := r.closure(r.object(id).links[1:], depth, nil)
			for _, child := range ext.order {
				if !want.seen[child] {
					want.seen[child] = true
					want.order = append(want.order, child)
				}
			}
			for boundary := range ext.boundary {
				want.boundary[boundary] = true
			}
			changes = append(changes, "unshallow "+id.String())
		} else if stops == nil && !want.boundary[id] {
			changes = append(changes, "unshallow "+id.String())
		}
	}
	for id := range want.boundary {
		if !shallow[id] {
			changes = append(changes, "shallow "+id.String())
		}
	}
	sort.Strings(changes)
	var out []byte
	if len(changes) > 0 || !v2 && depth != fullDepth {
		if v2 {
			out = line(out, "shallow-info")
		}
		for _, s := range changes {
			out = line(out, s)
		}
		if v2 {
			out = append(out, delim...)
		} else {
			out = append(out, flush...)
		}
	}
	if !v2 && !done && len(haves) == 0 && depth != fullDepth {
		return out
	}
	if v2 {
		out = line(out, "packfile")
	} else if len(haves) > 0 {
		out = line(out, "ACK "+haves[0].String())
	} else {
		out = line(out, "NAK")
	}
	ids := want.order
	if len(haves) > 0 {
		have := r.closure(haves, fullDepth, shallow)
		ids = ids[:0]
		for _, id := range want.order {
			if !have.seen[id] {
				ids = append(ids, id)
			}
		}
	}
	pack := r.pack(ids, offset)
	if band {
		out = sideband(out, pack)
	} else {
		out = append(out, pack...)
	}
	if v2 && len(data) <= 8192 && len(out) <= 1<<20 {
		r.lastFetch = wireCache{data, out}
	}
	return out
}

func (r *repository) acceptPack(data []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	r.unpack(data)
	return true
}

func (r *repository) receivePack(data []byte) []byte {
	lines, pack := readPackets(data, true)
	if len(lines) == 0 {
		return []byte(flush)
	}
	first, caps, _ := strings.Cut(lines[0], "\x00")
	lines[0] = first
	type command struct {
		old, next oid
		ref       string
	}
	cmds := make([]command, 0, len(lines))
	for _, s := range lines {
		if len(s) < 83 || s[40] != ' ' || s[81] != ' ' {
			badGit()
		}
		old, ok1 := parseID(s[:40])
		next, ok2 := parseID(s[41:81])
		ref := s[82:]
		if !ok1 || !ok2 || !refOK(ref) {
			badGit()
		}
		cmds = append(cmds, command{old, next, ref})
	}
	ok := len(pack) == 0 || r.acceptPack(pack)
	var report []byte
	if ok {
		report = line(nil, "unpack ok")
	} else {
		report = line(nil, "unpack invalid pack")
	}
	for _, c := range cmds {
		reason := ""
		switch {
		case !ok:
			reason = "unpacker error"
		case r.refs[c.ref] != c.old:
			reason = "stale info"
		case c.next != zero && r.objects[c.next] == nil:
			reason = "missing object"
		case c.next != zero && strings.HasPrefix(c.ref, heads) && r.object(c.next).kind != commitKind:
			reason = "not a commit"
		case c.old != zero && c.next != zero && c.old != c.next && (!strings.HasPrefix(c.ref, heads) || !r.ancestor(c.old, c.next)):
			reason = "non-fast-forward"
		}
		if reason != "" {
			report = line(report, "ng "+c.ref+" "+reason)
		} else {
			r.updateRef(c.ref, c.next)
			report = line(report, "ok "+c.ref)
		}
	}
	if !strings.Contains(" "+caps+" ", " report-status ") {
		return nil
	}
	report = append(report, flush...)
	if strings.Contains(" "+caps+" ", " side-band-64k ") {
		return sideband(nil, report)
	}
	return report
}

func (f *forge) smart(req *http.Request, data []byte) response {
	base, tail, ok := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), ".git/")
	owner, name, split := strings.Cut(base, "/")
	if !ok || !split {
		missing()
	}
	r := f.repo(owner, name)
	service := tail
	discovery := tail == "info/refs"
	if discovery {
		if req.Method != "GET" {
			missing()
		}
		service = req.URL.Query().Get("service")
	} else if req.Method != "POST" {
		missing()
	}
	if service != upload && service != receive {
		missing()
	}
	v2 := service == upload && strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	if discovery {
		return response{200, "application/x-" + service + "-advertisement", r.advertisement(service, v2)}
	}
	if service == receive {
		return response{200, "application/x-git-receive-pack-result", r.receivePack(data)}
	}
	return response{200, "application/x-git-upload-pack-result", r.uploadPack(data, v2)}
}
