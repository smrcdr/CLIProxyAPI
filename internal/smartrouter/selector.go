package smartrouter

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var (
	ErrModelNotFound    = errors.New("router model group not found")
	ErrNoRouteAvailable = errors.New("no router route available")
	ErrSelectionClosed  = errors.New("router selection is no longer active")
)

type SelectionRequest struct {
	PublicModel string
	AffinityKey string
}

type Selection struct {
	Route            Route
	Upstream         Upstream
	SnapshotRevision uint64
	HalfOpenProbe    bool
	token            uint64
}

type Selector struct {
	snapshots *SnapshotStore
	circuits  *CircuitStore
	now       func() time.Time

	snapshotMu   sync.RWMutex
	roundRobinMu sync.Mutex
	roundRobin   map[roundRobinKey]uint64
	nextToken    atomic.Uint64
}

type roundRobinKey struct {
	groupID  string
	priority int
}

type RequestPlan struct {
	selector      *Selector
	snapshot      *Snapshot
	group         ModelGroup
	affinityKey   string
	affinityEpoch int64

	mu          sync.Mutex
	attempted   map[string]struct{}
	outstanding map[uint64]Selection
}

func NewSelector(snapshots *SnapshotStore, circuits *CircuitStore) *Selector {
	if snapshots == nil {
		snapshots = NewSnapshotStore(nil)
	}
	if circuits == nil {
		circuits = NewCircuitStore(DefaultCircuitPolicy())
	}
	return &Selector{
		snapshots:  snapshots,
		circuits:   circuits,
		now:        time.Now,
		roundRobin: make(map[roundRobinKey]uint64),
	}
}

func (s *Selector) Begin(request SelectionRequest) (*RequestPlan, error) {
	if s == nil || s.snapshots == nil {
		return nil, fmt.Errorf("%w: selector is not initialized", ErrModelNotFound)
	}
	s.snapshotMu.RLock()
	snapshot := s.snapshots.Load()
	s.snapshotMu.RUnlock()
	if snapshot == nil {
		return nil, fmt.Errorf("%w: %q", ErrModelNotFound, request.PublicModel)
	}
	group, ok := snapshot.ModelGroupForModel(request.PublicModel)
	if !ok || !group.Enabled {
		return nil, fmt.Errorf("%w: %q", ErrModelNotFound, request.PublicModel)
	}

	now := s.now()
	var affinityEpoch int64
	if group.AffinityTTL > 0 {
		affinityEpoch = now.UnixNano() / group.AffinityTTL.Nanoseconds()
	}
	return &RequestPlan{
		selector:      s,
		snapshot:      snapshot,
		group:         group,
		affinityKey:   request.AffinityKey,
		affinityEpoch: affinityEpoch,
		attempted:     make(map[string]struct{}, len(group.Routes)),
		outstanding:   make(map[uint64]Selection),
	}, nil
}

func (s *Selector) SwapSnapshot(next *Snapshot) error {
	if s == nil || s.snapshots == nil {
		return fmt.Errorf("swap router selector snapshot: selector is not initialized")
	}
	if next == nil {
		return fmt.Errorf("swap router selector snapshot: snapshot is nil")
	}
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()

	previous := s.snapshots.Load()
	if err := s.snapshots.Swap(next); err != nil {
		return err
	}
	s.circuits.Reconcile(previous, next)
	return nil
}

func (s *Selector) CircuitStatus(routeID string) CircuitStatus {
	if s == nil {
		return CircuitStatus{State: CircuitClosed}
	}
	return s.circuits.Status(routeID, s.now())
}

func (s *Selector) ResetCircuit(routeID string) {
	if s == nil {
		return
	}
	s.circuits.Reset(routeID)
}

func (p *RequestPlan) SnapshotRevision() uint64 {
	if p == nil || p.snapshot == nil {
		return 0
	}
	return p.snapshot.Revision()
}

func (p *RequestPlan) ModelGroup() ModelGroup {
	if p == nil {
		return ModelGroup{}
	}
	return cloneModelGroup(p.group)
}

func (p *RequestPlan) Next() (Selection, error) {
	if p == nil || p.selector == nil || p.snapshot == nil {
		return Selection{}, ErrSelectionClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.selector.now()
	skipped := make(map[string]struct{})
	for {
		candidates := p.candidates(now, skipped)
		if len(candidates) == 0 {
			return Selection{}, fmt.Errorf("%w for model %q", ErrNoRouteAvailable, p.group.PublicModel)
		}

		route := p.choose(candidates)
		acquired, halfOpenProbe := p.selector.circuits.TryAcquire(route.ID, now)
		if !acquired {
			skipped[route.ID] = struct{}{}
			continue
		}
		upstream, ok := p.snapshot.Upstream(route.UpstreamID)
		if !ok {
			skipped[route.ID] = struct{}{}
			if halfOpenProbe {
				_ = p.selector.circuits.Abandon(route.ID, true, now)
			}
			continue
		}

		p.attempted[route.ID] = struct{}{}
		selection := Selection{
			Route:            route,
			Upstream:         upstream,
			SnapshotRevision: p.snapshot.Revision(),
			HalfOpenProbe:    halfOpenProbe,
			token:            p.selector.nextToken.Add(1),
		}
		p.outstanding[selection.token] = selection
		return selection, nil
	}
}

func (p *RequestPlan) Report(selection Selection, result AttemptResult) error {
	if p == nil || p.selector == nil {
		return ErrSelectionClosed
	}
	p.mu.Lock()
	active, ok := p.outstanding[selection.token]
	if !ok || selection.token == 0 || active.Route.ID != selection.Route.ID {
		p.mu.Unlock()
		return fmt.Errorf("%w: route %q", ErrSelectionClosed, selection.Route.ID)
	}
	delete(p.outstanding, selection.token)
	p.mu.Unlock()
	return p.selector.circuits.Record(selection.Route.ID, selection.HalfOpenProbe, result, p.selector.now())
}

func (p *RequestPlan) Abandon(selection Selection) error {
	if p == nil || p.selector == nil {
		return ErrSelectionClosed
	}
	p.mu.Lock()
	active, ok := p.outstanding[selection.token]
	if !ok || selection.token == 0 || active.Route.ID != selection.Route.ID {
		p.mu.Unlock()
		return fmt.Errorf("%w: route %q", ErrSelectionClosed, selection.Route.ID)
	}
	delete(p.outstanding, selection.token)
	p.mu.Unlock()
	return p.selector.circuits.Abandon(selection.Route.ID, selection.HalfOpenProbe, p.selector.now())
}

func (p *RequestPlan) candidates(now time.Time, skipped map[string]struct{}) []Route {
	prioritySet := false
	highestPriority := 0
	candidates := make([]Route, 0, len(p.group.Routes))
	for _, route := range p.group.Routes {
		if !route.Enabled {
			continue
		}
		if _, attempted := p.attempted[route.ID]; attempted {
			continue
		}
		if _, skip := skipped[route.ID]; skip {
			continue
		}
		upstream, ok := p.snapshot.upstreams[route.UpstreamID]
		if !ok || !upstream.Enabled || !p.selector.circuits.CanAttempt(route.ID, now) {
			continue
		}
		if !prioritySet || route.Priority > highestPriority {
			prioritySet = true
			highestPriority = route.Priority
			candidates = candidates[:0]
		}
		if route.Priority == highestPriority {
			candidates = append(candidates, route)
		}
	}
	return candidates
}

func (p *RequestPlan) choose(candidates []Route) Route {
	if p.group.SelectionStrategy == config.RouterSelectionWeightedAffinity && p.affinityKey != "" {
		return chooseWeightedAffinity(candidates, p.group.ID, p.affinityKey, p.affinityEpoch)
	}
	return p.selector.chooseWeightedRoundRobin(p.group.ID, candidates)
}

func (s *Selector) chooseWeightedRoundRobin(groupID string, candidates []Route) Route {
	if len(candidates) == 1 {
		return candidates[0]
	}
	totalWeight := uint64(0)
	for _, route := range candidates {
		totalWeight += uint64(route.Weight)
	}
	if totalWeight == 0 {
		return candidates[0]
	}

	key := roundRobinKey{groupID: groupID, priority: candidates[0].Priority}
	s.roundRobinMu.Lock()
	position := s.roundRobin[key] % totalWeight
	s.roundRobin[key]++
	s.roundRobinMu.Unlock()

	for _, route := range candidates {
		weight := uint64(route.Weight)
		if position < weight {
			return route
		}
		position -= weight
	}
	return candidates[len(candidates)-1]
}

func chooseWeightedAffinity(candidates []Route, groupID, affinityKey string, epoch int64) Route {
	best := candidates[0]
	bestScore := math.Inf(1)
	for _, route := range candidates {
		hash := sha256.New()
		_, _ = hash.Write([]byte(groupID))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(affinityKey))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(route.ID))
		var epochBytes [8]byte
		binary.BigEndian.PutUint64(epochBytes[:], uint64(epoch))
		_, _ = hash.Write(epochBytes[:])
		sum := hash.Sum(nil)
		value := binary.BigEndian.Uint64(sum[:8])
		uniform := (float64(value) + 1) / (float64(math.MaxUint64) + 1)
		score := -math.Log(uniform) / float64(route.Weight)
		if score < bestScore {
			best = route
			bestScore = score
		}
	}
	return best
}
