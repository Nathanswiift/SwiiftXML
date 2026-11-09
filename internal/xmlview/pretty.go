package xmlview

import (
	"bytes"
	"io"
)

// XML returns the XML source of the node starting at start, cut off after
// max bytes. With pretty set, elements are put on separate, indented lines.
func (d *Doc) XML(start, end int64, max int, pretty bool) (string, bool, error) {
	if !pretty {
		if end <= start {
			end = d.End(start)
		}
		n, trunc := end-start, false
		if n > int64(max) {
			n, trunc = int64(max), true
		}
		buf := make([]byte, n)
		m, err := d.f.ReadAt(buf, start)
		if err != nil && err != io.EOF {
			return "", false, err
		}
		return clean(d.appendText(nil, buf[:m], false), trunc), trunc, nil
	}

	s := d.newScanner()
	s.SetCapture(max)
	s.SeekTo(start)
	var out []byte
	const (
		afterStart = iota // a start tag was just written
		afterText         // text was written inline after a start tag
		other
	)
	state, depth := other, 0
	line := func() {
		if len(out) > 0 {
			out = append(out, '\n')
		}
		for i := 0; i < depth; i++ {
			out = append(out, ' ', ' ')
		}
	}
	textOnly := false // the node is a text run rather than an element
	for first := true; len(out) < max; first = false {
		tok := s.Next()
		if first {
			textOnly = tok.Kind == KText || tok.Kind == KCData
		} else if textOnly && tok.Kind != KText && tok.Kind != KCData {
			break
		}
		switch tok.Kind {
		case KEOF:
			return clean(out, false), false, s.Err()
		case KStart:
			line()
			out = d.appendText(out, tok.Data, false)
			depth++
			state = afterStart
		case KEmpty:
			line()
			out = d.appendText(out, tok.Data, false)
			state = other
		case KEnd:
			depth--
			if state != afterStart && state != afterText {
				line()
			}
			out = d.appendText(out, tok.Data, false)
			state = other
		case KText, KCData:
			text := tok.Data
			if tok.Kind == KText {
				if tok.Space {
					continue
				}
				text = bytes.TrimSpace(text)
			}
			if state == afterStart {
				state = afterText
			} else {
				line()
				state = other
			}
			if tok.Kind == KCData {
				out = append(out, "<![CDATA["...)
			}
			out = d.appendText(out, text, false)
			if tok.Trunc {
				out = append(out, "…"...)
			}
			if tok.Kind == KCData {
				out = append(out, "]]>"...)
			}
		case KComment:
			line()
			out = append(out, "<!--"...)
			out = d.appendText(out, tok.Data, false)
			out = append(out, "-->"...)
			state = other
		case KPI:
			line()
			out = append(out, "<?"...)
			out = d.appendText(out, tok.Data, false)
			out = append(out, "?>"...)
			state = other
		}
		if depth <= 0 && !textOnly {
			return clean(out, false), false, nil
		}
	}
	return clean(out, true), len(out) >= max, nil
}

// Section returns a reader for the node spanning [start, end). If end is not
// known (<= start) it is looked up first.
func (d *Doc) Section(start, end int64) *io.SectionReader {
	if end <= start {
		end = d.End(start)
	}
	return io.NewSectionReader(d.f, start, end-start)
}
