// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	configaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// Builder constructs a Service instance with customizable providers.
// It provides a fluent interface for configuring all aspects of the service
// including authentication, file watching, HTTP server options, and lifecycle hooks.
type Builder struct {
	// cfg holds the application configuration.
	cfg *config.Config

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// pluginHost owns dynamic plugin lifecycle and adapters.
	pluginHost *pluginhost.Host

	// postAuthHook is called after auth record creation and before persistence.
	postAuthHook coreauth.PostAuthHook

	// upstreamSecretResolver resolves Smart Router secret references at runtime.
	upstreamSecretResolver UpstreamSecretResolver

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption
}

// Hooks allows callers to plug into service lifecycle stages.
// These callbacks provide opportunities to perform custom initialization
// and cleanup operations during service startup and shutdown.
type Hooks struct {
	// OnBeforeStart is called before the service starts, allowing configuration
	// modifications or additional setup.
	OnBeforeStart func(*config.Config)

	// OnAfterStart is called after the service has started successfully,
	// providing access to the service instance for additional operations.
	OnAfterStart func(*Service)
}

// NewBuilder creates a Builder with default dependencies left unset.
// Use the fluent interface methods to configure the service before calling Build().
//
// Returns:
//   - *Builder: A new builder instance ready for configuration
func NewBuilder() *Builder {
	return &Builder{}
}

// WithConfig sets the configuration instance used by the service.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *Builder: The builder instance for method chaining
func (b *Builder) WithConfig(cfg *config.Config) *Builder {
	b.cfg = cfg
	return b
}

// WithConfigPath sets the absolute configuration file path used for reload watching.
//
// Parameters:
//   - path: The absolute path to the configuration file
//
// Returns:
//   - *Builder: The builder instance for method chaining
func (b *Builder) WithConfigPath(path string) *Builder {
	b.configPath = path
	return b
}

// WithTokenClientProvider overrides the provider responsible for token-backed clients.
func (b *Builder) WithTokenClientProvider(provider TokenClientProvider) *Builder {
	b.tokenProvider = provider
	return b
}

// WithAPIKeyClientProvider overrides the provider responsible for API key-backed clients.
func (b *Builder) WithAPIKeyClientProvider(provider APIKeyClientProvider) *Builder {
	b.apiKeyProvider = provider
	return b
}

// WithWatcherFactory allows customizing the watcher factory that handles reloads.
func (b *Builder) WithWatcherFactory(factory WatcherFactory) *Builder {
	b.watcherFactory = factory
	return b
}

// WithHooks registers lifecycle hooks executed around service startup.
func (b *Builder) WithHooks(h Hooks) *Builder {
	b.hooks = h
	return b
}

// WithAuthManager overrides the authentication manager used for token lifecycle operations.
func (b *Builder) WithAuthManager(mgr *sdkAuth.Manager) *Builder {
	b.authManager = mgr
	return b
}

// WithRequestAccessManager overrides the request authentication manager.
func (b *Builder) WithRequestAccessManager(mgr *sdkaccess.Manager) *Builder {
	b.accessManager = mgr
	return b
}

// WithCoreAuthManager overrides the runtime auth manager responsible for request execution.
func (b *Builder) WithCoreAuthManager(mgr *coreauth.Manager) *Builder {
	b.coreManager = mgr
	return b
}

// WithPluginHost overrides the dynamic plugin host used by the service.
func (b *Builder) WithPluginHost(host *pluginhost.Host) *Builder {
	b.pluginHost = host
	return b
}

// WithServerOptions appends server configuration options used during construction.
func (b *Builder) WithServerOptions(opts ...api.ServerOption) *Builder {
	b.serverOptions = append(b.serverOptions, opts...)
	return b
}

// WithLocalManagementPassword configures a password that is only accepted from localhost management requests.
func (b *Builder) WithLocalManagementPassword(password string) *Builder {
	if password == "" {
		return b
	}
	b.serverOptions = append(b.serverOptions, api.WithLocalManagementPassword(password))
	return b
}

// WithPostAuthHook registers a hook to be called after an Auth record is created
// but before it is persisted to storage.
func (b *Builder) WithPostAuthHook(hook coreauth.PostAuthHook) *Builder {
	if hook == nil {
		return b
	}
	b.postAuthHook = hook
	return b
}

// WithUpstreamSecretResolver supplies runtime-only Smart Router credentials.
// The resolver is never exposed to HTTP handlers or persisted by the service.
func (b *Builder) WithUpstreamSecretResolver(resolver UpstreamSecretResolver) *Builder {
	b.upstreamSecretResolver = resolver
	return b
}

// Build validates inputs, applies defaults, and returns a ready-to-run service.
func (b *Builder) Build() (*Service, error) {
	if b.cfg == nil {
		return nil, fmt.Errorf("cliproxy: configuration is required")
	}
	if b.configPath == "" {
		return nil, fmt.Errorf("cliproxy: configuration path is required")
	}
	routerRevision := uint64(1)
	var routerMetadataStore smartrouter.RouterMetadataStore
	var routerSecretStore smartrouter.RouterSecretStore
	var routerAuditSink smartrouter.RouterAuditSink
	if b.cfg.ServiceRole == internalconfig.ServiceRoleRouter {
		if errIsolation := b.cfg.ValidateRouterCredentialIsolation(); errIsolation != nil {
			return nil, fmt.Errorf("cliproxy: %w", errIsolation)
		}
		var errRouterStorage error
		b.cfg, routerRevision, routerMetadataStore, routerSecretStore, routerAuditSink, errRouterStorage = b.prepareRouterStorage()
		if errRouterStorage != nil {
			return nil, fmt.Errorf("cliproxy: %w", errRouterStorage)
		}
		if b.upstreamSecretResolver == nil && routerSecretStore != nil {
			b.upstreamSecretResolver = routerSecretStore
		}
		if routerConfigUsesSecrets(b.cfg.Router) && b.upstreamSecretResolver == nil {
			return nil, errors.New("cliproxy: SMART_ROUTER_MASTER_KEY or a custom upstream secret resolver is required")
		}
	}
	if err := b.cfg.NormalizeAndValidateRouter(); err != nil {
		return nil, fmt.Errorf("cliproxy: %w", err)
	}
	if b.cfg.ServiceRole == internalconfig.ServiceRoleRouter {
		routerStorageDirectory := filepath.Join(filepath.Dir(b.configPath), "router")
		if errIsolation := rejectRouterAuthDirectoryCredentials(b.cfg.AuthDir, routerStorageDirectory); errIsolation != nil {
			return nil, fmt.Errorf("cliproxy: %w", errIsolation)
		}
	}
	routerSnapshot, errRouterSnapshot := smartrouter.CompileSnapshot(b.cfg, routerRevision)
	if errRouterSnapshot != nil {
		return nil, fmt.Errorf("cliproxy: %w", errRouterSnapshot)
	}
	if b.cfg.ServiceRole != internalconfig.ServiceRoleRouter {
		routerSnapshot = smartrouter.EmptySnapshotAtRevision(1)
	}

	tokenProvider := b.tokenProvider
	if tokenProvider == nil {
		tokenProvider = NewFileTokenClientProvider()
	}

	apiKeyProvider := b.apiKeyProvider
	if apiKeyProvider == nil {
		apiKeyProvider = NewAPIKeyClientProvider()
	}

	watcherFactory := b.watcherFactory
	if watcherFactory == nil {
		watcherFactory = defaultWatcherFactory
	}

	authManager := b.authManager
	if authManager == nil {
		authManager = newDefaultAuthManager()
	}

	accessManager := b.accessManager
	if accessManager == nil {
		accessManager = sdkaccess.NewManager()
	}

	configaccess.Register(&b.cfg.SDKConfig)
	pluginHost := b.pluginHost
	if pluginHost == nil {
		pluginHost = pluginhost.New()
	}
	if b.cfg != nil {
		pluginHost.ApplyConfig(context.Background(), b.cfg)
		pluginHost.RegisterFrontendAuthProviders()
	}
	accessManager.SetProviders(sdkaccess.RegisteredProviders())

	coreManager := b.coreManager
	if coreManager == nil {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && b.cfg != nil {
			dirSetter.SetBaseDir(b.cfg.AuthDir)
		}

		strategy := ""
		sessionAffinity := false
		sessionAffinityTTL := time.Hour
		if b.cfg != nil {
			strategy = strings.ToLower(strings.TrimSpace(b.cfg.Routing.Strategy))
			// Support both legacy ClaudeCodeSessionAffinity and new universal SessionAffinity
			sessionAffinity = b.cfg.Routing.SessionAffinity
			if ttlStr := strings.TrimSpace(b.cfg.Routing.SessionAffinityTTL); ttlStr != "" {
				if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed > 0 {
					sessionAffinityTTL = parsed
				}
			}
		}
		var selector coreauth.Selector
		switch strategy {
		case "fill-first", "fillfirst", "ff":
			selector = &coreauth.FillFirstSelector{}
		default:
			selector = &coreauth.RoundRobinSelector{}
		}

		// Wrap with session affinity if enabled (failover is always on)
		if sessionAffinity {
			selector = coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
				Fallback: selector,
				TTL:      sessionAffinityTTL,
			})
		}
		if b.cfg != nil && b.cfg.Routing.SmartAPIAffinity {
			ttl := 30 * 24 * time.Hour
			if ttlStr := strings.TrimSpace(b.cfg.Routing.SmartAPIAffinityTTL); ttlStr != "" {
				if parsed, errParse := time.ParseDuration(ttlStr); errParse == nil && parsed > 0 {
					ttl = parsed
				}
			}
			selector = coreauth.NewSmartAPIAffinitySelector(selector, filepath.Join(b.cfg.AuthDir, "smartapi-affinity.json"), ttl)
		}

		coreManager = coreauth.NewManager(tokenStore, selector, nil)
	}
	if b.cfg.ServiceRole == internalconfig.ServiceRoleRouter && len(coreManager.List()) > 0 {
		return nil, errors.New("cliproxy: router role cannot start with preloaded local credentials")
	}
	// Attach a default RoundTripper provider so providers can opt-in per-auth transports.
	coreManager.SetRoundTripperProvider(newDefaultRoundTripperProvider())
	coreManager.SetConfig(b.cfg)
	coreManager.SetOAuthModelAlias(b.cfg.OAuthModelAlias)
	if pluginHost != nil {
		coreManager.SetPluginScheduler(pluginHost)
	}

	routerSnapshots := smartrouter.NewSnapshotStore(routerSnapshot)
	service := &Service{
		cfg:             b.cfg,
		configPath:      b.configPath,
		tokenProvider:   tokenProvider,
		apiKeyProvider:  apiKeyProvider,
		watcherFactory:  watcherFactory,
		hooks:           b.hooks,
		authManager:     authManager,
		accessManager:   accessManager,
		coreManager:     coreManager,
		pluginHost:      pluginHost,
		routerSnapshots: routerSnapshots,
		routerSelector:  smartrouter.NewSelector(routerSnapshots, nil),
		routerUpstreamRuntime: newRouterUpstreamRuntime(
			coreManager,
			b.upstreamSecretResolver,
		),
		routerMetadataStore: routerMetadataStore,
		routerSecretStore:   routerSecretStore,
		routerAuditSink:     routerAuditSink,
		serverOptions:       append([]api.ServerOption(nil), b.serverOptions...),
	}
	if routerMetadataStore != nil {
		routerManagement, errRouterManagement := smartrouter.NewRouterManagementService(
			routerMetadataStore,
			routerSecretStore,
			routerAuditSink,
			service,
		)
		if errRouterManagement != nil {
			return nil, fmt.Errorf("cliproxy: initialize router management: %w", errRouterManagement)
		}
		service.routerManagement = routerManagement
	}
	service.routerHealthPoller = smartrouter.NewRouterHealthPoller(
		service.routerSnapshots,
		service,
		service.routerSelector.TransportHealthStore(),
	)
	if b.postAuthHook != nil {
		service.serverOptions = append(service.serverOptions, api.WithPostAuthHook(b.postAuthHook))
	}
	service.serverOptions = append(service.serverOptions,
		api.WithPostAuthPersistHook(service.runtimeAuthSyncHook()),
		api.WithPluginHost(pluginHost),
		api.WithSmartRouterSelector(service.routerSelector),
		api.WithRouterManagementService(service.routerManagement),
		api.WithRouterProber(service),
		api.WithConfigReloadHook(func(_ context.Context, _ *config.Config) {
			service.reloadConfigFromWatcher()
		}),
	)
	return service, nil
}

func (b *Builder) prepareRouterStorage() (*config.Config, uint64, smartrouter.RouterMetadataStore, smartrouter.RouterSecretStore, smartrouter.RouterAuditSink, error) {
	storageDirectory := filepath.Join(filepath.Dir(b.configPath), "router")
	metadata, errMetadata := smartrouter.NewFileRouterMetadataStore(filepath.Join(storageDirectory, "metadata.json"))
	if errMetadata != nil {
		return nil, 0, nil, nil, nil, errMetadata
	}
	document, errLoad := metadata.LoadRouterMetadata(context.Background())
	if errLoad != nil {
		return nil, 0, nil, nil, nil, errLoad
	}
	cfg := b.cfg.CloneForRuntime()
	if document.Revision == 0 {
		created, errCreate := metadata.ReplaceRouterMetadata(context.Background(), 0, cfg.Router)
		if errCreate != nil {
			return nil, 0, nil, nil, nil, errCreate
		}
		document = created
	} else {
		cfg.Router = document.Router
	}

	var secrets smartrouter.RouterSecretStore
	if encodedKey := strings.TrimSpace(os.Getenv("SMART_ROUTER_MASTER_KEY")); encodedKey != "" {
		key, errKey := smartrouter.ParseRouterMasterKey(encodedKey)
		if errKey != nil {
			return nil, 0, nil, nil, nil, errKey
		}
		secretStore, errSecrets := smartrouter.NewEncryptedFileRouterSecretStore(filepath.Join(storageDirectory, "secrets.json"), key)
		clear(key)
		if errSecrets != nil {
			return nil, 0, nil, nil, nil, errSecrets
		}
		secrets = secretStore
	}
	audit, errAudit := smartrouter.NewFileRouterAuditSink(filepath.Join(storageDirectory, "audit.jsonl"))
	if errAudit != nil {
		return nil, 0, nil, nil, nil, errAudit
	}
	return cfg, document.Revision, metadata, secrets, audit, nil
}

func routerConfigUsesSecrets(router internalconfig.RouterConfig) bool {
	for index := range router.Upstreams {
		if router.Upstreams[index].Auth.Type != "" && router.Upstreams[index].Auth.Type != internalconfig.RouterAuthNone {
			return true
		}
	}
	return false
}

func rejectRouterAuthDirectoryCredentials(authDirectory, routerStorageDirectory string) error {
	resolved, errResolve := util.ResolveAuthDir(authDirectory)
	if errResolve != nil {
		return fmt.Errorf("validate router auth directory: %w", errResolve)
	}
	routerStorageDirectory, _ = filepath.Abs(filepath.Clean(routerStorageDirectory))
	credentialFiles := 0
	errWalk := filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		absolutePath, _ := filepath.Abs(filepath.Clean(path))
		if entry.IsDir() {
			if absolutePath == routerStorageDirectory {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}
		if strings.EqualFold(entry.Name(), "smartapi-affinity.json") {
			return nil
		}
		credentialFiles++
		return nil
	})
	if errWalk != nil && !errors.Is(errWalk, os.ErrNotExist) {
		return fmt.Errorf("validate router auth directory: %w", errWalk)
	}
	if credentialFiles > 0 {
		return fmt.Errorf("router auth directory contains %d local credential JSON file(s)", credentialFiles)
	}
	return nil
}

func (s *Service) runtimeAuthSyncHook() coreauth.PostAuthHook {
	return func(ctx context.Context, auth *coreauth.Auth) error {
		if s == nil || auth == nil || auth.ID == "" {
			return nil
		}
		action := watcher.AuthUpdateActionAdd
		if s.coreManager != nil {
			if _, ok := s.coreManager.GetByID(auth.ID); ok {
				action = watcher.AuthUpdateActionModify
			}
		}
		update := watcher.AuthUpdate{
			Action: action,
			ID:     auth.ID,
			Auth:   auth,
		}
		if s.watcher != nil && s.watcher.DispatchPersistedAuthUpdate(update) {
			return nil
		}
		s.handleAuthUpdate(coreauth.WithSkipPersist(ctx), update)
		return nil
	}
}
