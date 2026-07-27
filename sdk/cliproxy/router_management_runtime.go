package cliproxy

import (
	"context"
	"errors"
	"fmt"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
)

type preparedRouterManagementRuntime struct {
	service          *Service
	expectedRevision uint64
	cfg              *internalconfig.Config
	snapshot         *smartrouter.Snapshot
	upstreams        *routerUpstreamPlan
}

func (s *Service) PrepareRouterRuntime(ctx context.Context, document smartrouter.RouterMetadataDocument) (smartrouter.RouterRuntimeUpdate, error) {
	if s == nil {
		return nil, errors.New("prepare router management runtime: service is nil")
	}
	if document.Revision == 0 {
		return nil, errors.New("prepare router management runtime: revision is zero")
	}
	s.cfgMu.RLock()
	cfg := s.cfg.CloneForRuntime()
	s.cfgMu.RUnlock()
	if cfg == nil || cfg.ServiceRole != internalconfig.ServiceRoleRouter {
		return nil, errors.New("prepare router management runtime: service is not in router role")
	}
	cfg.Router = document.Router
	snapshot, errSnapshot := smartrouter.CompileSnapshot(cfg, document.Revision)
	if errSnapshot != nil {
		return nil, errSnapshot
	}
	if s.routerSelector == nil || s.routerUpstreamRuntime == nil {
		return nil, errors.New("prepare router management runtime: router runtime is not initialized")
	}
	current := s.routerSnapshots.Load()
	if current == nil {
		return nil, errors.New("prepare router management runtime: active router snapshot is unavailable")
	}
	plan, errPlan := s.routerUpstreamRuntime.Prepare(ctx, cfg, snapshot)
	if errPlan != nil {
		return nil, errPlan
	}
	return &preparedRouterManagementRuntime{
		service:          s,
		expectedRevision: current.Revision(),
		cfg:              cfg,
		snapshot:         snapshot,
		upstreams:        plan,
	}, nil
}

func (u *preparedRouterManagementRuntime) CommitRouterRuntime(ctx context.Context) error {
	if u == nil || u.service == nil || u.cfg == nil || u.snapshot == nil || u.upstreams == nil {
		return errors.New("commit router management runtime: update is incomplete")
	}
	service := u.service
	service.configUpdateMu.Lock()
	defer service.configUpdateMu.Unlock()

	current := service.routerSnapshots.Load()
	if current == nil || current.Revision() != u.expectedRevision {
		actual := uint64(0)
		if current != nil {
			actual = current.Revision()
		}
		return &smartrouter.RouterRevisionConflictError{
			Expected: u.expectedRevision,
			Actual:   actual,
		}
	}
	if errApply := service.routerUpstreamRuntime.Apply(ctx, u.upstreams); errApply != nil {
		return fmt.Errorf("commit router management runtime upstreams: %w", errApply)
	}
	if errSwap := service.routerSelector.SwapSnapshot(u.snapshot); errSwap != nil {
		return fmt.Errorf("commit router management runtime snapshot: %w", errSwap)
	}

	service.cfgMu.Lock()
	service.cfg = u.cfg
	service.cfgMu.Unlock()
	if service.coreManager != nil {
		service.coreManager.SetConfig(u.cfg)
		service.coreManager.SetOAuthModelAlias(u.cfg.OAuthModelAlias)
	}
	if service.server != nil {
		service.server.UpdateClients(u.cfg)
	}
	return nil
}
