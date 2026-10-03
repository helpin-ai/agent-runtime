package memory

// Default combined scoring ported from pinned engine/search/reranking.py.
// Observations/proof counts and alternate configured decay functions are not
// part of this text-only milestone. See upstream/LICENSE.
import (
	"math"
	"sort"
	"time"
)

func recency(f Fact, now time.Time) float64 {
	decay := func(date time.Time) float64 {
		return math.Max(0.1, math.Min(1, 1-now.Sub(date).Hours()/24/365))
	}
	if f.OccurredStart != nil && f.OccurredEnd != nil {
		start, end := *f.OccurredStart, *f.OccurredEnd
		span := end.Sub(start).Seconds()
		monthDays := time.Date(start.Year(), start.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		yearDays := time.Date(start.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Date(start.Year(), 1, 1, 0, 0, 0, 0, time.UTC)).Hours() / 24
		for _, period := range []float64{float64(monthDays) * 86400, yearDays * 86400} {
			if span > 0 && span >= period-86400 && span <= period {
				return math.Min(0.5, decay(end))
			}
		}
	}
	if f.OccurredStart != nil {
		return decay(*f.OccurredStart)
	}
	if !f.MentionedAt.IsZero() {
		return decay(f.MentionedAt)
	}
	if f.OccurredEnd != nil {
		return decay(*f.OccurredEnd)
	}
	return 0.5
}

func temporalProximity(f Fact, w *TemporalWindow) float64 {
	best := f.MentionedAt
	if f.OccurredStart != nil && f.OccurredEnd != nil {
		best = f.OccurredStart.Add(f.OccurredEnd.Sub(*f.OccurredStart) / 2)
	} else if f.OccurredStart != nil {
		best = *f.OccurredStart
	} else if f.OccurredEnd != nil {
		best = *f.OccurredEnd
	}
	if best.IsZero() {
		return 0.5
	}
	span := w.End.Sub(w.Start).Seconds()
	if span <= 0 {
		return 1
	}
	middle := w.Start.Add(w.End.Sub(w.Start) / 2)
	return 1 - math.Min(math.Abs(best.Sub(middle).Seconds())/(span/2), 1)
}

func combinedScoring(results []Result, now time.Time, window *TemporalWindow, neural bool) {
	calibrated := true
	for _, r := range results {
		if r.Score < 0 || r.Score > 1 {
			calibrated = false
		}
	}
	for i := range results {
		r := &results[i]
		base := r.Score
		if !neural {
			base = 1 - 0.9*float64(i)/float64(max(1, len(results)-1))
		} else if !calibrated {
			base = 1 / (1 + math.Exp(-base))
		}
		temporal := 0.5
		// Upstream fusion retains the first arm's retrieval object. Its temporal
		// proximity is present only when first discovered by the temporal arm.
		if window != nil && r.SourceRanks["temporal"] > 0 && r.SourceRanks["semantic"] == 0 && r.SourceRanks["bm25"] == 0 && r.SourceRanks["graph"] == 0 {
			temporal = temporalProximity(r.Fact, window)
		}
		r.Score = base * (1 + 0.2*(recency(r.Fact, now)-0.5)) * (1 + 0.2*(temporal-0.5))
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
}
