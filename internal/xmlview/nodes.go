package xmlview

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Node is one node of the tree shown for a row. Children are loaded lazily:
// a node with Loaded=false (or Next != 0) has more children that can be
// fetched later using its Start offset.
type Node struct {
	Kind    string  `json:"k"`            // "e" element, "t" text, "c" comment
	Name    string  `json:"n,omitempty"`  // element name
	Attrs   []Attr  `json:"a,omitempty"`  // element attributes
	Start   int64   `json:"s"`            // file offset where the node starts
	End     int64   `json:"e"`            // file offset just past the node; -1 if not known yet
	Text    string  `json:"t,omitempty"`  // leaf element value, text node or comment content
	Trunc   bool    `json:"tt,omitempty"` // Text was cut short
	Count   int     `json:"nc"`           // number of child nodes (elements, text runs, comments)
	Partial bool    `json:"np,omitempty"` // Count is only a lower bound
	Loaded  bool    `json:"l,omitempty"`  // Kids holds the children (up to Next)
	Kids    []*Node `json:"c,omitempty"`
	Next    int64   `json:"nx,omitempty"` // offset to continue loading children from; 0 = no more
}

// parseOpts bounds how much work and memory one request may use.
type parseOpts struct {
	depth     int   // levels of descendants to load
	budget    int   // max nodes to load in total
	kidLimit  int   // max children to load per node
	textLimit int   // max bytes kept per text value
	stopEarly bool  // stop scanning the top node once its child list is cut off
	maxBytes  int64 // with stopEarly: cut off the top node's child list after this many bytes
}

var (
	rowOpts     = parseOpts{depth: 64, budget: 4000, kidLimit: 1000, textLimit: 2000, stopEarly: true, maxBytes: 64 << 20}
	lazyOpts    = parseOpts{depth: 64, budget: 2000, kidLimit: 500, textLimit: 2000, stopEarly: true, maxBytes: 64 << 20}
	summaryOpts = parseOpts{depth: 1, budget: 12, kidLimit: 12, textLimit: 200, maxBytes: 64 << 10}
)

type parser struct {
	d      *Doc
	s      *Scanner
	o      parseOpts
	budget int
	base   int64 // offset where the top node's child list starts being read
	tmp    []byte
}

func (d *Doc) parser(s *Scanner, o parseOpts) *parser {
	s.SetCapture(o.textLimit)
	return &parser{d: d, s: s, o: o, budget: o.budget}
}

type textRun struct {
	active, nonSpace, trunc bool
	start, end              int64
	buf                     []byte
}

func appendLimited(dst, src []byte, limit int, trunc bool) ([]byte, bool) {
	if room := limit - len(dst); len(src) > room {
		if room > 0 {
			dst = append(dst, src[:room]...)
		}
		return dst, true
	}
	return append(dst, src...), trunc
}

// element parses the element whose start tag is tok, loading depth levels
// of descendants.
func (p *parser) element(tok Token, depth int, top bool) *Node {
	n := &Node{Kind: "e", Name: p.d.str(tok.Name, false), Attrs: p.d.parseAttrs(tok.Data), Start: tok.Start, End: -1}
	if top {
		p.base = tok.End
	}
	if tok.Kind == KEmpty {
		n.End, n.Loaded = tok.End, true
		return n
	}
	p.content(n, depth, top, false)
	return n
}

type admission int

const (
	skipKid    admission = iota // only count the child
	loadKid                     // load the child and its descendants
	shallowKid                  // load the child, but not its descendants
	stopKid                     // stop reading the top node here
)

// admit decides how the next child of n (starting at offset at) is read.
// When n's child list is cut off it records where to continue.
func (p *parser) admit(n *Node, at int64, top bool) admission {
	if !n.Loaded || n.Next != 0 {
		return skipKid
	}
	switch {
	case len(n.Kids) >= p.o.kidLimit, top && len(n.Kids) > 0 && p.o.maxBytes > 0 && at-p.base > p.o.maxBytes:
		// The child list is cut off below.
	case p.budget > 0:
		p.budget--
		return loadKid
	case top:
		return shallowKid // the top node always lists its children
	case len(n.Kids) == 0:
		n.Loaded = false // nothing loaded yet: load all children later
		return skipKid
	}
	n.Next = at
	if top && p.o.stopEarly {
		n.Partial = true
		return stopKid
	}
	return skipKid
}

// content reads the children of n up to and including its end tag. cont is
// set when continuing a child list from a Next offset.
func (p *parser) content(n *Node, depth int, top, cont bool) {
	n.Loaded = depth > 0
	var (
		run      textRun
		leaf     []byte
		leafCut  bool
		leafText bool // leaf has non-whitespace text
		hasElem  bool
	)
	flush := func() (stop bool) {
		if !run.active {
			return false
		}
		run.active = false
		if !run.nonSpace {
			return false
		}
		n.Count++
		switch p.admit(n, run.start, top) {
		case stopKid:
			return true
		case loadKid, shallowKid:
			n.Kids = append(n.Kids, &Node{Kind: "t", Start: run.start, End: run.end, Text: clean(run.buf, run.trunc), Trunc: run.trunc})
		}
		return false
	}
	finish := func(end int64) {
		n.End = end
		if !cont && !hasElem {
			n.Count, n.Kids, n.Next, n.Loaded = 0, nil, 0, true
			if leafText {
				n.Text, n.Trunc = clean(leaf, leafCut), leafCut
			}
		}
	}
	for {
		tok := p.s.Next()
		switch tok.Kind {
		case KText, KCData:
			if !run.active {
				run = textRun{active: true, start: tok.Start, buf: run.buf[:0]}
			}
			run.end = tok.End
			if !tok.Space {
				run.nonSpace, leafText = true, true
			}
			p.tmp = p.d.appendText(p.tmp[:0], tok.Data, tok.Kind == KText)
			if n.Loaded {
				run.buf, run.trunc = appendLimited(run.buf, p.tmp, p.o.textLimit, run.trunc || tok.Trunc)
			}
			if !hasElem {
				leaf, leafCut = appendLimited(leaf, p.tmp, p.o.textLimit, leafCut || tok.Trunc)
			}
		case KComment:
			if flush() {
				return
			}
			n.Count++
			switch p.admit(n, tok.Start, top) {
			case stopKid:
				return
			case loadKid, shallowKid:
				text := p.d.appendText(nil, tok.Data, false)
				n.Kids = append(n.Kids, &Node{Kind: "c", Start: tok.Start, End: tok.End, Text: clean(text, tok.Trunc), Trunc: tok.Trunc})
			}
		case KStart, KEmpty:
			if flush() {
				return
			}
			hasElem = true
			n.Count++
			switch p.admit(n, tok.Start, top) {
			case stopKid:
				return
			case loadKid:
				n.Kids = append(n.Kids, p.element(tok, depth-1, false))
			case shallowKid:
				n.Kids = append(n.Kids, p.element(tok, 0, false))
			default:
				skip(p.s, tok)
			}
		case KEnd:
			if flush() {
				return
			}
			finish(tok.End)
			return
		case KEOF:
			flush()
			finish(tok.End)
			return
		}
	}
}

// Row parses row r of a view, loading as much of its tree as the limits
// allow. It also returns the number of rows indexed so far.
func (d *Doc) Row(v View, r int64) (*Node, int64, error) {
	c, err := d.cursor(v)
	if err != nil {
		return nil, 0, err
	}
	tok, err := c.locate(r)
	if err != nil {
		return nil, c.set.count, err
	}
	p := d.parser(c.s, rowOpts)
	return p.element(tok, rowOpts.depth, true), c.set.count, nil
}

// Children loads the children of the element starting at start. With
// cursor > 0 it continues a child list that was cut off at that offset; the
// returned node then only holds the children from there on.
func (d *Doc) Children(start, cursor int64) (*Node, error) {
	s := d.newScanner()
	s.SeekTo(start)
	tok := s.Next()
	if tok.Kind != KStart && tok.Kind != KEmpty {
		return nil, errNotAnElement
	}
	p := d.parser(s, lazyOpts)
	if cursor <= 0 {
		return p.element(tok, lazyOpts.depth, true), nil
	}
	if cursor < tok.End {
		return nil, errNotAnElement
	}
	n := &Node{Kind: "e", Name: d.str(tok.Name, false), Start: start, End: -1}
	s.SeekTo(cursor)
	p.base = cursor
	p.content(n, lazyOpts.depth, true, true)
	return n, nil
}

// Text returns the full text of the leaf element, text node or comment at
// start, up to limit bytes.
func (d *Doc) Text(start int64, limit int) (string, bool, error) {
	s := d.newScanner()
	s.SeekTo(start)
	p := d.parser(s, parseOpts{textLimit: limit})
	tok := s.Next()
	switch tok.Kind {
	case KStart, KEmpty:
		n := p.element(tok, 0, false)
		return n.Text, n.Trunc, nil
	case KText, KCData:
		var buf []byte
		trunc := false
		for ; tok.Kind == KText || tok.Kind == KCData; tok = s.Next() {
			p.tmp = d.appendText(p.tmp[:0], tok.Data, tok.Kind == KText)
			buf, trunc = appendLimited(buf, p.tmp, limit, trunc || tok.Trunc)
		}
		return clean(buf, trunc), trunc, nil
	case KComment:
		return clean(d.appendText(nil, tok.Data, false), tok.Trunc), tok.Trunc, nil
	}
	return "", false, errNotAnElement
}

// RowSummary is a one-line description of a row for the row list.
type RowSummary struct {
	Index   int64  `json:"i"`
	Start   int64  `json:"s"`
	Name    string `json:"n"`
	Preview string `json:"p"`
}

// Rows summarizes up to n rows of a view starting at row from. It also
// returns the number of rows indexed so far.
func (d *Doc) Rows(v View, from int64, n int) ([]RowSummary, int64, error) {
	c, err := d.cursor(v)
	if err != nil {
		return nil, 0, err
	}
	total := c.set.count
	out := []RowSummary{}
	positioned := false
	for r := from; r < from+int64(n) && r < total; r++ {
		var tok Token
		if !positioned || c.isCheckpoint(r) {
			if tok, err = c.locate(r); err != nil {
				return out, total, err
			}
		} else if tok = c.next(); tok.Kind != KStart && tok.Kind != KEmpty {
			break
		}
		positioned = true
		// The scan may stop inside the row only when the next row can be
		// reached through a checkpoint.
		o := summaryOpts
		o.stopEarly = r+1 >= total || c.isCheckpoint(r+1)
		node := d.parser(c.s, o).element(tok, o.depth, true)
		out = append(out, summarize(r, node))
	}
	return out, total, nil
}

func summarize(r int64, n *Node) RowSummary {
	var parts []string
	for _, a := range n.Attrs {
		if len(parts) == 3 {
			break
		}
		parts = append(parts, a[0]+"="+short(a[1], 40))
	}
	if t := strings.TrimSpace(n.Text); t != "" {
		parts = append(parts, short(t, 120))
	}
	for _, k := range n.Kids {
		if len(parts) >= 6 {
			break
		}
		if t := strings.TrimSpace(k.Text); t != "" && k.Kind != "c" {
			if k.Kind == "e" {
				t = k.Name + ": " + short(t, 40)
			}
			parts = append(parts, short(t, 60))
		}
	}
	if len(parts) == 0 {
		for _, k := range n.Kids {
			if k.Kind == "e" && len(parts) < 6 {
				parts = append(parts, k.Name+" ("+strconv.Itoa(k.Count)+")")
			}
		}
	}
	return RowSummary{Index: r, Start: n.Start, Name: n.Name, Preview: strings.Join(parts, " · ")}
}

func short(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	rs := []rune(s)
	return string(rs[:max-1]) + "…"
}
