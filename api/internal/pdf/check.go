package pdf

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	startxrefRE = regexp.MustCompile(`startxref\n(\d+)\n%%EOF\n$`)
	streamRE    = regexp.MustCompile(`<< /Length (\d+) >>\nstream\n`)
)

// Check verifies what a PDF reader relies on, for documents this package
// wrote: the header, startxref pointing at the cross-reference table,
// every entry pointing at its object, each stream's /Length matching its
// bytes, and the trailer's /Size. Tests in other packages use it on the
// documents they render.
func Check(doc []byte) error {
	if !bytes.HasPrefix(doc, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(doc, []byte("%%EOF\n")) {
		return errors.New("pdf: missing header or end-of-file marker")
	}
	m := startxrefRE.FindSubmatch(doc)
	if m == nil {
		return errors.New("pdf: no startxref")
	}
	xref, err := strconv.Atoi(string(m[1]))
	if err != nil || xref >= len(doc) || !bytes.HasPrefix(doc[xref:], []byte("xref\n0 ")) {
		return fmt.Errorf("pdf: startxref %s does not point at the xref table", m[1])
	}
	lines := strings.Split(string(doc[xref:]), "\n")
	f := strings.Fields(lines[1])
	if len(f) != 2 {
		return fmt.Errorf("pdf: xref subsection header %q", lines[1])
	}
	count, err := strconv.Atoi(f[1])
	if err != nil || len(lines) < count+2 {
		return fmt.Errorf("pdf: xref count %q", f[1])
	}
	if lines[2] != "0000000000 65535 f " {
		return fmt.Errorf("pdf: object 0 entry %q", lines[2])
	}
	for i := 1; i < count; i++ {
		e := lines[2+i]
		if len(e) != 19 || !strings.HasSuffix(e, " 00000 n ") {
			return fmt.Errorf("pdf: xref entry %d is %q; entries are 20 bytes with the newline", i, e)
		}
		off, err := strconv.Atoi(e[:10])
		if err != nil || off >= len(doc) {
			return fmt.Errorf("pdf: xref entry %d offset %q", i, e[:10])
		}
		if want := fmt.Sprintf("%d 0 obj\n", i); !bytes.HasPrefix(doc[off:], []byte(want)) {
			return fmt.Errorf("pdf: xref entry %d points at %q, want %q", i, doc[off:min(off+12, len(doc))], want)
		}
	}
	if !strings.Contains(string(doc[xref:]), fmt.Sprintf("/Size %d ", count)) {
		return fmt.Errorf("pdf: trailer /Size is not %d", count)
	}
	for _, sm := range streamRE.FindAllSubmatchIndex(doc, -1) {
		n, _ := strconv.Atoi(string(doc[sm[2]:sm[3]]))
		if start := sm[1]; start+n > len(doc) || !bytes.HasPrefix(doc[start+n:], []byte("\nendstream")) {
			return fmt.Errorf("pdf: stream at %d: /Length %d does not end at endstream", sm[1], n)
		}
	}
	return nil
}

// TextLines returns the literal strings drawn with Tj, unescaped as far as
// this package escapes them, in drawing order. Tests use it to read a
// document back.
func TextLines(doc []byte) []string {
	var out []string
	s := string(doc)
	for {
		i := strings.Index(s, ") Tj")
		if i < 0 {
			return out
		}
		j := strings.LastIndex(s[:i], " Td (")
		if j < 0 {
			return out
		}
		raw := s[j+len(" Td (") : i]
		var b strings.Builder
		for k := 0; k < len(raw); k++ {
			c := raw[k]
			if c != '\\' || k+1 >= len(raw) {
				b.WriteByte(c)
				continue
			}
			k++
			if raw[k] >= '0' && raw[k] <= '7' && k+2 < len(raw) {
				v, _ := strconv.ParseUint(raw[k:k+3], 8, 8)
				b.WriteRune(rune(v))
				k += 2
				continue
			}
			b.WriteByte(raw[k])
		}
		out = append(out, b.String())
		s = s[i+len(") Tj"):]
	}
}
