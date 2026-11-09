// Package xmlview reads very large XML files lazily: it never loads a whole
// file into memory, but streams through it with a small tokenizer and keeps
// only byte offsets so that any part can be re-read later on demand.
package xmlview

import (
	"bytes"
	"errors"
	"io"
)

// Kind is the type of a token produced by Scanner.
type Kind uint8

const (
	KEOF     Kind = iota
	KText         // character data between tags
	KStart        // <name ...>
	KEmpty        // <name .../>
	KEnd          // </name>
	KCData        // <![CDATA[ ... ]]>
	KComment      // <!-- ... -->
	KPI           // <? ... ?>
	KDecl         // <!DOCTYPE ...> and other <!...> declarations
)

// Token is one lexical XML token. Name and Data point into the scanner's
// buffers and are only valid until the next call to Next.
type Token struct {
	Kind  Kind
	Start int64  // file offset of the first byte of the token
	End   int64  // file offset just past the token
	Name  []byte // element name (KStart, KEmpty, KEnd)
	Data  []byte // KStart/KEmpty: the whole tag; KText/KCData/KComment: content, cut to the capture limit
	Trunc bool   // Data was cut to the capture limit
	Space bool   // KText/KCData/KComment: content is whitespace only
}

var ErrTagTooLarge = errors.New("an XML tag is larger than 64 MB")

const maxBuf = 64 << 20

var (
	termComment = []byte("-->")
	termCData   = []byte("]]>")
	termPI      = []byte("?>")
	openComment = []byte("<!--")
	openCData   = []byte("<![CDATA[")
)

// Scanner is a minimal, fast XML tokenizer reading from an io.ReaderAt. It
// does not validate the document; it only finds token boundaries, which is
// all that is needed to navigate a file.
type Scanner struct {
	r       io.ReaderAt
	buf     []byte
	base    int64 // file offset of buf[0]
	pos, n  int   // read position and number of valid bytes in buf
	eof     bool
	err     error
	capture int // max bytes of text content kept in Token.Data
	text    []byte
}

func NewScanner(r io.ReaderAt, bufSize int) *Scanner {
	return &Scanner{r: r, buf: make([]byte, bufSize)}
}

// Err returns the first read error that was not io.EOF.
func (s *Scanner) Err() error { return s.err }

// Offset returns the file offset of the next unread byte.
func (s *Scanner) Offset() int64 { return s.base + int64(s.pos) }

// SetCapture sets how many bytes of text content tokens carry in Data and
// returns the previous setting. Zero makes skipping content cheaper.
func (s *Scanner) SetCapture(n int) int {
	prev := s.capture
	s.capture = n
	return prev
}

// SeekTo moves the scanner to an absolute file offset.
func (s *Scanner) SeekTo(off int64) {
	if off >= s.base && off <= s.base+int64(s.n) {
		s.pos = int(off - s.base)
		return
	}
	s.base, s.pos, s.n, s.eof = off, 0, 0, false
}

// fill reads more data while keeping buf[pos:n]. It reports whether any new
// bytes were added.
func (s *Scanner) fill() bool {
	if s.eof {
		return false
	}
	if s.pos > 0 {
		copy(s.buf, s.buf[s.pos:s.n])
		s.base += int64(s.pos)
		s.n -= s.pos
		s.pos = 0
	}
	if s.n == len(s.buf) {
		if len(s.buf) >= maxBuf {
			s.err, s.eof = ErrTagTooLarge, true
			return false
		}
		nb := make([]byte, 2*len(s.buf))
		copy(nb, s.buf[:s.n])
		s.buf = nb
	}
	m, err := s.r.ReadAt(s.buf[s.n:], s.base+int64(s.n))
	s.n += m
	if err != nil {
		s.eof = true
		if err != io.EOF {
			s.err = err
		}
	}
	return m > 0
}

// Next returns the next token.
func (s *Scanner) Next() Token {
	if s.pos >= s.n && !s.fill() {
		return Token{Kind: KEOF, Start: s.Offset(), End: s.Offset()}
	}
	if s.buf[s.pos] != '<' {
		return s.scanText()
	}
	for s.n-s.pos < len(openCData) && s.fill() {
	}
	start := s.Offset()
	b := s.buf[s.pos:s.n]
	if len(b) < 2 {
		s.pos = s.n
		return Token{Kind: KText, Start: start, End: s.Offset()}
	}
	switch b[1] {
	case '/':
		return s.scanEndTag(start)
	case '?':
		return s.scanDelimited(KPI, start, 2, termPI)
	case '!':
		if bytes.HasPrefix(b, openComment) {
			return s.scanDelimited(KComment, start, len(openComment), termComment)
		}
		if bytes.HasPrefix(b, openCData) {
			return s.scanDelimited(KCData, start, len(openCData), termCData)
		}
		return s.scanDecl(start)
	}
	return s.scanStartTag(start)
}

// keep appends chunk to the capture buffer and tracks whitespace.
func (s *Scanner) keep(t *Token, chunk []byte) {
	if t.Space && !isSpace(chunk) {
		t.Space = false
	}
	room := s.capture - len(s.text)
	if len(chunk) > room {
		t.Trunc = true
		if room <= 0 {
			return
		}
		chunk = chunk[:room]
	}
	s.text = append(s.text, chunk...)
}

func (s *Scanner) scanText() Token {
	t := Token{Kind: KText, Start: s.Offset(), Space: true}
	s.text = s.text[:0]
	for {
		chunk := s.buf[s.pos:s.n]
		i := bytes.IndexByte(chunk, '<')
		if i >= 0 {
			chunk = chunk[:i]
		}
		s.keep(&t, chunk)
		s.pos += len(chunk)
		if i >= 0 || !s.fill() {
			break
		}
	}
	t.End, t.Data = s.Offset(), s.text
	return t
}

// scanDelimited reads a construct such as a comment or CDATA section that
// ends with term. The content may be far larger than the buffer.
func (s *Scanner) scanDelimited(kind Kind, start int64, open int, term []byte) Token {
	t := Token{Kind: kind, Start: start, Space: true}
	s.text = s.text[:0]
	s.pos += open
	hold := len(term) - 1 // bytes that may be the start of a split terminator
	for {
		chunk := s.buf[s.pos:s.n]
		if i := bytes.Index(chunk, term); i >= 0 {
			s.keep(&t, chunk[:i])
			s.pos += i + len(term)
			break
		}
		if len(chunk) > hold {
			c := chunk[:len(chunk)-hold]
			s.keep(&t, c)
			s.pos += len(c)
		}
		if !s.fill() { // unterminated at end of file
			s.keep(&t, s.buf[s.pos:s.n])
			s.pos = s.n
			break
		}
	}
	t.End, t.Data = s.Offset(), s.text
	return t
}

func (s *Scanner) scanDecl(start int64) Token {
	s.pos += 2
	depth := 0
	var q byte
	for {
		for s.pos < s.n {
			c := s.buf[s.pos]
			s.pos++
			if q != 0 {
				if c == q {
					q = 0
				}
				continue
			}
			switch c {
			case '"', '\'':
				q = c
			case '[':
				depth++
			case ']':
				depth--
			case '>':
				if depth <= 0 {
					return Token{Kind: KDecl, Start: start, End: s.Offset()}
				}
			}
		}
		if !s.fill() {
			return Token{Kind: KDecl, Start: start, End: s.Offset()}
		}
	}
}

// tagEnd returns the index in buf of the '>' closing the tag that starts at
// buf[s.pos], ignoring '>' inside quoted attribute values, or -1 if the tag
// is not complete in the buffer.
func (s *Scanner) tagEnd(quoted bool) int {
	b := s.buf[s.pos:s.n]
	j := bytes.IndexByte(b, '>')
	if j < 0 {
		return -1
	}
	if !quoted || (bytes.IndexByte(b[:j], '"') < 0 && bytes.IndexByte(b[:j], '\'') < 0) {
		return s.pos + j
	}
	var q byte
	for i, c := range b {
		if q != 0 {
			if c == q {
				q = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			q = c
		case '>':
			return s.pos + i
		}
	}
	return -1
}

func (s *Scanner) readTag(quoted bool) (tag []byte, ok bool) {
	for {
		if i := s.tagEnd(quoted); i >= 0 {
			tag = s.buf[s.pos : i+1]
			s.pos = i + 1
			return tag, true
		}
		if !s.fill() {
			s.pos = s.n
			return nil, false
		}
	}
}

func (s *Scanner) scanStartTag(start int64) Token {
	tag, ok := s.readTag(true)
	if !ok {
		return Token{Kind: KText, Start: start, End: s.Offset()}
	}
	t := Token{Kind: KStart, Start: start, End: s.Offset(), Data: tag, Name: tagName(tag[1:])}
	if len(tag) >= 3 && tag[len(tag)-2] == '/' {
		t.Kind = KEmpty
	}
	return t
}

func (s *Scanner) scanEndTag(start int64) Token {
	tag, ok := s.readTag(false)
	if !ok {
		return Token{Kind: KText, Start: start, End: s.Offset()}
	}
	return Token{Kind: KEnd, Start: start, End: s.Offset(), Data: tag, Name: tagName(tag[2:])}
}

func tagName(b []byte) []byte {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r', '/', '>':
			return b[:i]
		}
	}
	return b
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isSpace(b []byte) bool {
	for _, c := range b {
		if !isWS(c) {
			return false
		}
	}
	return true
}
