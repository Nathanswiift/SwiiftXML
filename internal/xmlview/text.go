package xmlview

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// charset converts a single-byte encoding (ISO-8859-1 / Windows-1252) to
// UTF-8. A nil *charset means the file is already UTF-8.
type charset struct {
	name string
	dec  [256]rune
	enc  map[rune]byte
}

// cp1252 maps the bytes 0x80-0x9F, where Windows-1252 differs from
// ISO-8859-1. Browsers decode ISO-8859-1 the same way.
var cp1252 = [32]rune{
	0x20AC, 0x81, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8D, 0x017D, 0x8F,
	0x90, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x9D, 0x017E, 0x0178,
}

func newWindows1252(name string) *charset {
	c := &charset{name: name, enc: make(map[rune]byte, 256)}
	for i := range c.dec {
		r := rune(i)
		if i >= 0x80 && i < 0xA0 {
			r = cp1252[i-0x80]
		}
		c.dec[i] = r
		c.enc[r] = byte(i)
	}
	return c
}

func (c *charset) toUTF8(dst, src []byte) []byte {
	for _, b := range src {
		if b < utf8.RuneSelf {
			dst = append(dst, b)
		} else {
			dst = utf8.AppendRune(dst, c.dec[b])
		}
	}
	return dst
}

func (c *charset) fromUTF8(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if b, ok := c.enc[r]; ok {
			out = append(out, b)
		} else {
			out = append(out, '?')
		}
	}
	return out
}

var encodingAttr = regexp.MustCompile(`encoding\s*=\s*["']([A-Za-z0-9._-]+)["']`)

// detectEncoding inspects the start of a file and returns the charset to use
// (nil for UTF-8), a display name and an error for unsupported encodings.
func detectEncoding(head []byte) (*charset, string, error) {
	switch {
	case bytes.HasPrefix(head, []byte{0xEF, 0xBB, 0xBF}):
		return nil, "UTF-8", nil
	case bytes.HasPrefix(head, []byte{0xFF, 0xFE}), bytes.HasPrefix(head, []byte{0xFE, 0xFF}),
		bytes.HasPrefix(head, []byte{0x3C, 0x00}), bytes.HasPrefix(head, []byte{0x00, 0x3C}):
		return nil, "UTF-16", errUTF16
	}
	name := "UTF-8"
	if bytes.HasPrefix(head, []byte("<?xml")) {
		if end := bytes.Index(head, []byte("?>")); end > 0 {
			if m := encodingAttr.FindSubmatch(head[:end]); m != nil {
				name = string(m[1])
			}
		}
	}
	switch strings.ToLower(name) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "l1", "iso-8859-15", "iso8859-15",
		"windows-1252", "cp1252", "us-ascii", "ascii":
		return newWindows1252(name), strings.ToUpper(name), nil
	case "utf-16", "utf-16le", "utf-16be", "ucs-2":
		return nil, name, errUTF16
	}
	return nil, strings.ToUpper(name), nil
}

// appendText converts raw file bytes to UTF-8, decoding XML entities when
// entities is true.
func (d *Doc) appendText(dst, raw []byte, entities bool) []byte {
	if !entities || bytes.IndexByte(raw, '&') < 0 {
		if d.cs != nil {
			return d.cs.toUTF8(dst, raw)
		}
		return append(dst, raw...)
	}
	if d.cs != nil {
		raw = d.cs.toUTF8(nil, raw)
	}
	return decodeEntities(dst, raw)
}

// str converts raw bytes (names, attribute values) to a Go string.
func (d *Doc) str(raw []byte, entities bool) string {
	if d.cs == nil && (!entities || bytes.IndexByte(raw, '&') < 0) {
		return string(raw)
	}
	return string(d.appendText(nil, raw, entities))
}

// clean makes sure a possibly cut-off text is valid UTF-8.
func clean(b []byte, trunc bool) string {
	if trunc {
		for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size > 1 {
				break
			}
			b = b[:len(b)-1]
		}
	}
	return string(b)
}

func decodeEntities(dst, src []byte) []byte {
	for {
		i := bytes.IndexByte(src, '&')
		if i < 0 {
			return append(dst, src...)
		}
		dst = append(dst, src[:i]...)
		src = src[i:]
		j := bytes.IndexByte(src, ';')
		if j < 0 || j > 12 {
			dst = append(dst, '&')
			src = src[1:]
			continue
		}
		if r, ok := entity(src[1:j]); ok {
			dst = utf8.AppendRune(dst, r)
		} else {
			dst = append(dst, src[:j+1]...)
		}
		src = src[j+1:]
	}
}

func entity(name []byte) (rune, bool) {
	switch string(name) {
	case "lt":
		return '<', true
	case "gt":
		return '>', true
	case "amp":
		return '&', true
	case "quot":
		return '"', true
	case "apos":
		return '\'', true
	}
	if len(name) > 1 && name[0] == '#' {
		var n uint64
		var err error
		if name[1] == 'x' || name[1] == 'X' {
			n, err = strconv.ParseUint(string(name[2:]), 16, 32)
		} else {
			n, err = strconv.ParseUint(string(name[1:]), 10, 32)
		}
		if err == nil && utf8.ValidRune(rune(n)) {
			return rune(n), true
		}
	}
	return 0, false
}

// Attr is a name/value pair, encoded in JSON as a two-element array.
type Attr [2]string

const maxAttrValue = 64 << 10

// parseAttrs extracts the attributes from a start tag ("<name a='1' ...>").
func (d *Doc) parseAttrs(tag []byte) []Attr {
	if len(tag) < 2 {
		return nil
	}
	b := tag[1 : len(tag)-1]
	b = bytes.TrimSuffix(b, []byte("/"))
	i := 0
	for i < len(b) && !isWS(b[i]) {
		i++
	}
	var attrs []Attr
	for {
		for i < len(b) && isWS(b[i]) {
			i++
		}
		if i >= len(b) {
			return attrs
		}
		ns := i
		for i < len(b) && b[i] != '=' && !isWS(b[i]) {
			i++
		}
		name := b[ns:i]
		for i < len(b) && isWS(b[i]) {
			i++
		}
		if i >= len(b) || b[i] != '=' {
			attrs = append(attrs, Attr{d.str(name, false), ""})
			continue
		}
		i++
		for i < len(b) && isWS(b[i]) {
			i++
		}
		var val []byte
		if i < len(b) && (b[i] == '"' || b[i] == '\'') {
			q := b[i]
			i++
			vs := i
			for i < len(b) && b[i] != q {
				i++
			}
			val = b[vs:i]
			i++
		} else {
			vs := i
			for i < len(b) && !isWS(b[i]) {
				i++
			}
			val = b[vs:i]
		}
		if len(val) > maxAttrValue {
			val = val[:maxAttrValue]
		}
		attrs = append(attrs, Attr{d.str(name, false), d.str(val, true)})
	}
}

// foldUTF8 lower-cases ASCII letters and the Latin-1 letters (such as Å, Ä,
// Ö) in UTF-8 text, in place. It is used for case-insensitive search.
func foldUTF8(b []byte) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 32
		} else if c == 0xC3 && i+1 < len(b) {
			if c2 := b[i+1]; c2 >= 0x80 && c2 <= 0x9E && c2 != 0x97 {
				b[i+1] = c2 + 0x20
			}
			i++
		}
	}
}

// foldLatin1 is foldUTF8 for single-byte encodings.
func foldLatin1(b []byte) {
	for i, c := range b {
		if 'A' <= c && c <= 'Z' || c >= 0xC0 && c <= 0xDE && c != 0xD7 {
			b[i] = c + 32
		}
	}
}
