package smartrouter

import (
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"120", 120 * time.Second, true},
		{"0", 0, true},
		{" 5 ", 5 * time.Second, true},
		{"-1", 0, false},
		{"", 0, false},
		{"abc", 0, false},
		{"12.5", 0, false},
	}
	for _, test := range tests {
		got, ok := ParseRetryAfter(test.value, now)
		if got != test.want || ok != test.ok {
			t.Errorf("ParseRetryAfter(%q) = %s, %t; want %s, %t", test.value, got, ok, test.want, test.ok)
		}
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	future := now.Add(90 * time.Second)
	if got, ok := ParseRetryAfter(future.Format(http.TimeFormat), now); !ok || got != 90*time.Second {
		t.Errorf("ParseRetryAfter(future RFC1123) = %s, %t; want 90s, true", got, ok)
	}
	if got, ok := ParseRetryAfter(future.Format(time.RFC850), now); !ok || got != 90*time.Second {
		t.Errorf("ParseRetryAfter(future RFC850) = %s, %t; want 90s, true", got, ok)
	}

	past := now.Add(-time.Hour)
	if got, ok := ParseRetryAfter(past.Format(http.TimeFormat), now); !ok || got != 0 {
		t.Errorf("ParseRetryAfter(past date) = %s, %t; want 0s, true", got, ok)
	}

	if got, ok := ParseRetryAfter("Wed, 99 Oct 2015 07:28:00 GMT", now); ok || got != 0 {
		t.Errorf("ParseRetryAfter(invalid date) = %s, %t; want 0s, false", got, ok)
	}
}
