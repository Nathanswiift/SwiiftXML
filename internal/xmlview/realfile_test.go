package xmlview

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealFile indexes the file named by SWIIFTXML_FILE and reports timings.
// It prints structure only, never content.
func TestRealFile(t *testing.T) {
	path := os.Getenv("SWIIFTXML_FILE")
	if path == "" {
		t.Skip("set SWIIFTXML_FILE to run")
	}
	start := time.Now()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	waitPassFor(t, d.levels, 10*time.Minute)
	st := d.Status("")
	t.Logf("indexed %d MB in %v (%.0f MB/s), encoding %s, root <%s>, auto view %s",
		d.Size>>20, time.Since(start), float64(d.Size)/1e6/time.Since(start).Seconds(), st.Encoding, st.Root, st.Auto)
	for _, l := range st.Levels {
		t.Logf("  level %d: %d elements, mostly <%s>", l.Level, l.Count, l.Name)
	}
	v, _ := ParseView(st.Auto)
	total, _ := d.RowCount(v)
	for _, r := range []int64{0, 1, total / 2, total - 1} {
		t0 := time.Now()
		n, _, err := d.Row(v, r)
		if err != nil {
			t.Fatalf("row %d: %v", r, err)
		}
		t.Logf("  row %d: <%s> %d children, %d bytes, loaded in %v", r, n.Name, n.Count, n.End-n.Start, time.Since(t0))
	}
	t0 := time.Now()
	rows, _, err := d.Rows(v, total/3, 100)
	t.Logf("  100 row summaries in %v (err %v, got %d)", time.Since(t0), err, len(rows))
	t0 = time.Now()
	res, err := d.Search(context.Background(), v, "zzzz-not-there-zzzz", 0, false)
	t.Logf("  full search in %v: %+v %v", time.Since(t0), res, err)
}

func waitPassFor(t *testing.T, p *pass, max time.Duration) {
	deadline := time.Now().Add(max)
	for !p.done.Load() {
		if time.Now().After(deadline) {
			t.Fatal("indexing did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRealFileAggregate times a summary over the file named by SWIIFTXML_FILE
// (SWIIFTXML_VALUE names the field, SWIIFTXML_GROUP the comma-separated group
// fields). It prints totals only.
func TestRealFileAggregate(t *testing.T) {
	path := os.Getenv("SWIIFTXML_FILE")
	if path == "" {
		t.Skip("set SWIIFTXML_FILE to run")
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	waitPassFor(t, d.levels, 10*time.Minute)
	t0 := time.Now()
	r := runAgg(t, d, AggSpec{View: d.AutoView(), Value: os.Getenv("SWIIFTXML_VALUE"), Groups: splitEnv("SWIIFTXML_GROUP")})
	t.Logf("summary: %d rows, %d groups, sum %.2f in %v", r.Rows, r.GroupCount, r.Total.Sum(), time.Since(t0))
}

func splitEnv(name string) []string {
	if v := os.Getenv(name); v != "" {
		return strings.Split(v, ",")
	}
	return nil
}
