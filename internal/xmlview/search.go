package xmlview

import (
	"bytes"
	"context"
	"io"
)

// SearchResult is the outcome of Search.
type SearchResult struct {
	Found   bool  `json:"found"`
	Row     int64 `json:"row"`
	Offset  int64 `json:"offset"`  // file offset of the match
	Limit   int64 `json:"limit"`   // the search covered the file up to here
	Partial bool  `json:"partial"` // indexing is still running, so the rest was not searched
}

// Search looks for query in the raw XML text from file offset from onwards
// and returns the first match that lies inside a row of the view.
func (d *Doc) Search(ctx context.Context, v View, query string, from int64, matchCase bool) (SearchResult, error) {
	p, err := d.passFor(v)
	if err != nil {
		return SearchResult{}, err
	}
	limit, partial := d.Size, !p.done.Load()
	if partial {
		limit = p.scanned.Load() // read before the cursor so the row index covers it
	}
	c, err := d.cursor(v)
	if err != nil {
		return SearchResult{}, err
	}
	res := SearchResult{Limit: limit, Partial: partial}
	needle := []byte(query)
	if d.cs != nil {
		needle = d.cs.fromUTF8(query)
	}
	if len(needle) == 0 {
		return res, nil
	}
	if !matchCase {
		d.fold(needle)
	}
	for from < limit {
		m, err := d.find(ctx, needle, from, limit, !matchCase)
		if err != nil || m < 0 {
			return res, err
		}
		row, inside, next := c.rowAt(m)
		if inside {
			res.Found, res.Row, res.Offset = true, row, m
			return res, nil
		}
		if next < 0 {
			return res, nil
		}
		from = next
	}
	return res, nil
}

func (d *Doc) fold(b []byte) {
	if d.cs != nil {
		foldLatin1(b)
	} else {
		foldUTF8(b)
	}
}

// find returns the offset of the first occurrence of needle starting in
// [from, to), or -1.
func (d *Doc) find(ctx context.Context, needle []byte, from, to int64, fold bool) (int64, error) {
	buf := make([]byte, 8<<20+len(needle))
	for pos := from; pos < to; {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		n, err := d.f.ReadAt(buf, pos)
		if err != nil && err != io.EOF {
			return -1, err
		}
		hay := buf[:n]
		if fold {
			d.fold(hay)
		}
		if i := bytes.Index(hay, needle); i >= 0 {
			if pos+int64(i) < to {
				return pos + int64(i), nil
			}
			return -1, nil
		}
		if n < len(buf) {
			break
		}
		pos += int64(n - len(needle) + 1)
	}
	return -1, nil
}
