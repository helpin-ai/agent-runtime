package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var relativePeriod = regexp.MustCompile(`\b(this|last|next) (week|month|year)\b`)
var rollingPeriod = regexp.MustCompile(`\b(past|last) ([0-9]+) (days?|weeks?|months?|years?)\b`)
var namedMonth = regexp.MustCompile(`\b(january|february|march|april|may|june|july|august|september|october|november|december) (20[0-9]{2})\b`)

func relativeWindow(query string, day time.Time) *TemporalWindow {
	query = strings.ToLower(query)
	window := func(start, end time.Time) *TemporalWindow { return &TemporalWindow{start, end.Add(-time.Nanosecond)} }
	if match := rollingPeriod.FindStringSubmatch(query); match != nil {
		n, _ := strconv.Atoi(match[2])
		if n < 1 || n > 10000 {
			return nil
		}
		start := day
		switch strings.TrimSuffix(match[3], "s") {
		case "day":
			start = day.AddDate(0, 0, -n)
		case "week":
			start = day.AddDate(0, 0, -7*n)
		case "month":
			start = day.AddDate(0, -n, 0)
		case "year":
			start = day.AddDate(-n, 0, 0)
		}
		return window(start, day.AddDate(0, 0, 1))
	}
	if match := relativePeriod.FindStringSubmatch(query); match != nil {
		offset := 0
		if match[1] == "last" {
			offset = -1
		}
		if match[1] == "next" {
			offset = 1
		}
		switch match[2] {
		case "week":
			start := day.AddDate(0, 0, -(int(day.Weekday())+6)%7+7*offset)
			return window(start, start.AddDate(0, 0, 7))
		case "month":
			start := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location()).AddDate(0, offset, 0)
			return window(start, start.AddDate(0, 1, 0))
		case "year":
			start := time.Date(day.Year()+offset, 1, 1, 0, 0, 0, 0, day.Location())
			return window(start, start.AddDate(1, 0, 0))
		}
	}
	if match := namedMonth.FindStringSubmatch(query); match != nil {
		start, err := time.Parse("January 2006", strings.Title(match[1])+" "+match[2])
		if err == nil {
			return window(start, start.AddDate(0, 1, 0))
		}
	}
	return nil
}
