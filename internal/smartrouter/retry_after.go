package smartrouter

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter parses an HTTP Retry-After header value as either
// non-negative delta seconds or an HTTP date. It returns the wait duration
// relative to now and whether the value was valid. A past HTTP date yields a
// zero duration so callers fall back to their default cooldown.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delta := when.Sub(now)
	if delta < 0 {
		delta = 0
	}
	return delta, true
}
