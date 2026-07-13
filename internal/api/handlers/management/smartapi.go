package management

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const openCodeGoBaseURL = "https://opencode.ai/zen/go"

type smartAPIKeyView struct {
	ID             string                         `json:"id"`
	MaskedKey      string                         `json:"masked_key"`
	Status         string                         `json:"status"`
	Available      bool                           `json:"available"`
	Success        int64                          `json:"success"`
	Failed         int64                          `json:"failed"`
	CooldownUntil  *time.Time                     `json:"cooldown_until"`
	RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests,omitempty"`
}

type smartAPIManagedKey struct {
	APIKey      string
	ProviderKey string
}

func (h *Handler) smartAPIEnabled(c *gin.Context) bool {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return false
	}
	h.mu.Lock()
	enabled := h.cfg != nil && h.cfg.SmartManagementEnabled
	h.mu.Unlock()
	if !enabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "smart management is disabled"})
		return false
	}
	return true
}

// GetSmartAPIOverview returns compact runtime aggregates for OpenCode Go credentials.
func (h *Handler) GetSmartAPIOverview(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	keys := h.smartAPIKeyViews()
	var available, success, failed int64
	for _, key := range keys {
		if key.Available {
			available++
		}
		success += key.Success
		failed += key.Failed
	}
	c.JSON(http.StatusOK, gin.H{
		"total":      len(keys),
		"available":  available,
		"success":    success,
		"failed":     failed,
		"updated_at": time.Now().UTC(),
	})
}

// GetSmartAPIKeys returns masked OpenCode Go keys and their in-memory runtime status.
func (h *Handler) GetSmartAPIKeys(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"keys": h.smartAPIKeyViews()})
}

// GetSmartAPIAnalytics returns persistent token usage for a supported rolling window.
func (h *Handler) GetSmartAPIAnalytics(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	windowName, window, ok := smartAPIAnalyticsWindow(c.Query("window"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be one of: 30m, 1h, 12h, 24h"})
		return
	}
	h.mu.Lock()
	store := h.smartAPIUsage
	h.mu.Unlock()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage analytics unavailable"})
		return
	}

	snapshot := store.Snapshot(window, time.Now())
	c.JSON(http.StatusOK, gin.H{
		"window":                windowName,
		"from":                  snapshot.From,
		"to":                    snapshot.To,
		"requests":              snapshot.Requests,
		"successful":            snapshot.Successful(),
		"failed":                snapshot.Failed,
		"input_tokens":          snapshot.InputTokens,
		"output_tokens":         snapshot.OutputTokens,
		"cache_read_tokens":     snapshot.CacheReadTokens,
		"cache_write_tokens":    snapshot.CacheCreationTokens,
		"uncached_input_tokens": snapshot.UncachedInputTokens(),
		"total_tokens":          snapshot.TotalTokens(),
		"cache_hit_percent":     math.Round(snapshot.CacheHitPercent()*100) / 100,
	})
}

func smartAPIAnalyticsWindow(value string) (string, time.Duration, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "1h":
		return "1h", time.Hour, true
	case "30m":
		return "30m", 30 * time.Minute, true
	case "12h":
		return "12h", 12 * time.Hour, true
	case "24h", "1d":
		return "24h", 24 * time.Hour, true
	default:
		return "", 0, false
	}
}

// RevealSmartAPIKey returns only the selected credential's raw API key.
func (h *Handler) RevealSmartAPIKey(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, provider := range h.cfg.OpenAICompatibility {
		if !isOpenCodeGoBaseURL(provider.BaseURL) {
			continue
		}
		for _, entry := range provider.APIKeyEntries {
			if smartAPIKeyID(entry.APIKey) == id {
				c.JSON(http.StatusOK, gin.H{"id": id, "api_key": entry.APIKey})
				return
			}
		}
	}
	for _, entry := range h.cfg.ClaudeKey {
		if isOpenCodeGoCredential(entry) && smartAPIKeyID(entry.APIKey) == id {
			c.JSON(http.StatusOK, gin.H{"id": id, "api_key": entry.APIKey})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
}

// PostSmartAPIKey atomically clones the complete OpenCode Go credential template.
func (h *Handler) PostSmartAPIKey(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if len(apiKey) < 12 || !strings.HasPrefix(apiKey, "sk-") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid OpenCode Go key"})
		return
	}

	h.mu.Lock()
	for _, entry := range h.smartAPIManagedKeysLocked() {
		if strings.TrimSpace(entry.APIKey) == apiKey {
			h.mu.Unlock()
			c.JSON(http.StatusConflict, gin.H{"error": "key already exists"})
			return
		}
	}
	previousConfig := h.cfg.CloneForRuntime()
	providerIndex := -1
	for i := range h.cfg.OpenAICompatibility {
		provider := &h.cfg.OpenAICompatibility[i]
		if isOpenCodeGoBaseURL(provider.BaseURL) && len(provider.Models) > 0 {
			providerIndex = i
			break
		}
	}
	if providerIndex >= 0 {
		provider := &h.cfg.OpenAICompatibility[providerIndex]
		entry := config.OpenAICompatibilityAPIKey{APIKey: apiKey}
		if len(provider.APIKeyEntries) > 0 {
			entry = provider.APIKeyEntries[0]
			entry.APIKey = apiKey
		}
		provider.APIKeyEntries = append(provider.APIKeyEntries, entry)
		h.cfg.SanitizeOpenAICompatibility()
		if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
			h.cfg.OpenAICompatibility = previousConfig.OpenAICompatibility
			h.cfg.ClaudeKey = previousConfig.ClaudeKey
			h.mu.Unlock()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save key"})
			return
		}
		snapshot := h.reloadSnapshotConfigLocked()
		h.mu.Unlock()
		h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
		c.JSON(http.StatusCreated, gin.H{"id": smartAPIKeyID(apiKey), "masked_key": maskSmartAPIKey(apiKey), "status": "created"})
		return
	}

	templateIndex := -1
	for i := range h.cfg.ClaudeKey {
		if isOpenCodeGoCredential(h.cfg.ClaudeKey[i]) && len(h.cfg.ClaudeKey[i].Models) > 0 {
			templateIndex = i
			break
		}
	}
	if templateIndex < 0 {
		h.mu.Unlock()
		c.JSON(http.StatusConflict, gin.H{"error": "OpenCode Go template not found"})
		return
	}
	entry := cloneClaudeKey(h.cfg.ClaudeKey[templateIndex])
	entry.APIKey = apiKey
	h.cfg.ClaudeKey = append(h.cfg.ClaudeKey, entry)
	h.cfg.SanitizeClaudeKeys()
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		h.cfg.OpenAICompatibility = previousConfig.OpenAICompatibility
		h.cfg.ClaudeKey = previousConfig.ClaudeKey
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save key"})
		return
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	c.JSON(http.StatusCreated, gin.H{"id": smartAPIKeyID(apiKey), "masked_key": maskSmartAPIKey(apiKey), "status": "created"})
}

func (h *Handler) GetSmartAPISettings(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	h.mu.Lock()
	value := h.cfg.StripReasoning
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"strip_reasoning": value})
}

func (h *Handler) PutSmartAPISettings(c *gin.Context) {
	if !h.smartAPIEnabled(c) {
		return
	}
	var body struct {
		StripReasoning *bool `json:"strip_reasoning"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.StripReasoning == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	h.mu.Lock()
	previous := h.cfg.StripReasoning
	h.cfg.StripReasoning = *body.StripReasoning
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		h.cfg.StripReasoning = previous
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save settings"})
		return
	}
	snapshot := h.reloadSnapshotConfigLocked()
	h.mu.Unlock()
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{"strip_reasoning": *body.StripReasoning})
}

func (h *Handler) smartAPIKeyViews() []smartAPIKeyView {
	h.mu.Lock()
	entries := h.smartAPIManagedKeysLocked()
	manager := h.authManager
	h.mu.Unlock()

	runtimeByKey := make(map[string]*coreauth.Auth)
	if manager != nil {
		for _, auth := range manager.List() {
			if auth == nil {
				continue
			}
			kind, apiKey := auth.AccountInfo()
			if strings.EqualFold(strings.TrimSpace(kind), "api_key") {
				runtimeByKey[smartAPIRuntimeKey(auth.Provider, apiKey)] = auth
			}
		}
	}

	now := time.Now()
	views := make([]smartAPIKeyView, 0, len(entries))
	for _, entry := range entries {
		view := smartAPIKeyView{ID: smartAPIKeyID(entry.APIKey), MaskedKey: maskSmartAPIKey(entry.APIKey), Status: "unknown"}
		if auth := runtimeByKey[smartAPIRuntimeKey(entry.ProviderKey, entry.APIKey)]; auth != nil {
			view.Status = string(auth.Status)
			view.Success = auth.Success
			view.Failed = auth.Failed
			view.RecentRequests = auth.RecentRequestsSnapshot(now)
			view.Available = auth.Status == coreauth.StatusActive && !auth.Disabled && !auth.Unavailable && (auth.NextRetryAfter.IsZero() || !auth.NextRetryAfter.After(now))
			if auth.NextRetryAfter.After(now) {
				cooldown := auth.NextRetryAfter.UTC()
				view.CooldownUntil = &cooldown
			}
		}
		views = append(views, view)
	}
	return views
}

func (h *Handler) smartAPIManagedKeysLocked() []smartAPIManagedKey {
	if h == nil || h.cfg == nil {
		return nil
	}
	entries := make([]smartAPIManagedKey, 0)
	seen := make(map[string]struct{})
	appendEntry := func(providerKey, apiKey string) {
		providerKey = strings.ToLower(strings.TrimSpace(providerKey))
		apiKey = strings.TrimSpace(apiKey)
		if providerKey == "" || apiKey == "" {
			return
		}
		key := smartAPIRuntimeKey(providerKey, apiKey)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		entries = append(entries, smartAPIManagedKey{APIKey: apiKey, ProviderKey: providerKey})
	}

	for _, provider := range h.cfg.OpenAICompatibility {
		if !isOpenCodeGoBaseURL(provider.BaseURL) {
			continue
		}
		providerKey := util.OpenAICompatibleProviderKey(provider.Name)
		for _, entry := range provider.APIKeyEntries {
			appendEntry(providerKey, entry.APIKey)
		}
	}
	for _, entry := range h.cfg.ClaudeKey {
		if isOpenCodeGoCredential(entry) {
			appendEntry("claude", entry.APIKey)
		}
	}
	return entries
}

func smartAPIRuntimeKey(providerKey, apiKey string) string {
	return strings.ToLower(strings.TrimSpace(providerKey)) + "\x00" + strings.TrimSpace(apiKey)
}

func isOpenCodeGoCredential(entry config.ClaudeKey) bool {
	return isOpenCodeGoBaseURL(entry.BaseURL)
}

func isOpenCodeGoBaseURL(value string) bool {
	baseURL := strings.ToLower(strings.TrimRight(strings.TrimSpace(value), "/"))
	return baseURL == openCodeGoBaseURL || strings.HasPrefix(baseURL, openCodeGoBaseURL+"/")
}

func smartAPIKeyID(apiKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return "ocg_" + hex.EncodeToString(sum[:8])
}

func maskSmartAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if len(apiKey) <= 10 {
		return "••••••••"
	}
	return apiKey[:6] + "••••••••" + apiKey[len(apiKey)-4:]
}

func cloneClaudeKey(entry config.ClaudeKey) config.ClaudeKey {
	entry.Models = append([]config.ClaudeModel(nil), entry.Models...)
	entry.ExcludedModels = append([]string(nil), entry.ExcludedModels...)
	if entry.Headers != nil {
		headers := make(map[string]string, len(entry.Headers))
		for key, value := range entry.Headers {
			headers[key] = value
		}
		entry.Headers = headers
	}
	if entry.Cloak != nil {
		cloak := *entry.Cloak
		cloak.SensitiveWords = append([]string(nil), entry.Cloak.SensitiveWords...)
		if entry.Cloak.CacheUserID != nil {
			cacheUserID := *entry.Cloak.CacheUserID
			cloak.CacheUserID = &cacheUserID
		}
		entry.Cloak = &cloak
	}
	return entry
}
