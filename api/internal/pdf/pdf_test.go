package pdf_test

import (
	"strings"
	"testing"

	"github.com/JoshBlazer/waybill/api/internal/pdf"
)

func TestPDF_StructureAndText(t *testing.T) {
	d := pdf.New("Receipt (test)")
	d.Text(50, 60, 18, true, "Waybill receipt")
	d.Text(50, 90, 10, false, `Paid (in full) \ NGN 155,000.00`)
	d.TextRight(545, 90, 10, false, "400.000000 USDC")
	d.Line(50, 100, 545, 100, 0.5)
	d.AddPage()
	d.Text(50, 60, 10, false, "Page two")
	doc := d.Bytes()
	if err := pdf.Check(doc); err != nil {
		t.Fatal(err)
	}
	if got := pdf.TextLines(doc); len(got) != 4 || got[1] != `Paid (in full) \ NGN 155,000.00` {
		t.Fatalf("text read back: %q", got)
	}

	s := string(doc)
	for _, want := range []string{
		"(Waybill receipt) Tj",
		`(Paid \(in full\) \\ NGN 155,000.00) Tj`,
		"(Page two) Tj",
		"/Count 2",
		"/BaseFont /Helvetica-Bold",
		"/Title (Receipt \\(test\\))",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("document lacks %q", want)
		}
	}
	if d.Pages() != 2 {
		t.Fatalf("%d pages", d.Pages())
	}
}

// Text outside Latin-1 cannot be shown by the built-in fonts: it is
// replaced, never written as raw UTF-8 bytes; Latin-1 letters are octal
// escapes; control characters are dropped.
func TestPDF_TextEncoding(t *testing.T) {
	d := pdf.New("x")
	d.Text(0, 10, 10, false, "₦5 Adébáyọ̀\x07")
	s := string(d.Bytes())
	if !strings.Contains(s, `(?5 Ad\351b\341y??) Tj`) {
		i := strings.Index(s, "Tj")
		t.Fatalf("encoded text: %q", s[max(0, i-40):i+2])
	}
}

func TestPDF_WidthFromMetrics(t *testing.T) {
	// "0" is 556/1000 em in both fonts; at 10 pt, ten zeros are 55.6 pt.
	if w := pdf.Width("0000000000", 10, false); w != 55.6 {
		t.Fatalf("width %v", w)
	}
	if pdf.Width("W", 10, true) <= pdf.Width("i", 10, true) {
		t.Fatal("W should be wider than i")
	}
}
