package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigBytesNormalizesRouterDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
port: 8317
router:
  upstreams:
    - id: primary
      name: Primary
      protocol: openai-responses
      base-url: https://example.com/v1/
      capabilities:
        endpoints: [responses]
      health-check:
        mode: http
  model-groups:
    - id: model-a
      public-model: model-a
      capability: text
      routes:
        - id: model-a-primary
          upstream-id: primary
          upstream-model: upstream-model-a
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if cfg.ServiceRole != ServiceRoleCombined {
		t.Fatalf("ServiceRole = %q, want %q", cfg.ServiceRole, ServiceRoleCombined)
	}
	upstream := cfg.Router.Upstreams[0]
	if !upstream.IsEnabled() || upstream.Enabled == nil {
		t.Fatalf("upstream enabled = %#v, want true", upstream.Enabled)
	}
	if upstream.BaseURL != "https://example.com/v1" {
		t.Fatalf("BaseURL = %q, want trailing slash removed", upstream.BaseURL)
	}
	if upstream.Auth.Type != RouterAuthNone {
		t.Fatalf("Auth.Type = %q, want %q", upstream.Auth.Type, RouterAuthNone)
	}
	if upstream.HealthCheck.Path != "/healthz" || upstream.HealthCheck.Interval != "30s" {
		t.Fatalf("HealthCheck defaults = %#v", upstream.HealthCheck)
	}
	if upstream.HealthCheck.UnhealthyThreshold != 3 || upstream.HealthCheck.HealthyThreshold != 2 {
		t.Fatalf("HealthCheck thresholds = %#v", upstream.HealthCheck)
	}

	group := cfg.Router.ModelGroups[0]
	if group.Selection.Strategy != RouterSelectionWeightedAffinity {
		t.Fatalf("Selection.Strategy = %q, want %q", group.Selection.Strategy, RouterSelectionWeightedAffinity)
	}
	if group.Selection.AffinityTTL != "720h" {
		t.Fatalf("Selection.AffinityTTL = %q, want 720h", group.Selection.AffinityTTL)
	}
	if group.Routes[0].Weight != 100 {
		t.Fatalf("Route.Weight = %d, want 100", group.Routes[0].Weight)
	}
	if !group.IsEnabled() || !group.Routes[0].IsEnabled() {
		t.Fatal("model group and route should default to enabled")
	}
}

func TestLoadConfigOptionalAppliesServiceRoleEnvironment(t *testing.T) {
	t.Setenv("SERVICE_ROLE", "router")
	t.Setenv("SMARTCLI_SERVICE_ROLE", "pool")
	t.Setenv("POOL_KIND", "ignored")
	t.Setenv("SMARTCLI_POOL_KIND", "codex")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if cfg.ServiceRole != ServiceRolePool {
		t.Fatalf("ServiceRole = %q, want %q", cfg.ServiceRole, ServiceRolePool)
	}
	if cfg.PoolKind != "codex" {
		t.Fatalf("PoolKind = %q, want codex", cfg.PoolKind)
	}
}

func TestRouterConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "invalid service role",
			mutate: func(cfg *Config) {
				cfg.ServiceRole = "everything"
			},
			wantErr: "service-role",
		},
		{
			name: "pool kind required",
			mutate: func(cfg *Config) {
				cfg.ServiceRole = ServiceRolePool
				cfg.Router = RouterConfig{}
			},
			wantErr: "pool-kind is required",
		},
		{
			name: "pool rejects router config",
			mutate: func(cfg *Config) {
				cfg.ServiceRole = ServiceRolePool
				cfg.PoolKind = "codex"
			},
			wantErr: "router configuration is not allowed",
		},
		{
			name: "duplicate upstream",
			mutate: func(cfg *Config) {
				cfg.Router.Upstreams = append(cfg.Router.Upstreams, cfg.Router.Upstreams[0])
			},
			wantErr: "is duplicated",
		},
		{
			name: "invalid base URL",
			mutate: func(cfg *Config) {
				cfg.Router.Upstreams[0].BaseURL = "file:///tmp/upstream"
			},
			wantErr: "absolute HTTP or HTTPS URL",
		},
		{
			name: "reserved header",
			mutate: func(cfg *Config) {
				cfg.Router.Upstreams[0].Headers = map[string]string{"Authorization": "secret"}
			},
			wantErr: "is reserved",
		},
		{
			name: "protocol capability mismatch",
			mutate: func(cfg *Config) {
				cfg.Router.Upstreams[0].Capabilities.Endpoints = []string{RouterEndpointChatCompletions}
			},
			wantErr: "must include \"responses\"",
		},
		{
			name: "unknown upstream route",
			mutate: func(cfg *Config) {
				cfg.Router.ModelGroups[0].Routes[0].UpstreamID = "missing"
			},
			wantErr: "references unknown upstream",
		},
		{
			name: "invalid weight",
			mutate: func(cfg *Config) {
				cfg.Router.ModelGroups[0].Routes[0].Weight = 10_001
			},
			wantErr: "must be between 1 and 10000",
		},
		{
			name: "image group needs image upstream",
			mutate: func(cfg *Config) {
				cfg.Router.ModelGroups[0].Capability = RouterCapabilityImage
			},
			wantErr: "without image-generation capability",
		},
		{
			name: "duplicate route across groups",
			mutate: func(cfg *Config) {
				second := cfg.Router.ModelGroups[0]
				second.ID = "model-b"
				second.PublicModel = "model-b"
				cfg.Router.ModelGroups = append(cfg.Router.ModelGroups, second)
			},
			wantErr: "duplicated across model groups",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validRouterTestConfig()
			tt.mutate(cfg)
			err := cfg.NormalizeAndValidateRouter()
			if err == nil {
				t.Fatalf("NormalizeAndValidateRouter() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("NormalizeAndValidateRouter() error = %q, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRouterConfigAllowsDisabledReferencedUpstream(t *testing.T) {
	cfg := validRouterTestConfig()
	disabled := false
	cfg.Router.Upstreams[0].Enabled = &disabled

	if err := cfg.NormalizeAndValidateRouter(); err != nil {
		t.Fatalf("NormalizeAndValidateRouter() error = %v", err)
	}
}

func validRouterTestConfig() *Config {
	return &Config{
		ServiceRole: ServiceRoleRouter,
		Router: RouterConfig{
			Upstreams: []RouterUpstream{
				{
					ID:       "primary",
					Name:     "Primary",
					Protocol: RouterProtocolOpenAIResponses,
					BaseURL:  "https://example.com/v1",
					Capabilities: RouterCapabilities{
						Endpoints: []string{RouterEndpointResponses},
						Streaming: true,
						Tools:     true,
					},
				},
			},
			ModelGroups: []RouterModelGroup{
				{
					ID:          "model-a",
					PublicModel: "model-a",
					Capability:  RouterCapabilityText,
					Routes: []RouterRoute{
						{
							ID:            "model-a-primary",
							UpstreamID:    "primary",
							UpstreamModel: "upstream-model-a",
							Priority:      100,
							Weight:        100,
						},
					},
				},
			},
		},
	}
}
