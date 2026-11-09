package xmlview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errUTF16        = errors.New("UTF-16 encoded XML files are not supported; please save the file as UTF-8")
	ErrNoRow        = errors.New("row does not exist")
	ErrNotIndexed   = errors.New("rows for this element have not been indexed")
	errNotAnElement = errors.New("there is no element at this position")
)

const (
	maxLevel     = 4         // levels below the root that are indexed in the first pass
	cpRows       = 32        // a checkpoint is stored at least every cpRows rows ...
	cpBytes      = 256 << 10 // ... and at least every cpBytes bytes
	publishEvery = 4 << 20   // how often a running pass publishes its progress
	maxNames     = 5000      // distinct element names tracked for statistics
	maxTracked   = 400       // (level, name) combinations that get their own row index
)

type checkpoint struct{ row, off int64 }

// rowSet is a read-only snapshot of a row index. To find row r, a reader
// seeks to the last checkpoint at or before r and scans forward; thanks to
// the checkpoint rules that never means scanning more than cpRows rows or
// cpBytes bytes plus one row.
type rowSet struct {
	cps     []checkpoint
	count   int64
	maxSize int64
}

type rowBuilder struct {
	rowSet
	lastRow, lastOff int64
}

func (b *rowBuilder) add(off int64) {
	if b.count == 0 || b.count-b.lastRow >= cpRows || off-b.lastOff >= cpBytes {
		b.cps = append(b.cps, checkpoint{b.count, off})
		b.lastRow, b.lastOff = b.count, off
	}
	b.count++
}

func (b *rowBuilder) size(n int64) {
	if n > b.maxSize {
		b.maxSize = n
	}
}

// NameStat describes one element name found in the file.
type NameStat struct {
	Name       string `json:"name"`
	Count      int64  `json:"count"`      // occurrences anywhere in the file
	Depth      int    `json:"depth"`      // shallowest level it occurs at (the root is level 0)
	Level      int    `json:"level"`      // level 1..maxLevel where it occurs most, 0 if none
	LevelCount int64  `json:"levelCount"` // occurrences at that level
}

type nameTrack struct {
	NameStat
	lv [maxLevel]*rowBuilder // rows with this name, per level
}

// View selects what counts as a row:
//
//	{Level: 2}               all elements at nesting level 2 (children of the root are level 1)
//	{Level: 2, Name: "Item"} the <Item> elements at level 2
//	{Name: "Item"}           all outermost <Item> elements, at any depth (needs a name pass)
type View struct {
	Level int
	Name  string
}

// ParseView parses a view key: "L2", "L2:Item" or "N:Item".
func ParseView(key string) (View, error) {
	if name, ok := strings.CutPrefix(key, "N:"); ok && name != "" {
		return View{Name: name}, nil
	}
	if rest, ok := strings.CutPrefix(key, "L"); ok {
		lv, name, _ := strings.Cut(rest, ":")
		if n, err := strconv.Atoi(lv); err == nil && n >= 1 && n <= maxLevel {
			return View{Level: n, Name: name}, nil
		}
	}
	return View{}, fmt.Errorf("invalid row selection %q", key)
}

func (v View) Key() string {
	if v.Level == 0 {
		return "N:" + v.Name
	}
	k := "L" + strconv.Itoa(v.Level)
	if v.Name != "" {
		k += ":" + v.Name
	}
	return k
}

// pass is one sequential scan over the file that builds row indexes. The
// level pass indexes levels 1..maxLevel (also per element name) at once; a
// name pass indexes the outermost elements with one name.
type pass struct {
	name    string
	cancel  context.CancelFunc
	scanned atomic.Int64
	done    atomic.Bool

	mu      sync.RWMutex
	sets    []rowSet
	byName  [maxLevel]map[string]rowSet
	main    [maxLevel]string // most common element name per level
	stats   []NameStat
	root    string
	err     error
	elapsed time.Duration
}

func (p *pass) set(v View) rowSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	switch {
	case p.name != "":
		return p.sets[0]
	case v.Name == "":
		return p.sets[v.Level-1]
	}
	return p.byName[v.Level-1][v.Name] // empty if the name was not seen at that level
}

// Doc is an open XML file.
type Doc struct {
	Path     string
	Size     int64
	Encoding string
	f        *os.File
	cs       *charset
	levels   *pass

	mu    sync.Mutex
	named *pass
	agg   *aggJob
}

// Open opens an XML file and starts indexing it in the background.
func Open(path string) (*Doc, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s is a folder, not a file", path)
	}
	head := make([]byte, 4096)
	n, _ := f.ReadAt(head, 0)
	cs, enc, err := detectEncoding(head[:n])
	if err != nil {
		f.Close()
		return nil, err
	}
	d := &Doc{Path: path, Size: st.Size(), Encoding: enc, f: f, cs: cs}
	d.levels = d.startPass("")
	return d, nil
}

// Close stops background work and closes the file.
func (d *Doc) Close() {
	d.levels.cancel()
	d.mu.Lock()
	if d.named != nil {
		d.named.cancel()
	}
	if d.agg != nil {
		d.agg.cancel()
	}
	d.mu.Unlock()
	d.f.Close()
}

func (d *Doc) newScanner() *Scanner { return NewScanner(d.f, 256<<10) }

func (d *Doc) startPass(name string) *pass {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pass{name: name, cancel: cancel}
	if name == "" {
		p.sets = make([]rowSet, maxLevel)
	} else {
		p.sets = make([]rowSet, 1)
	}
	go d.run(ctx, p)
	return p
}

// IndexName starts indexing the outermost elements called name, unless that
// is already running or done.
func (d *Doc) IndexName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.named != nil {
		if d.named.name == name {
			return
		}
		d.named.cancel()
	}
	d.named = d.startPass(name)
}

func (d *Doc) run(ctx context.Context, p *pass) {
	start := time.Now()
	s := NewScanner(d.f, 4<<20)
	b := make([]rowBuilder, len(p.sets))
	name := []byte(p.name)
	levelPass := p.name == ""
	stats := map[string]*nameTrack{}
	tracked := 0
	var open [maxLevel + 1]int64 // start offsets of the open elements per level
	var root string
	depth, rowDepth := 0, -1
	var rowStart int64
	nextPublish := int64(publishEvery)

	publish := func(final bool) {
		var byName [maxLevel]map[string]rowSet
		var main [maxLevel]string
		var mainCount [maxLevel]int64
		var names []NameStat
		if levelPass {
			for i := range byName {
				byName[i] = map[string]rowSet{}
			}
			for _, st := range stats {
				ns := st.NameStat
				for l, nb := range st.lv {
					if nb == nil {
						continue
					}
					byName[l][st.Name] = nb.rowSet
					if nb.count > mainCount[l] || nb.count == mainCount[l] && st.Name < main[l] {
						main[l], mainCount[l] = st.Name, nb.count
					}
					if nb.count > ns.LevelCount {
						ns.Level, ns.LevelCount = l+1, nb.count
					}
				}
				names = append(names, ns)
			}
		}
		p.mu.Lock()
		for i := range b {
			p.sets[i] = b[i].rowSet
		}
		p.byName, p.main, p.stats = byName, main, topNames(names)
		p.root = root
		p.elapsed = time.Since(start)
		if final {
			p.err = s.Err()
		}
		p.mu.Unlock()
		p.scanned.Store(s.Offset())
	}

	for {
		t := s.Next()
		switch t.Kind {
		case KStart, KEmpty:
			if levelPass {
				if depth == 0 && root == "" {
					root = string(t.Name)
				}
				st := stats[string(t.Name)]
				if st == nil && len(stats) < maxNames {
					st = &nameTrack{NameStat: NameStat{Name: string(t.Name), Depth: depth}}
					stats[st.Name] = st
				}
				if st != nil {
					st.Count++
					st.Depth = min(st.Depth, depth)
				}
				if depth >= 1 && depth <= maxLevel {
					b[depth-1].add(t.Start)
					if t.Kind == KEmpty {
						b[depth-1].size(t.End - t.Start)
					} else {
						open[depth] = t.Start
					}
					if st != nil {
						nb := st.lv[depth-1]
						if nb == nil && tracked < maxTracked {
							nb = &rowBuilder{}
							st.lv[depth-1] = nb
							tracked++
						}
						if nb != nil {
							nb.add(t.Start)
						}
					}
				}
			} else if rowDepth < 0 && bytes.Equal(t.Name, name) {
				b[0].add(t.Start)
				if t.Kind == KEmpty {
					b[0].size(t.End - t.Start)
				} else {
					rowDepth, rowStart = depth, t.Start
				}
			}
			if t.Kind == KStart {
				depth++
			}
		case KEnd:
			if depth > 0 {
				depth--
			}
			if levelPass {
				if depth >= 1 && depth <= maxLevel {
					b[depth-1].size(t.End - open[depth])
				}
			} else if depth == rowDepth {
				b[0].size(t.End - rowStart)
				rowDepth = -1
			}
		case KEOF:
			publish(true)
			p.done.Store(true)
			return
		}
		if t.End >= nextPublish {
			nextPublish = t.End + publishEvery
			publish(false)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func topNames(all []NameStat) []NameStat {
	out := all[:0]
	for _, st := range all {
		if st.Count > 1 {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Depth != out[j].Depth {
			return out[i].Depth < out[j].Depth
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// pickLevel chooses which nesting level makes good rows: the shallowest one
// with a reasonable number of elements, where no single element is most of
// the file. For <Export><Records><Record/>...</Records></Export> that is the
// level of <Record>.
func pickLevel(sets []rowSet, done bool, size int64) int {
	many := 0
	for i, s := range sets {
		if s.count >= 10 {
			many = i + 1
			break
		}
	}
	if done {
		for i, s := range sets {
			if s.count >= 2 && s.maxSize <= size/2 {
				if many == 0 || i+1 < many {
					return i + 1
				}
				break
			}
		}
	}
	if many > 0 {
		return many
	}
	best := 0
	for i, s := range sets {
		if s.count > sets[best].count {
			best = i
		}
	}
	return best + 1
}

// AutoView returns the rows SwiiftXML picks by itself: the elements of the
// level chosen by pickLevel, limited to the main element name when a few
// other elements (such as header fields) share the level.
func (d *Doc) AutoView() View {
	p := d.levels
	p.mu.RLock()
	defer p.mu.RUnlock()
	lv := pickLevel(p.sets, p.done.Load(), d.Size)
	main := p.main[lv-1]
	mainCount := p.byName[lv-1][main].count
	if total := p.sets[lv-1].count; mainCount < total && mainCount*10 >= total*9 {
		return View{Level: lv, Name: main}
	}
	return View{Level: lv}
}

func (d *Doc) passFor(v View) (*pass, error) {
	if v.Level > 0 {
		if v.Level > maxLevel {
			return nil, ErrNotIndexed
		}
		return d.levels, nil
	}
	d.mu.Lock()
	p := d.named
	d.mu.Unlock()
	if p == nil || p.name != v.Name {
		return nil, ErrNotIndexed
	}
	return p, nil
}

func (d *Doc) rowSet(v View) (rowSet, error) {
	p, err := d.passFor(v)
	if err != nil {
		return rowSet{}, err
	}
	return p.set(v), nil
}

// RowCount returns the number of rows indexed so far for a view.
func (d *Doc) RowCount(v View) (int64, error) {
	set, err := d.rowSet(v)
	return set.count, err
}

// cursor walks the rows of a view.
type cursor struct {
	s     *Scanner
	set   rowSet
	level bool   // rows are the elements at one level
	name  []byte // only elements with this name are rows
}

func (d *Doc) cursor(v View) (*cursor, error) {
	set, err := d.rowSet(v)
	if err != nil {
		return nil, err
	}
	c := &cursor{s: d.newScanner(), set: set, level: v.Level > 0}
	if v.Name != "" {
		c.name = []byte(v.Name)
	}
	return c, nil
}

func (c *cursor) isCheckpoint(r int64) bool {
	cps := c.set.cps
	i := sort.Search(len(cps), func(i int) bool { return cps[i].row >= r })
	return i < len(cps) && cps[i].row == r
}

// locate positions the scanner just after the start tag of row r and
// returns that tag.
func (c *cursor) locate(r int64) (Token, error) {
	if r < 0 || r >= c.set.count {
		return Token{}, ErrNoRow
	}
	cps := c.set.cps
	cp := cps[sort.Search(len(cps), func(i int) bool { return cps[i].row > r })-1]
	c.s.SeekTo(cp.off)
	tok := c.s.Next()
	for row := cp.row; row < r && tok.Kind != KEOF; row++ {
		skip(c.s, tok)
		tok = c.next()
	}
	if tok.Kind != KStart && tok.Kind != KEmpty {
		if err := c.s.Err(); err != nil {
			return tok, err
		}
		return tok, ErrNoRow
	}
	return tok, nil
}

// next scans forward from between two rows to the start tag of the next row.
func (c *cursor) next() Token {
	if !c.level {
		for {
			t := c.s.Next()
			switch t.Kind {
			case KStart, KEmpty:
				if bytes.Equal(t.Name, c.name) {
					return t
				}
			case KEOF:
				return t
			}
		}
	}
	rel := 0 // depth relative to the level of the rows
	for {
		t := c.s.Next()
		switch t.Kind {
		case KStart, KEmpty:
			if rel == 0 {
				if c.name == nil || bytes.Equal(t.Name, c.name) {
					return t
				}
				skip(c.s, t) // another element on the row level
				continue
			}
			if t.Kind == KStart {
				rel++
			}
		case KEnd:
			rel--
		case KEOF:
			return t
		}
	}
}

// rowAt finds the row containing file offset m. It returns the row, whether
// m is inside it, and otherwise the offset where the next row starts (-1 if
// there is none).
func (c *cursor) rowAt(m int64) (int64, bool, int64) {
	cps := c.set.cps
	if c.set.count == 0 {
		return 0, false, -1
	}
	i := sort.Search(len(cps), func(i int) bool { return cps[i].off > m }) - 1
	if i < 0 {
		return 0, false, cps[0].off
	}
	r := cps[i].row
	c.s.SeekTo(cps[i].off)
	tok := c.s.Next()
	for {
		if tok.Start > m {
			return r, false, tok.Start
		}
		if skipPast(c.s, tok, m) {
			return r, true, 0
		}
		if r++; r >= c.set.count {
			return r, false, -1
		}
		if tok = c.next(); tok.Kind == KEOF {
			return r, false, -1
		}
	}
}

// RowAt returns the row containing file offset off, or the first row after it.
func (d *Doc) RowAt(v View, off int64) (int64, error) {
	c, err := d.cursor(v)
	if err != nil {
		return 0, err
	}
	r, _, _ := c.rowAt(off)
	return max(0, min(r, c.set.count-1)), nil
}

// skip consumes the rest of the element whose start tag is tok and returns
// the offset just past its end tag.
func skip(s *Scanner, tok Token) int64 {
	if tok.Kind != KStart {
		return tok.End
	}
	prev := s.SetCapture(0)
	defer s.SetCapture(prev)
	depth := 1
	for {
		t := s.Next()
		switch t.Kind {
		case KStart:
			depth++
		case KEnd:
			if depth--; depth == 0 {
				return t.End
			}
		case KEOF:
			return t.End
		}
	}
}

// skipPast is like skip, but reports whether offset m lies inside the
// element, and stops as soon as that is known.
func skipPast(s *Scanner, tok Token, m int64) bool {
	if tok.End > m {
		return true
	}
	if tok.Kind != KStart {
		return false
	}
	prev := s.SetCapture(0)
	defer s.SetCapture(prev)
	depth := 1
	for {
		t := s.Next()
		if t.Kind == KEOF {
			return false
		}
		if t.End > m {
			return true
		}
		switch t.Kind {
		case KStart:
			depth++
		case KEnd:
			if depth--; depth == 0 {
				return false
			}
		}
	}
}

// End returns the offset just past the node starting at start.
func (d *Doc) End(start int64) int64 {
	s := d.newScanner()
	s.SetCapture(0)
	s.SeekTo(start)
	tok := s.Next()
	switch tok.Kind {
	case KStart:
		return skip(s, tok)
	case KText, KCData:
		end := tok.End
		for t := s.Next(); t.Kind == KText || t.Kind == KCData; t = s.Next() {
			end = t.End
		}
		return end
	}
	return tok.End
}

// LevelInfo describes the elements at one nesting level.
type LevelInfo struct {
	Level int    `json:"level"`
	Count int64  `json:"count"`
	Name  string `json:"name"` // the most common element name at this level
}

// PassInfo is the progress of an indexing pass.
type PassInfo struct {
	Name    string  `json:"name,omitempty"`
	Scanned int64   `json:"scanned"`
	Done    bool    `json:"done"`
	Count   int64   `json:"count"`
	Seconds float64 `json:"seconds"`
	Error   string  `json:"error,omitempty"`
}

// Status describes the file and the indexing progress.
type Status struct {
	Path     string      `json:"path"`
	FileName string      `json:"fileName"`
	Size     int64       `json:"size"`
	Encoding string      `json:"encoding"`
	Root     string      `json:"root"`
	Index    PassInfo    `json:"index"`
	Levels   []LevelInfo `json:"levels"`
	Auto     string      `json:"auto"`
	Names    []NameStat  `json:"names"`
	Named    *PassInfo   `json:"named,omitempty"`
	Rows     int64       `json:"rows"` // rows indexed so far for the view asked about; -1 if unavailable
}

func (p *pass) info(size int64) PassInfo {
	scanned := p.scanned.Load()
	done := p.done.Load()
	if done {
		scanned = size
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	pi := PassInfo{Name: p.name, Scanned: scanned, Done: done, Seconds: p.elapsed.Seconds(), Count: p.sets[0].count}
	if p.err != nil {
		pi.Error = p.err.Error()
	}
	return pi
}

// Status reports on the file. viewKey names a view whose row count is
// included; it may be empty.
func (d *Doc) Status(viewKey string) Status {
	p := d.levels
	st := Status{
		Path: d.Path, FileName: filepath.Base(d.Path), Size: d.Size, Encoding: d.Encoding,
		Index: p.info(d.Size), Auto: d.AutoView().Key(), Rows: -1,
	}
	p.mu.RLock()
	st.Root = p.root
	st.Names = p.stats
	for i, s := range p.sets {
		if s.count > 0 {
			st.Levels = append(st.Levels, LevelInfo{Level: i + 1, Count: s.count, Name: p.main[i]})
		}
	}
	p.mu.RUnlock()
	d.mu.Lock()
	named := d.named
	d.mu.Unlock()
	if named != nil {
		info := named.info(d.Size)
		st.Named = &info
	}
	if v, err := ParseView(viewKey); err == nil {
		if n, err := d.RowCount(v); err == nil {
			st.Rows = n
		}
	}
	return st
}
