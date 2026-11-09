package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"swiiftxml/internal/xmlview"
)

//go:embed web
var webFiles embed.FS

// app is the local web server behind the user interface. It only listens on
// 127.0.0.1 and every API call must carry the random token from the start
// URL, so other websites open in the browser cannot talk to it.
type app struct {
	token  string
	host   string
	quit   chan struct{}
	notice string // shown once in the UI, e.g. a file from the command line could not be opened

	mu  sync.Mutex
	doc *xmlview.Doc
}

func (a *app) routes() http.Handler {
	static, _ := fs.Sub(webFiles, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("POST /api/open", a.handleOpen)
	mux.HandleFunc("GET /api/ls", a.handleList)
	mux.HandleFunc("POST /api/index", a.withDoc(a.handleIndex))
	mux.HandleFunc("GET /api/rows", a.withDoc(a.handleRows))
	mux.HandleFunc("GET /api/row", a.withDoc(a.handleRow))
	mux.HandleFunc("GET /api/locate", a.withDoc(a.handleLocate))
	mux.HandleFunc("GET /api/children", a.withDoc(a.handleChildren))
	mux.HandleFunc("GET /api/text", a.withDoc(a.handleText))
	mux.HandleFunc("GET /api/xml", a.withDoc(a.handleXML))
	mux.HandleFunc("GET /api/download", a.withDoc(a.handleDownload))
	mux.HandleFunc("GET /api/search", a.withDoc(a.handleSearch))
	mux.HandleFunc("POST /api/aggregate", a.withDoc(a.handleAggregate))
	mux.HandleFunc("GET /api/aggregate", a.withDoc(a.handleAggregateStatus))
	mux.HandleFunc("POST /api/aggregate/stop", a.withDoc(a.handleAggregateStop))
	mux.HandleFunc("POST /api/quit", a.handleQuit)
	return a.guard(mux)
}

func (a *app) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject requests for other host names (DNS rebinding).
		if r.Host != a.host && r.Host != strings.Replace(a.host, "127.0.0.1", "localhost", 1) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			t := r.Header.Get("X-Token")
			if t == "" {
				t = r.URL.Query().Get("t")
			}
			if subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) != 1 {
				writeErr(w, http.StatusForbidden, errors.New("invalid session; please restart SwiiftXML"))
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (a *app) current() *xmlview.Doc {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.doc
}

func (a *app) withDoc(h func(http.ResponseWriter, *http.Request, *xmlview.Doc)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := a.current()
		if d == nil {
			writeErr(w, http.StatusConflict, errors.New("no file is open"))
			return
		}
		h(w, r, d)
	}
}

func (a *app) openFile(path string) error {
	d, err := xmlview.Open(path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	old := a.doc
	a.doc = d
	a.mu.Unlock()
	if old != nil {
		old.Close()
	}
	addRecent(d.Path)
	return nil
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"version": version,
		"os":      runtime.GOOS,
		"recent":  loadRecent(),
	}
	if d := a.current(); d != nil {
		resp["doc"] = d.Status(r.URL.Query().Get("v"))
	}
	a.mu.Lock()
	if a.notice != "" {
		resp["notice"] = a.notice
		a.notice = ""
	}
	a.mu.Unlock()
	writeJSON(w, resp)
}

func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`) // Windows "Copy as path" adds quotes
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	return p
}

func (a *app) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	path := cleanPath(req.Path)
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		writeJSON(w, map[string]any{"dir": path})
		return
	}
	if err := a.openFile(path); err != nil {
		writeErr(w, http.StatusBadRequest, friendly(err))
		return
	}
	writeJSON(w, map[string]any{"doc": a.current().Status("")})
}

func friendly(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("the file does not exist")
	case errors.Is(err, fs.ErrPermission):
		return errors.New("you do not have permission to read this file")
	}
	return err
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

const drivesDir = "::drives"

func (a *app) handleList(w http.ResponseWriter, r *http.Request) {
	dir := cleanPath(r.URL.Query().Get("dir"))
	if dir == "" {
		if d := a.current(); d != nil {
			dir = filepath.Dir(d.Path)
		} else if wd, err := os.Getwd(); err == nil {
			dir = wd
		} else {
			dir, _ = os.UserHomeDir()
		}
	}
	resp := struct {
		Dir     string     `json:"dir"`
		Parent  string     `json:"parent"`
		Home    string     `json:"home"`
		Entries []dirEntry `json:"entries"`
	}{Entries: []dirEntry{}}
	resp.Home, _ = os.UserHomeDir()

	if dir == drivesDir && runtime.GOOS == "windows" {
		resp.Dir = "This PC"
		for c := 'A'; c <= 'Z'; c++ {
			root := string(c) + `:\`
			if _, err := os.Stat(root); err == nil {
				resp.Entries = append(resp.Entries, dirEntry{Name: root, Path: root, Dir: true})
			}
		}
		writeJSON(w, resp)
		return
	}
	dir, _ = filepath.Abs(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, friendly(err))
		return
	}
	resp.Dir = dir
	if parent := filepath.Dir(dir); parent != dir {
		resp.Parent = parent
	} else if runtime.GOOS == "windows" {
		resp.Parent = drivesDir
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		full := filepath.Join(dir, name)
		isDir := e.IsDir()
		var size int64
		if info, err := os.Stat(full); err == nil { // follows links and shortcuts
			isDir = info.IsDir()
			size = info.Size()
		}
		resp.Entries = append(resp.Entries, dirEntry{Name: name, Path: full, Dir: isDir, Size: size})
		if len(resp.Entries) >= 5000 {
			break
		}
	}
	sort.Slice(resp.Entries, func(i, j int) bool {
		x, y := resp.Entries[i], resp.Entries[j]
		if x.Dir != y.Dir {
			return x.Dir
		}
		return strings.ToLower(x.Name) < strings.ToLower(y.Name)
	})
	writeJSON(w, resp)
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	var req struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing element name"))
		return
	}
	d.IndexName(req.Name)
	writeJSON(w, map[string]any{"ok": true})
}

func view(r *http.Request) (xmlview.View, error) {
	return xmlview.ParseView(r.URL.Query().Get("v"))
}

func intParam(r *http.Request, name string, def int64) int64 {
	if n, err := strconv.ParseInt(r.URL.Query().Get(name), 10, 64); err == nil {
		return n
	}
	return def
}

func (a *app) handleRows(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	v, err := view(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n := int(min(max(intParam(r, "n", 100), 1), 500))
	rows, total, err := d.Rows(v, max(intParam(r, "from", 0), 0), n)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"rows": rows, "total": total})
}

func (a *app) handleRow(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	v, err := view(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	i := intParam(r, "i", 0)
	node, total, err := d.Row(v, i)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"i": i, "total": total, "node": node})
}

func (a *app) handleLocate(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	v, err := view(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	row, err := d.RowAt(v, intParam(r, "off", 0))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"row": row})
}

func (a *app) handleChildren(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	node, err := d.Children(intParam(r, "s", -1), intParam(r, "c", 0))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, node)
}

func (a *app) handleText(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	text, trunc, err := d.Text(intParam(r, "s", -1), 4<<20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"text": text, "trunc": trunc})
}

func (a *app) handleXML(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	pretty := r.URL.Query().Get("pretty") == "1"
	xml, trunc, err := d.XML(intParam(r, "s", -1), intParam(r, "e", -1), 4<<20, pretty)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]any{"xml": xml, "trunc": trunc})
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	start := intParam(r, "s", -1)
	if start < 0 || start >= d.Size {
		writeErr(w, http.StatusBadRequest, errors.New("invalid position"))
		return
	}
	name := unsafeName.ReplaceAllString(r.URL.Query().Get("name"), "_")
	if name == "" {
		name = "node"
	}
	sec := d.Section(start, intParam(r, "e", -1))
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.xml"`, name))
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"%s\"?>\n", d.Encoding)
	io.Copy(w, sec)
}

func (a *app) handleSearch(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	v, err := view(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	q := r.URL.Query()
	res, err := d.Search(r.Context(), v, q.Get("q"), max(intParam(r, "from", 0), 0), q.Get("case") == "1")
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, res)
}

func (a *app) handleAggregate(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	var req struct {
		View, Value string
		Groups      []string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := xmlview.ParseView(req.View)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := d.StartAggregate(xmlview.AggSpec{View: v, Value: req.Value, Groups: req.Groups}); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *app) handleAggregateStatus(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	writeJSON(w, map[string]any{"size": d.Size, "status": d.AggregateStatus()})
}

func (a *app) handleAggregateStop(w http.ResponseWriter, r *http.Request, d *xmlview.Doc) {
	d.StopAggregate()
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *app) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(a.quit)
	}()
}

// Recently opened files are remembered in the user's config folder.

const maxRecent = 12

var recentMu sync.Mutex

func recentFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "SwiiftXML", "recent.json")
}

func loadRecent() []string {
	recentMu.Lock()
	defer recentMu.Unlock()
	var list []string
	if f := recentFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			json.Unmarshal(b, &list)
		}
	}
	return list
}

func addRecent(path string) {
	list := loadRecent()
	recentMu.Lock()
	defer recentMu.Unlock()
	out := []string{path}
	for _, p := range list {
		if p != path && len(out) < maxRecent {
			out = append(out, p)
		}
	}
	f := recentFile()
	if f == "" {
		return
	}
	os.MkdirAll(filepath.Dir(f), 0o700)
	b, _ := json.Marshal(out)
	os.WriteFile(f, b, 0o600)
}
