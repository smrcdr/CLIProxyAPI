package smartapiusage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	storeVersion = 1
	retention    = 25 * time.Hour
)

// Bucket stores protocol-normalized usage totals for one UTC minute.
type Bucket struct {
	StartedAt           time.Time `json:"started_at"`
	Requests            int64     `json:"requests"`
	Failed              int64     `json:"failed"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
}

// Snapshot contains aggregate usage for a selected rolling window.
type Snapshot struct {
	From                time.Time
	To                  time.Time
	Requests            int64
	Failed              int64
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// Successful returns the number of requests without an upstream failure.
func (s Snapshot) Successful() int64 {
	successful := s.Requests - s.Failed
	if successful < 0 {
		return 0
	}
	return successful
}

// TotalTokens returns normalized prompt plus output tokens.
func (s Snapshot) TotalTokens() int64 {
	return s.InputTokens + s.OutputTokens
}

// UncachedInputTokens returns the prompt portion that was not read from cache.
func (s Snapshot) UncachedInputTokens() int64 {
	uncached := s.InputTokens - s.CacheReadTokens
	if uncached < 0 {
		return 0
	}
	return uncached
}

// CacheHitPercent returns cache reads as a share of normalized input tokens.
func (s Snapshot) CacheHitPercent() float64 {
	if s.InputTokens <= 0 {
		return 0
	}
	return float64(s.CacheReadTokens) * 100 / float64(s.InputTokens)
}

type diskState struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Buckets   []Bucket  `json:"buckets"`
}

// Store aggregates usage in minute buckets and optionally persists it to disk.
type Store struct {
	mu       sync.Mutex
	filePath string
	buckets  map[int64]*Bucket
	now      func() time.Time
}

// NewStore constructs an in-memory usage store.
func NewStore() *Store {
	return &Store{buckets: make(map[int64]*Bucket), now: time.Now}
}

var defaultStore = NewStore()

func init() {
	coreusage.RegisterNamedPlugin("smartapi-management-analytics", defaultStore)
}

// DefaultStore returns the process-wide SmartAPI analytics store.
func DefaultStore() *Store {
	return defaultStore
}

// Configure enables persistence and restores retained buckets from filePath.
func (s *Store) Configure(filePath string) error {
	if s == nil {
		return errors.New("usage analytics store is nil")
	}
	filePath = strings.TrimSpace(filePath)
	s.mu.Lock()
	defer s.mu.Unlock()
	if filePath == s.filePath && filePath != "" {
		return nil
	}

	buckets := make(map[int64]*Bucket)
	if filePath != "" {
		data, errRead := os.ReadFile(filePath)
		if errRead != nil && !errors.Is(errRead, os.ErrNotExist) {
			return errRead
		}
		if len(data) > 0 {
			var state diskState
			if errDecode := json.Unmarshal(data, &state); errDecode != nil {
				return errDecode
			}
			if state.Version != storeVersion {
				return errors.New("unsupported usage analytics file version")
			}
			for i := range state.Buckets {
				bucket := state.Buckets[i]
				bucket.StartedAt = bucket.StartedAt.UTC().Truncate(time.Minute)
				if bucket.StartedAt.IsZero() {
					continue
				}
				cloned := bucket
				buckets[bucket.StartedAt.Unix()] = &cloned
			}
		}
	}

	s.filePath = filePath
	s.buckets = buckets
	s.pruneLocked(s.now().UTC())
	return nil
}

// HandleUsage implements usage.Plugin.
func (s *Store) HandleUsage(_ context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	now := s.now().UTC()
	requestedAt := record.RequestedAt.UTC()
	if requestedAt.IsZero() || requestedAt.After(now.Add(5*time.Minute)) {
		requestedAt = now
	}
	startedAt := requestedAt.Truncate(time.Minute)

	inputTokens := nonNegative(record.Detail.InputTokens)
	outputTokens := nonNegative(record.Detail.OutputTokens)
	cacheReadTokens := nonNegative(record.Detail.CacheReadTokens)
	cacheCreationTokens := nonNegative(record.Detail.CacheCreationTokens)
	if cacheReadTokens == 0 && cacheCreationTokens == 0 {
		cacheReadTokens = nonNegative(record.Detail.CachedTokens)
	}
	if usesSeparateCacheCounters(record.Provider) {
		inputTokens += cacheReadTokens + cacheCreationTokens
	} else if inputTokens < cacheReadTokens {
		inputTokens = cacheReadTokens
	}

	s.mu.Lock()
	s.pruneLocked(now)
	key := startedAt.Unix()
	bucket := s.buckets[key]
	if bucket == nil {
		bucket = &Bucket{StartedAt: startedAt}
		s.buckets[key] = bucket
	}
	bucket.Requests++
	if record.Failed {
		bucket.Failed++
	}
	bucket.InputTokens += inputTokens
	bucket.OutputTokens += outputTokens
	bucket.CacheReadTokens += cacheReadTokens
	bucket.CacheCreationTokens += cacheCreationTokens
	errPersist := s.persistLocked(now)
	s.mu.Unlock()
	if errPersist != nil {
		log.WithError(errPersist).Warn("failed to persist SmartAPI usage analytics")
	}
}

// Snapshot aggregates retained buckets intersecting the selected rolling window.
func (s *Store) Snapshot(window time.Duration, now time.Time) Snapshot {
	if s == nil || window <= 0 {
		return Snapshot{}
	}
	now = now.UTC()
	from := now.Add(-window)
	result := Snapshot{From: from, To: now}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	for _, bucket := range s.buckets {
		if bucket.StartedAt.Before(from.Truncate(time.Minute)) || bucket.StartedAt.After(now) {
			continue
		}
		result.Requests += bucket.Requests
		result.Failed += bucket.Failed
		result.InputTokens += bucket.InputTokens
		result.OutputTokens += bucket.OutputTokens
		result.CacheReadTokens += bucket.CacheReadTokens
		result.CacheCreationTokens += bucket.CacheCreationTokens
	}
	return result
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-retention).Truncate(time.Minute)
	for key, bucket := range s.buckets {
		if bucket == nil || bucket.StartedAt.Before(cutoff) {
			delete(s.buckets, key)
		}
	}
}

func (s *Store) persistLocked(now time.Time) error {
	if s.filePath == "" {
		return nil
	}
	buckets := make([]Bucket, 0, len(s.buckets))
	for _, bucket := range s.buckets {
		if bucket != nil {
			buckets = append(buckets, *bucket)
		}
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].StartedAt.Before(buckets[j].StartedAt) })
	data, errMarshal := json.Marshal(diskState{Version: storeVersion, UpdatedAt: now, Buckets: buckets})
	if errMarshal != nil {
		return errMarshal
	}

	directory := filepath.Dir(s.filePath)
	if errMkdir := os.MkdirAll(directory, 0o700); errMkdir != nil {
		return errMkdir
	}
	temporary, errCreate := os.CreateTemp(directory, ".smartapi-usage-*")
	if errCreate != nil {
		return errCreate
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if errChmod := temporary.Chmod(0o600); errChmod != nil {
		_ = temporary.Close()
		return errChmod
	}
	if _, errWrite := temporary.Write(data); errWrite != nil {
		_ = temporary.Close()
		return errWrite
	}
	if errSync := temporary.Sync(); errSync != nil {
		_ = temporary.Close()
		return errSync
	}
	if errClose := temporary.Close(); errClose != nil {
		return errClose
	}
	return os.Rename(temporaryPath, s.filePath)
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func usesSeparateCacheCounters(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return provider == "claude" || strings.Contains(provider, "anthropic")
}
