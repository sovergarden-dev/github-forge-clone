package main

import (
	"bytes"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

type hunk struct {
	start, end int
	insert     []string
}

func fileLines(data []byte) []string {
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func edits(a, b []string) []hunk {
	lo, ai, bi := 0, len(a), len(b)
	for lo < ai && lo < bi && a[lo] == b[lo] {
		lo++
	}
	for ai > lo && bi > lo && a[ai-1] == b[bi-1] {
		ai--
		bi--
	}
	if ai == lo && bi == lo {
		return nil
	}
	w, h := bi-lo+1, ai-lo+1
	if w*h > 1000000 {
		return []hunk{{lo, ai, b[lo:bi]}}
	}
	lcs := make([]uint32, w*h)
	for i := ai - 1; i >= lo; i-- {
		for j := bi - 1; j >= lo; j-- {
			k := (i-lo)*w + j - lo
			if a[i] == b[j] {
				lcs[k] = 1 + lcs[k+w+1]
			} else {
				lcs[k] = max(lcs[k+w], lcs[k+1])
			}
		}
	}
	var result []hunk
	var change *hunk
	for i, j := lo, lo; i < ai || j < bi; {
		if i < ai && j < bi && a[i] == b[j] {
			i++
			j++
			change = nil
			continue
		}
		if change == nil {
			result = append(result, hunk{start: i, end: i})
			change = &result[len(result)-1]
		}
		k := (i-lo)*w + j - lo
		if j < bi && (i == ai || lcs[k+1] > lcs[k+w]) {
			change.insert = append(change.insert, b[j])
			j++
		} else {
			i++
			change.end = i
		}
	}
	return result
}

func (r *repository) fileDiff(p *issue) []doc {
	result := make([]doc, 0)
	raw := func(e treeEntry) []byte {
		if e.id == zero || e.mode == "160000" {
			return nil
		}
		return r.object(e.id).data
	}
	for _, c := range r.changes(p) {
		status, id := "modified", c.after.id
		if c.before.id == zero {
			status = "added"
		}
		if c.after.id == zero {
			status, id = "removed", c.before.id
		}
		a, b := raw(c.before), raw(c.after)
		add, del := 0, 0
		if bytes.IndexByte(a, 0) < 0 && bytes.IndexByte(b, 0) < 0 {
			for _, h := range edits(fileLines(a), fileLines(b)) {
				add += len(h.insert)
				del += h.end - h.start
			}
		}
		result = append(result, doc{"filename": c.path, "sha": id.String(), "status": status, "additions": add, "deletions": del, "changes": add + del})
	}
	return result
}

func (r *repository) mergeFile(base, ours, theirs treeEntry) (treeEntry, bool) {
	for _, e := range []treeEntry{base, ours, theirs} {
		if e.mode != "100644" && e.mode != "100755" {
			return treeEntry{}, false
		}
		raw := r.object(e.id).data
		if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
			return treeEntry{}, false
		}
	}
	a := fileLines(r.object(base.id).data)
	left := edits(a, fileLines(r.object(ours.id).data))
	both := slices.Clone(left)
	for _, newEdit := range edits(a, fileLines(r.object(theirs.id).data)) {
		duplicate := false
		for _, old := range left {
			if old.start == newEdit.start && old.end == newEdit.end && slices.Equal(old.insert, newEdit.insert) {
				duplicate = true
				break
			}
			if old.start == newEdit.start || old.start < newEdit.end && newEdit.start < old.end {
				return treeEntry{}, false
			}
		}
		if !duplicate {
			both = append(both, newEdit)
		}
	}
	sort.Slice(both, func(i, j int) bool { return both[i].start < both[j].start })
	var merged strings.Builder
	cursor := 0
	for _, e := range both {
		for _, line := range a[cursor:e.start] {
			merged.WriteString(line)
		}
		for _, line := range e.insert {
			merged.WriteString(line)
		}
		cursor = e.end
	}
	for _, line := range a[cursor:] {
		merged.WriteString(line)
	}
	if ours.mode == base.mode {
		ours.mode = theirs.mode
	}
	ours.id = r.put(blobKind, []byte(merged.String()))
	return ours, true
}

func (r *repository) mergeTree(p *issue) map[string]treeEntry {
	_, b := r.tips(p)
	files := r.flatten(b)
	for _, c := range r.changes(p) {
		current := files[c.path]
		switch {
		case current == c.after:
			continue
		case current != c.before:
			merged, ok := r.mergeFile(c.before, current, c.after)
			if !ok {
				return nil
			}
			files[c.path] = merged
		case c.after.id == zero:
			delete(files, c.path)
		default:
			files[c.path] = c.after
		}
	}
	if !pathsFit(files) {
		return nil
	}
	return files
}
