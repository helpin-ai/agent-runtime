package memory

import (
	"testing"
	"time"
)

func TestRelativeTemporalQueries(t *testing.T) {
	reference := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct{ query, start, end string }{
		{"What happened last week?", "2026-09-21", "2026-09-27"},
		{"this week", "2026-09-28", "2026-10-04"},
		{"last month", "2026-09-01", "2026-09-30"},
		{"next year", "2027-01-01", "2027-12-31"},
		{"past 7 days", "2026-09-25", "2026-10-02"},
		{"during September 2026", "2026-09-01", "2026-09-30"},
	}
	for _, c := range cases {
		w := inferWindow(c.query, reference)
		if w == nil || w.Start.Format("2006-01-02") != c.start || w.End.Format("2006-01-02") != c.end {
			t.Errorf("%s: %+v", c.query, w)
		}
	}
	if w := inferWindow("last 99999 days", reference); w != nil {
		t.Fatalf("unbounded window: %+v", w)
	}
}
