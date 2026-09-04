package management

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexaccount"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	codexQuotaURL                = "https://chatgpt.com/backend-api/wham/usage"
	codexAccountImportLimit      = 8 << 20
	codexQuotaResponseLimit      = 1 << 20
	codexDeviceSessionTTL        = 15 * time.Minute
	codexQuotaPollInterval       = 5 * time.Minute
	codexQuotaPollJitter         = 30 * time.Second
	codexCredentialStatusKey     = "smartcli_credential_status"
	codexManualDisabledKey       = "smartcli_manual_disabled"
	codexQuarantineReasonKey     = "smartcli_quarantine_reason"
	codexCredentialStatusReady   = "ready"
	codexCredentialStatusInvalid = "invalid"
	codexCredentialStatusPending = "quarantine"
)

type codexQuotaWindow struct {
	UsedPercent   float64    `json:"used_percent"`
	ResetAt       *time.Time `json:"reset_at,omitempty"`
	WindowSeconds int64      `json:"window_seconds,omitempty"`
}

type codexQuotaSnapshot struct {
	Primary    *codexQuotaWindow `json:"primary,omitempty"`
	Secondary  *codexQuotaWindow `json:"secondary,omitempty"`
	MeasuredAt time.Time         `json:"measured_at"`
}

type codexAccountDTO struct {
	Fingerprint      string              `json:"fingerprint"`
	Email            string              `json:"email"`
	Plan             string              `json:"plan,omitempty"`
	ProxyID          string              `json:"proxy_id,omitempty"`
	ProxyName        string              `json:"proxy_name,omitempty"`
	ProxyMode        string              `json:"proxy_mode"`
	Enabled          bool                `json:"enabled"`
	Routable         bool                `json:"routable"`
	CredentialStatus string              `json:"credential_status"`
	RoutingStatus    string              `json:"routing_status"`
	SuccessCount     int64               `json:"success_count"`
	FailureCount     int64               `json:"failure_count"`
	LastRefreshedAt  *time.Time          `json:"last_refreshed_at,omitempty"`
	NextRetryAfter   *time.Time          `json:"next_retry_after,omitempty"`
	Quota            *codexQuotaSnapshot `json:"quota,omitempty"`
}

type codexDeviceSession struct {
	ID              string
	DeviceAuthID    string
	UserCode        string
	VerificationURL string
	PollInterval    time.Duration
	CreatedAt       time.Time
	ExpiresAt       time.Time
	Status          string
	Fingerprint     string
	Error           string
	ProxyID         string
	ProxyURL        string
}

type codexDeviceSessionDTO struct {
	ID              string    `json:"id"`
	UserCode        string    `json:"user_code"`
	VerificationURL string    `json:"verification_url"`
	PollIntervalMS  int64     `json:"poll_interval_ms"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Status          string    `json:"status"`
	Fingerprint     string    `json:"fingerprint,omitempty"`
	Error           string    `json:"error,omitempty"`
	ProxyID         string    `json:"proxy_id,omitempty"`
}

type importedCodexCredential struct {
	AccessToken  string `json:"access_token"`
	AccountID    string `json:"account_id"`
	Disabled     bool   `json:"disabled"`
	Email        string `json:"email"`
	Expired      string `json:"expired"`
	IDToken      string `json:"id_token"`
	LastRefresh  string `json:"last_refresh"`
	RefreshToken string `json:"refresh_token"`
	Type         string `json:"type"`
}

type codexImportResult struct {
	Created int                     `json:"created"`
	Updated int                     `json:"updated"`
	Skipped int                     `json:"skipped"`
	Failed  int                     `json:"failed"`
	Items   []codexImportResultItem `json:"items"`
}

type codexImportResultItem struct {
	Index       int    `json:"index"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
}

func (h *Handler) StartCodexQuotaPoller(ctx context.Context) {
	if h == nil || len(h.codexFingerprintSecret) == 0 {
		return
	}
	h.codexPollerOnce.Do(func() {
		go func() {
			h.refreshAllCodexAccounts(context.WithoutCancel(ctx))
			timer := time.NewTimer(jitteredCodexPollDelay())
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					h.refreshAllCodexAccounts(context.Background())
					timer.Reset(jitteredCodexPollDelay())
				}
			}
		}()
	})
}

func jitteredCodexPollDelay() time.Duration {
	var raw [2]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return codexQuotaPollInterval
	}
	value := int(raw[0])<<8 | int(raw[1])
	span := int64(codexQuotaPollJitter * 2)
	offset := time.Duration(int64(value)%span) - codexQuotaPollJitter
	return codexQuotaPollInterval + offset
}

func (h *Handler) ListCodexAccounts(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	h.pruneCodexDeviceSessions()
	c.JSON(http.StatusOK, gin.H{
		"data":        h.codexAccountDTOs(),
		"observed_at": time.Now().UTC(),
	})
}

func (h *Handler) ImportCodexAccounts(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, codexAccountImportLimit+1))
	if err != nil || len(body) > codexAccountImportLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or oversized import body"})
		return
	}
	var records []importedCodexCredential
	var requestedProxyID *string
	if err := json.Unmarshal(body, &records); err != nil {
		var wrapped struct {
			Accounts []importedCodexCredential `json:"accounts"`
			ProxyID  *string                   `json:"proxy_id"`
		}
		if wrappedErr := json.Unmarshal(body, &wrapped); wrappedErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "body must be an account array or {accounts: [...]}"})
			return
		}
		records = wrapped.Accounts
		requestedProxyID = wrapped.ProxyID
	}
	proxyID, proxyURL := "", ""
	if requestedProxyID != nil {
		proxyID = strings.TrimSpace(*requestedProxyID)
		proxyURL = "direct"
		if proxyID != "" {
			proxy, exists := h.accountProxyByID(proxyID)
			if !exists {
				c.JSON(http.StatusBadRequest, gin.H{"error": "account proxy not found"})
				return
			}
			proxyURL = accountProxyURL(proxy)
		}
	}
	result := codexImportResult{Items: make([]codexImportResultItem, 0, len(records))}
	for index, record := range records {
		item := codexImportResultItem{Index: index}
		if record.Disabled {
			item.Status, item.Reason = "skipped", "source credential is disabled"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(record.Type), "codex") ||
			strings.TrimSpace(record.RefreshToken) == "" ||
			strings.TrimSpace(record.Email) == "" {
			item.Status, item.Reason = "skipped", "credential is not an active Codex refresh credential"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		identity := firstCodexIdentity(record.AccountID, record.Email)
		item.Fingerprint = h.codexFingerprint(identity)
		existing := h.findCodexAuthByFingerprint(item.Fingerprint)
		created := existing == nil
		auth := h.authFromImportedCodex(record, existing)
		if requestedProxyID != nil {
			setCodexAuthProxy(auth, proxyID, proxyURL)
		}
		applyCodexQuarantine(auth, "awaiting refresh and quota validation")
		if _, err := h.refreshAndMeasureCodexInMemory(c.Request.Context(), auth); err != nil {
			item.Status, item.Reason = "failed", "refresh or quota validation failed"
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		manualDisabled := existing != nil && codexMetadataBool(existing.Metadata, codexManualDisabledKey)
		auth.Disabled = manualDisabled
		auth.Status = coreauth.StatusActive
		auth.StatusMessage = ""
		if manualDisabled {
			auth.Status = coreauth.StatusDisabled
			auth.StatusMessage = "disabled via Codex account management"
		}
		auth.Metadata[codexCredentialStatusKey] = codexCredentialStatusReady
		delete(auth.Metadata, codexQuarantineReasonKey)
		auth.Metadata["disabled"] = manualDisabled
		if _, err := h.persistCodexAuth(c.Request.Context(), auth); err != nil {
			item.Status, item.Reason = "failed", "validated credential persistence failed"
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		if created {
			item.Status = "created"
			result.Created++
		} else {
			item.Status = "updated"
			result.Updated++
		}
		result.Items = append(result.Items, item)
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PatchCodexAccount(c *gin.Context) {
	h.PatchCodexAccountStatus(c)
}

func (h *Handler) PatchCodexAccountStatus(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	auth := h.findCodexAuthByFingerprint(c.Param("fingerprint"))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Codex account not found"})
		return
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled is required"})
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[codexManualDisabledKey] = !*request.Enabled
	applyAuthDisabledState(auth, !*request.Enabled)
	auth.StatusMessage = ""
	if auth.Disabled {
		auth.StatusMessage = "disabled via Codex account management"
	}
	if _, err := h.persistCodexAuth(c.Request.Context(), auth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist account status"})
		return
	}
	c.JSON(http.StatusOK, h.codexAccountDTO(auth))
}

func (h *Handler) DeleteCodexAccount(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	auth := h.findCodexAuthByFingerprint(c.Param("fingerprint"))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Codex account not found"})
		return
	}
	var request struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&request); err != nil ||
		!strings.EqualFold(strings.TrimSpace(request.Email), codexMetadataString(auth.Metadata, "email")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email confirmation does not match"})
		return
	}
	_, status, err := h.deleteAuthFileByName(c.Request.Context(), auth.FileName)
	if err != nil {
		c.JSON(status, gin.H{"error": "failed to delete Codex account"})
		return
	}
	fingerprint := h.codexFingerprintForAuth(auth)
	codexaccount.Delete(auth.ID)
	h.codexAccountMu.Lock()
	delete(h.codexQuota, fingerprint)
	h.codexAccountMu.Unlock()
	c.Status(http.StatusNoContent)
}

func (h *Handler) RefreshCodexAccountQuota(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	auth := h.findCodexAuthByFingerprint(c.Param("fingerprint"))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Codex account not found"})
		return
	}
	if _, err := h.refreshAndMeasureCodex(c.Request.Context(), auth); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "credential refresh or quota check failed"})
		return
	}
	c.JSON(http.StatusOK, h.codexAccountDTO(auth))
}

func (h *Handler) RefreshAllCodexAccountQuotas(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	refreshed, failed := h.refreshAllCodexAccounts(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"refreshed":   refreshed,
		"failed":      failed,
		"observed_at": time.Now().UTC(),
	})
}

func (h *Handler) CreateCodexDeviceSession(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	h.pruneCodexDeviceSessions()
	var request struct {
		ProxyID string `json:"proxy_id"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid device session body"})
			return
		}
	}
	request.ProxyID = strings.TrimSpace(request.ProxyID)
	proxyURL := "direct"
	if request.ProxyID != "" {
		proxy, exists := h.accountProxyByID(request.ProxyID)
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "account proxy not found"})
			return
		}
		proxyURL = accountProxyURL(proxy)
	}
	authorization, err := sdkauth.StartCodexDeviceAuthorizationWithProxyURL(
		c.Request.Context(), h.cfg, proxyURL,
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to start Codex device authorization"})
		return
	}
	id, err := randomCodexSessionID()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create device session"})
		return
	}
	session := &codexDeviceSession{
		ID:              id,
		DeviceAuthID:    authorization.DeviceAuthID,
		UserCode:        authorization.UserCode,
		VerificationURL: authorization.VerificationURL,
		PollInterval:    authorization.PollInterval,
		CreatedAt:       time.Now().UTC(),
		ExpiresAt:       authorization.ExpiresAt.UTC(),
		Status:          "pending",
		ProxyID:         request.ProxyID,
		ProxyURL:        proxyURL,
	}
	h.codexAccountMu.Lock()
	h.codexDeviceSessions[id] = session
	h.codexAccountMu.Unlock()
	c.JSON(http.StatusCreated, codexDeviceSessionView(session))
}

func (h *Handler) GetCodexDeviceSession(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	h.pruneCodexDeviceSessions()
	h.codexAccountMu.RLock()
	session := h.codexDeviceSessions[c.Param("id")]
	h.codexAccountMu.RUnlock()
	if session == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "device session not found or expired"})
		return
	}
	if session.Status != "pending" {
		c.JSON(http.StatusOK, codexDeviceSessionView(session))
		return
	}
	auth, pending, err := sdkauth.PollCodexDeviceAuthorizationWithProxyURL(
		c.Request.Context(), h.cfg, session.DeviceAuthID, session.UserCode, session.ProxyURL,
	)
	if err != nil {
		session.Status = "failed"
		session.Error = "device authorization failed"
		c.JSON(http.StatusBadGateway, codexDeviceSessionView(session))
		return
	}
	if pending {
		c.JSON(http.StatusOK, codexDeviceSessionView(session))
		return
	}
	setCodexAuthProxy(auth, session.ProxyID, session.ProxyURL)
	applyCodexQuarantine(auth, "awaiting refresh and quota validation")
	_, err = h.refreshAndMeasureCodexInMemory(c.Request.Context(), auth)
	if err != nil {
		session.Status = "failed"
		session.Error = "credential validation failed"
		c.JSON(http.StatusBadGateway, codexDeviceSessionView(session))
		return
	}
	auth.Disabled = false
	auth.Status = coreauth.StatusActive
	auth.StatusMessage = ""
	auth.Metadata[codexCredentialStatusKey] = codexCredentialStatusReady
	auth.Metadata["disabled"] = false
	delete(auth.Metadata, codexQuarantineReasonKey)
	if _, err := h.persistCodexAuth(c.Request.Context(), auth); err != nil {
		session.Status = "failed"
		session.Error = "credential persistence failed"
		c.JSON(http.StatusInternalServerError, codexDeviceSessionView(session))
		return
	}
	session.Status = "completed"
	session.Fingerprint = h.codexFingerprintForAuth(auth)
	session.DeviceAuthID = ""
	session.UserCode = ""
	c.JSON(http.StatusOK, codexDeviceSessionView(session))
}

func (h *Handler) DeleteCodexDeviceSession(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	h.codexAccountMu.Lock()
	_, exists := h.codexDeviceSessions[c.Param("id")]
	delete(h.codexDeviceSessions, c.Param("id"))
	h.codexAccountMu.Unlock()
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "device session not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) codexFingerprintReady(c *gin.Context) bool {
	if h == nil || len(h.codexFingerprintSecret) < 16 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Codex account fingerprint secret is not configured"})
		return false
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return false
	}
	return true
}

func (h *Handler) codexFingerprint(identity string) string {
	mac := hmac.New(sha256.New, h.codexFingerprintSecret)
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSpace(identity))))
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

func (h *Handler) codexFingerprintForAuth(auth *coreauth.Auth) string {
	fingerprint := h.codexFingerprint(firstCodexIdentity(
		codexMetadataString(auth.Metadata, "account_id"),
		codexMetadataString(auth.Metadata, "email"),
	))
	codexaccount.Register(auth.ID, fingerprint)
	return fingerprint
}

func firstCodexIdentity(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "missing-identity"
}

func (h *Handler) codexAuths() []*coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	all := h.authManager.List()
	out := make([]*coreauth.Auth, 0, len(all))
	for _, auth := range all {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			out = append(out, auth)
		}
	}
	return out
}

func (h *Handler) findCodexAuthByFingerprint(fingerprint string) *coreauth.Auth {
	fingerprint = strings.TrimSpace(fingerprint)
	for _, auth := range h.codexAuths() {
		if h.codexFingerprintForAuth(auth) == fingerprint {
			return auth
		}
	}
	return nil
}

func (h *Handler) codexAccountDTOs() []codexAccountDTO {
	auths := h.codexAuths()
	out := make([]codexAccountDTO, 0, len(auths))
	for _, auth := range auths {
		out = append(out, h.codexAccountDTO(auth))
	}
	return out
}

func (h *Handler) codexAccountDTO(auth *coreauth.Auth) codexAccountDTO {
	fingerprint := h.codexFingerprintForAuth(auth)
	status := codexMetadataString(auth.Metadata, codexCredentialStatusKey)
	if status == "" {
		status = codexCredentialStatusReady
	}
	routable := !auth.Disabled && !auth.Unavailable && status == codexCredentialStatusReady
	routingStatus := "available"
	switch {
	case auth.Disabled:
		routingStatus = "disabled"
	case auth.Unavailable && !auth.NextRetryAfter.IsZero():
		routingStatus = "cooldown"
	case auth.Unavailable:
		routingStatus = "unavailable"
	case !routable:
		routingStatus = "quarantine"
	}
	var lastRefresh, nextRetry *time.Time
	if !auth.LastRefreshedAt.IsZero() {
		value := auth.LastRefreshedAt.UTC()
		lastRefresh = &value
	}
	if !auth.NextRetryAfter.IsZero() {
		value := auth.NextRetryAfter.UTC()
		nextRetry = &value
	}
	h.codexAccountMu.RLock()
	quotaValue, hasQuota := h.codexQuota[fingerprint]
	h.codexAccountMu.RUnlock()
	var quota *codexQuotaSnapshot
	if hasQuota {
		copyValue := quotaValue
		quota = &copyValue
	}
	proxyID := codexMetadataString(auth.Metadata, accountProxyAssignmentKey)
	proxyName := ""
	proxyMode := "global"
	if strings.EqualFold(strings.TrimSpace(auth.ProxyURL), "direct") {
		proxyMode = "direct"
	}
	if proxyID != "" {
		proxyMode = "proxy"
		if proxy, ok := h.accountProxyByID(proxyID); ok {
			proxyName = proxy.Name
		}
	}
	return codexAccountDTO{
		Fingerprint:      fingerprint,
		Email:            codexMetadataString(auth.Metadata, "email"),
		Plan:             authAttribute(auth, "plan_type"),
		ProxyID:          proxyID,
		ProxyName:        proxyName,
		ProxyMode:        proxyMode,
		Enabled:          !auth.Disabled,
		Routable:         routable,
		CredentialStatus: status,
		RoutingStatus:    routingStatus,
		SuccessCount:     auth.Success,
		FailureCount:     auth.Failed,
		LastRefreshedAt:  lastRefresh,
		NextRetryAfter:   nextRetry,
		Quota:            quota,
	}
}

func (h *Handler) authFromImportedCodex(record importedCodexCredential, existing *coreauth.Auth) *coreauth.Auth {
	accountHash := sha256.Sum256([]byte(strings.TrimSpace(record.AccountID)))
	plan := codexPlanFromIDToken(record.IDToken)
	fileName := codexauth.CredentialFileName(record.Email, plan, hex.EncodeToString(accountHash[:])[:8], true)
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:       fileName,
		FileName: fileName,
		Provider: "codex",
		Label:    record.Email,
		Status:   coreauth.StatusDisabled,
		Disabled: true,
		Attributes: map[string]string{
			"plan_type": plan,
		},
		Metadata: map[string]any{
			"access_token":           record.AccessToken,
			"account_id":             record.AccountID,
			"email":                  record.Email,
			"expired":                record.Expired,
			"id_token":               record.IDToken,
			"last_refresh":           record.LastRefresh,
			"refresh_token":          record.RefreshToken,
			"type":                   "codex",
			codexCredentialStatusKey: codexCredentialStatusPending,
			codexManualDisabledKey:   false,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	auth.Storage = &codexauth.CodexTokenStorage{
		AccessToken:  record.AccessToken,
		AccountID:    record.AccountID,
		Email:        record.Email,
		Expire:       record.Expired,
		IDToken:      record.IDToken,
		LastRefresh:  record.LastRefresh,
		RefreshToken: record.RefreshToken,
		Type:         "codex",
	}
	if existing != nil {
		auth.ID = existing.ID
		auth.FileName = existing.FileName
		auth.Attributes = make(map[string]string, len(existing.Attributes)+1)
		for key, value := range existing.Attributes {
			auth.Attributes[key] = value
		}
		auth.Attributes["plan_type"] = plan
		auth.CreatedAt = existing.CreatedAt
		auth.Runtime = existing.Runtime
		auth.Success = existing.Success
		auth.Failed = existing.Failed
		auth.ProxyURL = existing.ProxyURL
		for _, key := range []string{"proxy_url", accountProxyAssignmentKey} {
			if value, ok := existing.Metadata[key]; ok {
				auth.Metadata[key] = value
			}
		}
		auth.Metadata[codexManualDisabledKey] = existing.Disabled ||
			codexMetadataBool(existing.Metadata, codexManualDisabledKey)
	}
	return auth
}

func codexPlanFromIDToken(idToken string) string {
	claims, err := codexauth.ParseJWTToken(strings.TrimSpace(idToken))
	if err != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
}

func applyCodexQuarantine(auth *coreauth.Auth, reason string) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata[codexCredentialStatusKey] = codexCredentialStatusPending
	auth.Metadata[codexQuarantineReasonKey] = reason
	auth.Metadata["disabled"] = true
	auth.Disabled = true
	auth.Status = coreauth.StatusDisabled
	auth.StatusMessage = "credential quarantined"
}

func (h *Handler) persistCodexAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	path, err := h.saveTokenRecord(ctx, auth)
	if err != nil {
		return path, err
	}
	if err := h.upsertAuthRecord(ctx, auth); err != nil {
		return path, err
	}
	return path, nil
}

func (h *Handler) codexRefreshLock(fingerprint string) *sync.Mutex {
	h.codexAccountMu.Lock()
	defer h.codexAccountMu.Unlock()
	lock := h.codexRefreshLocks[fingerprint]
	if lock == nil {
		lock = &sync.Mutex{}
		h.codexRefreshLocks[fingerprint] = lock
	}
	return lock
}

func (h *Handler) refreshAndMeasureCodex(ctx context.Context, auth *coreauth.Auth) (*codexQuotaSnapshot, error) {
	fingerprint := h.codexFingerprintForAuth(auth)
	lock := h.codexRefreshLock(fingerprint)
	lock.Lock()
	defer lock.Unlock()
	if _, err := h.refreshCodexAuth(ctx, auth); err != nil {
		return nil, err
	}
	return h.fetchCodexQuota(ctx, auth)
}

func (h *Handler) refreshAndMeasureCodexInMemory(ctx context.Context, auth *coreauth.Auth) (*codexQuotaSnapshot, error) {
	fingerprint := h.codexFingerprintForAuth(auth)
	lock := h.codexRefreshLock(fingerprint)
	lock.Lock()
	defer lock.Unlock()
	if _, err := h.refreshCodexAuthTokens(ctx, auth); err != nil {
		return nil, err
	}
	return h.fetchCodexQuota(ctx, auth)
}

func (h *Handler) refreshCodexAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if _, err := h.refreshCodexAuthTokens(ctx, auth); err != nil {
		return nil, err
	}
	_, err := h.persistCodexAuth(ctx, auth)
	return auth, err
}

func (h *Handler) refreshCodexAuthTokens(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	refreshToken := codexMetadataString(auth.Metadata, "refresh_token")
	if refreshToken == "" {
		return nil, errors.New("refresh token is missing")
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, defaultAPICallTimeout)
	defer cancel()
	service := codexauth.NewCodexAuthWithProxyURL(h.cfg, auth.ProxyURL)
	tokenData, err := service.RefreshTokensWithRetry(timeoutCtx, refreshToken, 1)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = tokenData.AccessToken
	auth.Metadata["id_token"] = tokenData.IDToken
	if tokenData.RefreshToken != "" {
		auth.Metadata["refresh_token"] = tokenData.RefreshToken
	}
	if tokenData.AccountID != "" {
		auth.Metadata["account_id"] = tokenData.AccountID
	}
	if tokenData.Email != "" {
		auth.Metadata["email"] = tokenData.Email
	}
	auth.Metadata["expired"] = tokenData.Expire
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	auth.Metadata["type"] = "codex"
	auth.LastRefreshedAt = time.Now().UTC()
	auth.UpdatedAt = auth.LastRefreshedAt
	auth.Storage = &codexauth.CodexTokenStorage{
		AccessToken:  codexMetadataString(auth.Metadata, "access_token"),
		AccountID:    codexMetadataString(auth.Metadata, "account_id"),
		Email:        codexMetadataString(auth.Metadata, "email"),
		Expire:       codexMetadataString(auth.Metadata, "expired"),
		IDToken:      codexMetadataString(auth.Metadata, "id_token"),
		LastRefresh:  codexMetadataString(auth.Metadata, "last_refresh"),
		RefreshToken: codexMetadataString(auth.Metadata, "refresh_token"),
		Type:         "codex",
	}
	return auth, nil
}

func (h *Handler) fetchCodexQuota(ctx context.Context, auth *coreauth.Auth) (*codexQuotaSnapshot, error) {
	accessToken := codexMetadataString(auth.Metadata, "access_token")
	accountID := codexMetadataString(auth.Metadata, "account_id")
	if accessToken == "" || accountID == "" {
		return nil, errors.New("Codex access token or account identity is missing")
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, defaultAPICallTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, codexQuotaURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("ChatGPT-Account-Id", accountID)
	request.Header.Set("Accept", "application/json")
	response, err := (&http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}).Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, codexQuotaResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > codexQuotaResponseLimit {
		return nil, errors.New("quota response exceeded limit")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota endpoint returned status %d", response.StatusCode)
	}
	snapshot, err := parseCodexQuota(body, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	fingerprint := h.codexFingerprintForAuth(auth)
	h.codexAccountMu.Lock()
	h.codexQuota[fingerprint] = *snapshot
	h.codexAccountMu.Unlock()
	return snapshot, nil
}

func parseCodexQuota(body []byte, measuredAt time.Time) (*codexQuotaSnapshot, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, errors.New("invalid quota response")
	}
	rateLimit, _ := payload["rate_limit"].(map[string]any)
	if rateLimit == nil {
		rateLimit = payload
	}
	snapshot := &codexQuotaSnapshot{
		Primary:    parseCodexQuotaWindow(rateLimit["primary_window"]),
		Secondary:  parseCodexQuotaWindow(rateLimit["secondary_window"]),
		MeasuredAt: measuredAt,
	}
	if snapshot.Primary == nil && snapshot.Secondary == nil {
		return nil, errors.New("quota response contains no supported windows")
	}
	return snapshot, nil
}

func parseCodexQuotaWindow(raw any) *codexQuotaWindow {
	value, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	window := &codexQuotaWindow{
		UsedPercent:   codexJSONFloat(value["used_percent"]),
		WindowSeconds: int64(codexJSONFloat(value["limit_window_seconds"])),
	}
	resetAt := int64(codexJSONFloat(value["reset_at"]))
	if resetAt > 0 {
		parsed := time.Unix(resetAt, 0).UTC()
		window.ResetAt = &parsed
	}
	return window
}

func codexJSONFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		number, _ := typed.Float64()
		return number
	case string:
		number, _ := strconv.ParseFloat(typed, 64)
		return number
	default:
		return 0
	}
}

func (h *Handler) refreshAllCodexAccounts(ctx context.Context) (int, int) {
	refreshed, failed := 0, 0
	for _, auth := range h.codexAuths() {
		if auth == nil || auth.Disabled && codexMetadataBool(auth.Metadata, codexManualDisabledKey) {
			continue
		}
		if _, err := h.refreshAndMeasureCodex(ctx, auth); err != nil {
			failed++
			continue
		}
		refreshed++
	}
	return refreshed, failed
}

func randomCodexSessionID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (h *Handler) pruneCodexDeviceSessions() {
	now := time.Now()
	h.codexAccountMu.Lock()
	defer h.codexAccountMu.Unlock()
	for id, session := range h.codexDeviceSessions {
		if session == nil || now.After(session.ExpiresAt) {
			delete(h.codexDeviceSessions, id)
		}
	}
}

func codexDeviceSessionView(session *codexDeviceSession) codexDeviceSessionDTO {
	return codexDeviceSessionDTO{
		ID:              session.ID,
		UserCode:        session.UserCode,
		VerificationURL: session.VerificationURL,
		PollIntervalMS:  session.PollInterval.Milliseconds(),
		CreatedAt:       session.CreatedAt,
		ExpiresAt:       session.ExpiresAt,
		Status:          session.Status,
		Fingerprint:     session.Fingerprint,
		Error:           session.Error,
		ProxyID:         session.ProxyID,
	}
}

func codexMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func codexMetadataBool(metadata map[string]any, key string) bool {
	if metadata == nil {
		return false
	}
	value, _ := metadata[key].(bool)
	return value
}
