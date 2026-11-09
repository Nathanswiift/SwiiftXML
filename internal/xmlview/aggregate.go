package xmlview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AggSpec describes a summary over all rows of a view. Paths are element
// names relative to the row, separated by "/", e.g. "Duration" or
// "Customer/City". Every element at the value path is counted (and summed).
// Rows are grouped by the combination of the first element at each group
// path, e.g. ClientNumber + EmployeeNumber.
type AggSpec struct {
	View   View
	Value  string   // "" = the row element itself
	Groups []string // no groups = one total
}

// MaxGroupFields is how many fields a summary can be grouped by.
const MaxGroupFields = 5

// keySep joins the values of a group's fields into one map key. XML text
// cannot contain this control character.
const keySep = "\x1f"

// AggGroup holds the figures for one group.
type AggGroup struct {
	Keys    []string // the group's value for each group field
	Rows    int64    // rows in the group
	Count   int64    // non-empty values
	Numbers int64    // values that are numbers
	Min     float64
	Max     float64
	sum     float64
	comp    float64 // compensation term of Neumaier summation
}

func (g *AggGroup) add(x float64) {
	if g.Numbers == 0 || x < g.Min {
		g.Min = x
	}
	if g.Numbers == 0 || x > g.Max {
		g.Max = x
	}
	g.Numbers++
	t := g.sum + x
	if math.Abs(g.sum) >= math.Abs(x) {
		g.comp += (g.sum - t) + x
	} else {
		g.comp += (x - t) + g.sum
	}
	g.sum = t
}

// Sum returns the sum of the numeric values.
func (g *AggGroup) Sum() float64 { return g.sum + g.comp }

// MarshalJSON encodes a group compactly as [keys, rows, count, numbers, sum, min, max].
func (g AggGroup) MarshalJSON() ([]byte, error) {
	keys := g.Keys
	if keys == nil {
		keys = []string{}
	}
	return json.Marshal([]any{keys, g.Rows, g.Count, g.Numbers, g.Sum(), g.Min, g.Max})
}

// AggResult is the outcome of a summary.
type AggResult struct {
	Rows       int64      `json:"rows"`       // rows read
	Missing    int64      `json:"missing"`    // rows without an element at the value path
	NoGroup    []int64    `json:"noGroup"`    // per group field: rows without that element
	Decimals   int        `json:"decimals"`   // most decimals seen in a number
	Total      AggGroup   `json:"total"`      // all rows together
	Groups     []AggGroup `json:"groups"`     // largest groups first
	GroupCount int        `json:"groupCount"` // number of groups, also those not in Groups
	Cut        bool       `json:"cut"`        // there were more than maxAggGroups groups
}

// AggStatus reports on the running or finished summary.
type AggStatus struct {
	View    string     `json:"view"`
	Value   string     `json:"value"`
	Groups  []string   `json:"groups"`
	Done    bool       `json:"done"`
	Scanned int64      `json:"scanned"`
	Rows    int64      `json:"rows"`
	Seconds float64    `json:"seconds"`
	Error   string     `json:"error,omitempty"`
	Result  *AggResult `json:"result,omitempty"`
}

const (
	maxAggGroups  = 1_000_000 // groups kept in memory
	maxAggReturn  = 100_000   // groups sent to the user interface
	maxValueBytes = 1024      // longer values are cut (they are not numbers anyway)
)

var (
	errNoRows       = errors.New("there are no rows to summarize yet")
	errGroupFields  = errors.New("a summary can be grouped by at most 5 fields")
	errEmptyGroupBy = errors.New("one of the group-by fields is empty")
)

type aggJob struct {
	spec    AggSpec
	started time.Time
	elapsed atomic.Int64 // nanoseconds, set when done
	cancel  context.CancelFunc
	scanned atomic.Int64
	rows    atomic.Int64
	done    atomic.Bool

	mu     sync.Mutex
	result *AggResult
	err    error
}

// StartAggregate starts summarizing in the background, replacing any
// summary that is still running.
func (d *Doc) StartAggregate(spec AggSpec) error {
	if len(spec.Groups) > MaxGroupFields {
		return errGroupFields
	}
	for _, g := range spec.Groups {
		if g == "" {
			return errEmptyGroupBy
		}
	}
	c, err := d.cursor(spec.View)
	if err != nil {
		return err
	}
	if c.set.count == 0 {
		return errNoRows
	}
	c.s = NewScanner(d.f, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	job := &aggJob{spec: spec, cancel: cancel, started: time.Now()}
	d.mu.Lock()
	if d.agg != nil {
		d.agg.cancel()
	}
	d.agg = job
	d.mu.Unlock()
	go job.run(ctx, d, c)
	return nil
}

// StopAggregate stops a running summary.
func (d *Doc) StopAggregate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.agg != nil {
		d.agg.cancel()
		d.agg = nil
	}
}

// AggregateStatus returns the state of the current summary, or nil.
func (d *Doc) AggregateStatus() *AggStatus {
	d.mu.Lock()
	j := d.agg
	d.mu.Unlock()
	if j == nil {
		return nil
	}
	st := &AggStatus{
		View: j.spec.View.Key(), Value: j.spec.Value, Groups: j.spec.Groups,
		Done: j.done.Load(), Scanned: j.scanned.Load(), Rows: j.rows.Load(),
		Seconds: time.Since(j.started).Seconds(),
	}
	if st.Done {
		st.Seconds = time.Duration(j.elapsed.Load()).Seconds()
		j.mu.Lock()
		st.Result = j.result
		if j.err != nil {
			st.Error = j.err.Error()
		}
		j.mu.Unlock()
	}
	return st
}

func splitPath(p string) [][]byte {
	if p == "" {
		return nil
	}
	var out [][]byte
	for _, s := range strings.Split(p, "/") {
		out = append(out, []byte(s))
	}
	return out
}

// target follows one element path while a row is read.
type target struct {
	path  [][]byte
	match []bool // match[d]: the open elements down to depth d follow the path
	at    int    // depth of the element whose text is being collected, -1 if none
	text  []byte
}

func (t *target) reset() {
	t.match = append(t.match[:0], true)
	t.at = -1
}

// enter records a start tag at depth d (the row element is depth 0) and
// reports whether it is an element at the full path.
func (t *target) enter(name []byte, d int) bool {
	ok := t.match[d-1] && d <= len(t.path) && bytes.Equal(name, t.path[d-1])
	t.match = append(t.match[:d], ok)
	return ok && d == len(t.path)
}

func (t *target) begin(d int) {
	t.at = d
	t.text = t.text[:0]
}

func (t *target) collect(d *Doc, tok Token) {
	if len(t.text) < maxValueBytes {
		t.text = d.appendText(t.text, tok.Data, tok.Kind == KText)
	}
}

// aggRow holds what was found in one row.
type aggRow struct {
	found    int // elements at the value path
	nonEmpty int
	numbers  []float64
	decimals int
	has      []bool   // per group field: the row has the element
	vals     [][]byte // per group field: its value
	key      []byte   // vals joined by keySep
}

func (j *aggJob) run(ctx context.Context, d *Doc, c *cursor) {
	defer j.done.Store(true)
	v := &target{path: splitPath(j.spec.Value)}
	gs := make([]*target, len(j.spec.Groups))
	for i, p := range j.spec.Groups {
		gs[i] = &target{path: splitPath(p)}
	}
	c.s.SetCapture(maxValueBytes)
	res := &AggResult{NoGroup: make([]int64, len(gs))}
	groups := map[string]*AggGroup{}
	var other *AggGroup // collects rows once there are too many groups
	row := aggRow{has: make([]bool, len(gs)), vals: make([][]byte, len(gs))}

	tok, err := c.locate(0)
	for err == nil && (tok.Kind == KStart || tok.Kind == KEmpty) {
		readRow(d, c.s, tok, v, gs, &row)
		res.Rows++
		res.Decimals = max(res.Decimals, row.decimals)
		if row.found == 0 {
			res.Missing++
		}
		grps := []*AggGroup{&res.Total}
		if len(gs) > 0 {
			row.key = row.key[:0]
			for i, val := range row.vals {
				if !row.has[i] {
					res.NoGroup[i]++
				}
				if i > 0 {
					row.key = append(row.key, keySep...)
				}
				row.key = append(row.key, val...)
			}
			grp := groups[string(row.key)]
			if grp == nil {
				if len(groups) < maxAggGroups {
					key := string(row.key)
					grp = &AggGroup{Keys: strings.Split(key, keySep)}
					groups[key] = grp
				} else {
					res.Cut = true
					if other == nil {
						other = &AggGroup{Keys: make([]string, len(gs))}
						other.Keys[0] = "(other groups)"
					}
					grp = other
				}
			}
			grps = append(grps, grp)
		}
		for _, grp := range grps {
			grp.Rows++
			grp.Count += int64(row.nonEmpty)
			for _, x := range row.numbers {
				grp.add(x)
			}
		}
		if res.Rows%4096 == 0 {
			j.rows.Store(res.Rows)
			j.scanned.Store(c.s.Offset())
			if ctx.Err() != nil {
				return
			}
		}
		tok = c.next()
	}
	if err == nil {
		err = c.s.Err()
	}

	list := make([]AggGroup, 0, len(groups)+1)
	for _, grp := range groups {
		list = append(list, *grp)
	}
	if other != nil {
		list = append(list, *other)
	}
	sort.Slice(list, func(a, b int) bool {
		if list[a].Rows != list[b].Rows {
			return list[a].Rows > list[b].Rows
		}
		x, y := list[a].Keys, list[b].Keys
		for i := range x {
			if x[i] != y[i] {
				return x[i] < y[i]
			}
		}
		return false
	})
	res.GroupCount = len(list)
	if len(list) > maxAggReturn {
		list, res.Cut = list[:maxAggReturn], true
	}
	res.Groups = list
	j.rows.Store(res.Rows)
	j.scanned.Store(d.Size)
	j.elapsed.Store(int64(time.Since(j.started)))
	j.mu.Lock()
	j.result, j.err = res, err
	j.mu.Unlock()
}

// readRow reads the row whose start tag is tok and fills in what was found
// at the value path and at each group path.
func readRow(d *Doc, s *Scanner, tok Token, v *target, gs []*target, row *aggRow) {
	row.found, row.nonEmpty, row.numbers, row.decimals = 0, 0, row.numbers[:0], 0
	for i := range gs {
		row.has[i], row.vals[i] = false, row.vals[i][:0]
	}
	finishValue := func() {
		v.at = -1
		row.found++
		text := bytes.TrimSpace(v.text)
		if len(text) == 0 {
			return
		}
		row.nonEmpty++
		if x, dec, ok := ParseNumber(string(text)); ok {
			row.numbers = append(row.numbers, x)
			row.decimals = max(row.decimals, dec)
		}
	}
	finishGroup := func(i int) {
		gs[i].at = -1
		row.has[i] = true
		row.vals[i] = append(row.vals[i], bytes.TrimSpace(gs[i].text)...)
	}

	v.reset()
	for _, g := range gs {
		g.reset()
	}
	if len(v.path) == 0 {
		v.begin(0)
	}
	if tok.Kind == KEmpty {
		if v.at == 0 {
			finishValue()
		}
		return
	}
	depth := 0
	for {
		t := s.Next()
		switch t.Kind {
		case KStart, KEmpty:
			depth++
			if v.enter(t.Name, depth) && v.at < 0 {
				v.begin(depth)
			}
			for i, g := range gs {
				if g.enter(t.Name, depth) && g.at < 0 && !row.has[i] {
					g.begin(depth)
				}
			}
			if t.Kind == KEmpty {
				if v.at == depth {
					finishValue()
				}
				for i, g := range gs {
					if g.at == depth {
						finishGroup(i)
					}
				}
				depth--
			}
		case KText, KCData:
			if v.at == depth {
				v.collect(d, t)
			}
			for _, g := range gs {
				if g.at == depth {
					g.collect(d, t)
				}
			}
		case KEnd, KEOF:
			if v.at == depth {
				finishValue()
			}
			for i, g := range gs {
				if g.at == depth {
					finishGroup(i)
				}
			}
			if depth == 0 || t.Kind == KEOF {
				return
			}
			depth--
		}
	}
}

// ParseNumber reads a number such as 12, -3.5, 1,5 (decimal comma),
// 1 234,50 or 1,234.50. It returns the value and its number of decimals.
func ParseNumber(s string) (float64, int, bool) {
	if s == "" || len(s) > 64 {
		return 0, 0, false
	}
	if strings.ContainsAny(s, "   ") {
		s = strings.NewReplacer(" ", "", " ", "", " ", "").Replace(s)
	}
	comma, dot := strings.LastIndexByte(s, ','), strings.LastIndexByte(s, '.')
	switch {
	case comma >= 0 && dot >= 0: // the last separator is the decimal one
		if comma > dot {
			s = strings.Replace(strings.ReplaceAll(s, ".", ""), ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case comma >= 0:
		if strings.Count(s, ",") == 1 {
			s = strings.Replace(s, ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	}
	i, digits, decimals, seenDot := 0, 0, 0, false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		i++
	}
	for ; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
			if seenDot {
				decimals++
			}
		case c == '.' && !seenDot:
			seenDot = true
		default:
			return 0, 0, false
		}
	}
	if digits == 0 {
		return 0, 0, false
	}
	x, err := strconv.ParseFloat(s, 64)
	return x, decimals, err == nil
}
