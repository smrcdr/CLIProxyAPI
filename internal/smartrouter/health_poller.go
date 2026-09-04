package smartrouter

import (
	"context"
	"strings"
	"time"
)

const routerHealthSchedulerInterval = 100 * time.Millisecond

type RouterHealthPoller struct {
	snapshots *SnapshotStore
	prober    RouterProber
	health    *TransportHealthStore
	now       func() time.Time
}

type routerHealthPollResult struct {
	upstream Upstream
	key      string
	result   RouterProbeResult
}

func NewRouterHealthPoller(snapshots *SnapshotStore, prober RouterProber, health *TransportHealthStore) *RouterHealthPoller {
	if health == nil {
		health = NewTransportHealthStore()
	}
	return &RouterHealthPoller{
		snapshots: snapshots,
		prober:    prober,
		health:    health,
		now:       time.Now,
	}
}

func (p *RouterHealthPoller) Run(ctx context.Context) {
	if p == nil || p.snapshots == nil || p.prober == nil || p.health == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(routerHealthSchedulerInterval)
	defer ticker.Stop()

	results := make(chan routerHealthPollResult)
	nextDue := make(map[string]time.Time)
	inFlight := make(map[string]string)

	schedule := func() {
		snapshot := p.snapshots.Load()
		if snapshot == nil {
			return
		}
		now := p.now()
		active := make(map[string]struct{})
		for _, upstream := range snapshot.Upstreams() {
			if !upstream.Enabled || upstream.HealthCheck.Mode != "http" {
				continue
			}
			active[upstream.ID] = struct{}{}
			if _, running := inFlight[upstream.ID]; running {
				continue
			}
			due, exists := nextDue[upstream.ID]
			if exists && due.After(now) {
				continue
			}
			key := routerHealthPollKey(upstream)
			inFlight[upstream.ID] = key
			go p.poll(ctx, upstream, key, results)
		}
		for upstreamID := range nextDue {
			if _, exists := active[upstreamID]; !exists {
				delete(nextDue, upstreamID)
			}
		}
	}

	schedule()
	for {
		select {
		case <-ctx.Done():
			return
		case completed := <-results:
			if inFlight[completed.upstream.ID] != completed.key {
				continue
			}
			delete(inFlight, completed.upstream.ID)
			nextDue[completed.upstream.ID] = p.now().Add(completed.upstream.HealthCheck.Interval)
			current := p.snapshots.Load()
			upstream, exists := current.Upstream(completed.upstream.ID)
			if exists && routerHealthPollKey(upstream) == completed.key {
				p.health.Record(upstream, completed.result)
			}
			schedule()
		case <-ticker.C:
			schedule()
		}
	}
}

func (p *RouterHealthPoller) poll(ctx context.Context, upstream Upstream, key string, results chan<- routerHealthPollResult) {
	result, err := p.prober.ProbeRouterUpstream(ctx, upstream.ID)
	if err != nil {
		result = RouterProbeResult{
			UpstreamID:     upstream.ID,
			Healthy:        false,
			Category:       FailureTransient,
			TransportError: true,
			CheckedAt:      p.now(),
		}
	}
	select {
	case results <- routerHealthPollResult{upstream: upstream, key: key, result: result}:
	case <-ctx.Done():
	}
}

func routerHealthPollKey(upstream Upstream) string {
	return strings.Join([]string{
		upstream.ID,
		upstream.BaseURL,
		upstream.HealthCheck.Mode,
		upstream.HealthCheck.Path,
		upstream.HealthCheck.Interval.String(),
	}, "\x00")
}
