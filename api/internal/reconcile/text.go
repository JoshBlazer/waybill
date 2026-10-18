package reconcile

import (
	"context"
	"fmt"
	"io"
	"time"
)

// WriteText prints the report for a person.
func (r Report) WriteText(w io.Writer) error {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("Reconciliation %s\n", r.ID)
	p("Started %s, records since %s\n", r.StartedAt.UTC().Format(time.RFC3339), r.Since.UTC().Format(time.RFC3339))
	for n, h := range r.AsOf {
		p("Read %s at block %d\n", n, h)
	}
	p("\n")
	for _, c := range r.Checks {
		p("%-14s %-10s %s\n", c.ID, c.Status, c.Name)
		if c.Chain != "" || c.Ledger != "" {
			p("               chain %s, ledger %s, in flight %s, unexplained %s (minor units)\n", c.Chain, c.Ledger, c.Explained, c.Unexplained)
		}
		if c.Checked > 0 {
			p("               %d records checked\n", c.Checked)
		}
		for _, f := range c.InFlight {
			p("               in flight: %s, %s, %s\n", f.What, f.Ref, f.Amount)
		}
		for _, x := range c.Problems {
			p("               ! %s\n", x)
		}
		if c.Note != "" {
			p("               %s\n", c.Note)
		}
	}
	p("\n%d differences\n", r.Differences)
	return nil
}

// RunEvery reconciles every interval until ctx is done, logging a summary
// of each report.
func (r *Reconciler) RunEvery(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		rep, err := r.Run(ctx, Options{})
		switch {
		case err != nil && ctx.Err() == nil:
			r.Log.Warn("reconcile: run failed; will retry", "err", err)
		case err == nil && rep.Differences > 0:
			r.Log.Error("reconcile: differences found", "report", rep.ID, "differences", rep.Differences)
		case err == nil:
			r.Log.Info("reconcile: clean", "report", rep.ID, "unsettled", rep.Unsettled())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
