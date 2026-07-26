package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

type ServiceRole string

const (
	ServiceRoleCombined ServiceRole = "combined"
	ServiceRoleRouter   ServiceRole = "router"
	ServiceRolePool     ServiceRole = "pool"
)

type RouterProtocol string

const (
	RouterProtocolOpenAIResponses       RouterProtocol = "openai-responses"
	RouterProtocolOpenAIChatCompletions RouterProtocol = "openai-chat-completions"
	RouterProtocolAnthropicMessages     RouterProtocol = "anthropic-messages"
)

type RouterAuthType string

const (
	RouterAuthNone         RouterAuthType = "none"
	RouterAuthBearer       RouterAuthType = "bearer"
	RouterAuthAPIKeyHeader RouterAuthType = "api-key-header"
	RouterAuthBasic        RouterAuthType = "basic"
)

type RouterCapability string

const (
	RouterCapabilityText  RouterCapability = "text"
	RouterCapabilityImage RouterCapability = "image"
)

type RouterSelectionStrategy string

const (
	RouterSelectionWeightedRoundRobin RouterSelectionStrategy = "weighted-round-robin"
	RouterSelectionWeightedAffinity   RouterSelectionStrategy = "weighted-affinity"
)

const (
	RouterEndpointResponses       = "responses"
	RouterEndpointChatCompletions = "chat-completions"
	RouterEndpointMessages        = "messages"
	RouterEndpointCountTokens     = "count-tokens"
	RouterEndpointImages          = "images"
)

type RouterConfig struct {
	Upstreams   []RouterUpstream   `yaml:"upstreams,omitempty" json:"upstreams,omitempty"`
	ModelGroups []RouterModelGroup `yaml:"model-groups,omitempty" json:"model-groups,omitempty"`
}

type RouterUpstream struct {
	ID                      string             `yaml:"id" json:"id"`
	Name                    string             `yaml:"name" json:"name"`
	Enabled                 *bool              `yaml:"enabled,omitempty" json:"enabled"`
	Protocol                RouterProtocol     `yaml:"protocol" json:"protocol"`
	BaseURL                 string             `yaml:"base-url" json:"base-url"`
	Auth                    RouterUpstreamAuth `yaml:"auth,omitempty" json:"auth"`
	Headers                 map[string]string  `yaml:"headers,omitempty" json:"headers,omitempty"`
	Capabilities            RouterCapabilities `yaml:"capabilities" json:"capabilities"`
	HealthCheck             RouterHealthCheck  `yaml:"health-check,omitempty" json:"health-check"`
	ForwardSmartAPIAffinity bool               `yaml:"forward-smartapi-affinity,omitempty" json:"forward-smartapi-affinity"`
}

type RouterUpstreamAuth struct {
	Type      RouterAuthType `yaml:"type,omitempty" json:"type"`
	SecretRef string         `yaml:"secret-ref,omitempty" json:"secret-ref,omitempty"`
	Header    string         `yaml:"header,omitempty" json:"header,omitempty"`
}

type RouterCapabilities struct {
	Endpoints       []string `yaml:"endpoints" json:"endpoints"`
	Streaming       bool     `yaml:"streaming,omitempty" json:"streaming"`
	Tools           bool     `yaml:"tools,omitempty" json:"tools"`
	VisionInput     bool     `yaml:"vision-input,omitempty" json:"vision-input"`
	ImageGeneration bool     `yaml:"image-generation,omitempty" json:"image-generation"`
}

type RouterHealthCheck struct {
	Mode               string `yaml:"mode,omitempty" json:"mode"`
	Path               string `yaml:"path,omitempty" json:"path,omitempty"`
	Interval           string `yaml:"interval,omitempty" json:"interval,omitempty"`
	UnhealthyThreshold int    `yaml:"unhealthy-threshold,omitempty" json:"unhealthy-threshold,omitempty"`
	HealthyThreshold   int    `yaml:"healthy-threshold,omitempty" json:"healthy-threshold,omitempty"`
}

type RouterModelGroup struct {
	ID          string           `yaml:"id" json:"id"`
	PublicModel string           `yaml:"public-model" json:"public-model"`
	Enabled     *bool            `yaml:"enabled,omitempty" json:"enabled"`
	Capability  RouterCapability `yaml:"capability" json:"capability"`
	Selection   RouterSelection  `yaml:"selection,omitempty" json:"selection"`
	Routes      []RouterRoute    `yaml:"routes,omitempty" json:"routes,omitempty"`
}

type RouterSelection struct {
	Strategy    RouterSelectionStrategy `yaml:"strategy,omitempty" json:"strategy"`
	AffinityTTL string                  `yaml:"affinity-ttl,omitempty" json:"affinity-ttl,omitempty"`
}

type RouterRoute struct {
	ID            string `yaml:"id" json:"id"`
	UpstreamID    string `yaml:"upstream-id" json:"upstream-id"`
	UpstreamModel string `yaml:"upstream-model" json:"upstream-model"`
	Priority      int    `yaml:"priority,omitempty" json:"priority"`
	Weight        int    `yaml:"weight,omitempty" json:"weight"`
	Enabled       *bool  `yaml:"enabled,omitempty" json:"enabled"`
}

type RouterValidationError struct {
	Problems []string
}

func (e *RouterValidationError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return "invalid router configuration"
	}
	return "invalid router configuration: " + strings.Join(e.Problems, "; ")
}

func (u RouterUpstream) IsEnabled() bool {
	return boolValueDefaultTrue(u.Enabled)
}

func (g RouterModelGroup) IsEnabled() bool {
	return boolValueDefaultTrue(g.Enabled)
}

func (r RouterRoute) IsEnabled() bool {
	return boolValueDefaultTrue(r.Enabled)
}

func (c RouterCapabilities) Supports(endpoint string) bool {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	for _, candidate := range c.Endpoints {
		if candidate == endpoint {
			return true
		}
	}
	return false
}

func (cfg *Config) ApplyServiceRoleEnvironment() {
	if cfg == nil {
		return
	}
	if value, ok := firstEnvironmentValue("SMARTCLI_SERVICE_ROLE", "SERVICE_ROLE"); ok {
		cfg.ServiceRole = ServiceRole(value)
	}
	if value, ok := firstEnvironmentValue("SMARTCLI_POOL_KIND", "POOL_KIND"); ok {
		cfg.PoolKind = value
	}
}

func (cfg *Config) NormalizeAndValidateRouter() error {
	if cfg == nil {
		return nil
	}

	cfg.ServiceRole = ServiceRole(strings.ToLower(strings.TrimSpace(string(cfg.ServiceRole))))
	if cfg.ServiceRole == "" {
		cfg.ServiceRole = ServiceRoleCombined
	}
	cfg.PoolKind = strings.ToLower(strings.TrimSpace(cfg.PoolKind))

	for i := range cfg.Router.Upstreams {
		normalizeRouterUpstream(&cfg.Router.Upstreams[i])
	}
	for i := range cfg.Router.ModelGroups {
		normalizeRouterModelGroup(&cfg.Router.ModelGroups[i])
	}

	return cfg.ValidateRouter()
}

func (cfg *Config) ValidateRouter() error {
	if cfg == nil {
		return nil
	}

	problems := make([]string, 0)
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	switch cfg.ServiceRole {
	case ServiceRoleCombined:
		if cfg.PoolKind != "" {
			add("pool-kind must be empty when service-role is %q", cfg.ServiceRole)
		}
	case ServiceRoleRouter:
		if cfg.PoolKind != "" {
			add("pool-kind must be empty when service-role is %q", cfg.ServiceRole)
		}
	case ServiceRolePool:
		if cfg.PoolKind == "" {
			add("pool-kind is required when service-role is %q", cfg.ServiceRole)
		} else if !routerIdentifierPattern.MatchString(cfg.PoolKind) {
			add("pool-kind %q must be a lowercase identifier", cfg.PoolKind)
		}
		if len(cfg.Router.Upstreams) > 0 || len(cfg.Router.ModelGroups) > 0 {
			add("router configuration is not allowed when service-role is %q", cfg.ServiceRole)
		}
	default:
		add("service-role %q must be one of %q, %q, or %q", cfg.ServiceRole, ServiceRoleCombined, ServiceRoleRouter, ServiceRolePool)
	}

	upstreamByID := make(map[string]RouterUpstream, len(cfg.Router.Upstreams))
	for index, upstream := range cfg.Router.Upstreams {
		path := fmt.Sprintf("router.upstreams[%d]", index)
		validateRouterUpstream(path, upstream, add)
		if upstream.ID == "" {
			continue
		}
		if _, exists := upstreamByID[upstream.ID]; exists {
			add("%s.id %q is duplicated", path, upstream.ID)
			continue
		}
		upstreamByID[upstream.ID] = upstream
	}

	groupIDs := make(map[string]struct{}, len(cfg.Router.ModelGroups))
	publicModels := make(map[string]struct{}, len(cfg.Router.ModelGroups))
	routeIDs := make(map[string]struct{})
	for index, group := range cfg.Router.ModelGroups {
		path := fmt.Sprintf("router.model-groups[%d]", index)
		validateRouterModelGroup(path, group, upstreamByID, routeIDs, add)
		if group.ID != "" {
			if _, exists := groupIDs[group.ID]; exists {
				add("%s.id %q is duplicated", path, group.ID)
			}
			groupIDs[group.ID] = struct{}{}
		}
		if group.PublicModel != "" {
			if _, exists := publicModels[group.PublicModel]; exists {
				add("%s.public-model %q is duplicated", path, group.PublicModel)
			}
			publicModels[group.PublicModel] = struct{}{}
		}
	}

	if len(problems) > 0 {
		return &RouterValidationError{Problems: problems}
	}
	return nil
}

func finalizeRouterConfig(cfg *Config, applyEnvironment bool) error {
	if cfg == nil {
		return nil
	}
	if applyEnvironment {
		cfg.ApplyServiceRoleEnvironment()
	}
	if err := cfg.NormalizeAndValidateRouter(); err != nil {
		return err
	}
	return nil
}

func firstEnvironmentValue(keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func normalizeRouterUpstream(upstream *RouterUpstream) {
	if upstream == nil {
		return
	}
	upstream.ID = strings.TrimSpace(upstream.ID)
	upstream.Name = strings.TrimSpace(upstream.Name)
	upstream.Protocol = RouterProtocol(strings.ToLower(strings.TrimSpace(string(upstream.Protocol))))
	upstream.BaseURL = strings.TrimRight(strings.TrimSpace(upstream.BaseURL), "/")
	upstream.Auth.Type = RouterAuthType(strings.ToLower(strings.TrimSpace(string(upstream.Auth.Type))))
	if upstream.Auth.Type == "" {
		upstream.Auth.Type = RouterAuthNone
	}
	upstream.Auth.SecretRef = strings.TrimSpace(upstream.Auth.SecretRef)
	upstream.Auth.Header = strings.TrimSpace(upstream.Auth.Header)
	upstream.Headers = NormalizeHeaders(upstream.Headers)
	upstream.Capabilities.Endpoints = normalizeRouterEndpoints(upstream.Capabilities.Endpoints)
	upstream.HealthCheck.Mode = strings.ToLower(strings.TrimSpace(upstream.HealthCheck.Mode))
	if upstream.HealthCheck.Mode == "" {
		upstream.HealthCheck.Mode = "none"
	}
	upstream.HealthCheck.Path = strings.TrimSpace(upstream.HealthCheck.Path)
	upstream.HealthCheck.Interval = strings.TrimSpace(upstream.HealthCheck.Interval)
	if upstream.HealthCheck.Mode == "http" {
		if upstream.HealthCheck.Path == "" {
			upstream.HealthCheck.Path = "/healthz"
		}
		if upstream.HealthCheck.Interval == "" {
			upstream.HealthCheck.Interval = "30s"
		}
		if upstream.HealthCheck.UnhealthyThreshold == 0 {
			upstream.HealthCheck.UnhealthyThreshold = 3
		}
		if upstream.HealthCheck.HealthyThreshold == 0 {
			upstream.HealthCheck.HealthyThreshold = 2
		}
	}
	ensureBoolDefaultTrue(&upstream.Enabled)
}

func normalizeRouterModelGroup(group *RouterModelGroup) {
	if group == nil {
		return
	}
	group.ID = strings.TrimSpace(group.ID)
	group.PublicModel = strings.TrimSpace(group.PublicModel)
	group.Capability = RouterCapability(strings.ToLower(strings.TrimSpace(string(group.Capability))))
	group.Selection.Strategy = RouterSelectionStrategy(strings.ToLower(strings.TrimSpace(string(group.Selection.Strategy))))
	if group.Selection.Strategy == "" {
		if group.Capability == RouterCapabilityImage {
			group.Selection.Strategy = RouterSelectionWeightedRoundRobin
		} else {
			group.Selection.Strategy = RouterSelectionWeightedAffinity
		}
	}
	group.Selection.AffinityTTL = strings.TrimSpace(group.Selection.AffinityTTL)
	if group.Selection.Strategy == RouterSelectionWeightedAffinity && group.Selection.AffinityTTL == "" {
		group.Selection.AffinityTTL = "720h"
	}
	ensureBoolDefaultTrue(&group.Enabled)
	for i := range group.Routes {
		route := &group.Routes[i]
		route.ID = strings.TrimSpace(route.ID)
		route.UpstreamID = strings.TrimSpace(route.UpstreamID)
		route.UpstreamModel = strings.TrimSpace(route.UpstreamModel)
		if route.Weight == 0 {
			route.Weight = 100
		}
		ensureBoolDefaultTrue(&route.Enabled)
	}
}

func normalizeRouterEndpoints(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func validateRouterUpstream(path string, upstream RouterUpstream, add func(string, ...any)) {
	validateRouterIdentifier(path+".id", upstream.ID, add)
	if upstream.Name == "" {
		add("%s.name is required", path)
	}

	switch upstream.Protocol {
	case RouterProtocolOpenAIResponses, RouterProtocolOpenAIChatCompletions, RouterProtocolAnthropicMessages:
	default:
		add("%s.protocol %q is unsupported", path, upstream.Protocol)
	}

	validateRouterBaseURL(path+".base-url", upstream.BaseURL, add)
	validateRouterAuth(path+".auth", upstream.Auth, add)
	validateRouterHeaders(path+".headers", upstream.Headers, add)

	if len(upstream.Capabilities.Endpoints) == 0 {
		add("%s.capabilities.endpoints must contain at least one endpoint", path)
	}
	allowedEndpoints := map[string]struct{}{
		RouterEndpointResponses:       {},
		RouterEndpointChatCompletions: {},
		RouterEndpointMessages:        {},
		RouterEndpointCountTokens:     {},
		RouterEndpointImages:          {},
	}
	for _, endpoint := range upstream.Capabilities.Endpoints {
		if _, ok := allowedEndpoints[endpoint]; !ok {
			add("%s.capabilities.endpoints contains unsupported endpoint %q", path, endpoint)
		}
	}

	switch upstream.Protocol {
	case RouterProtocolOpenAIResponses:
		if !upstream.Capabilities.Supports(RouterEndpointResponses) {
			add("%s.capabilities.endpoints must include %q for protocol %q", path, RouterEndpointResponses, upstream.Protocol)
		}
	case RouterProtocolOpenAIChatCompletions:
		if !upstream.Capabilities.Supports(RouterEndpointChatCompletions) {
			add("%s.capabilities.endpoints must include %q for protocol %q", path, RouterEndpointChatCompletions, upstream.Protocol)
		}
	case RouterProtocolAnthropicMessages:
		if !upstream.Capabilities.Supports(RouterEndpointMessages) {
			add("%s.capabilities.endpoints must include %q for protocol %q", path, RouterEndpointMessages, upstream.Protocol)
		}
	}

	hasImages := upstream.Capabilities.Supports(RouterEndpointImages)
	if hasImages != upstream.Capabilities.ImageGeneration {
		add("%s.capabilities.images endpoint and image-generation flag must be enabled together", path)
	}
	if hasImages && upstream.Protocol == RouterProtocolAnthropicMessages {
		add("%s cannot enable image generation for protocol %q", path, upstream.Protocol)
	}

	validateRouterHealthCheck(path+".health-check", upstream.HealthCheck, add)
}

func validateRouterModelGroup(path string, group RouterModelGroup, upstreamByID map[string]RouterUpstream, routeIDs map[string]struct{}, add func(string, ...any)) {
	validateRouterIdentifier(path+".id", group.ID, add)
	if group.PublicModel == "" {
		add("%s.public-model is required", path)
	}
	switch group.Capability {
	case RouterCapabilityText, RouterCapabilityImage:
	default:
		add("%s.capability %q must be %q or %q", path, group.Capability, RouterCapabilityText, RouterCapabilityImage)
	}
	switch group.Selection.Strategy {
	case RouterSelectionWeightedRoundRobin:
	case RouterSelectionWeightedAffinity:
		if _, err := parsePositiveDuration(group.Selection.AffinityTTL); err != nil {
			add("%s.selection.affinity-ttl %q must be a positive duration", path, group.Selection.AffinityTTL)
		}
	default:
		add("%s.selection.strategy %q is unsupported", path, group.Selection.Strategy)
	}

	for index, route := range group.Routes {
		routePath := fmt.Sprintf("%s.routes[%d]", path, index)
		validateRouterIdentifier(routePath+".id", route.ID, add)
		if route.ID != "" {
			if _, exists := routeIDs[route.ID]; exists {
				add("%s.id %q is duplicated across model groups", routePath, route.ID)
			}
			routeIDs[route.ID] = struct{}{}
		}
		validateRouterIdentifier(routePath+".upstream-id", route.UpstreamID, add)
		if route.UpstreamModel == "" {
			add("%s.upstream-model is required", routePath)
		}
		if route.Weight < 1 || route.Weight > 10_000 {
			add("%s.weight %d must be between 1 and 10000", routePath, route.Weight)
		}
		if route.Priority < -10_000 || route.Priority > 10_000 {
			add("%s.priority %d must be between -10000 and 10000", routePath, route.Priority)
		}

		upstream, exists := upstreamByID[route.UpstreamID]
		if !exists {
			if route.UpstreamID != "" {
				add("%s references unknown upstream %q", routePath, route.UpstreamID)
			}
			continue
		}
		switch group.Capability {
		case RouterCapabilityText:
			if !supportsRouterText(upstream.Capabilities) {
				add("%s references upstream %q without text capability", routePath, route.UpstreamID)
			}
		case RouterCapabilityImage:
			if !upstream.Capabilities.ImageGeneration || !upstream.Capabilities.Supports(RouterEndpointImages) {
				add("%s references upstream %q without image-generation capability", routePath, route.UpstreamID)
			}
		}
	}
}

func validateRouterIdentifier(path, value string, add func(string, ...any)) {
	if value == "" {
		add("%s is required", path)
		return
	}
	if !routerIdentifierPattern.MatchString(value) {
		add("%s %q must be a lowercase identifier", path, value)
	}
}

func validateRouterBaseURL(path, value string, add func(string, ...any)) {
	if value == "" {
		add("%s is required", path)
		return
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		add("%s %q must be an absolute HTTP or HTTPS URL", path, value)
		return
	}
	if parsed.User != nil {
		add("%s must not contain user information", path)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		add("%s must not contain a query or fragment", path)
	}
}

func validateRouterAuth(path string, auth RouterUpstreamAuth, add func(string, ...any)) {
	switch auth.Type {
	case RouterAuthNone:
		if auth.SecretRef != "" {
			add("%s.secret-ref must be empty for auth type %q", path, auth.Type)
		}
		if auth.Header != "" {
			add("%s.header must be empty for auth type %q", path, auth.Type)
		}
	case RouterAuthBearer, RouterAuthBasic:
		validateRouterSecretRef(path+".secret-ref", auth.SecretRef, add)
		if auth.Header != "" {
			add("%s.header must be empty for auth type %q", path, auth.Type)
		}
	case RouterAuthAPIKeyHeader:
		validateRouterSecretRef(path+".secret-ref", auth.SecretRef, add)
		if auth.Header == "" {
			add("%s.header is required for auth type %q", path, auth.Type)
		} else {
			validateRouterHeaderName(path+".header", auth.Header, add)
		}
	default:
		add("%s.type %q is unsupported", path, auth.Type)
	}
}

func validateRouterSecretRef(path, value string, add func(string, ...any)) {
	if value == "" {
		add("%s is required", path)
		return
	}
	if len(value) > 160 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") || !routerSecretRefPattern.MatchString(value) {
		add("%s %q is invalid", path, value)
	}
}

func validateRouterHeaders(path string, headers map[string]string, add func(string, ...any)) {
	if len(headers) == 0 {
		return
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	canonicalSeen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		validateRouterHeaderName(path+"."+key, key, add)
		canonical := strings.ToLower(key)
		if _, exists := canonicalSeen[canonical]; exists {
			add("%s contains duplicate header %q with different casing", path, key)
		}
		canonicalSeen[canonical] = struct{}{}
	}
}

func validateRouterHeaderName(path, name string, add func(string, ...any)) {
	if !httpguts.ValidHeaderFieldName(name) {
		add("%s %q is not a valid HTTP header name", path, name)
		return
	}
	lower := strings.ToLower(name)
	switch lower {
	case "authorization", "proxy-authorization", "host", "content-length", "cookie", "set-cookie":
		add("%s %q is reserved", path, name)
	default:
		if strings.HasPrefix(lower, "x-smartapi-") || strings.HasPrefix(lower, "x-smartrouter-") {
			add("%s %q is reserved", path, name)
		}
	}
}

func validateRouterHealthCheck(path string, health RouterHealthCheck, add func(string, ...any)) {
	switch health.Mode {
	case "none":
		if health.Path != "" || health.Interval != "" || health.UnhealthyThreshold != 0 || health.HealthyThreshold != 0 {
			add("%s fields must be empty when mode is %q", path, health.Mode)
		}
	case "http":
		if !strings.HasPrefix(health.Path, "/") || strings.HasPrefix(health.Path, "//") {
			add("%s.path %q must be an absolute request path", path, health.Path)
		}
		if strings.ContainsAny(health.Path, "?#") {
			add("%s.path %q must not contain a query or fragment", path, health.Path)
		}
		if _, err := parsePositiveDuration(health.Interval); err != nil {
			add("%s.interval %q must be a positive duration", path, health.Interval)
		}
		if health.UnhealthyThreshold < 1 {
			add("%s.unhealthy-threshold must be positive", path)
		}
		if health.HealthyThreshold < 1 {
			add("%s.healthy-threshold must be positive", path)
		}
	default:
		add("%s.mode %q must be %q or %q", path, health.Mode, "none", "http")
	}
}

func supportsRouterText(capabilities RouterCapabilities) bool {
	return capabilities.Supports(RouterEndpointResponses) ||
		capabilities.Supports(RouterEndpointChatCompletions) ||
		capabilities.Supports(RouterEndpointMessages)
}

func parsePositiveDuration(value string) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return parsed, nil
}

func ensureBoolDefaultTrue(target **bool) {
	if target == nil || *target != nil {
		return
	}
	value := true
	*target = &value
}

func boolValueDefaultTrue(value *bool) bool {
	return value == nil || *value
}

var (
	routerIdentifierPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	routerSecretRefPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._/-]{0,158}[A-Za-z0-9])?$`)
)
