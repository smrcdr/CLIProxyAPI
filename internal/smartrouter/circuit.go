package smartrouter

import (
	"fmt"
	"reflect"
	"sync"
	"time"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

type FailureCategory string

const (
	FailureNone      FailureCategory = ""
	FailureAuth      FailureCategory = "auth"
	FailureRateLimit FailureCategory = "rate_limit"
	FailureTransient FailureCategory = "transient"
	FailureProtocol  FailureCategory = "protocol"
	FailureClient    FailureCategory = "client"
	FailureCancelled FailureCategory = "cancelled"
)

type CircuitPolicy struct {
	TransientThreshold int
	TransientWindow    time.Duration
	BaseCooldown       time.Duration
	MaxCooldown        time.Duration
	RateLimitCooldown  time.Duration
}

type AttemptResult struct {
	Success    bool
	Category   FailureCategory
	RetryAfter time.Duration
}

type CircuitStatus struct {
	State         CircuitState
	OpenUntil     time.Time
	RequiresReset bool
	ProbeReady    bool
	ProbeInFlight bool
	FailureCount  int
	CooldownLevel int
	LastCategory  FailureCategory
	LastFailureAt time.Time
	LastSuccessAt time.Time
}

type circuitEntry struct {
	state         CircuitState
	openUntil     time.Time
	requiresReset bool
	probeInFlight bool
	failures      []time.Time
	cooldownLevel int
	lastCategory  FailureCategory
	lastFailureAt time.Time
	lastSuccessAt time.Time
}

type CircuitStore struct {
	mu      sync.Mutex
	policy  CircuitPolicy
	entries map[string]*circuitEntry
}

func DefaultCircuitPolicy() CircuitPolicy {
	return CircuitPolicy{
		TransientThreshold: 3,
		TransientWindow:    30 * time.Second,
		BaseCooldown:       30 * time.Second,
		MaxCooldown:        5 * time.Minute,
		RateLimitCooldown:  time.Minute,
	}
}

func NewCircuitStore(policy CircuitPolicy) *CircuitStore {
	defaults := DefaultCircuitPolicy()
	if policy.TransientThreshold <= 0 {
		policy.TransientThreshold = defaults.TransientThreshold
	}
	if policy.TransientWindow <= 0 {
		policy.TransientWindow = defaults.TransientWindow
	}
	if policy.BaseCooldown <= 0 {
		policy.BaseCooldown = defaults.BaseCooldown
	}
	if policy.MaxCooldown <= 0 {
		policy.MaxCooldown = defaults.MaxCooldown
	}
	if policy.MaxCooldown < policy.BaseCooldown {
		policy.MaxCooldown = policy.BaseCooldown
	}
	if policy.RateLimitCooldown <= 0 {
		policy.RateLimitCooldown = defaults.RateLimitCooldown
	}
	return &CircuitStore{
		policy:  policy,
		entries: make(map[string]*circuitEntry),
	}
}

func (s *CircuitStore) CanAttempt(routeID string, now time.Time) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.entries[routeID]
	if entry == nil || entry.state == CircuitClosed {
		return true
	}
	if entry.state == CircuitHalfOpen {
		return !entry.probeInFlight
	}
	return !entry.requiresReset && !entry.openUntil.IsZero() && !entry.openUntil.After(now)
}

// TryAcquire admits normal closed-circuit traffic or reserves the sole
// half-open probe after an open circuit's cooldown has elapsed.
func (s *CircuitStore) TryAcquire(routeID string, now time.Time) (bool, bool) {
	if s == nil {
		return true, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.entries[routeID]
	if entry == nil || entry.state == CircuitClosed {
		return true, false
	}
	if entry.state == CircuitHalfOpen {
		if entry.probeInFlight {
			return false, false
		}
		entry.probeInFlight = true
		return true, true
	}
	if entry.requiresReset || entry.openUntil.IsZero() || entry.openUntil.After(now) {
		return false, false
	}
	entry.state = CircuitHalfOpen
	entry.probeInFlight = true
	return true, true
}

func (s *CircuitStore) Record(routeID string, halfOpenProbe bool, result AttemptResult, now time.Time) error {
	if s == nil {
		return nil
	}
	if routeID == "" {
		return fmt.Errorf("record circuit result: route ID is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.entry(routeID)
	if halfOpenProbe && (entry.state != CircuitHalfOpen || !entry.probeInFlight) {
		return fmt.Errorf("record circuit result for route %q: half-open probe is not active", routeID)
	}
	if result.Success {
		if entry.state != CircuitClosed && !halfOpenProbe {
			entry.lastSuccessAt = now
			return nil
		}
		s.close(entry, now)
		return nil
	}

	switch result.Category {
	case FailureAuth:
		s.openForAuth(entry, now)
	case FailureRateLimit:
		s.openForRateLimit(entry, result.RetryAfter, now)
	case FailureTransient, FailureProtocol:
		s.recordTransient(entry, halfOpenProbe, result.Category, now)
	case FailureNone, FailureClient, FailureCancelled:
		if halfOpenProbe {
			s.abandonProbe(entry, now)
		}
	default:
		return fmt.Errorf("record circuit result for route %q: unknown failure category %q", routeID, result.Category)
	}
	return nil
}

func (s *CircuitStore) Abandon(routeID string, halfOpenProbe bool, now time.Time) error {
	if s == nil || !halfOpenProbe {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.entries[routeID]
	if entry == nil || entry.state != CircuitHalfOpen || !entry.probeInFlight {
		return fmt.Errorf("abandon circuit probe for route %q: half-open probe is not active", routeID)
	}
	s.abandonProbe(entry, now)
	return nil
}

func (s *CircuitStore) Reset(routeID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.entries, routeID)
	s.mu.Unlock()
}

// Reconcile preserves runtime state only when both the route and its upstream
// are unchanged. A credential, endpoint, protocol, or enablement update gets a
// fresh circuit without disturbing unrelated routes.
func (s *CircuitStore) Reconcile(previous, next *Snapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for routeID := range s.entries {
		previousRoute, previousOK := snapshotRoute(previous, routeID)
		nextRoute, nextOK := snapshotRoute(next, routeID)
		if !previousOK || !nextOK || previousRoute != nextRoute {
			delete(s.entries, routeID)
			continue
		}
		previousUpstream, previousOK := snapshotUpstream(previous, previousRoute.UpstreamID)
		nextUpstream, nextOK := snapshotUpstream(next, nextRoute.UpstreamID)
		if !previousOK || !nextOK || !sameCircuitUpstream(previousUpstream, nextUpstream) {
			delete(s.entries, routeID)
		}
	}
}

func (s *CircuitStore) Status(routeID string, now time.Time) CircuitStatus {
	if s == nil {
		return CircuitStatus{State: CircuitClosed}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.entries[routeID]
	if entry == nil {
		return CircuitStatus{State: CircuitClosed}
	}
	s.pruneFailures(entry, now)
	return CircuitStatus{
		State:         entry.state,
		OpenUntil:     entry.openUntil,
		RequiresReset: entry.requiresReset,
		ProbeReady: entry.state == CircuitOpen &&
			!entry.requiresReset &&
			!entry.openUntil.IsZero() &&
			!entry.openUntil.After(now),
		ProbeInFlight: entry.probeInFlight,
		FailureCount:  len(entry.failures),
		CooldownLevel: entry.cooldownLevel,
		LastCategory:  entry.lastCategory,
		LastFailureAt: entry.lastFailureAt,
		LastSuccessAt: entry.lastSuccessAt,
	}
}

func snapshotRoute(snapshot *Snapshot, routeID string) (Route, bool) {
	if snapshot == nil {
		return Route{}, false
	}
	route, ok := snapshot.routesByID[routeID]
	return route, ok
}

func snapshotUpstream(snapshot *Snapshot, upstreamID string) (Upstream, bool) {
	if snapshot == nil {
		return Upstream{}, false
	}
	upstream, ok := snapshot.upstreams[upstreamID]
	return upstream, ok
}

func sameCircuitUpstream(left, right Upstream) bool {
	left.Name = ""
	right.Name = ""
	return reflect.DeepEqual(left, right)
}

func ClassifyHTTPFailure(statusCode int) FailureCategory {
	switch {
	case statusCode == 401 || statusCode == 403:
		return FailureAuth
	case statusCode == 429:
		return FailureRateLimit
	case statusCode == 408 ||
		statusCode == 500 ||
		statusCode == 502 ||
		statusCode == 503 ||
		statusCode == 504:
		return FailureTransient
	case statusCode >= 400:
		return FailureClient
	default:
		return FailureNone
	}
}

func (s *CircuitStore) entry(routeID string) *circuitEntry {
	entry := s.entries[routeID]
	if entry == nil {
		entry = &circuitEntry{state: CircuitClosed}
		s.entries[routeID] = entry
	}
	return entry
}

func (s *CircuitStore) close(entry *circuitEntry, now time.Time) {
	entry.state = CircuitClosed
	entry.openUntil = time.Time{}
	entry.requiresReset = false
	entry.probeInFlight = false
	entry.failures = nil
	entry.cooldownLevel = 0
	entry.lastCategory = FailureNone
	entry.lastSuccessAt = now
}

func (s *CircuitStore) openForAuth(entry *circuitEntry, now time.Time) {
	entry.state = CircuitOpen
	entry.openUntil = time.Time{}
	entry.requiresReset = true
	entry.probeInFlight = false
	entry.failures = nil
	entry.cooldownLevel = 0
	entry.lastCategory = FailureAuth
	entry.lastFailureAt = now
}

func (s *CircuitStore) openForRateLimit(entry *circuitEntry, retryAfter time.Duration, now time.Time) {
	if retryAfter <= 0 {
		retryAfter = s.policy.RateLimitCooldown
	}
	entry.state = CircuitOpen
	entry.openUntil = now.Add(retryAfter)
	entry.requiresReset = false
	entry.probeInFlight = false
	entry.failures = nil
	entry.cooldownLevel = 0
	entry.lastCategory = FailureRateLimit
	entry.lastFailureAt = now
}

func (s *CircuitStore) recordTransient(entry *circuitEntry, halfOpenProbe bool, category FailureCategory, now time.Time) {
	entry.lastCategory = category
	entry.lastFailureAt = now
	if halfOpenProbe {
		s.openForTransient(entry, now)
		return
	}

	s.pruneFailures(entry, now)
	entry.failures = append(entry.failures, now)
	if len(entry.failures) >= s.policy.TransientThreshold {
		s.openForTransient(entry, now)
	}
}

func (s *CircuitStore) openForTransient(entry *circuitEntry, now time.Time) {
	entry.cooldownLevel++
	cooldown := s.policy.BaseCooldown
	for level := 1; level < entry.cooldownLevel && cooldown < s.policy.MaxCooldown; level++ {
		if cooldown > s.policy.MaxCooldown/2 {
			cooldown = s.policy.MaxCooldown
			break
		}
		cooldown *= 2
	}
	if cooldown > s.policy.MaxCooldown {
		cooldown = s.policy.MaxCooldown
	}
	entry.state = CircuitOpen
	entry.openUntil = now.Add(cooldown)
	entry.requiresReset = false
	entry.probeInFlight = false
	entry.failures = nil
}

func (s *CircuitStore) abandonProbe(entry *circuitEntry, now time.Time) {
	entry.state = CircuitOpen
	entry.openUntil = now.Add(s.policy.BaseCooldown)
	entry.requiresReset = false
	entry.probeInFlight = false
}

func (s *CircuitStore) pruneFailures(entry *circuitEntry, now time.Time) {
	if len(entry.failures) == 0 {
		return
	}
	cutoff := now.Add(-s.policy.TransientWindow)
	first := 0
	for first < len(entry.failures) && entry.failures[first].Before(cutoff) {
		first++
	}
	if first == 0 {
		return
	}
	entry.failures = append([]time.Time(nil), entry.failures[first:]...)
}
