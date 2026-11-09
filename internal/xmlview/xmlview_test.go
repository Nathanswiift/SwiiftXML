package xmlview

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tricky = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE root [ <!ENTITY e "x>y"> <!ELEMENT root ANY> ]>
<!-- prolog <comment> -->
<root xmlns:a="urn:a" b='q"u>ote'>
  <a:item id="1" note="a > b">one &amp; two</a:item>
  <item id='2'><![CDATA[<not-a-tag> ]] ]>]]></item>
  <empty/><empty2 x="1" />
  <?pi some > data?>
  <mixed>hello <b>bold</b> world<!-- c --></mixed>
  <deep><x><y><z>v</z></y></x></deep>
</root>
<!-- trailing -->`

type elem struct {
	name       string
	start, end bool
}

func stdElems(t *testing.T, doc string) []elem {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.Strict = false
	var out []elem
	for {
		tok, err := dec.RawToken()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			out = append(out, elem{name: qname(tk.Name), start: true})
		case xml.EndElement:
			out = append(out, elem{name: qname(tk.Name), end: true})
		}
	}
}

func qname(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

func ourElems(doc string, bufSize int) []elem {
	s := NewScanner(strings.NewReader(doc), bufSize)
	var out []elem
	for {
		t := s.Next()
		switch t.Kind {
		case KEOF:
			return out
		case KStart:
			out = append(out, elem{name: string(t.Name), start: true})
		case KEmpty:
			out = append(out, elem{name: string(t.Name), start: true}, elem{name: string(t.Name), end: true})
		case KEnd:
			out = append(out, elem{name: string(t.Name), end: true})
		}
	}
}

func TestScannerMatchesEncodingXML(t *testing.T) {
	want := stdElems(t, tricky)
	for _, size := range []int{16, 17, 64, 4096} {
		got := ourElems(tricky, size)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("buffer %d:\n got %v\nwant %v", size, got, want)
		}
	}
}

func TestScannerOffsetsAreExact(t *testing.T) {
	s := NewScanner(strings.NewReader(tricky), 16)
	s.SetCapture(1 << 20)
	var rebuilt []byte
	prev := int64(0)
	for {
		tk := s.Next()
		if tk.Kind == KEOF {
			break
		}
		if tk.Start != prev {
			t.Fatalf("gap before token at %d (prev end %d)", tk.Start, prev)
		}
		rebuilt = append(rebuilt, tricky[tk.Start:tk.End]...)
		prev = tk.End
	}
	if string(rebuilt) != tricky {
		t.Fatal("tokens do not cover the document exactly")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.xml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func openDone(t *testing.T, content string) *Doc {
	t.Helper()
	d, err := Open(writeTemp(t, content))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	waitPass(t, d.levels)
	return d
}

func waitPass(t *testing.T, p *pass) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !p.done.Load() {
		if time.Now().After(deadline) {
			t.Fatal("indexing did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// records builds <Export><Header>..</Header><Records><Record>...</Records></Export>
// and returns the document plus the start offset of every <Record>.
func records(n int) (string, []int64) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` + "\n<Export><Header><Created>2026-01-01</Created></Header><Records>")
	var starts []int64
	for i := 0; i < n; i++ {
		starts = append(starts, int64(b.Len()))
		fmt.Fprintf(&b, `<Record id="%d"><Name>Name %d</Name><City>Göteborg</City>`, i, i)
		if i%100 == 7 {
			b.WriteString(strings.Repeat("<Pad>xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx</Pad>", 2000)) // ~100 KB rows
		}
		fmt.Fprintf(&b, "<Lines><Line>a</Line><Line>b</Line></Lines><Note><![CDATA[row %d]]></Note></Record>\n", i)
	}
	b.WriteString("</Records></Export>\n")
	return b.String(), starts
}

func TestLevelsAndAutoPick(t *testing.T) {
	doc, starts := records(1000)
	d := openDone(t, doc)
	st := d.Status("L2:Record")
	if st.Root != "Export" {
		t.Fatalf("root = %q", st.Root)
	}
	// Level 2 holds <Created> and the records; the header field is left out.
	if st.Auto != "L2:Record" || st.Rows != 1000 {
		t.Fatalf("auto view = %s with %d rows, want L2:Record (levels %+v)", st.Auto, st.Rows, st.Levels)
	}
	if n, _ := d.RowCount(View{Level: 2}); n != 1001 { // Created + 1000 records
		t.Fatalf("level 2 rows = %d", n)
	}
	rc, _ := d.cursor(View{Level: 2, Name: "Record"})
	for _, r := range []int64{0, 31, 32, 33, 107, 108, 999} {
		if tok, err := rc.locate(r); err != nil || tok.Start != starts[r] {
			t.Fatalf("Record row %d: %v at %d, want %d", r, err, tok.Start, starts[r])
		}
	}
	if r, _ := d.RowAt(View{Level: 2, Name: "Record"}, starts[500]+20); r != 500 {
		t.Fatalf("RowAt = %d, want 500", r)
	}
	// Every row can be located, in any order.
	c, _ := d.cursor(View{Level: 2})
	for _, r := range []int64{1000, 0, 1, 31, 32, 33, 500, 8, 9, 108, 999, 64} {
		tok, err := c.locate(r)
		if err != nil {
			t.Fatalf("locate %d: %v", r, err)
		}
		want := int64(0)
		if r > 0 {
			want = starts[r-1]
		} else {
			want = int64(strings.Index(doc, "<Created>"))
		}
		if tok.Start != want {
			t.Fatalf("row %d starts at %d, want %d", r, tok.Start, want)
		}
	}
}

func TestNameView(t *testing.T) {
	doc, starts := records(300)
	d := openDone(t, doc)
	d.IndexName("Record")
	waitPass(t, d.named)
	rows, total, err := d.Rows(View{Name: "Record"}, 95, 20)
	if err != nil || total != 300 || len(rows) != 20 {
		t.Fatalf("rows: %v total=%d len=%d", err, total, len(rows))
	}
	for i, r := range rows {
		if r.Start != starts[95+i] {
			t.Fatalf("row %d at %d, want %d", r.Index, r.Start, starts[95+i])
		}
		if !strings.Contains(r.Preview, fmt.Sprintf("id=%d", 95+i)) || !strings.Contains(r.Preview, "City: Göteborg") {
			t.Fatalf("preview %q", r.Preview)
		}
	}
	// Nested elements with the row name do not become rows of their own.
	d2 := openDone(t, `<r><a><a>inner</a></a><b><a/></b><a>x</a></r>`)
	d2.IndexName("a")
	waitPass(t, d2.named)
	if n, _ := d2.RowCount(View{Name: "a"}); n != 3 {
		t.Fatalf("name rows = %d, want 3", n)
	}
	// Level + name: other elements on the same level are skipped.
	doc3 := `<r><a/><b><a/></b><a>1</a><c><x/></c><a>2</a></r>`
	d3 := openDone(t, doc3)
	c, _ := d3.cursor(View{Level: 1, Name: "a"})
	if c.set.count != 3 {
		t.Fatalf("L1:a rows = %d", c.set.count)
	}
	for r, want := range []int{3, strings.Index(doc3, "<a>1"), strings.Index(doc3, "<a>2")} {
		if tok, err := c.locate(int64(r)); err != nil || tok.Start != int64(want) {
			t.Fatalf("L1:a row %d at %d, want %d (%v)", r, tok.Start, want, err)
		}
	}
	if rows, _, _ := d3.Rows(View{Level: 1, Name: "a"}, 0, 10); len(rows) != 3 || rows[2].Preview != "2" {
		t.Fatalf("L1:a summaries %+v", rows)
	}
}

func TestRowTree(t *testing.T) {
	doc, _ := records(20)
	d := openDone(t, doc)
	n, _, err := d.Row(View{Level: 2}, 8) // record 7, the big one
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "Record" || n.Attrs[0] != (Attr{"id", "7"}) {
		t.Fatalf("row node %+v", n)
	}
	if n.Kids[0].Name != "Name" || n.Kids[0].Text != "Name 7" || n.Kids[1].Text != "Göteborg" {
		t.Fatalf("leaf values: %+v %+v", n.Kids[0], n.Kids[1])
	}
	// More than kidLimit children: the list is cut off and the scan stops.
	if !n.Partial || n.End != -1 || len(n.Kids) != 1000 || n.Next == 0 {
		t.Fatalf("count=%d kids=%d next=%d partial=%v", n.Count, len(n.Kids), n.Next, n.Partial)
	}
	// Continue the cut-off child list until all children are loaded.
	all := len(n.Kids)
	for cur := n.Next; cur != 0; {
		more, err := d.Children(n.Start, cur)
		if err != nil {
			t.Fatal(err)
		}
		all += len(more.Kids)
		cur = more.Next
	}
	if all != 2004 {
		t.Fatalf("loaded %d children, want 2004", all)
	}
	last := n.Kids[len(n.Kids)-1]
	if last.Name != "Pad" {
		t.Fatalf("last loaded kid %q", last.Name)
	}
	// Mixed content and CDATA.
	d2 := openDone(t, `<r><m>hello <b>bold</b> world<!-- c --></m><c><![CDATA[a<b]]> &amp; c</c><e/></r>`)
	m, err := d2.Children(3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Kids) != 4 || m.Kids[0].Kind != "t" || m.Kids[1].Text != "bold" || m.Kids[3].Kind != "c" {
		t.Fatalf("mixed: %+v", m.Kids)
	}
	r, _, _ := d2.Row(View{Level: 1}, 1)
	if r.Text != "a<b & c" {
		t.Fatalf("cdata text %q", r.Text)
	}
	if e, _, _ := d2.Row(View{Level: 1}, 2); e.Name != "e" || !e.Loaded || e.Count != 0 {
		t.Fatalf("empty element %+v", e)
	}
}

func TestBudgetKeepsRowChildren(t *testing.T) {
	var b strings.Builder
	b.WriteString("<root><row><a>1</a><many>")
	for i := 0; i < 3000; i++ {
		b.WriteString("<item><x>1</x><y>2</y></item>")
	}
	b.WriteString("</many><last>z</last></row></root>")
	d := openDone(t, b.String())
	n, _, err := d.Row(View{Level: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n.Partial || n.End != int64(b.Len()-len("</root>")) || len(n.Kids) != 3 || n.Kids[2].Text != "z" {
		t.Fatalf("row: partial=%v end=%d kids=%d", n.Partial, n.End, len(n.Kids))
	}
	many := n.Kids[1]
	if many.Count != 3000 || many.Next == 0 || len(many.Kids) == 0 || len(many.Kids) >= 3000 {
		t.Fatalf("many: count=%d next=%d kids=%d", many.Count, many.Next, len(many.Kids))
	}
	// Children that got nothing loaded are marked as not loaded rather than
	// as loaded with a cut-off list.
	for _, k := range many.Kids {
		if k.Loaded && len(k.Kids) == 0 && k.Next != 0 {
			t.Fatalf("item at %d is loaded with no children but more to load", k.Start)
		}
	}
}

func TestLargeContainerStopsEarly(t *testing.T) {
	// A single huge row: loading it must not scan the whole element.
	var b strings.Builder
	b.WriteString("<root><big>")
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "<v>%d</v>", i)
	}
	b.WriteString("</big></root>")
	d := openDone(t, b.String())
	n, _, err := d.Row(View{Level: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Partial || n.End != -1 || len(n.Kids) != 1000 || n.Next == 0 {
		t.Fatalf("partial=%v end=%d kids=%d", n.Partial, n.End, len(n.Kids))
	}
	more, _ := d.Children(n.Start, n.Next)
	if more.Kids[0].Text != "1000" {
		t.Fatalf("continuation starts with %q", more.Kids[0].Text)
	}
	if end := d.End(n.Start); end != int64(b.Len()-len("</root>")) {
		t.Fatalf("End = %d", end)
	}
}

func TestSearch(t *testing.T) {
	doc, _ := records(500)
	d := openDone(t, doc)
	v := View{Level: 2}
	ctx := context.Background()
	res, err := d.Search(ctx, v, "name 345<", 0, false)
	if err != nil || !res.Found || res.Row != 346 {
		t.Fatalf("search: %+v %v", res, err)
	}
	if got := doc[res.Offset : res.Offset+9]; got != "Name 345<" {
		t.Fatalf("match at %q", got)
	}
	if res, _ = d.Search(ctx, v, "name 345<", 0, true); res.Found {
		t.Fatal("case-sensitive search should not match")
	}
	if res, _ = d.Search(ctx, v, "GÖTEBORG", res.Offset+1, false); !res.Found {
		t.Fatal("case-insensitive search for Ö failed")
	}
	// Matches outside of rows (here: in the root's start tag) are skipped.
	if res, _ = d.Search(ctx, v, "<Records>", 0, false); res.Found {
		t.Fatalf("match outside rows reported: %+v", res)
	}
}

func TestLatin1(t *testing.T) {
	doc := []byte("<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><r><n a=\"\xe5\">G\xf6teborg &#8364;</n></r>")
	path := filepath.Join(t.TempDir(), "l1.xml")
	os.WriteFile(path, doc, 0o600)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	waitPass(t, d.levels)
	n, _, _ := d.Row(View{Level: 1}, 0)
	if n.Text != "Göteborg €" || n.Attrs[0][1] != "å" {
		t.Fatalf("latin1: %q %q", n.Text, n.Attrs[0][1])
	}
	res, _ := d.Search(context.Background(), View{Level: 1}, "GÖTE", 0, false)
	if !res.Found {
		t.Fatal("latin1 search failed")
	}
}

func TestPretty(t *testing.T) {
	d := openDone(t, `<r><a x="1"><b>t</b><c/><d><![CDATA[x]]></d></a></r>`)
	got, trunc, err := d.XML(3, -1, 1<<20, true)
	want := "<a x=\"1\">\n  <b>t</b>\n  <c/>\n  <d><![CDATA[x]]></d>\n</a>"
	if err != nil || trunc || got != want {
		t.Fatalf("pretty:\n%s\nwant\n%s", got, want)
	}
	raw, _, _ := d.XML(3, -1, 1<<20, false)
	if raw != `<a x="1"><b>t</b><c/><d><![CDATA[x]]></d></a>` {
		t.Fatalf("raw: %s", raw)
	}
}

func TestPickLevel(t *testing.T) {
	set := func(count, max int64) rowSet { return rowSet{count: count, maxSize: max} }
	cases := []struct {
		sets []rowSet
		done bool
		want int
	}{
		{[]rowSet{set(1000, 10), set(9000, 5)}, true, 1},            // flat file
		{[]rowSet{set(1, 990), set(1000, 10)}, true, 2},             // single container
		{[]rowSet{set(2, 950), set(1003, 10)}, true, 2},             // header + records
		{[]rowSet{set(5, 150), set(50, 10)}, true, 1},               // small file, few big records
		{[]rowSet{set(3, 100), set(40, 10), set(400, 1)}, false, 2}, // still indexing
		{[]rowSet{set(1, 1000), set(1, 990), set(3, 300)}, true, 3}, // deep nesting
	}
	for i, c := range cases {
		if got := pickLevel(c.sets, c.done, 1000); got != c.want {
			t.Errorf("case %d: got %d want %d", i, got, c.want)
		}
	}
}

func BenchmarkIndexPass(b *testing.B) {
	doc, _ := records(20000)
	r := bytes.NewReader([]byte(doc))
	b.SetBytes(int64(len(doc)))
	for i := 0; i < b.N; i++ {
		s := NewScanner(r, 4<<20)
		for s.Next().Kind != KEOF {
		}
	}
}

func TestTruncatedFile(t *testing.T) {
	doc, starts := records(100)
	cut := doc[:starts[60]+30] // ends in the middle of record 60
	d := openDone(t, cut)
	v := View{Level: 2, Name: "Record"}
	if n, _ := d.RowCount(v); n != 61 {
		t.Fatalf("rows = %d, want 61", n)
	}
	n, _, err := d.Row(v, 60)
	if err != nil || n.End != int64(len(cut)) {
		t.Fatalf("last row: %v, end %d", err, n.End)
	}
	if rows, _, err := d.Rows(v, 50, 20); err != nil || len(rows) != 11 {
		t.Fatalf("rows: %v %d", err, len(rows))
	}
	if xml, _, err := d.XML(starts[60], -1, 1<<20, true); err != nil || !strings.HasPrefix(xml, `<Record id="60">`) {
		t.Fatalf("xml: %v %q", err, xml)
	}
}
