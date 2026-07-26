package smartrouter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCircuitStoreTransientLifecycle(t *testing.T) {
	store := NewCircuitStore(DefaultCircuitPolicy())
	start := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	for attempt := 0; attempt < 2; attempt++ {
		if err := store.Record("route-a", false, AttemptResult{Category: FailureTransient}, start.Add(time.Duration(attempt)*time.Second)); err != nil {
			t.Fatalf("Record(transient %d) error = %v", attempt+1, err)
		}
		if status := store.Status("route-a", start.Add(time.Duration(attempt)*time.Second)); status.State != CircuitClosed {
			t.Fatalf("state after transient %d = %q, want closed", attempt+1, status.State)
		}
	}
	thirdFailureAt := start.Add(2 * time.Second)
	if err := store.Record("route-a", false, AttemptResult{Category: FailureTransient}, thirdFailureAt); err != nil {
		t.Fatalf("Record(third transient) error = %v", err)
	}
	status := store.Status("route-a", thirdFailureAt)
	if status.State != CircuitOpen || status.CooldownLevel != 1 {
		t.Fatalf("status after threshold = %#v", status)
	}
	if store.CanAttempt("route-a", thirdFailureAt.Add(29*time.Second)) {
		t.Fatal("route became available before cooldown elapsed")
	}

	probeAt := thirdFailureAt.Add(31 * time.Second)
	acquired, probe := store.TryAcquire("route-a", probeAt)
	if !acquired || !probe {
		t.Fatalf("TryAcquire() = %t, %t; want true, true", acquired, probe)
	}
	if err := store.Record("route-a", probe, AttemptResult{Category: FailureProtocol}, probeAt); err != nil {
		t.Fatalf("Record(failed probe) error = %v", err)
	}
	status = store.Status("route-a", probeAt)
	if status.State != CircuitOpen || status.CooldownLevel != 2 {
		t.Fatalf("status after failed probe = %#v", status)
	}
	if want := probeAt.Add(time.Minute); !status.OpenUntil.Equal(want) {
		t.Fatalf("OpenUntil = %s, want %s", status.OpenUntil, want)
	}

	successAt := status.OpenUntil.Add(time.Second)
	acquired, probe = store.TryAcquire("route-a", successAt)
	if !acquired || !probe {
		t.Fatalf("TryAcquire(second probe) = %t, %t; want true, true", acquired, probe)
	}
	if err := store.Record("route-a", probe, AttemptResult{Success: true}, successAt); err != nil {
		t.Fatalf("Record(success) error = %v", err)
	}
	status = store.Status("route-a", successAt)
	if status.State != CircuitClosed || status.CooldownLevel != 0 || !status.LastSuccessAt.Equal(successAt) {
		t.Fatalf("status after success = %#v", status)
	}
}

func TestCircuitStoreAuthAndRateLimit(t *testing.T) {
	store := NewCircuitStore(DefaultCircuitPolicy())
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	if err := store.Record("auth-route", false, AttemptResult{Category: FailureAuth}, now); err != nil {
		t.Fatalf("Record(auth) error = %v", err)
	}
	authStatus := store.Status("auth-route", now.Add(24*time.Hour))
	if authStatus.State != CircuitOpen || !authStatus.RequiresReset || authStatus.ProbeReady {
		t.Fatalf("auth status = %#v", authStatus)
	}
	if store.CanAttempt("auth-route", now.Add(24*time.Hour)) {
		t.Fatal("auth circuit became available without reset")
	}
	store.Reset("auth-route")
	if !store.CanAttempt("auth-route", now) {
		t.Fatal("reset auth circuit is unavailable")
	}

	if err := store.Record("rate-route", false, AttemptResult{
		Category:   FailureRateLimit,
		RetryAfter: 90 * time.Second,
	}, now); err != nil {
		t.Fatalf("Record(rate limit) error = %v", err)
	}
	rateStatus := store.Status("rate-route", now)
	if want := now.Add(90 * time.Second); !rateStatus.OpenUntil.Equal(want) {
		t.Fatalf("rate OpenUntil = %s, want %s", rateStatus.OpenUntil, want)
	}
	if store.CanAttempt("rate-route", now.Add(89*time.Second)) {
		t.Fatal("rate-limited route became available before Retry-After")
	}
}

func TestCircuitStoreAdmitsOneHalfOpenProbeConcurrently(t *testing.T) {
	store := NewCircuitStore(DefaultCircuitPolicy())
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if err := store.Record("route-a", false, AttemptResult{
		Category:   FailureRateLimit,
		RetryAfter: time.Second,
	}, now); err != nil {
		t.Fatalf("Record(rate limit) error = %v", err)
	}

	var admitted atomic.Int32
	var probes atomic.Int32
	var wg sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, probe := store.TryAcquire("route-a", now.Add(2*time.Second))
			if ok {
				admitted.Add(1)
			}
			if probe {
				probes.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 || probes.Load() != 1 {
		t.Fatalf("admitted = %d, probes = %d; want 1, 1", admitted.Load(), probes.Load())
	}
}

func TestCircuitStoreIgnoresClientFailures(t *testing.T) {
	store := NewCircuitStore(DefaultCircuitPolicy())
	now := time.Now()
	for attempt := 0; attempt < 10; attempt++ {
		if err := store.Record("route-a", false, AttemptResult{Category: FailureClient}, now); err != nil {
			t.Fatalf("Record(client) error = %v", err)
		}
	}
	if status := store.Status("route-a", now); status.State != CircuitClosed || status.FailureCount != 0 {
		t.Fatalf("status = %#v, want untouched closed circuit", status)
	}
}

func TestCircuitStoreLateSuccessDoesNotCloseOpenCircuit(t *testing.T) {
	store := NewCircuitStore(DefaultCircuitPolicy())
	now := time.Now()
	if err := store.Record("route-a", false, AttemptResult{Category: FailureAuth}, now); err != nil {
		t.Fatalf("Record(auth) error = %v", err)
	}
	if err := store.Record("route-a", false, AttemptResult{Success: true}, now.Add(time.Second)); err != nil {
		t.Fatalf("Record(late success) error = %v", err)
	}
	status := store.Status("route-a", now.Add(time.Second))
	if status.State != CircuitOpen || !status.RequiresReset {
		t.Fatalf("late success closed auth circuit: %#v", status)
	}
}

func TestCircuitStoreReconcilePreservesOnlyUnchangedRoutes(t *testing.T) {
	previous, err := CompileSnapshot(selectorTestConfig("weighted-round-robin", 1, 1), 1)
	if err != nil {
		t.Fatalf("CompileSnapshot(previous) error = %v", err)
	}
	nextConfig := selectorTestConfig("weighted-round-robin", 1, 1)
	nextConfig.Router.Upstreams[0].BaseURL = "https://changed.example/v1"
	next, err := CompileSnapshot(nextConfig, 2)
	if err != nil {
		t.Fatalf("CompileSnapshot(next) error = %v", err)
	}

	store := NewCircuitStore(DefaultCircuitPolicy())
	now := time.Now()
	if err := store.Record("route-a", false, AttemptResult{Category: FailureAuth}, now); err != nil {
		t.Fatalf("Record(route-a) error = %v", err)
	}
	if err := store.Record("route-b", false, AttemptResult{Category: FailureAuth}, now); err != nil {
		t.Fatalf("Record(route-b) error = %v", err)
	}
	store.Reconcile(previous, next)

	if status := store.Status("route-a", now); status.State != CircuitClosed {
		t.Fatalf("changed route circuit was preserved: %#v", status)
	}
	if status := store.Status("route-b", now); status.State != CircuitOpen || !status.RequiresReset {
		t.Fatalf("unchanged route circuit was reset: %#v", status)
	}
}

func TestClassifyHTTPFailure(t *testing.T) {
	tests := map[int]FailureCategory{
		400: FailureClient,
		401: FailureAuth,
		403: FailureAuth,
		408: FailureTransient,
		429: FailureRateLimit,
		500: FailureTransient,
		501: FailureClient,
		502: FailureTransient,
		503: FailureTransient,
		504: FailureTransient,
		200: FailureNone,
	}
	for statusCode, want := range tests {
		if got := ClassifyHTTPFailure(statusCode); got != want {
			t.Errorf("ClassifyHTTPFailure(%d) = %q, want %q", statusCode, got, want)
		}
	}
}
