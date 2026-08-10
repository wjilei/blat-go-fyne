package fyneui

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "00:00.000"},
		{"sub-second-ms-only", 7 * time.Millisecond, "00:00.007"},
		{"leading-zero-ms", 600 * time.Millisecond, "00:00.600"},
		{"sub-second-round-ms", 999 * time.Millisecond, "00:00.999"},
		{"one-second", time.Second, "00:01.000"},
		{"with-ms", 1234 * time.Millisecond, "00:01.234"},
		{"minute-boundary", 60 * time.Second, "01:00.000"},
		{"minute-plus", 65*time.Second + 123*time.Millisecond, "01:05.123"},
		{"negative-clamped", -5 * time.Second, "00:00.000"},
		{"long-but-fine", 99*time.Minute + 59*time.Second + 999*time.Millisecond, "99:59.999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatDuration(c.in)
			if got != c.want {
				t.Fatalf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
