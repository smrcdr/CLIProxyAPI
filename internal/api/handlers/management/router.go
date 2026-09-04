package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/smartrouter"
)

type routerAuthInput struct {
	Type   *config.RouterAuthType `json:"type"`
	Header *string                `json:"header"`
	Secret *string                `json:"secret"`
}

type routerUpstreamInput struct {
	ID                      *string                  `json:"id"`
	Name                    *string                  `json:"name"`
	Enabled                 *bool                    `json:"enabled"`
	Protocol                *config.RouterProtocol   `json:"protocol"`
	BaseURL                 *string                  `json:"base_url"`
	Auth                    *routerAuthInput         `json:"auth"`
	Headers                 *map[string]string       `json:"headers"`
	Capabilities            *routerCapabilitiesInput `json:"capabilities"`
	HealthCheck             *routerHealthCheckInput  `json:"health_check"`
	TrustedPool             *bool                    `json:"trusted_pool"`
	ForwardSmartAPIAffinity *bool                    `json:"forward_smartapi_affinity"`
}

type routerCapabilitiesInput struct {
	Endpoints       *[]string `json:"endpoints"`
	Streaming       *bool     `json:"streaming"`
	Tools           *bool     `json:"tools"`
	VisionInput     *bool     `json:"vision_input"`
	ImageGeneration *bool     `json:"image_generation"`
}

type routerHealthCheckInput struct {
	Mode               *string `json:"mode"`
	Path               *string `json:"path"`
	Interval           *string `json:"interval"`
	UnhealthyThreshold *int    `json:"unhealthy_threshold"`
	HealthyThreshold   *int    `json:"healthy_threshold"`
}

type routerNetworkPolicyInput struct {
	AllowHTTP            *bool     `json:"allow_http"`
	AllowedPrivateHosts  *[]string `json:"allowed_private_hosts"`
	AllowedPrivateCIDRs  *[]string `json:"allowed_private_cidrs"`
	AllowedRedirectHosts *[]string `json:"allowed_redirect_hosts"`
}

type routerNetworkPolicyDTO struct {
	AllowHTTP            bool     `json:"allow_http"`
	AllowedPrivateHosts  []string `json:"allowed_private_hosts"`
	AllowedPrivateCIDRs  []string `json:"allowed_private_cidrs"`
	AllowedRedirectHosts []string `json:"allowed_redirect_hosts"`
}

type routerAuthDTO struct {
	Type       config.RouterAuthType `json:"type"`
	Header     string                `json:"header,omitempty"`
	Configured bool                  `json:"configured"`
	UpdatedAt  *time.Time            `json:"updated_at,omitempty"`
}

type routerUpstreamDTO struct {
	ID                      string                `json:"id"`
	Name                    string                `json:"name"`
	Enabled                 bool                  `json:"enabled"`
	Protocol                config.RouterProtocol `json:"protocol"`
	BaseURL                 string                `json:"base_url"`
	Auth                    routerAuthDTO         `json:"auth"`
	Headers                 map[string]string     `json:"headers,omitempty"`
	Capabilities            routerCapabilitiesDTO `json:"capabilities"`
	HealthCheck             routerHealthCheckDTO  `json:"health_check"`
	TrustedPool             bool                  `json:"trusted_pool"`
	ForwardSmartAPIAffinity bool                  `json:"forward_smartapi_affinity"`
}

type routerCapabilitiesDTO struct {
	Endpoints       []string `json:"endpoints"`
	Streaming       bool     `json:"streaming"`
	Tools           bool     `json:"tools"`
	VisionInput     bool     `json:"vision_input"`
	ImageGeneration bool     `json:"image_generation"`
}

type routerHealthCheckDTO struct {
	Mode               string `json:"mode"`
	Path               string `json:"path,omitempty"`
	Interval           string `json:"interval,omitempty"`
	UnhealthyThreshold int    `json:"unhealthy_threshold,omitempty"`
	HealthyThreshold   int    `json:"healthy_threshold,omitempty"`
}

type routerModelGroupInput struct {
	ID          *string                  `json:"id"`
	PublicModel *string                  `json:"public_model"`
	Enabled     *bool                    `json:"enabled"`
	Capability  *config.RouterCapability `json:"capability"`
	Selection   *routerSelectionInput    `json:"selection"`
	Routes      *[]routerRouteInput      `json:"routes"`
}

type routerSelectionInput struct {
	Strategy    *config.RouterSelectionStrategy `json:"strategy"`
	AffinityTTL *string                         `json:"affinity_ttl"`
}

type routerRouteInput struct {
	ID            *string `json:"id"`
	UpstreamID    *string `json:"upstream_id"`
	UpstreamModel *string `json:"upstream_model"`
	Priority      *int    `json:"priority"`
	Weight        *int    `json:"weight"`
	Enabled       *bool   `json:"enabled"`
}

type routerModelGroupDTO struct {
	ID          string                  `json:"id"`
	PublicModel string                  `json:"public_model"`
	Enabled     bool                    `json:"enabled"`
	Capability  config.RouterCapability `json:"capability"`
	Selection   routerSelectionDTO      `json:"selection"`
	Routes      []routerRouteDTO        `json:"routes"`
}

type routerSelectionDTO struct {
	Strategy    config.RouterSelectionStrategy `json:"strategy"`
	AffinityTTL string                         `json:"affinity_ttl,omitempty"`
}

type routerRouteDTO struct {
	ID            string `json:"id"`
	UpstreamID    string `json:"upstream_id"`
	UpstreamModel string `json:"upstream_model"`
	Priority      int    `json:"priority"`
	Weight        int    `json:"weight"`
	Enabled       bool   `json:"enabled"`
}

type routerRouteStateDTO struct {
	RouteID                   string                           `json:"route_id"`
	UpstreamID                string                           `json:"upstream_id"`
	ModelGroupID              string                           `json:"model_group_id"`
	Enabled                   bool                             `json:"enabled"`
	State                     smartrouter.CircuitState         `json:"state"`
	OpenUntil                 *time.Time                       `json:"open_until,omitempty"`
	RequiresReset             bool                             `json:"requires_reset"`
	ProbeReady                bool                             `json:"probe_ready"`
	FailureCount              int                              `json:"failure_count"`
	CooldownLevel             int                              `json:"cooldown_level"`
	CircuitTransitions        uint64                           `json:"circuit_transitions"`
	LastCategory              smartrouter.FailureCategory      `json:"last_error_category,omitempty"`
	LastFailureAt             *time.Time                       `json:"last_failure_at,omitempty"`
	LastSuccessAt             *time.Time                       `json:"last_success_at,omitempty"`
	TransportHealth           smartrouter.TransportHealthState `json:"transport_health"`
	HealthConsecutiveSuccess  int                              `json:"health_consecutive_success"`
	HealthConsecutiveFailures int                              `json:"health_consecutive_failures"`
	LastHealthCheckedAt       *time.Time                       `json:"last_health_checked_at,omitempty"`
	LastHealthStatusCode      int                              `json:"last_health_status_code,omitempty"`
	LastHealthCategory        smartrouter.FailureCategory      `json:"last_health_error_category,omitempty"`
	LastHealthTransportError  bool                             `json:"last_health_transport_error"`
	LastHealthLatencyMS       int64                            `json:"last_health_latency_ms,omitempty"`
}

type routerProbeDTO struct {
	UpstreamID     string                      `json:"upstream_id"`
	RouteID        string                      `json:"route_id,omitempty"`
	Healthy        bool                        `json:"healthy"`
	StatusCode     int                         `json:"status_code,omitempty"`
	Category       smartrouter.FailureCategory `json:"category,omitempty"`
	TransportError bool                        `json:"transport_error"`
	CheckedAt      time.Time                   `json:"checked_at"`
	LatencyMS      int64                       `json:"latency_ms"`
}

func (h *Handler) GetRouterUpstreamTypes(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"schema_version": 1,
		"fields": []gin.H{
			{"name": "id", "type": "identifier", "required": true, "mutable": false},
			{"name": "name", "type": "string", "required": true},
			{"name": "enabled", "type": "boolean", "default": true},
			{"name": "base_url", "type": "url", "required": true},
			{"name": "headers", "type": "string_map", "required": false},
			{"name": "trusted_pool", "type": "boolean", "default": false},
			{"name": "forward_smartapi_affinity", "type": "boolean", "default": false},
			{"name": "capabilities", "type": "capabilities", "required": true},
			{"name": "health_check", "type": "health_check", "required": false},
		},
		"auth_types": []gin.H{
			{"id": config.RouterAuthNone, "fields": []gin.H{}},
			{"id": config.RouterAuthBearer, "fields": []gin.H{
				{"name": "secret", "type": "secret", "required_on_create": true, "write_only": true},
			}},
			{"id": config.RouterAuthAPIKeyHeader, "fields": []gin.H{
				{"name": "header", "type": "header_name", "required": true},
				{"name": "secret", "type": "secret", "required_on_create": true, "write_only": true},
			}},
			{"id": config.RouterAuthBasic, "fields": []gin.H{
				{"name": "secret", "type": "secret", "required_on_create": true, "write_only": true},
			}},
		},
		"types": []gin.H{
			{
				"id":                "openai-responses",
				"label":             "OpenAI Responses",
				"protocol":          config.RouterProtocolOpenAIResponses,
				"auth_types":        []config.RouterAuthType{config.RouterAuthNone, config.RouterAuthBearer, config.RouterAuthAPIKeyHeader, config.RouterAuthBasic},
				"allowed_endpoints": []string{config.RouterEndpointResponses, config.RouterEndpointCountTokens, config.RouterEndpointImages},
				"default_endpoints": []string{config.RouterEndpointResponses},
			},
			{
				"id":                "openai-chat-completions",
				"label":             "OpenAI Chat Completions",
				"protocol":          config.RouterProtocolOpenAIChatCompletions,
				"auth_types":        []config.RouterAuthType{config.RouterAuthNone, config.RouterAuthBearer, config.RouterAuthAPIKeyHeader, config.RouterAuthBasic},
				"allowed_endpoints": []string{config.RouterEndpointChatCompletions},
				"default_endpoints": []string{config.RouterEndpointChatCompletions},
			},
			{
				"id":                "anthropic-messages",
				"label":             "Anthropic Messages",
				"protocol":          config.RouterProtocolAnthropicMessages,
				"auth_types":        []config.RouterAuthType{config.RouterAuthNone, config.RouterAuthBearer, config.RouterAuthAPIKeyHeader, config.RouterAuthBasic},
				"allowed_endpoints": []string{config.RouterEndpointMessages, config.RouterEndpointCountTokens},
				"default_endpoints": []string{config.RouterEndpointMessages},
			},
		},
	})
}

func (h *Handler) GetRouterModelGroupSchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"schema_version": 1,
		"fields": []gin.H{
			{"name": "id", "type": "identifier", "required": true, "mutable": false},
			{"name": "public_model", "type": "string", "required": true},
			{"name": "enabled", "type": "boolean", "default": true},
			{"name": "capability", "type": "enum", "required": true, "values": []config.RouterCapability{config.RouterCapabilityText, config.RouterCapabilityImage}},
			{"name": "selection", "type": "selection", "required": false},
			{"name": "routes", "type": "route_list", "required": false},
		},
		"capabilities": []config.RouterCapability{config.RouterCapabilityText, config.RouterCapabilityImage},
		"selection_strategies": []config.RouterSelectionStrategy{
			config.RouterSelectionWeightedRoundRobin,
			config.RouterSelectionWeightedAffinity,
		},
		"route_fields": []gin.H{
			{"name": "id", "type": "identifier", "required": true, "mutable": false},
			{"name": "upstream_id", "type": "upstream_reference", "required": true},
			{"name": "upstream_model", "type": "string", "required": true},
			{"name": "priority", "type": "integer", "default": 0, "minimum": -10000, "maximum": 10000},
			{"name": "weight", "type": "integer", "default": 100, "minimum": 1, "maximum": 10000},
			{"name": "enabled", "type": "boolean", "default": true},
		},
	})
}

func (h *Handler) GetRouterNetworkPolicySchema(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"schema_version": 1,
		"fields": []gin.H{
			{"name": "allow_http", "type": "boolean", "default": false},
			{"name": "allowed_private_hosts", "type": "hostname_list", "default": []string{}},
			{"name": "allowed_private_cidrs", "type": "cidr_list", "default": []string{}},
			{"name": "allowed_redirect_hosts", "type": "hostname_list", "default": []string{}},
		},
	})
}

func (h *Handler) GetRouterNetworkPolicy(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{
		"revision":       document.Revision,
		"network_policy": routerNetworkPolicyResponse(document.Router.NetworkPolicy),
	})
}

func (h *Handler) PatchRouterNetworkPolicy(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	var input routerNetworkPolicyInput
	if !bindRouterJSON(c, &input) {
		return
	}
	applyRouterNetworkPolicyInput(&document.Router.NetworkPolicy, input)
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(
		c,
		expected,
		document.Router,
		nil,
		"patch",
		"network_policy",
		"router",
	))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) ListRouterUpstreams(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	items := make([]routerUpstreamDTO, 0, len(document.Router.Upstreams))
	for index := range document.Router.Upstreams {
		dto, errDTO := routerUpstreamResponse(c, service, document.Router.Upstreams[index])
		if errDTO != nil {
			writeRouterManagementError(c, errDTO)
			return
		}
		items = append(items, dto)
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "items": items})
}

func (h *Handler) CreateRouterUpstream(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	var input routerUpstreamInput
	if !bindRouterJSON(c, &input) {
		return
	}
	upstream := config.RouterUpstream{}
	secret, errApply := applyRouterUpstreamInput(&upstream, input, true)
	if errApply != nil {
		writeRouterManagementError(c, errApply)
		return
	}
	if errSecret := ensureRouterUpstreamSecret(c, service, upstream, secret); errSecret != nil {
		clear(secret)
		writeRouterManagementError(c, errSecret)
		return
	}
	for index := range document.Router.Upstreams {
		if document.Router.Upstreams[index].ID == upstream.ID {
			clear(secret)
			c.JSON(http.StatusConflict, gin.H{"error": "upstream already exists"})
			return
		}
	}
	document.Router.Upstreams = append(document.Router.Upstreams, upstream)
	mutations := routerUpstreamSecretMutations(upstream, secret, false)
	defer clearRouterSecretMutations(mutations)
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, mutations, "create", "upstream", upstream.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusCreated)
}

func (h *Handler) GetRouterUpstream(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	upstream, _, found := findRouterUpstream(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
		return
	}
	dto, errDTO := routerUpstreamResponse(c, service, upstream)
	if errDTO != nil {
		writeRouterManagementError(c, errDTO)
		return
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "upstream": dto})
}

func (h *Handler) PatchRouterUpstream(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	upstream, index, found := findRouterUpstream(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
		return
	}
	var input routerUpstreamInput
	if !bindRouterJSON(c, &input) {
		return
	}
	if input.ID != nil && strings.TrimSpace(*input.ID) != upstream.ID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "upstream id cannot be changed"})
		return
	}
	previousReference := upstream.Auth.SecretRef
	secret, errApply := applyRouterUpstreamInput(&upstream, input, false)
	if errApply != nil {
		writeRouterManagementError(c, errApply)
		return
	}
	if errSecret := ensureRouterUpstreamSecret(c, service, upstream, secret); errSecret != nil {
		clear(secret)
		writeRouterManagementError(c, errSecret)
		return
	}
	document.Router.Upstreams[index] = upstream
	mutations := routerUpstreamSecretMutations(upstream, secret, previousReference != "" && upstream.Auth.Type == config.RouterAuthNone)
	defer clearRouterSecretMutations(mutations)
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, mutations, "patch", "upstream", upstream.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) DeleteRouterUpstream(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	upstream, index, found := findRouterUpstream(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
		return
	}
	for groupIndex := range document.Router.ModelGroups {
		for routeIndex := range document.Router.ModelGroups[groupIndex].Routes {
			if document.Router.ModelGroups[groupIndex].Routes[routeIndex].UpstreamID == upstream.ID {
				c.JSON(http.StatusConflict, gin.H{"error": "upstream is referenced by a model route"})
				return
			}
		}
	}
	document.Router.Upstreams = append(document.Router.Upstreams[:index], document.Router.Upstreams[index+1:]...)
	var mutations []smartrouter.RouterSecretMutation
	if upstream.Auth.SecretRef != "" {
		mutations = []smartrouter.RouterSecretMutation{{Reference: upstream.Auth.SecretRef, Delete: true}}
	}
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, mutations, "delete", "upstream", upstream.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) EnableRouterUpstream(c *gin.Context) {
	h.setRouterUpstreamEnabled(c, true)
}

func (h *Handler) DisableRouterUpstream(c *gin.Context) {
	h.setRouterUpstreamEnabled(c, false)
}

func (h *Handler) setRouterUpstreamEnabled(c *gin.Context, enabled bool) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	upstream, index, found := findRouterUpstream(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "upstream not found"})
		return
	}
	upstream.Enabled = boolPointer(enabled)
	document.Router.Upstreams[index] = upstream
	action := "enable"
	if !enabled {
		action = "disable"
	}
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, action, "upstream", upstream.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) TestRouterUpstream(c *gin.Context) {
	prober := h.routerProberForRequest(c)
	if prober == nil {
		return
	}
	result, err := prober.ProbeRouterUpstream(c.Request.Context(), c.Param("id"))
	writeRouterProbeResult(c, result, err)
}

func (h *Handler) ListRouterModelGroups(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	writeRouterRevision(c, document.Revision)
	items := make([]routerModelGroupDTO, 0, len(document.Router.ModelGroups))
	for index := range document.Router.ModelGroups {
		items = append(items, routerModelGroupResponse(document.Router.ModelGroups[index]))
	}
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "items": items})
}

func (h *Handler) CreateRouterModelGroup(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	var input routerModelGroupInput
	if !bindRouterJSON(c, &input) {
		return
	}
	group := config.RouterModelGroup{}
	applyRouterModelGroupInput(&group, input)
	for index := range document.Router.ModelGroups {
		if document.Router.ModelGroups[index].ID == group.ID {
			c.JSON(http.StatusConflict, gin.H{"error": "model group already exists"})
			return
		}
	}
	document.Router.ModelGroups = append(document.Router.ModelGroups, group)
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, "create", "model_group", group.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusCreated)
}

func (h *Handler) GetRouterModelGroup(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	group, _, found := findRouterModelGroup(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "model_group": routerModelGroupResponse(group)})
}

func (h *Handler) PatchRouterModelGroup(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	group, index, found := findRouterModelGroup(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	var input routerModelGroupInput
	if !bindRouterJSON(c, &input) {
		return
	}
	if input.ID != nil && strings.TrimSpace(*input.ID) != group.ID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "model group id cannot be changed"})
		return
	}
	applyRouterModelGroupInput(&group, input)
	document.Router.ModelGroups[index] = group
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, "patch", "model_group", group.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) DeleteRouterModelGroup(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	group, index, found := findRouterModelGroup(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	document.Router.ModelGroups = append(document.Router.ModelGroups[:index], document.Router.ModelGroups[index+1:]...)
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, "delete", "model_group", group.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) CreateRouterRoute(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	group, groupIndex, found := findRouterModelGroup(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	var input routerRouteInput
	if !bindRouterJSON(c, &input) {
		return
	}
	route := config.RouterRoute{}
	applyRouterRouteInput(&route, input)
	for index := range group.Routes {
		if group.Routes[index].ID == route.ID {
			c.JSON(http.StatusConflict, gin.H{"error": "route already exists"})
			return
		}
	}
	group.Routes = append(group.Routes, route)
	document.Router.ModelGroups[groupIndex] = group
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, "create", "route", route.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusCreated)
}

func (h *Handler) PatchRouterRoute(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	group, groupIndex, found := findRouterModelGroup(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	route, routeIndex, found := findRouterRoute(group, c.Param("route_id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
		return
	}
	var input routerRouteInput
	if !bindRouterJSON(c, &input) {
		return
	}
	if input.ID != nil && strings.TrimSpace(*input.ID) != route.ID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "route id cannot be changed"})
		return
	}
	applyRouterRouteInput(&route, input)
	group.Routes[routeIndex] = route
	document.Router.ModelGroups[groupIndex] = group
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, "patch", "route", route.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) DeleteRouterRoute(c *gin.Context) {
	service, document, expected, ok := h.routerMutationContext(c)
	if !ok {
		return
	}
	group, groupIndex, found := findRouterModelGroup(document.Router, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	route, routeIndex, found := findRouterRoute(group, c.Param("route_id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
		return
	}
	group.Routes = append(group.Routes[:routeIndex], group.Routes[routeIndex+1:]...)
	document.Router.ModelGroups[groupIndex] = group
	persisted, err := service.Replace(c.Request.Context(), routerMutationForRequest(c, expected, document.Router, nil, "delete", "route", route.ID))
	writeRouterMutationResult(c, persisted, err, http.StatusOK)
}

func (h *Handler) ValidateRouterModelGroup(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	if _, _, found := findRouterModelGroup(document.Router, c.Param("id")); !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "model group not found"})
		return
	}
	if err = service.Validate(document.Router, document.Revision); err != nil {
		writeRouterManagementError(c, err)
		return
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "valid": true})
}

func (h *Handler) GetRouterState(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{
		"revision":     document.Revision,
		"upstreams":    len(document.Router.Upstreams),
		"model_groups": len(document.Router.ModelGroups),
		"routes":       countRouterRoutes(document.Router),
	})
}

func (h *Handler) GetRouterMetrics(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	h.mu.Lock()
	selector := h.routerSelector
	h.mu.Unlock()
	metrics := smartrouter.NewRouterMetrics().Snapshot()
	if selector != nil && selector.Metrics() != nil {
		metrics = selector.Metrics().Snapshot()
	}
	routes := make([]routerRouteStateDTO, 0, countRouterRoutes(document.Router))
	for groupIndex := range document.Router.ModelGroups {
		group := document.Router.ModelGroups[groupIndex]
		for routeIndex := range group.Routes {
			routes = append(routes, h.routerRouteState(group.ID, group.Routes[routeIndex]))
		}
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{
		"revision": document.Revision,
		"metrics":  metrics,
		"routes":   routes,
	})
}

func (h *Handler) ListRouterRouteStates(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	items := make([]routerRouteStateDTO, 0, countRouterRoutes(document.Router))
	for groupIndex := range document.Router.ModelGroups {
		group := document.Router.ModelGroups[groupIndex]
		for routeIndex := range group.Routes {
			items = append(items, h.routerRouteState(group.ID, group.Routes[routeIndex]))
		}
	}
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "items": items})
}

func (h *Handler) GetRouterRouteState(c *gin.Context) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	for groupIndex := range document.Router.ModelGroups {
		group := document.Router.ModelGroups[groupIndex]
		if route, _, found := findRouterRoute(group, c.Param("route_id")); found {
			writeRouterRevision(c, document.Revision)
			c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "route": h.routerRouteState(group.ID, route)})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
}

func (h *Handler) ProbeRouterRoute(c *gin.Context) {
	prober := h.routerProberForRequest(c)
	if prober == nil {
		return
	}
	result, err := prober.ProbeRouterRoute(c.Request.Context(), c.Param("route_id"))
	writeRouterProbeResult(c, result, err)
}

func (h *Handler) ResetRouterRouteCircuit(c *gin.Context) {
	h.mu.Lock()
	selector := h.routerSelector
	h.mu.Unlock()
	if selector == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "router runtime is unavailable"})
		return
	}
	routeID := strings.TrimSpace(c.Param("route_id"))
	service := h.routerManagementForRequest(c)
	if service == nil {
		return
	}
	document, err := service.Load(c.Request.Context())
	if err != nil {
		writeRouterManagementError(c, err)
		return
	}
	if !routerRouteExists(document.Router, routeID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
		return
	}
	selector.ResetCircuit(routeID)
	writeRouterRevision(c, document.Revision)
	c.JSON(http.StatusOK, gin.H{"revision": document.Revision, "route_id": routeID, "state": smartrouter.CircuitClosed})
}

func (h *Handler) routerManagementForRequest(c *gin.Context) *smartrouter.RouterManagementService {
	h.mu.Lock()
	service := h.routerManagement
	h.mu.Unlock()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "router management is unavailable"})
		return nil
	}
	return service
}

func (h *Handler) routerProberForRequest(c *gin.Context) smartrouter.RouterProber {
	h.mu.Lock()
	prober := h.routerProber
	h.mu.Unlock()
	if prober == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "router probe runtime is unavailable"})
	}
	return prober
}

func (h *Handler) routerMutationContext(c *gin.Context) (*smartrouter.RouterManagementService, smartrouter.RouterMetadataDocument, uint64, bool) {
	service := h.routerManagementForRequest(c)
	if service == nil {
		return nil, smartrouter.RouterMetadataDocument{}, 0, false
	}
	expected, errRevision := parseRouterIfMatch(c.GetHeader("If-Match"))
	if errRevision != nil {
		c.JSON(http.StatusPreconditionRequired, gin.H{"error": errRevision.Error()})
		return nil, smartrouter.RouterMetadataDocument{}, 0, false
	}
	document, errLoad := service.Load(c.Request.Context())
	if errLoad != nil {
		writeRouterManagementError(c, errLoad)
		return nil, smartrouter.RouterMetadataDocument{}, 0, false
	}
	return service, document, expected, true
}

func routerMutationForRequest(c *gin.Context, expected uint64, router config.RouterConfig, secrets []smartrouter.RouterSecretMutation, action, resourceType, resourceID string) smartrouter.RouterMutation {
	actor := ""
	if c != nil {
		actor = c.ClientIP()
	}
	return smartrouter.RouterMutation{
		ExpectedRevision: expected,
		Router:           router,
		Secrets:          secrets,
		Actor:            actor,
		Action:           action,
		ResourceType:     resourceType,
		ResourceID:       resourceID,
	}
}

func bindRouterJSON(c *gin.Context, destination any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return false
	}
	return true
}

func applyRouterUpstreamInput(upstream *config.RouterUpstream, input routerUpstreamInput, creating bool) ([]byte, error) {
	if upstream == nil {
		return nil, errors.New("upstream is nil")
	}
	if input.ID != nil {
		upstream.ID = strings.TrimSpace(*input.ID)
	}
	if input.Name != nil {
		upstream.Name = strings.TrimSpace(*input.Name)
	}
	if input.Enabled != nil {
		upstream.Enabled = boolPointer(*input.Enabled)
	}
	if input.Protocol != nil {
		upstream.Protocol = *input.Protocol
	}
	if input.BaseURL != nil {
		upstream.BaseURL = strings.TrimSpace(*input.BaseURL)
	}
	if input.Headers != nil {
		upstream.Headers = cloneRouterHeaders(*input.Headers)
	}
	if input.Capabilities != nil {
		applyRouterCapabilitiesInput(&upstream.Capabilities, *input.Capabilities)
	}
	if input.HealthCheck != nil {
		applyRouterHealthCheckInput(&upstream.HealthCheck, *input.HealthCheck)
	}
	if input.TrustedPool != nil {
		upstream.TrustedPool = *input.TrustedPool
	}
	if input.ForwardSmartAPIAffinity != nil {
		upstream.ForwardSmartAPIAffinity = *input.ForwardSmartAPIAffinity
	}
	var secret []byte
	if input.Auth != nil {
		if input.Auth.Type != nil {
			upstream.Auth.Type = *input.Auth.Type
		}
		if input.Auth.Header != nil {
			upstream.Auth.Header = strings.TrimSpace(*input.Auth.Header)
		}
		if input.Auth.Secret != nil {
			secret = []byte(*input.Auth.Secret)
		}
	}
	if upstream.Auth.Type == "" {
		upstream.Auth.Type = config.RouterAuthNone
	}
	if upstream.Auth.Type == config.RouterAuthNone {
		upstream.Auth.SecretRef = ""
		upstream.Auth.Header = ""
		clear(secret)
		secret = nil
	} else {
		upstream.Auth.SecretRef = routerUpstreamSecretReference(upstream.ID)
	}
	if creating && strings.TrimSpace(upstream.ID) == "" {
		return secret, errors.New("upstream id is required")
	}
	return secret, nil
}

func applyRouterCapabilitiesInput(capabilities *config.RouterCapabilities, input routerCapabilitiesInput) {
	if input.Endpoints != nil {
		capabilities.Endpoints = append([]string(nil), (*input.Endpoints)...)
	}
	if input.Streaming != nil {
		capabilities.Streaming = *input.Streaming
	}
	if input.Tools != nil {
		capabilities.Tools = *input.Tools
	}
	if input.VisionInput != nil {
		capabilities.VisionInput = *input.VisionInput
	}
	if input.ImageGeneration != nil {
		capabilities.ImageGeneration = *input.ImageGeneration
	}
}

func applyRouterHealthCheckInput(health *config.RouterHealthCheck, input routerHealthCheckInput) {
	if input.Mode != nil {
		health.Mode = strings.TrimSpace(*input.Mode)
	}
	if input.Path != nil {
		health.Path = strings.TrimSpace(*input.Path)
	}
	if input.Interval != nil {
		health.Interval = strings.TrimSpace(*input.Interval)
	}
	if input.UnhealthyThreshold != nil {
		health.UnhealthyThreshold = *input.UnhealthyThreshold
	}
	if input.HealthyThreshold != nil {
		health.HealthyThreshold = *input.HealthyThreshold
	}
}

func applyRouterNetworkPolicyInput(policy *config.RouterNetworkPolicy, input routerNetworkPolicyInput) {
	if input.AllowHTTP != nil {
		policy.AllowHTTP = *input.AllowHTTP
	}
	if input.AllowedPrivateHosts != nil {
		policy.AllowedPrivateHosts = append([]string(nil), (*input.AllowedPrivateHosts)...)
	}
	if input.AllowedPrivateCIDRs != nil {
		policy.AllowedPrivateCIDRs = append([]string(nil), (*input.AllowedPrivateCIDRs)...)
	}
	if input.AllowedRedirectHosts != nil {
		policy.AllowedRedirectHosts = append([]string(nil), (*input.AllowedRedirectHosts)...)
	}
}

func applyRouterModelGroupInput(group *config.RouterModelGroup, input routerModelGroupInput) {
	if input.ID != nil {
		group.ID = strings.TrimSpace(*input.ID)
	}
	if input.PublicModel != nil {
		group.PublicModel = strings.TrimSpace(*input.PublicModel)
	}
	if input.Enabled != nil {
		group.Enabled = boolPointer(*input.Enabled)
	}
	if input.Capability != nil {
		group.Capability = *input.Capability
	}
	if input.Selection != nil {
		if input.Selection.Strategy != nil {
			group.Selection.Strategy = *input.Selection.Strategy
		}
		if input.Selection.AffinityTTL != nil {
			group.Selection.AffinityTTL = strings.TrimSpace(*input.Selection.AffinityTTL)
		}
	}
	if input.Routes != nil {
		group.Routes = make([]config.RouterRoute, len(*input.Routes))
		for index := range *input.Routes {
			applyRouterRouteInput(&group.Routes[index], (*input.Routes)[index])
		}
	}
}

func applyRouterRouteInput(route *config.RouterRoute, input routerRouteInput) {
	if input.ID != nil {
		route.ID = strings.TrimSpace(*input.ID)
	}
	if input.UpstreamID != nil {
		route.UpstreamID = strings.TrimSpace(*input.UpstreamID)
	}
	if input.UpstreamModel != nil {
		route.UpstreamModel = strings.TrimSpace(*input.UpstreamModel)
	}
	if input.Priority != nil {
		route.Priority = *input.Priority
	}
	if input.Weight != nil {
		route.Weight = *input.Weight
	}
	if input.Enabled != nil {
		route.Enabled = boolPointer(*input.Enabled)
	}
}

func routerUpstreamResponse(c *gin.Context, service *smartrouter.RouterManagementService, upstream config.RouterUpstream) (routerUpstreamDTO, error) {
	metadata, err := service.SecretMetadata(c.Request.Context(), upstream.Auth.SecretRef)
	if err != nil {
		return routerUpstreamDTO{}, err
	}
	var updatedAt *time.Time
	if !metadata.UpdatedAt.IsZero() {
		value := metadata.UpdatedAt
		updatedAt = &value
	}
	return routerUpstreamDTO{
		ID:       upstream.ID,
		Name:     upstream.Name,
		Enabled:  upstream.IsEnabled(),
		Protocol: upstream.Protocol,
		BaseURL:  upstream.BaseURL,
		Auth: routerAuthDTO{
			Type:       upstream.Auth.Type,
			Header:     upstream.Auth.Header,
			Configured: metadata.Configured,
			UpdatedAt:  updatedAt,
		},
		Headers: cloneRouterHeaders(upstream.Headers),
		Capabilities: routerCapabilitiesDTO{
			Endpoints:       append([]string(nil), upstream.Capabilities.Endpoints...),
			Streaming:       upstream.Capabilities.Streaming,
			Tools:           upstream.Capabilities.Tools,
			VisionInput:     upstream.Capabilities.VisionInput,
			ImageGeneration: upstream.Capabilities.ImageGeneration,
		},
		HealthCheck: routerHealthCheckDTO{
			Mode:               upstream.HealthCheck.Mode,
			Path:               upstream.HealthCheck.Path,
			Interval:           upstream.HealthCheck.Interval,
			UnhealthyThreshold: upstream.HealthCheck.UnhealthyThreshold,
			HealthyThreshold:   upstream.HealthCheck.HealthyThreshold,
		},
		TrustedPool:             upstream.TrustedPool,
		ForwardSmartAPIAffinity: upstream.ForwardSmartAPIAffinity,
	}, nil
}

func routerModelGroupResponse(group config.RouterModelGroup) routerModelGroupDTO {
	routes := make([]routerRouteDTO, 0, len(group.Routes))
	for index := range group.Routes {
		route := group.Routes[index]
		routes = append(routes, routerRouteDTO{
			ID:            route.ID,
			UpstreamID:    route.UpstreamID,
			UpstreamModel: route.UpstreamModel,
			Priority:      route.Priority,
			Weight:        route.Weight,
			Enabled:       route.IsEnabled(),
		})
	}
	return routerModelGroupDTO{
		ID:          group.ID,
		PublicModel: group.PublicModel,
		Enabled:     group.IsEnabled(),
		Capability:  group.Capability,
		Selection: routerSelectionDTO{
			Strategy:    group.Selection.Strategy,
			AffinityTTL: group.Selection.AffinityTTL,
		},
		Routes: routes,
	}
}

func routerNetworkPolicyResponse(policy config.RouterNetworkPolicy) routerNetworkPolicyDTO {
	return routerNetworkPolicyDTO{
		AllowHTTP:            policy.AllowHTTP,
		AllowedPrivateHosts:  append([]string(nil), policy.AllowedPrivateHosts...),
		AllowedPrivateCIDRs:  append([]string(nil), policy.AllowedPrivateCIDRs...),
		AllowedRedirectHosts: append([]string(nil), policy.AllowedRedirectHosts...),
	}
}

func ensureRouterUpstreamSecret(c *gin.Context, service *smartrouter.RouterManagementService, upstream config.RouterUpstream, pending []byte) error {
	if upstream.Auth.Type == config.RouterAuthNone || len(pending) > 0 {
		return nil
	}
	metadata, err := service.SecretMetadata(c.Request.Context(), upstream.Auth.SecretRef)
	if err != nil {
		return err
	}
	if !metadata.Configured {
		return smartrouter.ErrRouterSecretNotFound
	}
	return nil
}

func (h *Handler) routerRouteState(groupID string, route config.RouterRoute) routerRouteStateDTO {
	h.mu.Lock()
	selector := h.routerSelector
	h.mu.Unlock()
	status := smartrouter.CircuitStatus{State: smartrouter.CircuitClosed}
	health := smartrouter.TransportHealthStatus{State: smartrouter.TransportHealthUnknown}
	if selector != nil {
		status = selector.CircuitStatus(route.ID)
		health = selector.TransportHealthStatus(route.UpstreamID)
	}
	var openUntil, lastFailureAt, lastSuccessAt *time.Time
	if !status.OpenUntil.IsZero() {
		value := status.OpenUntil
		openUntil = &value
	}
	if !status.LastFailureAt.IsZero() {
		value := status.LastFailureAt
		lastFailureAt = &value
	}
	if !status.LastSuccessAt.IsZero() {
		value := status.LastSuccessAt
		lastSuccessAt = &value
	}
	var lastHealthCheckedAt *time.Time
	if !health.LastCheckedAt.IsZero() {
		value := health.LastCheckedAt
		lastHealthCheckedAt = &value
	}
	return routerRouteStateDTO{
		RouteID:                   route.ID,
		UpstreamID:                route.UpstreamID,
		ModelGroupID:              groupID,
		Enabled:                   route.IsEnabled(),
		State:                     status.State,
		OpenUntil:                 openUntil,
		RequiresReset:             status.RequiresReset,
		ProbeReady:                status.ProbeReady,
		FailureCount:              status.FailureCount,
		CooldownLevel:             status.CooldownLevel,
		CircuitTransitions:        status.Transitions,
		LastCategory:              status.LastCategory,
		LastFailureAt:             lastFailureAt,
		LastSuccessAt:             lastSuccessAt,
		TransportHealth:           health.State,
		HealthConsecutiveSuccess:  health.ConsecutiveSuccess,
		HealthConsecutiveFailures: health.ConsecutiveFailures,
		LastHealthCheckedAt:       lastHealthCheckedAt,
		LastHealthStatusCode:      health.LastStatusCode,
		LastHealthCategory:        health.LastCategory,
		LastHealthTransportError:  health.LastTransportError,
		LastHealthLatencyMS:       health.LastLatency.Milliseconds(),
	}
}

func routerUpstreamSecretMutations(upstream config.RouterUpstream, secret []byte, deleteSecret bool) []smartrouter.RouterSecretMutation {
	reference := routerUpstreamSecretReference(upstream.ID)
	switch {
	case deleteSecret:
		return []smartrouter.RouterSecretMutation{{Reference: reference, Delete: true}}
	case len(secret) > 0:
		return []smartrouter.RouterSecretMutation{{Reference: reference, Value: secret}}
	default:
		return nil
	}
}

func clearRouterSecretMutations(mutations []smartrouter.RouterSecretMutation) {
	for index := range mutations {
		clear(mutations[index].Value)
	}
}

func routerUpstreamSecretReference(id string) string {
	return "router/upstreams/" + strings.TrimSpace(id)
}

func findRouterUpstream(router config.RouterConfig, id string) (config.RouterUpstream, int, bool) {
	id = strings.TrimSpace(id)
	for index := range router.Upstreams {
		if router.Upstreams[index].ID == id {
			return router.Upstreams[index], index, true
		}
	}
	return config.RouterUpstream{}, -1, false
}

func findRouterModelGroup(router config.RouterConfig, id string) (config.RouterModelGroup, int, bool) {
	id = strings.TrimSpace(id)
	for index := range router.ModelGroups {
		if router.ModelGroups[index].ID == id {
			return router.ModelGroups[index], index, true
		}
	}
	return config.RouterModelGroup{}, -1, false
}

func findRouterRoute(group config.RouterModelGroup, id string) (config.RouterRoute, int, bool) {
	id = strings.TrimSpace(id)
	for index := range group.Routes {
		if group.Routes[index].ID == id {
			return group.Routes[index], index, true
		}
	}
	return config.RouterRoute{}, -1, false
}

func routerRouteExists(router config.RouterConfig, id string) bool {
	for groupIndex := range router.ModelGroups {
		if _, _, found := findRouterRoute(router.ModelGroups[groupIndex], id); found {
			return true
		}
	}
	return false
}

func countRouterRoutes(router config.RouterConfig) int {
	count := 0
	for index := range router.ModelGroups {
		count += len(router.ModelGroups[index].Routes)
	}
	return count
}

func parseRouterIfMatch(value string) (uint64, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") {
		value = strings.TrimSpace(strings.TrimPrefix(value, "W/"))
	}
	value = strings.Trim(value, `"`)
	if value == "" {
		return 0, errors.New("If-Match revision is required")
	}
	revision, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, errors.New("If-Match revision is invalid")
	}
	return revision, nil
}

func writeRouterRevision(c *gin.Context, revision uint64) {
	c.Header("ETag", fmt.Sprintf(`"%d"`, revision))
}

func writeRouterMutationResult(c *gin.Context, document smartrouter.RouterMetadataDocument, err error, successStatus int) {
	if err == nil {
		writeRouterRevision(c, document.Revision)
		c.JSON(successStatus, gin.H{"revision": document.Revision})
		return
	}
	var postCommit *smartrouter.RouterPostCommitError
	if errors.As(err, &postCommit) && document.Revision > 0 {
		writeRouterRevision(c, document.Revision)
		c.JSON(successStatus, gin.H{
			"revision": document.Revision,
			"warning":  "mutation committed but audit persistence failed",
		})
		return
	}
	writeRouterManagementError(c, err)
}

func writeRouterManagementError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := "router management request failed"
	switch {
	case errors.Is(err, smartrouter.ErrRouterRevisionConflict):
		status = http.StatusConflict
		message = "router revision conflict"
	case errors.Is(err, smartrouter.ErrRouterSecretNotFound):
		status = http.StatusUnprocessableEntity
		message = "upstream secret is not configured"
	case strings.Contains(err.Error(), "validate router management document"):
		status = http.StatusUnprocessableEntity
		message = "router configuration is invalid"
	case strings.Contains(err.Error(), "secret store is not configured"):
		status = http.StatusServiceUnavailable
		message = "router secret storage is unavailable"
	}
	c.JSON(status, gin.H{"error": message})
}

func writeRouterProbeResult(c *gin.Context, result smartrouter.RouterProbeResult, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		message := "router probe failed"
		switch {
		case errors.Is(err, smartrouter.ErrRouterProbeUnavailable):
			status = http.StatusServiceUnavailable
			message = "router probe runtime is unavailable"
		case errors.Is(err, smartrouter.ErrRouterProbeUnsupported):
			status = http.StatusUnprocessableEntity
			message = "upstream has no non-inference health check"
		case strings.Contains(err.Error(), "not found"):
			status = http.StatusNotFound
			message = "router probe target not found"
		}
		c.JSON(status, gin.H{"error": message})
		return
	}
	c.JSON(http.StatusOK, gin.H{"probe": routerProbeDTO{
		UpstreamID:     result.UpstreamID,
		RouteID:        result.RouteID,
		Healthy:        result.Healthy,
		StatusCode:     result.StatusCode,
		Category:       result.Category,
		TransportError: result.TransportError,
		CheckedAt:      result.CheckedAt,
		LatencyMS:      result.Latency.Milliseconds(),
	}})
}

func boolPointer(value bool) *bool {
	return &value
}

func cloneRouterHeaders(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
