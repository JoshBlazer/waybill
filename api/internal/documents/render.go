package documents

import (
	"strings"
	"time"

	"github.com/JoshBlazer/waybill/api/internal/invoice"
	"github.com/JoshBlazer/waybill/api/internal/money"
	"github.com/JoshBlazer/waybill/api/internal/pdf"
)

// Display formats an amount for people: the asset code, thousands
// separators, and at least two decimals (more only when they are not
// zero), for example "USDC 1,250.50" or "NGN 155,000.00".
func Display(a money.Amount, asset money.Asset) string {
	s := a.Format(asset.Scale)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	for len(frac) > 2 && strings.HasSuffix(frac, "0") {
		frac = frac[:len(frac)-1]
	}
	for len(frac) < 2 {
		frac += "0"
	}
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := string(asset.Code) + " " + b.String() + "." + frac
	if neg {
		return "-" + out
	}
	return out
}

const (
	left   = 50.0
	right  = pdf.PageWidth - 50
	bottom = pdf.PageHeight - 60
)

// page lays text out top to bottom, starting a new page when it runs out
// of room. Every page carries the test-money header.
type page struct {
	d     *pdf.Doc
	y     float64
	title string
}

func newPage(title string) *page {
	p := &page{d: pdf.New(title), title: title}
	p.header()
	return p
}

func (p *page) header() {
	p.d.Text(left, 50, 9, true, "WAYBILL")
	p.d.TextRight(right, 50, 9, false, "Test money only. Nothing here is real.")
	p.d.Line(left, 58, right, 58, 0.5)
	p.y = 90
}

func (p *page) room(h float64) {
	if p.y+h > bottom {
		p.d.AddPage()
		p.header()
	}
}

func (p *page) heading(s string) {
	p.room(30)
	p.d.Text(left, p.y, 18, true, s)
	p.y += 28
}

func (p *page) section(s string) {
	p.room(40)
	p.y += 8
	p.d.Text(left, p.y, 11, true, s)
	p.y += 6
	p.d.Line(left, p.y, right, p.y, 0.5)
	p.y += 16
}

// field writes a label and a value on one line; long values wrap.
func (p *page) field(label, value string) {
	for i, line := range wrap(value, 62) {
		p.room(16)
		if i == 0 {
			p.d.Text(left, p.y, 10, false, label)
		}
		p.d.Text(left+150, p.y, 10, false, line)
		p.y += 15
	}
}

// row writes text on the left and an amount on the right.
func (p *page) row(text, amount string, bold bool) {
	lines := wrap(text, 70)
	for i, line := range lines {
		p.room(16)
		p.d.Text(left, p.y, 10, bold, line)
		if i == 0 {
			p.d.TextRight(right, p.y, 10, bold, amount)
		}
		p.y += 15
	}
}

func (p *page) note(s string) {
	for _, line := range wrap(s, 95) {
		p.room(14)
		p.d.Text(left, p.y, 8, false, line)
		p.y += 12
	}
}

func (p *page) bytes() []byte { return p.d.Bytes() }

// wrap breaks s into lines of at most n characters, at spaces where it can.
func wrap(s string, n int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	cur := ""
	for _, w := range words {
		for len(w) > n { // a hash or an address: break it
			if cur != "" {
				lines, cur = append(lines, cur), ""
			}
			lines, w = append(lines, w[:n]), w[n:]
		}
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= n:
			cur += " " + w
		default:
			lines, cur = append(lines, cur), w
		}
	}
	return append(lines, cur)
}

func when(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04 UTC") }

// InvoicePDF renders an invoice.
func InvoicePDF(inv invoice.Invoice, payURL string) []byte {
	p := newPage("Invoice " + inv.TrackingCode)
	p.heading("Invoice")
	p.field("Reference", inv.TrackingCode)
	p.field("From", inv.ContractorName)
	p.field("Issued", when(inv.CreatedAt))
	p.field("Pay by", when(inv.ExpiresAt))
	p.section("For")
	p.row(inv.Description, Display(inv.Amount, inv.Asset), false)
	p.row("Total due", Display(inv.Amount, inv.Asset), true)
	if inv.Quote != nil {
		p.section("Naira")
		p.field("Locked rate", Display(inv.Quote.Rate, money.NGN)+" per "+string(inv.Asset.Code))
		p.field("Contractor receives", Display(inv.NairaAmount(), money.NGN))
		p.field("Rate holds until", when(inv.Quote.ExpiresAt))
	}
	p.section("How to pay")
	p.field("Payment page", payURL)
	for _, a := range inv.DepositAddresses {
		p.field("Send "+string(inv.Asset.Code)+" on", string(a.Network))
		p.field("To", a.Address)
	}
	p.note("Check the contractor's verified name on the payment page before paying. Send only the test token named there; other tokens are not credited.")
	return p.bytes()
}

// PDF renders the receipt.
func (r InvoiceReceipt) PDF() []byte {
	p := newPage("Receipt " + r.TrackingCode)
	p.heading("Receipt")
	p.field("Invoice", r.TrackingCode)
	p.field("Contractor", r.ContractorName)
	p.field("For", r.Description)
	p.field("Settled", when(r.SettledAt))
	p.section("Invoiced")
	p.row(r.Description, Display(r.Amount, r.Asset), false)
	if len(r.Payments) > 0 {
		p.section("Received on-chain")
		for _, x := range r.Payments {
			p.row("Final "+when(x.FinalAt)+" on "+x.Network, Display(x.Amount, r.Asset), false)
			p.note("Transaction " + x.TxHash)
		}
		p.row("Total received", Display(r.Received, r.Asset), true)
	}
	if r.Card != nil {
		p.section("Received by card or bank transfer")
		p.row("Paid "+when(r.Card.PaidAt), Display(r.Card.Amount, money.NGN), false)
		p.note("Provider reference " + r.Card.Reference)
	}
	if r.Payout != nil {
		p.section("Sent to the contractor's bank")
		if r.Payout.Rate != nil {
			p.field("Rate", Display(*r.Payout.Rate, money.NGN)+" per "+string(r.Asset.Code))
		}
		p.field("Amount", Display(r.Payout.Naira, money.NGN))
		p.field("Status", r.Payout.State)
		if r.Payout.SentAt != nil {
			p.field("Sent", when(*r.Payout.SentAt))
		}
		p.field("Transfer reference", r.Payout.Reference)
	}
	return p.bytes()
}

// PDF renders the receipt for a run.
func (r BatchReceipt) PDF() []byte {
	p := newPage("Run receipt " + r.ID.String())
	p.heading("Payment run receipt")
	p.field("Organisation", r.OrgName)
	p.field("Run", r.ID.String())
	p.field("Paid", when(r.PaidAt))
	p.field("Network", r.Network)
	p.field("Transaction", r.TxHash)
	p.field("Created by", r.CreatedBy)
	if len(r.ApprovedBy) > 0 {
		p.field("Approved by", strings.Join(r.ApprovedBy, ", "))
	}
	if r.SentBy != "" {
		p.field("Sent by", r.SentBy)
	}
	p.section("Lines")
	for _, l := range r.Lines {
		p.row(itoa(l.No)+". "+l.ContractorName+" - "+l.Reference, Display(l.Amount, r.Asset), false)
		p.note("Wallet " + l.Wallet + "   Tracking " + l.TrackingCode)
	}
	p.row("Total", Display(r.Total, r.Asset), true)
	return p.bytes()
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
