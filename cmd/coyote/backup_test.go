package main

import (
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	if got, err := parseWhen("15m ago", now); err != nil || !got.Equal(now.Add(-15*time.Minute)) {
		t.Fatal(got, err)
	}
	if got, err := parseWhen("2026-11-24T14:05:00Z", now); err != nil || got.Hour() != 14 {
		t.Fatal(got, err)
	}
	got, err := parseWhen("2026-11-24 14:05", now)
	if err != nil || got.Location() != time.Local || got.Minute() != 5 {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"yesterday", "-5m ago", "2026-13-01 00:00"} {
		if _, err := parseWhen(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
