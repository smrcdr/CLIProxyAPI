package cliproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// UpstreamSecretResolver resolves an encrypted secret reference for runtime
// use. Implementations must return a new byte slice owned by the caller.
type UpstreamSecretResolver interface {
	ResolveUpstreamSecret(context.Context, string) ([]byte, error)
}

type routerUpstreamRuntime struct {
	manager  *coreauth.Manager
	resolver UpstreamSecretResolver

	mu        sync.Mutex
	authIDs   map[string]string
	providers map[string]struct{}
}

type preparedRouterUpstream struct {
	auth     *coreauth.Auth
	executor coreauth.ProviderExecutor
}

type routerUpstreamPlan struct {
	entries map[string]preparedRouterUpstream
}

func newRouterUpstreamRuntime(manager *coreauth.Manager, resolver UpstreamSecretResolver) *routerUpstreamRuntime {
	return &routerUpstreamRuntime{
		manager:   manager,
		resolver:  resolver,
		authIDs:   make(map[string]string),
		providers: make(map[string]struct{}),
	}
}

// Prepare resolves and validates every enabled upstream without changing the
// active runtime registrations.
func (r *routerUpstreamRuntime) Prepare(ctx context.Context, cfg *config.Config, snapshot *smartrouter.Snapshot) (*routerUpstreamPlan, error) {
	if r == nil || r.manager == nil {
		return nil, fmt.Errorf("smart router upstream runtime is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return nil, fmt.Errorf("smart router upstream runtime config is not configured")
	}
	if snapshot == nil {
		snapshot = smartrouter.EmptySnapshot()
	}

	prepared := make(map[string]preparedRouterUpstream)
	for _, upstream := range snapshot.Upstreams() {
		if !upstream.Enabled {
			continue
		}
		entry, err := r.prepare(ctx, cfg, upstream)
		if err != nil {
			return nil, err
		}
		prepared[upstream.ID] = entry
	}
	return &routerUpstreamPlan{entries: prepared}, nil
}

// Apply replaces active runtime registrations with a prepared plan.
func (r *routerUpstreamRuntime) Apply(ctx context.Context, plan *routerUpstreamPlan) error {
	if r == nil || r.manager == nil {
		return fmt.Errorf("smart router upstream runtime is not configured")
	}
	if plan == nil {
		return fmt.Errorf("smart router upstream runtime plan is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	nextAuthIDs := make(map[string]string, len(plan.entries))
	nextProviders := make(map[string]struct{}, len(plan.entries))
	for upstreamID, entry := range plan.entries {
		r.manager.RegisterExecutor(entry.executor)
		r.manager.RegisterRuntime(entry.auth)
		nextAuthIDs[upstreamID] = entry.auth.ID
		nextProviders[entry.auth.Provider] = struct{}{}
	}

	for upstreamID, authID := range r.authIDs {
		if _, keep := nextAuthIDs[upstreamID]; keep {
			continue
		}
		r.manager.Remove(ctx, authID)
	}
	for provider := range r.providers {
		if _, keep := nextProviders[provider]; !keep {
			r.manager.UnregisterExecutor(provider)
		}
	}
	r.authIDs = nextAuthIDs
	r.providers = nextProviders
	return nil
}

// Reconcile is the startup convenience path. Config reloads use Prepare and
// Apply separately so all fallible work finishes before shared state changes.
func (r *routerUpstreamRuntime) Reconcile(ctx context.Context, cfg *config.Config, snapshot *smartrouter.Snapshot) error {
	plan, err := r.Prepare(ctx, cfg, snapshot)
	if err != nil {
		return err
	}
	return r.Apply(ctx, plan)
}

func (r *routerUpstreamRuntime) prepare(ctx context.Context, cfg *config.Config, upstream smartrouter.Upstream) (preparedRouterUpstream, error) {
	provider := handlers.SmartRouterProviderID(upstream.ID)
	attributes := map[string]string{
		"base_url":            upstream.BaseURL,
		"provider_key":        provider,
		"smartrouter_managed": "true",
	}
	for key, value := range upstream.Headers {
		attributes["header:"+key] = value
	}

	var secret []byte
	if upstream.Auth.Type != config.RouterAuthNone {
		if r.resolver == nil {
			return preparedRouterUpstream{}, fmt.Errorf("resolve smart router upstream %q secret: resolver is not configured", upstream.ID)
		}
		var err error
		secret, err = r.resolver.ResolveUpstreamSecret(ctx, upstream.Auth.SecretRef)
		if err != nil || len(secret) == 0 {
			clear(secret)
			return preparedRouterUpstream{}, fmt.Errorf("resolve smart router upstream %q secret: unavailable", upstream.ID)
		}
		defer clear(secret)
	}

	switch upstream.Auth.Type {
	case config.RouterAuthNone:
	case config.RouterAuthBearer:
		attributes[coreauth.AttributeAPIKey] = string(secret)
	case config.RouterAuthAPIKeyHeader:
		attributes["header:"+upstream.Auth.Header] = string(secret)
	case config.RouterAuthBasic:
		attributes["header:Authorization"] = "Basic " + base64.StdEncoding.EncodeToString(secret)
	default:
		return preparedRouterUpstream{}, fmt.Errorf("prepare smart router upstream %q: unsupported auth type", upstream.ID)
	}

	var providerExecutor coreauth.ProviderExecutor
	switch upstream.Protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		providerExecutor = runtimeexecutor.NewOpenAICompatExecutorForFormat(provider, cfg, sdktranslator.FormatOpenAI)
	case config.RouterProtocolOpenAIResponses:
		providerExecutor = runtimeexecutor.NewOpenAICompatExecutorForFormat(provider, cfg, sdktranslator.FormatOpenAIResponse)
	case config.RouterProtocolAnthropicMessages:
		providerExecutor = runtimeexecutor.NewClaudeExecutorForProvider(provider, cfg)
	default:
		return preparedRouterUpstream{}, fmt.Errorf("prepare smart router upstream %q: unsupported protocol", upstream.ID)
	}

	now := time.Now()
	authID := "smartrouter-upstream-" + strings.ToLower(strings.TrimSpace(upstream.ID))
	return preparedRouterUpstream{
		auth: &coreauth.Auth{
			ID:         authID,
			Provider:   provider,
			Label:      upstream.Name,
			Status:     coreauth.StatusActive,
			Attributes: attributes,
			CreatedAt:  now,
			UpdatedAt:  now,
		},
		executor: providerExecutor,
	}, nil
}
