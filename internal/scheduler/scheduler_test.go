package scheduler

import (
	"testing"
)

func TestParseSchedule(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"08:00", []int{480}},
		{"08:00,20:00", []int{480, 1200}},
		{"  08:00 , 20:30 ", []int{480, 1230}},
		{"", nil},
		{"bogus", nil},
		{"08:00,bogus,21:00", []int{480, 1260}},
	}
	for _, c := range cases {
		got := parseSchedule(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseSchedule(%q) len=%d want %d", c.in, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseSchedule(%q)[%d]=%d want %d", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestFormatSchedule(t *testing.T) {
	if got := formatSchedule(" 08:00 , 20:00 "); got != "08:00,20:00" {
		t.Errorf("formatSchedule=%q", got)
	}
	if got := formatSchedule(""); got != "08:00" {
		t.Errorf("formatSchedule(empty)=%q", got)
	}
}
