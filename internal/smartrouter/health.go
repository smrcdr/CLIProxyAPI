package smartrouter

import (
	"reflect"
	"sync"
	"time"
)

type TransportHealthState string

const (
	TransportHealthUnknown   TransportHealthState = "unknown"
	TransportHealthHealthy   TransportHealthState = "healthy"
	TransportHealthUnhealthy TransportHealthState = "unhealthy"
)

type TransportHealthStatus struct {
	State               TransportHealthState
	ConsecutiveSuccess  int
	ConsecutiveFailures int
	LastCheckedAt       time.Time
	LastHealthyAt       time.Time
	LastUnhealthyAt     time.Time
	LastStatusCode      int
	LastCategory        FailureCategory
	LastTransportError  bool
	LastLatency         time.Duration
}

type transportHealthEntry struct {
	status TransportHealthStatus
}

// TransportHealthStore is independent from inference circuits. Successful
// health checks can restore transport availability without clearing auth or
// rate-limit failures recorded by CircuitStore.
type TransportHealthStore struct {
	mu      sync.RWMutex
	entries map[string]*transportHealthEntry
}

func NewTransportHealthStore() *TransportHealthStore {
	return &TransportHealthStore{entries: make(map[string]*transportHealthEntry)}
}

func (s *TransportHealthStore) CanRoute(upstreamID string) bool {
	if s == nil {
		return true
	}
	s.mu.RLock()
	entry := s.entries[upstreamID]
	s.mu.RUnlock()
	return entry == nil || entry.status.State != TransportHealthUnhealthy
}

func (s *TransportHealthStore) Status(upstreamID string) TransportHealthStatus {
	if s == nil {
		return TransportHealthStatus{State: TransportHealthUnknown}
	}
	s.mu.RLock()
	entry := s.entries[upstreamID]
	s.mu.RUnlock()
	if entry == nil {
		return TransportHealthStatus{State: TransportHealthUnknown}
	}
	return entry.status
}

func (s *TransportHealthStore) Record(upstream Upstream, result RouterProbeResult) TransportHealthStatus {
	if s == nil {
		return TransportHealthStatus{State: TransportHealthUnknown}
	}
	healthyThreshold := upstream.HealthCheck.HealthyThreshold
	if healthyThreshold < 1 {
		healthyThreshold = 1
	}
	unhealthyThreshold := upstream.HealthCheck.UnhealthyThreshold
	if unhealthyThreshold < 1 {
		unhealthyThreshold = 1
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[upstream.ID]
	if entry == nil {
		entry = &transportHealthEntry{
			status: TransportHealthStatus{State: TransportHealthUnknown},
		}
		s.entries[upstream.ID] = entry
	}
	status := &entry.status
	status.LastCheckedAt = result.CheckedAt
	status.LastStatusCode = result.StatusCode
	status.LastCategory = result.Category
	status.LastTransportError = result.TransportError
	status.LastLatency = result.Latency

	if result.Healthy {
		status.ConsecutiveSuccess++
		status.ConsecutiveFailures = 0
		status.LastHealthyAt = result.CheckedAt
		if status.State != TransportHealthHealthy && status.ConsecutiveSuccess >= healthyThreshold {
			status.State = TransportHealthHealthy
		}
	} else {
		status.ConsecutiveSuccess = 0
		status.ConsecutiveFailures++
		status.LastUnhealthyAt = result.CheckedAt
		if status.ConsecutiveFailures >= unhealthyThreshold {
			status.State = TransportHealthUnhealthy
		}
	}
	return *status
}

func (s *TransportHealthStore) Reset(upstreamID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.entries, upstreamID)
	s.mu.Unlock()
}

func (s *TransportHealthStore) Reconcile(previous, next *Snapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for upstreamID := range s.entries {
		previousUpstream, previousOK := snapshotUpstream(previous, upstreamID)
		nextUpstream, nextOK := snapshotUpstream(next, upstreamID)
		if !previousOK || !nextOK || !sameTransportHealthUpstream(previousUpstream, nextUpstream) {
			delete(s.entries, upstreamID)
		}
	}
}

func sameTransportHealthUpstream(left, right Upstream) bool {
	left.Name = ""
	right.Name = ""
	left.Auth = right.Auth
	left.Headers = right.Headers
	left.Capabilities = right.Capabilities
	left.TrustedPool = right.TrustedPool
	left.ForwardSmartAPIAffinity = right.ForwardSmartAPIAffinity
	return reflect.DeepEqual(left, right)
}
