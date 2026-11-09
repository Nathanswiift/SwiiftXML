package xmlview

import (
	"strings"
	"testing"
	"time"
)

func TestParseNumber(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		dec  int
		ok   bool
	}{
		{"12", 12, 0, true},
		{"-3.5", -3.5, 1, true},
		{"+7", 7, 0, true},
		{"1,25", 1.25, 2, true},
		{"1 234,50", 1234.5, 2, true},
		{"1 234", 1234, 0, true},
		{"1,234.50", 1234.5, 2, true},
		{"1.234,5", 1234.5, 1, true},
		{"1,234,567", 1234567, 0, true},
		{"007", 7, 0, true},
		{".5", 0.5, 1, true},
		{"", 0, 0, false},
		{"abc", 0, 0, false},
		{"NaN", 0, 0, false},
		{"Inf", 0, 0, false},
		{"1e5", 0, 0, false},
		{"1.2.3", 0, 0, false},
		{"2026-01-01", 0, 0, false},
		{"-", 0, 0, false},
	}
	for _, c := range cases {
		got, dec, ok := ParseNumber(c.in)
		if ok != c.ok || (ok && (got != c.want || dec != c.dec)) {
			t.Errorf("ParseNumber(%q) = %v, %d, %v; want %v, %d, %v", c.in, got, dec, ok, c.want, c.dec, c.ok)
		}
	}
}

const aggDoc = `<root>
 <row><c>A</c><h>1.5</h><x><y>10</y></x></row>
 <row><c>B</c><h>2</h><h>3</h><x><y>20</y></x></row>
 <row><c>A</c><h>1,25</h></row>
 <row><h>text</h></row>
 <row><c>B</c></row>
 <row><c>A</c><h/></row>
 <other>not a row</other>
</root>`

func runAgg(t *testing.T, d *Doc, spec AggSpec) *AggResult {
	t.Helper()
	if err := d.StartAggregate(spec); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := d.AggregateStatus()
		if st.Done {
			if st.Error != "" {
				t.Fatal(st.Error)
			}
			return st.Result
		}
		if time.Now().After(deadline) {
			t.Fatal("summary did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func group(t *testing.T, r *AggResult, keys ...string) AggGroup {
	t.Helper()
	for _, g := range r.Groups {
		if strings.Join(g.Keys, "|") == strings.Join(keys, "|") {
			return g
		}
	}
	t.Fatalf("no group %q in %+v", keys, r.Groups)
	return AggGroup{}
}

func TestAggregateGrouped(t *testing.T) {
	d := openDone(t, aggDoc)
	v := View{Level: 1, Name: "row"}
	r := runAgg(t, d, AggSpec{View: v, Value: "h", Groups: []string{"c"}})
	if r.Rows != 6 || r.Missing != 1 || r.NoGroup[0] != 1 || r.Decimals != 2 || r.GroupCount != 3 {
		t.Fatalf("result %+v", r)
	}
	if r.Total.Count != 5 || r.Total.Numbers != 4 || r.Total.Sum() != 7.75 {
		t.Fatalf("total %+v sum %v", r.Total, r.Total.Sum())
	}
	a := group(t, r, "A")
	if a.Rows != 3 || a.Count != 2 || a.Numbers != 2 || a.Sum() != 2.75 || a.Min != 1.25 || a.Max != 1.5 {
		t.Fatalf("group A %+v sum %v", a, a.Sum())
	}
	b := group(t, r, "B")
	if b.Rows != 2 || b.Count != 2 || b.Sum() != 5 {
		t.Fatalf("group B %+v", b)
	}
	none := group(t, r, "")
	if none.Rows != 1 || none.Count != 1 || none.Numbers != 0 {
		t.Fatalf("group without key %+v", none)
	}
	if r.Groups[0].Keys[0] != "A" { // largest group first
		t.Fatalf("order %+v", r.Groups)
	}
}

func TestAggregateVariants(t *testing.T) {
	d := openDone(t, aggDoc)
	v := View{Level: 1, Name: "row"}
	// Nested path, no grouping.
	r := runAgg(t, d, AggSpec{View: v, Value: "x/y"})
	if r.Total.Sum() != 30 || r.Total.Count != 2 || r.Missing != 4 || r.Groups == nil && r.GroupCount != 0 {
		t.Fatalf("nested: %+v", r)
	}
	// Count rows per group.
	r = runAgg(t, d, AggSpec{View: v, Groups: []string{"c"}})
	if r.Rows != 6 || group(t, r, "A").Rows != 3 || r.Missing != 0 {
		t.Fatalf("rows per group: %+v", r)
	}
	// Grouping by the counted field gives the distribution of its values.
	r = runAgg(t, d, AggSpec{View: v, Value: "c", Groups: []string{"c"}})
	if group(t, r, "A").Count != 3 || group(t, r, "B").Count != 2 {
		t.Fatalf("distribution: %+v", r.Groups)
	}
	// Level views that include other elements see them as rows too.
	r = runAgg(t, d, AggSpec{View: View{Level: 1}, Value: "h"})
	if r.Rows != 7 || r.Missing != 2 {
		t.Fatalf("level view: %+v", r)
	}
}

func TestAggregateRealisticRecords(t *testing.T) {
	doc, _ := records(500)
	d := openDone(t, doc)
	r := runAgg(t, d, AggSpec{View: View{Level: 2, Name: "Record"}, Value: "Lines/Line", Groups: []string{"City"}})
	if r.Rows != 500 || r.GroupCount != 1 || r.Total.Count != 1000 || r.Total.Numbers != 0 {
		t.Fatalf("records: %+v", r)
	}
	if g := group(t, r, "Göteborg"); g.Rows != 500 {
		t.Fatalf("group %+v", g)
	}
}

func TestAggregateSeveralGroupFields(t *testing.T) {
	d := openDone(t, `<r>
 <row><c>A</c><e>1</e><h>1</h></row>
 <row><e>2</e><c>A</c><h>2</h></row>
 <row><c>A</c><e>1</e><h>3</h></row>
 <row><c>B</c><e>1</e><h>4</h></row>
 <row><e>1</e><h>5</h><c/></row>
 <row><h>6</h></row>
</r>`)
	v := View{Level: 1, Name: "row"}
	r := runAgg(t, d, AggSpec{View: v, Value: "h", Groups: []string{"c", "e"}})
	if r.GroupCount != 5 || r.NoGroup[0] != 1 || r.NoGroup[1] != 1 {
		t.Fatalf("result %+v", r)
	}
	for _, want := range []struct {
		keys []string
		rows int64
		sum  float64
	}{
		{[]string{"A", "1"}, 2, 4},
		{[]string{"A", "2"}, 1, 2}, // field order in the row does not matter
		{[]string{"B", "1"}, 1, 4},
		{[]string{"", "1"}, 1, 5}, // empty <c/>
		{[]string{"", ""}, 1, 6},  // neither field present
	} {
		g := group(t, r, want.keys...)
		if g.Rows != want.rows || g.Sum() != want.sum {
			t.Errorf("group %q: rows %d sum %v, want %d %v", want.keys, g.Rows, g.Sum(), want.rows, want.sum)
		}
	}
	if err := d.StartAggregate(AggSpec{View: v, Groups: []string{"a", "b", "c", "d", "e", "f"}}); err == nil {
		t.Fatal("more than MaxGroupFields fields should be refused")
	}
	if err := d.StartAggregate(AggSpec{View: v, Groups: []string{"c", ""}}); err == nil {
		t.Fatal("an empty group field should be refused")
	}
}
