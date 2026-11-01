// Package slack holds the Slack asks settings and the rules around them.
package slack

import (
	"fmt"
	"strings"
	"time"
)

// Hours is when Slack gets checked, in local time.
type Hours struct {
	always     bool
	days       [7]bool // indexed by time.Weekday
	start, end int     // minutes after midnight; end may be 1440
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseHours reads "always" or "<days> <HH:MM>-<HH:MM>", such as
// "Mon-Fri 08:00-18:00" or "Sat-Mon,Wed 10:00-24:00".
func ParseHours(s string) (Hours, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "always") {
		return Hours{always: true}, nil
	}
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return Hours{}, fmt.Errorf(`want "always" or "Mon-Fri 08:00-18:00", got %q`, s)
	}
	var h Hours
	for _, item := range strings.Split(fields[0], ",") {
		from, to, isRange := strings.Cut(item, "-")
		a, ok := dayNames[strings.ToLower(from)]
		if !ok {
			return Hours{}, fmt.Errorf("unknown day %q", from)
		}
		b := a
		if isRange {
			if b, ok = dayNames[strings.ToLower(to)]; !ok {
				return Hours{}, fmt.Errorf("unknown day %q", to)
			}
		}
		for d := a; ; d = (d + 1) % 7 {
			h.days[d] = true
			if d == b {
				break
			}
		}
	}
	from, to, ok := strings.Cut(fields[1], "-")
	if !ok {
		return Hours{}, fmt.Errorf("want times like 08:00-18:00, got %q", fields[1])
	}
	var err error
	if h.start, err = clock(from); err != nil {
		return Hours{}, err
	}
	if h.end, err = clock(to); err != nil {
		return Hours{}, err
	}
	if h.end <= h.start {
		return Hours{}, fmt.Errorf("the end time must be after the start time, got %q", fields[1])
	}
	return h, nil
}

// clock reads HH:MM as minutes after midnight; 24:00 is the end of the day.
func clock(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("want a time like 08:00, got %q", s)
	}
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%02d:%02d", &hh, &mm); err != nil || mm > 59 || hh > 24 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("want a time like 08:00, got %q", s)
	}
	return hh*60 + mm, nil
}

// Open reports whether t falls inside the hours.
func (h Hours) Open(t time.Time) bool {
	if h.always {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	return h.days[t.Weekday()] && m >= h.start && m < h.end
}

// Next returns t when the hours are open, and the next opening otherwise.
func (h Hours) Next(t time.Time) time.Time {
	if h.Open(t) {
		return t
	}
	// Today may still open later; after that, the first open day.
	for off := 0; off <= 7; off++ {
		d := time.Date(t.Year(), t.Month(), t.Day()+off, 0, h.start, 0, 0, t.Location())
		if h.days[d.Weekday()] && d.After(t) {
			return d
		}
	}
	return t // no open day; unreachable for parsed hours
}
