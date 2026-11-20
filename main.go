package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type doc = map[string]any

type fault struct {
	status  int
	message string
}

func reject(status int, message string) { panic(fault{status, message}) }
func missing()                          { reject(404, "Not Found") }
func invalid(message string)            { reject(422, message) }
func badGit()                           { reject(400, "Invalid git data") }

func str(v any) string {
	s, _ := v.(string)
	return s
}

func required(d doc, key string) string {
	s := str(d[key])
	if s == "" {
		invalid("Missing or invalid " + key)
	}
	return s
}

func integer(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		x, _ := strconv.Atoi(n)
		return x
	}
	return 0
}

type response struct {
	status int
	mime   string
	body   []byte
}

const jsonType = "application/json; charset=utf-8"

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func jsonReply(status int, v any) response { return response{status, jsonType, marshal(v)} }
func ok(v any) response                    { return jsonReply(200, v) }

type forge struct {
	mu                      sync.RWMutex
	auth, owner, iso, stamp string
	user                    account
	next                    int
	repos                   map[string]*repository
	byID                    map[string]nodeRef
	requestCount            atomic.Uint64
}

type nodeRef struct {
	repo *repository
	item *issue
}

func newForge() *forge {
	owner := os.Getenv("FORGE_OWNER")
	if owner == "" {
		owner = "adaiversen"
	}
	now := time.Unix(0, 0).UTC()
	if s := os.Getenv("MAKEFUN_TRIAL_NOW"); s != "" {
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
			now = time.UnixMilli(ms).UTC()
		} else {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				panic("invalid MAKEFUN_TRIAL_NOW")
			}
			now = t.UTC()
		}
	}
	f := &forge{owner: owner, next: 2, iso: now.Format("2006-01-02T15:04:05.000Z"), stamp: strconv.FormatInt(now.Unix(), 10) + " +0000", repos: make(map[string]*repository), byID: make(map[string]nodeRef)}
	if token := os.Getenv("MAKEFUN_SHOP_TOKEN"); token != "" {
		f.auth = "Bearer " + token
	}
	f.user = account{ID: 1, NodeID: globalID("User", 1), Login: owner, Type: "User"}
	return f
}

const maxBody = 128 << 20

func bodyOf(req *http.Request) []byte {
	if req.Body == nil || req.ContentLength == 0 {
		return nil
	}
	if req.ContentLength > maxBody {
		reject(413, "Request body too large")
	}
	var reader io.Reader = req.Body
	switch req.Header.Get("Content-Encoding") {
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			reject(400, "Invalid compressed request")
		}
		defer gz.Close()
		reader = gz
	case "", "identity":
		if req.ContentLength >= 0 {
			b := make([]byte, req.ContentLength)
			if _, err := io.ReadFull(reader, b); err != nil {
				reject(400, "Incomplete request")
			}
			return b
		}
	default:
		reject(415, "Unsupported content encoding")
	}
	b, err := io.ReadAll(io.LimitReader(reader, maxBody+1))
	if err != nil {
		reject(400, "Invalid request body")
	}
	if len(b) > maxBody {
		reject(413, "Request body too large")
	}
	return b
}

func jsonObject(data []byte) doc {
	if len(data) == 0 {
		return doc{}
	}
	var d doc
	if err := json.Unmarshal(data, &d); err != nil || d == nil {
		reject(400, "Problems parsing JSON")
	}
	return d
}

func (f *forge) route(w http.ResponseWriter, req *http.Request) response {
	path := req.URL.Path
	if path == "/healthz" && req.Method == "GET" {
		return response{200, jsonType, []byte("{\"ok\":true}")}
	}
	isAPI := strings.HasPrefix(path, "/api/") || path == "/mcp"
	isGit := strings.Contains(path, ".git/")
	if !isAPI && !isGit {
		missing()
	}
	if f.auth == "" || req.Header.Get("Authorization") != f.auth {
		reject(401, "Bad credentials")
	}
	body := bodyOf(req)
	write := req.Method != "GET" && req.Method != "HEAD"
	if isGit && !isAPI {
		write = req.Method == "POST" && strings.HasSuffix(path, "/git-receive-pack")
	}
	if write {
		f.mu.Lock()
		defer f.mu.Unlock()
	} else {
		f.mu.RLock()
		defer f.mu.RUnlock()
	}
	switch {
	case strings.HasPrefix(path, "/api/v3/") || path == "/api/v3":
		return f.rest(w, req, body)
	case path == "/api/graphql" && req.Method == "POST":
		return f.graphQL(body)
	case path == "/mcp" && req.Method == "POST":
		return f.mcp(body)
	case isGit:
		w.Header().Set("Cache-Control", "no-cache")
		return f.smart(req, body)
	default:
		missing()
	}
	return response{}
}

func (f *forge) safely(w http.ResponseWriter, req *http.Request) (r response) {
	defer func() {
		if err := recover(); err != nil {
			if e, yes := err.(fault); yes {
				r = jsonReply(e.status, doc{"message": e.message})
			} else {
				fmt.Fprintln(os.Stderr, "forge handler:", err)
				r = jsonReply(500, doc{"message": "Internal Server Error"})
			}
		}
	}()
	return f.route(w, req)
}

func (f *forge) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if f.serveIssueRead(w, req) {
		return
	}
	r := f.safely(w, req)
	h := w.Header()
	h["Date"] = nil
	h["X-Github-Request-Id"] = []string{strconv.FormatUint(f.requestCount.Add(1), 16)}
	if r.mime != "" {
		if r.mime == jsonType {
			h["Content-Type"] = jsonHeader
		} else {
			h["Content-Type"] = []string{r.mime}
		}
	}
	if r.status != 204 {
		h["Content-Length"] = []string{strconv.Itoa(len(r.body))}
	}
	w.WriteHeader(r.status)
	if len(r.body) > 0 {
		_, _ = w.Write(r.body)
	}
}

var jsonHeader = []string{jsonType}

// Cached issue bytes and their length are replaced together on each write.
func (f *forge) serveIssueRead(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodGet || req.URL.RawQuery != "" {
		return false
	}
	path, ok := strings.CutPrefix(req.URL.Path, "/api/v3/repos/")
	if !ok || f.auth == "" || req.Header.Get("Authorization") != f.auth {
		return false
	}
	owner, path, ok := strings.Cut(path, "/")
	if !ok || !strings.EqualFold(owner, f.owner) {
		return false
	}
	name, tail, ok := strings.Cut(path, "/issues/")
	if !ok {
		return false
	}
	number, err := strconv.Atoi(tail)
	if err != nil {
		return false
	}
	f.mu.RLock()
	var data []byte
	var length string
	if repo := f.repos[strings.ToLower(name)]; repo != nil {
		if issue := repo.issues[number]; issue != nil {
			data, length = issue.wire, issue.wireLength
		}
	}
	f.mu.RUnlock()
	if data == nil {
		return false
	}
	header := w.Header()
	header["Date"] = nil
	header["Content-Type"] = jsonHeader
	header["Content-Length"] = []string{length}
	header["X-Github-Request-Id"] = []string{strconv.FormatUint(f.requestCount.Add(1), 16)}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return true
}

func main() {
	f := newForge()
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		panic(err)
	}
	fmt.Println(ln.Addr().(*net.TCPAddr).Port)
	srv := http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
