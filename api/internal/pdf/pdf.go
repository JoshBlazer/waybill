// Package pdf writes small text-and-rule PDF documents with the standard
// library only (ADR-044): A4 pages, the built-in Helvetica and
// Helvetica-Bold fonts (nothing embedded), WinAnsi text. Characters outside
// Latin-1 are written as "?", so callers spell currencies as codes ("NGN"),
// not symbols.
//
// The output is uncompressed PDF 1.4: every object is plain text, and the
// cross-reference table gives each object's exact byte offset.
package pdf

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// A4 in points.
const (
	PageWidth  = 595.0
	PageHeight = 842.0
)

// Doc is a document being written.
type Doc struct {
	title string
	pages []*bytes.Buffer
}

// New starts a document with one empty page. The title goes in the
// document information dictionary.
func New(title string) *Doc {
	d := &Doc{title: title}
	d.AddPage()
	return d
}

// AddPage starts a new page; later drawing goes on it.
func (d *Doc) AddPage() { d.pages = append(d.pages, &bytes.Buffer{}) }

// Pages is the number of pages so far.
func (d *Doc) Pages() int { return len(d.pages) }

func (d *Doc) cur() *bytes.Buffer { return d.pages[len(d.pages)-1] }

// Text draws s with its baseline starting at (x, y), y measured from the
// top of the page.
func (d *Doc) Text(x, y, size float64, bold bool, s string) {
	font := "F1"
	if bold {
		font = "F2"
	}
	fmt.Fprintf(d.cur(), "BT /%s %s Tf %s %s Td (%s) Tj ET\n", font, num(size), num(x), num(PageHeight-y), escape(s))
}

// TextRight draws s ending at x.
func (d *Doc) TextRight(x, y, size float64, bold bool, s string) {
	d.Text(x-Width(s, size, bold), y, size, bold, s)
}

// Line draws a straight rule from (x1, y1) to (x2, y2).
func (d *Doc) Line(x1, y1, x2, y2, width float64) {
	fmt.Fprintf(d.cur(), "%s w %s %s m %s %s l S\n", num(width), num(x1), num(PageHeight-y1), num(x2), num(PageHeight-y2))
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// latin1 maps s to single bytes: Latin-1 characters as themselves, anything
// else as "?".
func latin1(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r == '\t':
			out = append(out, ' ')
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			// control characters: dropped
		case r < 0x100:
			out = append(out, byte(r))
		default:
			out = append(out, '?')
		}
	}
	return out
}

// escape makes s safe inside a PDF literal string.
func escape(s string) string {
	var b strings.Builder
	for _, c := range latin1(s) {
		switch c {
		case '(', ')', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			if c >= 0x80 {
				fmt.Fprintf(&b, "\\%03o", c)
				continue
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Bytes returns the finished document.
func (d *Doc) Bytes() []byte {
	var out bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	// Objects: 1 catalog, 2 page tree, 3 and 4 fonts, 5 info, then a page
	// and its content stream for each page.
	n := len(d.pages)
	kids := make([]string, n)
	for i := range n {
		kids[i] = fmt.Sprintf("%d 0 R", 6+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	obj(fmt.Sprintf("<< /Title (%s) /Producer (Waybill) >>", escape(d.title)))
	for i, p := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			num(PageWidth), num(PageHeight), 7+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", p.Len(), p.String()))
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 5 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return out.Bytes()
}

// Width is the width of s in points, from the Helvetica metrics.
func Width(s string, size float64, bold bool) float64 {
	widths := helvetica
	if bold {
		widths = helveticaBold
	}
	total := 0
	for _, c := range latin1(s) {
		if c >= 32 && c <= 126 {
			total += widths[c-32]
		} else {
			total += 556
		}
	}
	return float64(total) * size / 1000
}

// Advance widths for characters 32 to 126, from the Adobe core font
// metrics (AFM) for Helvetica and Helvetica-Bold.
var helvetica = [95]int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 278, 278, 584, 584, 584, 556,
	1015, 667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 278, 278, 278, 469, 556,
	333, 556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, 556, 556,
	556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, 334, 260, 334, 584,
}

var helveticaBold = [95]int{
	278, 333, 474, 556, 556, 889, 722, 238, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, 333, 333, 584, 584, 584, 611,
	975, 722, 722, 722, 722, 667, 611, 778, 722, 278, 556, 722, 611, 833, 722, 778,
	667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, 333, 278, 333, 584, 556,
	333, 556, 611, 556, 611, 556, 333, 611, 611, 278, 278, 556, 278, 889, 611, 611,
	611, 611, 389, 556, 333, 611, 556, 778, 556, 556, 500, 389, 280, 389, 584,
}
