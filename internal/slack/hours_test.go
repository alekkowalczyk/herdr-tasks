package slack

import (
	"testing"
	"time"
)

func mustHours(t *testing.T, s string) Hours {
	t.Helper()
	h, err := ParseHours(s)
	if err != nil {
		t.Fatalf("ParseHours(%q): %v", s, err)
	}
	return h
}

// 2026-10-26 is a Monday.
func at(day, hour, min int) time.Time {
	return time.Date(2026, 10, 26+day, hour, min, 0, 0, time.Local)
}

func TestHoursWorkWeek(t *testing.T) {
	h := mustHours(t, "Mon-Fri 08:00-18:00")
	open := []struct {
		t    time.Time
		want bool
	}{
		{at(0, 7, 59), false}, {at(0, 8, 0), true}, {at(4, 17, 59), true},
		{at(4, 18, 0), false}, {at(5, 12, 0), false},
	}
	for _, c := range open {
		if got := h.Open(c.t); got != c.want {
			t.Errorf("Open(%v) = %v, want %v", c.t, got, c.want)
		}
	}
	if got := h.Next(at(4, 18, 0)); !got.Equal(at(7, 8, 0)) {
		t.Errorf("Next(Fri 18:00) = %v, want Mon 08:00", got)
	}
	if got := h.Next(at(1, 9, 0)); !got.Equal(at(1, 9, 0)) {
		t.Errorf("Next(Tue 09:00) = %v, want itself", got)
	}
	if got := h.Next(at(0, 7, 0)); !got.Equal(at(0, 8, 0)) {
		t.Errorf("Next(Mon 07:00) = %v, want Mon 08:00", got)
	}
}

func TestHoursAlwaysAndLists(t *testing.T) {
	a := mustHours(t, "always")
	for _, tm := range []time.Time{at(0, 0, 0), at(5, 3, 0), at(6, 23, 59)} {
		if !a.Open(tm) {
			t.Errorf("always should be open at %v", tm)
		}
	}
	l := mustHours(t, "Mon,Wed 09:00-12:00")
	if !l.Open(at(2, 10, 0)) || l.Open(at(1, 10, 0)) {
		t.Error("Mon,Wed list: want open Wed 10:00, shut Tue 10:00")
	}
	w := mustHours(t, "sat-MON 10:00-24:00")
	if !w.Open(at(6, 23, 59)) || w.Open(at(1, 12, 0)) {
		t.Error("Sat-Mon wrap: want open Sun 23:59, shut Tue")
	}
}

func TestHoursErrors(t *testing.T) {
	for _, s := range []string{"", "Mon-Fri", "Mon-Fri 18:00-08:00", "Mon-Fry 08:00-18:00", "Mon-Fri 8-18"} {
		if _, err := ParseHours(s); err == nil {
			t.Errorf("ParseHours(%q) should fail", s)
		}
	}
}
