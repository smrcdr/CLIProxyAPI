package smartrouter

import (
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type Upstream struct {
	ID                      string
	Name                    string
	Enabled                 bool
	Protocol                config.RouterProtocol
	BaseURL                 string
	Auth                    config.RouterUpstreamAuth
	Headers                 map[string]string
	Capabilities            config.RouterCapabilities
	HealthCheck             HealthCheck
	ForwardSmartAPIAffinity bool
}

type HealthCheck struct {
	Mode               string
	Path               string
	Interval           time.Duration
	UnhealthyThreshold int
	HealthyThreshold   int
}

type ModelGroup struct {
	ID                string
	PublicModel       string
	Enabled           bool
	Capability        config.RouterCapability
	SelectionStrategy config.RouterSelectionStrategy
	AffinityTTL       time.Duration
	Routes            []Route
}

type Route struct {
	ID            string
	UpstreamID    string
	UpstreamModel string
	Priority      int
	Weight        int
	Enabled       bool
}

// Snapshot is an immutable, compiled view of one complete router configuration.
// Its internal maps are never exposed and all returned values are defensive copies.
type Snapshot struct {
	revision       uint64
	upstreams      map[string]Upstream
	groupsByID     map[string]ModelGroup
	groupIDByModel map[string]string
	routesByID     map[string]Route
}

type SnapshotStore struct {
	current atomic.Pointer[Snapshot]
}

func CompileSnapshot(cfg *config.Config, revision uint64) (*Snapshot, error) {
	if cfg == nil {
		return nil, fmt.Errorf("compile router snapshot: config is nil")
	}

	normalized := cfg.CloneForRuntime()
	if err := normalized.NormalizeAndValidateRouter(); err != nil {
		return nil, fmt.Errorf("compile router snapshot: %w", err)
	}

	snapshot := &Snapshot{
		revision:       revision,
		upstreams:      make(map[string]Upstream, len(normalized.Router.Upstreams)),
		groupsByID:     make(map[string]ModelGroup, len(normalized.Router.ModelGroups)),
		groupIDByModel: make(map[string]string, len(normalized.Router.ModelGroups)),
		routesByID:     make(map[string]Route),
	}

	for _, source := range normalized.Router.Upstreams {
		healthInterval := time.Duration(0)
		if source.HealthCheck.Mode == "http" {
			parsed, err := time.ParseDuration(source.HealthCheck.Interval)
			if err != nil {
				return nil, fmt.Errorf("compile router snapshot: upstream %q health interval: %w", source.ID, err)
			}
			healthInterval = parsed
		}
		snapshot.upstreams[source.ID] = Upstream{
			ID:           source.ID,
			Name:         source.Name,
			Enabled:      source.IsEnabled(),
			Protocol:     source.Protocol,
			BaseURL:      source.BaseURL,
			Auth:         source.Auth,
			Headers:      cloneStringMap(source.Headers),
			Capabilities: cloneCapabilities(source.Capabilities),
			HealthCheck: HealthCheck{
				Mode:               source.HealthCheck.Mode,
				Path:               source.HealthCheck.Path,
				Interval:           healthInterval,
				UnhealthyThreshold: source.HealthCheck.UnhealthyThreshold,
				HealthyThreshold:   source.HealthCheck.HealthyThreshold,
			},
			ForwardSmartAPIAffinity: source.ForwardSmartAPIAffinity,
		}
	}

	for _, source := range normalized.Router.ModelGroups {
		affinityTTL := time.Duration(0)
		if source.Selection.AffinityTTL != "" {
			parsed, err := time.ParseDuration(source.Selection.AffinityTTL)
			if err != nil {
				return nil, fmt.Errorf("compile router snapshot: model group %q affinity TTL: %w", source.ID, err)
			}
			affinityTTL = parsed
		}

		routes := make([]Route, 0, len(source.Routes))
		for _, routeSource := range source.Routes {
			route := Route{
				ID:            routeSource.ID,
				UpstreamID:    routeSource.UpstreamID,
				UpstreamModel: routeSource.UpstreamModel,
				Priority:      routeSource.Priority,
				Weight:        routeSource.Weight,
				Enabled:       routeSource.IsEnabled(),
			}
			routes = append(routes, route)
			snapshot.routesByID[route.ID] = route
		}
		sort.SliceStable(routes, func(i, j int) bool {
			if routes[i].Priority != routes[j].Priority {
				return routes[i].Priority > routes[j].Priority
			}
			return routes[i].ID < routes[j].ID
		})

		group := ModelGroup{
			ID:                source.ID,
			PublicModel:       source.PublicModel,
			Enabled:           source.IsEnabled(),
			Capability:        source.Capability,
			SelectionStrategy: source.Selection.Strategy,
			AffinityTTL:       affinityTTL,
			Routes:            routes,
		}
		snapshot.groupsByID[group.ID] = group
		snapshot.groupIDByModel[group.PublicModel] = group.ID
	}

	return snapshot, nil
}

func EmptySnapshot() *Snapshot {
	return &Snapshot{
		upstreams:      make(map[string]Upstream),
		groupsByID:     make(map[string]ModelGroup),
		groupIDByModel: make(map[string]string),
		routesByID:     make(map[string]Route),
	}
}

func NewSnapshotStore(initial *Snapshot) *SnapshotStore {
	if initial == nil {
		initial = EmptySnapshot()
	}
	store := &SnapshotStore{}
	store.current.Store(initial)
	return store
}

func (s *SnapshotStore) Load() *Snapshot {
	if s == nil {
		return nil
	}
	return s.current.Load()
}

func (s *SnapshotStore) Swap(next *Snapshot) error {
	if s == nil {
		return fmt.Errorf("swap router snapshot: store is nil")
	}
	if next == nil {
		return fmt.Errorf("swap router snapshot: snapshot is nil")
	}
	s.current.Store(next)
	return nil
}

func (s *Snapshot) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision
}

func (s *Snapshot) Upstream(id string) (Upstream, bool) {
	if s == nil {
		return Upstream{}, false
	}
	upstream, ok := s.upstreams[id]
	if !ok {
		return Upstream{}, false
	}
	return cloneUpstream(upstream), true
}

func (s *Snapshot) Upstreams() []Upstream {
	if s == nil {
		return nil
	}
	values := make([]Upstream, 0, len(s.upstreams))
	for _, upstream := range s.upstreams {
		values = append(values, cloneUpstream(upstream))
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].ID < values[j].ID
	})
	return values
}

func (s *Snapshot) ModelGroupByID(id string) (ModelGroup, bool) {
	if s == nil {
		return ModelGroup{}, false
	}
	group, ok := s.groupsByID[id]
	if !ok {
		return ModelGroup{}, false
	}
	return cloneModelGroup(group), true
}

func (s *Snapshot) ModelGroupForModel(publicModel string) (ModelGroup, bool) {
	if s == nil {
		return ModelGroup{}, false
	}
	id, ok := s.groupIDByModel[publicModel]
	if !ok {
		return ModelGroup{}, false
	}
	return s.ModelGroupByID(id)
}

func (s *Snapshot) ModelGroups() []ModelGroup {
	if s == nil {
		return nil
	}
	values := make([]ModelGroup, 0, len(s.groupsByID))
	for _, group := range s.groupsByID {
		values = append(values, cloneModelGroup(group))
	}
	sort.Slice(values, func(i, j int) bool {
		return values[i].ID < values[j].ID
	})
	return values
}

func (s *Snapshot) Route(id string) (Route, bool) {
	if s == nil {
		return Route{}, false
	}
	route, ok := s.routesByID[id]
	return route, ok
}

func (s *Snapshot) AvailableRoutesForModel(publicModel string) []Route {
	group, ok := s.ModelGroupForModel(publicModel)
	if !ok || !group.Enabled {
		return nil
	}
	routes := make([]Route, 0, len(group.Routes))
	for _, route := range group.Routes {
		if !route.Enabled {
			continue
		}
		upstream, exists := s.upstreams[route.UpstreamID]
		if !exists || !upstream.Enabled {
			continue
		}
		routes = append(routes, route)
	}
	return routes
}

func cloneUpstream(source Upstream) Upstream {
	source.Headers = cloneStringMap(source.Headers)
	source.Capabilities = cloneCapabilities(source.Capabilities)
	return source
}

func cloneModelGroup(source ModelGroup) ModelGroup {
	if source.Routes == nil {
		return source
	}
	source.Routes = append([]Route(nil), source.Routes...)
	return source
}

func cloneCapabilities(source config.RouterCapabilities) config.RouterCapabilities {
	source.Endpoints = append([]string(nil), source.Endpoints...)
	return source
}

func cloneStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
