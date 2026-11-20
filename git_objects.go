package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type oid [sha1.Size]byte

var zero oid
var kinds = [...]string{"", "commit", "tree", "blob", "tag"}

const (
	commitKind = 1
	treeKind   = 2
	blobKind   = 3
	tagKind    = 4
	maxObject  = 64 << 20
)

func (id oid) String() string { return hex.EncodeToString(id[:]) }

func parseID(s string) (oid, bool) {
	var id oid
	if len(s) != 40 {
		return id, false
	}
	_, err := hex.Decode(id[:], []byte(s))
	return id, err == nil
}

func hashObject(kind byte, raw []byte) oid {
	h := sha1.New()
	var prefix [40]byte
	header := append(prefix[:0], kinds[kind]...)
	header = strconv.AppendInt(append(header, ' '), int64(len(raw)), 10)
	h.Write(append(header, 0))
	h.Write(raw)
	var id oid
	h.Sum(id[:0])
	return id
}

type treeEntry struct {
	name, mode string
	id         oid
}

type object struct {
	kind             byte
	data, compressed []byte
	entries          []treeEntry
	links            []oid
}

func newObject(kind byte, data, compressed []byte) *object {
	o := &object{kind: kind, data: data, compressed: compressed}
	switch kind {
	case treeKind:
		for remaining := data; len(remaining) > 0; {
			space := bytes.IndexByte(remaining, ' ')
			nul := bytes.IndexByte(remaining, 0)
			if space < 1 || nul <= space+1 || len(remaining) < nul+21 {
				badGit()
			}
			e := treeEntry{mode: string(remaining[:space]), name: string(remaining[space+1 : nul]), id: oid(remaining[nul+1 : nul+21])}
			if strings.Contains(e.name, "/") || e.name == "." || e.name == ".." {
				badGit()
			}
			o.entries = append(o.entries, e)
			if e.mode != "160000" {
				o.links = append(o.links, e.id)
			}
			remaining = remaining[nul+21:]
		}
	case commitKind, tagKind:
		header, _, ok := bytes.Cut(data, []byte("\n\n"))
		if !ok {
			badGit()
		}
		for len(header) > 0 {
			var line []byte
			line, header, _ = bytes.Cut(header, []byte{'\n'})
			key, value, _ := bytes.Cut(line, []byte{' '})
			if (kind == tagKind && string(key) == "object") || (kind == commitKind && (string(key) == "tree" || string(key) == "parent")) {
				id, ok := parseID(string(value))
				if !ok {
					badGit()
				}
				o.links = append(o.links, id)
			}
		}
		if len(o.links) == 0 {
			badGit()
		}
	case blobKind:
	default:
		badGit()
	}
	return o
}

func (r *repository) object(id oid) *object {
	obj := r.objects[id]
	if obj == nil {
		badGit()
	}
	return obj
}

func (r *repository) put(kind byte, raw []byte) oid {
	id := hashObject(kind, raw)
	if r.objects[id] == nil {
		r.objects[id] = newObject(kind, raw, nil)
	}
	return id
}

func (r *repository) peel(id oid) oid {
	for {
		obj := r.object(id)
		if obj.kind != tagKind {
			return id
		}
		id = obj.links[0]
	}
}

func (r *repository) root(id oid) oid {
	id = r.peel(id)
	obj := r.object(id)
	if obj.kind == treeKind {
		return id
	}
	if obj.kind != commitKind {
		badGit()
	}
	return obj.links[0]
}

func (r *repository) walkHistory(id oid, visit func(oid) bool) {
	seen := map[oid]bool{}
	queue := []oid{id}
	for i := 0; i < len(queue); i++ {
		cur := queue[i]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.object(cur)
		if obj.kind != commitKind {
			continue
		}
		if !visit(cur) {
			return
		}
		queue = append(queue, obj.links[1:]...)
	}
}

func (r *repository) ancestor(old, next oid) bool {
	if old == next {
		return true
	}
	obj := r.object(next)
	if obj.kind != commitKind {
		return false
	}
	for _, parent := range obj.links[1:] {
		if parent == old {
			return true
		}
	}
	found := false
	r.walkHistory(next, func(id oid) bool {
		found = id == old
		return !found
	})
	return found
}

func (r *repository) flatten(commit oid) map[string]treeEntry {
	files := make(map[string]treeEntry)
	if commit == zero {
		return files
	}
	var walk func(oid, string)
	walk = func(id oid, prefix string) {
		for _, e := range r.object(id).entries {
			path := prefix + e.name
			if e.mode == "40000" {
				walk(e.id, path+"/")
			} else {
				files[path] = e
			}
		}
	}
	walk(r.root(commit), "")
	return files
}

type directory struct {
	files []treeEntry
	dirs  map[string]*directory
}

func (r *repository) makeTree(files map[string]treeEntry) oid {
	root := new(directory)
	for path, entry := range files {
		d := root
		for {
			name, tail, more := strings.Cut(path, "/")
			if !more {
				entry.name = name
				d.files = append(d.files, entry)
				break
			}
			if d.dirs == nil {
				d.dirs = make(map[string]*directory)
			}
			if d.dirs[name] == nil {
				d.dirs[name] = new(directory)
			}
			d, path = d.dirs[name], tail
		}
	}
	var write func(*directory) oid
	write = func(d *directory) oid {
		for name, sub := range d.dirs {
			d.files = append(d.files, treeEntry{name: name, mode: "40000", id: write(sub)})
		}
		key := func(e treeEntry) string {
			if e.mode == "40000" {
				return e.name + "/"
			}
			return e.name
		}
		sort.Slice(d.files, func(i, j int) bool { return key(d.files[i]) < key(d.files[j]) })
		var data []byte
		for _, e := range d.files {
			data = append(data, e.mode...)
			data = append(data, ' ')
			data = append(data, e.name...)
			data = append(data, 0)
			data = append(data, e.id[:]...)
		}
		return r.put(treeKind, data)
	}
	return write(root)
}

func deltaApply(base, delta []byte) []byte {
	readSize := func() int {
		n, used := binary.Uvarint(delta)
		if used <= 0 || n > maxObject {
			badGit()
		}
		delta = delta[used:]
		return int(n)
	}
	if readSize() != len(base) {
		badGit()
	}
	total := readSize()
	result := make([]byte, 0, total)
	pop := func() byte {
		if len(delta) == 0 {
			badGit()
		}
		b := delta[0]
		delta = delta[1:]
		return b
	}
	for len(delta) > 0 {
		op := pop()
		if op == 0 {
			badGit()
		}
		if op < 128 {
			n := int(op)
			if n > len(delta) || n > total-len(result) {
				badGit()
			}
			result = append(result, delta[:n]...)
			delta = delta[n:]
			continue
		}
		offset, size := 0, 0
		for i := 0; i < 4; i++ {
			if op&(1<<i) != 0 {
				offset |= int(pop()) << (8 * i)
			}
		}
		for i := 0; i < 3; i++ {
			if op&(16<<i) != 0 {
				size |= int(pop()) << (8 * i)
			}
		}
		if size == 0 {
			size = 65536
		}
		if offset+size > len(base) || size > total-len(result) {
			badGit()
		}
		result = append(result, base[offset:offset+size]...)
	}
	if len(result) != total {
		badGit()
	}
	return result
}

var readers sync.Pool

func inflated(r *bytes.Reader, size int) []byte {
	var z io.ReadCloser
	var err error
	if pooled := readers.Get(); pooled != nil {
		z = pooled.(io.ReadCloser)
		err = z.(zlib.Resetter).Reset(r, nil)
	} else {
		z, err = zlib.NewReader(r)
	}
	if err != nil {
		badGit()
	}
	data := make([]byte, size)
	if _, err = io.ReadFull(z, data); err != nil {
		badGit()
	}
	var extra [1]byte
	n, err := z.Read(extra[:])
	if n != 0 || err != io.EOF || z.Close() != nil {
		badGit()
	}
	readers.Put(z)
	return data
}

type pendingDelta struct {
	pos, basePos int
	baseID       oid
	data         []byte
}

func (repo *repository) unpack(pack []byte) {
	end := len(pack) - 20
	if end < 12 || string(pack[:4]) != "PACK" {
		badGit()
	}
	version := binary.BigEndian.Uint32(pack[4:])
	if version != 2 && version != 3 || sha1.Sum(pack[:end]) != oid(pack[end:]) {
		badGit()
	}
	count := int(binary.BigEndian.Uint32(pack[8:]))
	if count > (end-12)/2 {
		badGit()
	}
	rd := bytes.NewReader(pack[12:end])
	octet := func() byte {
		b, err := rd.ReadByte()
		if err != nil {
			badGit()
		}
		return b
	}
	staged := make(map[oid]*object, count)
	type located struct {
		pos int
		obj *object
	}
	var local [8]located
	locations := local[:0]
	var offsets map[int]*object
	var pending []pendingDelta
	accept := func(pos int, kind byte, data, compressed []byte) {
		id := hashObject(kind, data)
		obj := repo.objects[id]
		if obj == nil {
			obj = newObject(kind, data, compressed)
		}
		staged[id] = obj
		if offsets == nil {
			locations = append(locations, located{pos, obj})
		} else {
			offsets[pos] = obj
		}
	}
	resolve := func(d pendingDelta) bool {
		var base *object
		if d.basePos != 0 {
			if offsets == nil {
				offsets = make(map[int]*object, len(locations))
				for _, entry := range locations {
					offsets[entry.pos] = entry.obj
				}
				locations = nil
			}
			base = offsets[d.basePos]
		} else {
			base = staged[d.baseID]
			if base == nil {
				base = repo.objects[d.baseID]
			}
		}
		if base == nil {
			return false
		}
		accept(d.pos, base.kind, deltaApply(base.data, d.data), nil)
		return true
	}
	for i := 0; i < count; i++ {
		pos := end - rd.Len()
		b := octet()
		kind, size := (b>>4)&7, int(b&15)
		for shift := 4; b&128 != 0; shift += 7 {
			if shift > 32 {
				badGit()
			}
			b = octet()
			size |= int(b&127) << shift
		}
		if size > maxObject {
			badGit()
		}
		d := pendingDelta{pos: pos}
		switch kind {
		case 6:
			b = octet()
			distance := int(b & 127)
			for b&128 != 0 {
				b = octet()
				distance = ((distance + 1) << 7) | int(b&127)
				if distance >= pos {
					badGit()
				}
			}
			if distance <= 0 || distance >= pos {
				badGit()
			}
			d.basePos = pos - distance
		case 7:
			if _, err := io.ReadFull(rd, d.baseID[:]); err != nil {
				badGit()
			}
		case 1, 2, 3, 4:
		default:
			badGit()
		}
		start := end - rd.Len()
		raw := inflated(rd, size)
		if kind <= 4 {
			accept(pos, kind, raw, pack[start:end-rd.Len()])
		} else {
			d.data = raw
			if !resolve(d) {
				pending = append(pending, d)
			}
		}
	}
	if rd.Len() != 0 {
		badGit()
	}
	for len(pending) > 0 {
		rest := pending[:0]
		for _, d := range pending {
			if !resolve(d) {
				rest = append(rest, d)
			}
		}
		if len(rest) == len(pending) {
			badGit()
		}
		pending = rest
	}
	for _, obj := range staged {
		for _, child := range obj.links {
			if repo.objects[child] == nil && staged[child] == nil {
				badGit()
			}
		}
	}
	for id, obj := range staged {
		repo.objects[id] = obj
	}
}

func packHeader(dst []byte, kind byte, size int) []byte {
	b := kind<<4 | byte(size&15)
	for size >>= 4; size > 0; size >>= 7 {
		dst = append(dst, b|128)
		b = byte(size & 127)
	}
	return append(dst, b)
}

var writers sync.Pool

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	var w *zlib.Writer
	if pooled := writers.Get(); pooled != nil {
		w = pooled.(*zlib.Writer)
		w.Reset(&buf)
	} else {
		w, _ = zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	}
	_, _ = w.Write(data)
	_ = w.Close()
	writers.Put(w)
	return buf.Bytes()
}

func blobDelta(base, target []byte) []byte {
	limit := min(len(base), len(target))
	prefix, suffix := 0, 0
	for prefix < limit && base[prefix] == target[prefix] {
		prefix++
	}
	for suffix < limit-prefix && base[len(base)-1-suffix] == target[len(target)-1-suffix] {
		suffix++
	}
	if prefix+suffix < max(96, len(target)/2) {
		return nil
	}
	d := binary.AppendUvarint(nil, uint64(len(base)))
	d = binary.AppendUvarint(d, uint64(len(target)))
	copyRun := func(at, size int) {
		for size > 0 {
			n := min(size, 0xffffff)
			start := len(d)
			d = append(d, 128)
			for i := 0; i < 7; i++ {
				var b byte
				if i < 4 {
					b = byte(at >> (8 * i))
				} else {
					b = byte(n >> (8 * (i - 4)))
				}
				if b != 0 {
					d[start] |= 1 << i
					d = append(d, b)
				}
			}
			at, size = at+n, size-n
		}
	}
	copyRun(0, prefix)
	for i := prefix; i < len(target)-suffix; {
		n := min(127, len(target)-suffix-i)
		d = append(d, byte(n))
		d = append(d, target[i:i+n]...)
		i += n
	}
	copyRun(len(base)-suffix, suffix)
	return d
}

func deltaDistance(n int) []byte {
	var out [10]byte
	i := len(out) - 1
	out[i] = byte(n & 127)
	for n >>= 7; n > 0; n >>= 7 {
		n--
		i--
		out[i] = byte(n&127) | 128
	}
	return out[i:]
}

func (r *repository) pack(ids []oid, allowOffset bool) []byte {
	out := append([]byte("PACK"), 0, 0, 0, 2)
	out = binary.BigEndian.AppendUint32(out, uint32(len(ids)))
	var base *object
	var baseID oid
	basePos := 0
	for _, id := range ids {
		obj := r.object(id)
		data := obj.compressed
		if data == nil {
			data = deflate(obj.data)
		}
		pos := len(out)
		header := packHeader(nil, obj.kind, len(obj.data))
		if obj.kind == blobKind && len(obj.data) >= 256 {
			used := false
			if base != nil {
				if delta := blobDelta(base.data, obj.data); delta != nil {
					enc := deflate(delta)
					ptr, kind := baseID[:], byte(7)
					if allowOffset {
						ptr, kind = deltaDistance(pos-basePos), 6
					}
					if len(enc)+len(ptr) < len(data) {
						header = append(packHeader(header[:0], kind, len(delta)), ptr...)
						data, used = enc, true
					}
				}
			}
			if !used {
				base, baseID, basePos = obj, id, pos
			}
		}
		out = append(out, header...)
		out = append(out, data...)
	}
	sum := sha1.Sum(out)
	return append(out, sum[:]...)
}

const fullDepth = math.MaxInt
