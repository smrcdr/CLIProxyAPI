package smartapiusage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestStoreAggregatesRollingWindowsAndCache(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 30, 30, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return now }

	store.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "openai-compatible-opencode-go",
		RequestedAt: now.Add(-20 * time.Minute),
		Detail: coreusage.Detail{
			InputTokens:     1000,
			OutputTokens:    80,
			CachedTokens:    750,
			CacheReadTokens: 750,
		},
	})
	store.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "openai-compatible-opencode-go",
		RequestedAt: now.Add(-2 * time.Hour),
		Failed:      true,
		Detail: coreusage.Detail{
			InputTokens:     300,
			OutputTokens:    20,
			CacheReadTokens: 100,
		},
	})

	halfHour := store.Snapshot(30*time.Minute, now)
	if halfHour.Requests != 1 || halfHour.Failed != 0 {
		t.Fatalf("30m requests = %d failed = %d", halfHour.Requests, halfHour.Failed)
	}
	if halfHour.InputTokens != 1000 || halfHour.OutputTokens != 80 || halfHour.CacheReadTokens != 750 {
		t.Fatalf("30m tokens = %#v", halfHour)
	}
	if halfHour.TotalTokens() != 1080 || halfHour.UncachedInputTokens() != 250 || halfHour.CacheHitPercent() != 75 {
		t.Fatalf("30m derived metrics = %#v", halfHour)
	}

	twelveHours := store.Snapshot(12*time.Hour, now)
	if twelveHours.Requests != 2 || twelveHours.Failed != 1 || twelveHours.Successful() != 1 {
		t.Fatalf("12h requests = %#v", twelveHours)
	}
	if twelveHours.InputTokens != 1300 || twelveHours.OutputTokens != 100 || twelveHours.CacheReadTokens != 850 {
		t.Fatalf("12h tokens = %#v", twelveHours)
	}
}

func TestStoreNormalizesAnthropicSeparateCacheCounters(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 30, 0, 0, time.UTC)
	store := NewStore()
	store.now = func() time.Time { return now }
	store.HandleUsage(context.Background(), coreusage.Record{
		Provider:    "claude",
		RequestedAt: now,
		Detail: coreusage.Detail{
			InputTokens:         100,
			OutputTokens:        20,
			CachedTokens:        700,
			CacheReadTokens:     700,
			CacheCreationTokens: 200,
		},
	})

	snapshot := store.Snapshot(time.Hour, now)
	if snapshot.InputTokens != 1000 || snapshot.CacheReadTokens != 700 || snapshot.CacheCreationTokens != 200 {
		t.Fatalf("normalized Anthropic tokens = %#v", snapshot)
	}
	if snapshot.CacheHitPercent() != 70 {
		t.Fatalf("cache hit = %f, want 70", snapshot.CacheHitPercent())
	}
}

func TestStorePersistsAndRestoresBuckets(t *testing.T) {
	now := time.Date(2026, time.July, 13, 12, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "analytics", "usage.json")
	first := NewStore()
	first.now = func() time.Time { return now }
	if err := first.Configure(path); err != nil {
		t.Fatal(err)
	}
	first.HandleUsage(context.Background(), coreusage.Record{
		RequestedAt: now.Add(-time.Minute),
		Detail: coreusage.Detail{
			InputTokens:     500,
			OutputTokens:    40,
			CacheReadTokens: 320,
		},
	})

	restored := NewStore()
	restored.now = func() time.Time { return now }
	if err := restored.Configure(path); err != nil {
		t.Fatal(err)
	}
	snapshot := restored.Snapshot(time.Hour, now)
	if snapshot.Requests != 1 || snapshot.InputTokens != 500 || snapshot.OutputTokens != 40 || snapshot.CacheReadTokens != 320 {
		t.Fatalf("restored snapshot = %#v", snapshot)
	}
}
