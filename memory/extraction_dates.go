package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var isoWeekDate = regexp.MustCompile(`^([0-9]{4})(?:-W([0-9]{2})(?:-([1-7]))?|W([0-9]{2})([1-7])?)$`)
var isoOrdinalDate = regexp.MustCompile(`^([0-9]{4})-?([0-9]{3})$`)
var isoZone = regexp.MustCompile(`^(?:[Zz]|[+-][0-9]{2}(?:[0-9]{2}|:[0-9]{2})?)$`)

// parseExtractionDate mirrors upstream _parse_datetime/dateutil.isoparse:
// invalid model dates become unknown, never a dropped fact or guessed date.
// Incomplete calendar dates use their first day; naive timestamps use UTC.
func parseExtractionDate(value *string) *time.Time {
	if value == nil {
		return nil
	}
	text := *value
	if date, ok, _ := parseISODatePart(text); ok {
		return &date
	}
	for _, size := range []int{10, 8, 7} {
		if len(text) <= size+1 || text[size] >= 128 {
			continue
		}
		date, ok, complete := parseISODatePart(text[:size])
		if !ok || !complete {
			continue
		}
		return parseISOClock(date, text[size+1:])
	}
	return nil
}

func parseISODatePart(text string) (time.Time, bool, bool) {
	for _, layout := range []string{"2006-01-02", "20060102", "2006-01", "2006"} {
		date, err := time.Parse(layout, text)
		if err == nil && date.Year() >= 1 && date.Year() <= 9999 {
			return date, true, layout == "2006-01-02" || layout == "20060102"
		}
	}
	if match := isoOrdinalDate.FindStringSubmatch(text); match != nil {
		year, _ := strconv.Atoi(match[1])
		day, _ := strconv.Atoi(match[2])
		if year < 1 || day < 1 {
			return time.Time{}, false, false
		}
		date := time.Date(year, 1, day, 0, 0, 0, 0, time.UTC)
		if date.Year() != year {
			return time.Time{}, false, false
		}
		return date, true, true
	}
	if match := isoWeekDate.FindStringSubmatch(text); match != nil {
		year, _ := strconv.Atoi(match[1])
		weekText, dayText := match[2], match[3]
		if weekText == "" {
			weekText, dayText = match[4], match[5]
		}
		week, _ := strconv.Atoi(weekText)
		day := 1
		if dayText != "" {
			day, _ = strconv.Atoi(dayText)
		}
		if year < 1 || week < 1 || week > 53 {
			return time.Time{}, false, false
		}
		jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
		weekday := (int(jan4.Weekday()) + 6) % 7
		date := jan4.AddDate(0, 0, -weekday+(week-1)*7+day-1)
		if date.Year() < 1 || date.Year() > 9999 {
			return time.Time{}, false, false
		}
		return date, true, dayText != ""
	}
	return time.Time{}, false, false
}

func parseISOClock(date time.Time, text string) *time.Time {
	zone := time.UTC
	if index := strings.LastIndexAny(text, "Zz+-"); index >= 0 {
		tail := text[index:]
		if !isoZone.MatchString(tail) {
			return nil
		}
		text = text[:index]
		if tail != "Z" && tail != "z" {
			digits := strings.ReplaceAll(tail[1:], ":", "")
			hours, _ := strconv.Atoi(digits[:2])
			minutes := 0
			if len(digits) == 4 {
				minutes, _ = strconv.Atoi(digits[2:])
			}
			if hours > 23 || minutes > 59 {
				return nil
			}
			offset := (hours*60 + minutes) * 60
			if tail[0] == '-' {
				offset = -offset
			}
			if offset != 0 {
				zone = time.FixedZone("", offset)
			}
		}
	}
	fraction := ""
	if index := strings.IndexAny(text, ".,"); index >= 0 {
		fraction = text[index+1:]
		text = text[:index]
		if fraction == "" || !asciiDigits(fraction) {
			return nil
		}
	}
	parts := strings.Split(text, ":")
	if len(parts) == 1 {
		switch len(text) {
		case 2:
			parts = []string{text}
		case 4:
			parts = []string{text[:2], text[2:]}
		case 6:
			parts = []string{text[:2], text[2:4], text[4:]}
		default:
			return nil
		}
	}
	if len(parts) > 3 || len(parts) == 0 || (fraction != "" && len(parts) != 3) {
		return nil
	}
	values := [3]int{}
	for i, part := range parts {
		if len(part) != 2 || !asciiDigits(part) {
			return nil
		}
		values[i], _ = strconv.Atoi(part)
	}
	micros := 0
	if fraction != "" {
		if len(fraction) > 6 {
			fraction = fraction[:6]
		}
		fraction += strings.Repeat("0", 6-len(fraction))
		micros, _ = strconv.Atoi(fraction)
	}
	if values[0] > 24 || values[1] > 59 || values[2] > 59 {
		return nil
	}
	if values[0] == 24 && (values[1] != 0 || values[2] != 0 || micros != 0) {
		return nil
	}
	result := time.Date(date.Year(), date.Month(), date.Day(), values[0], values[1], values[2], micros*1000, zone)
	if result.Year() < 1 || result.Year() > 9999 {
		return nil
	}
	return &result
}
func asciiDigits(text string) bool {
	for _, c := range text {
		if c < '0' || c > '9' {
			return false
		}
	}
	return text != ""
}
