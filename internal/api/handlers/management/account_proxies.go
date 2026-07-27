package management

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

const (
	accountProxyStoreFile       = ".smartapi-account-proxies"
	accountProxyStoreVersion    = 1
	accountProxyRequestLimit    = 64 << 10
	accountProxyTestTimeout     = 20 * time.Second
	accountProxyAssignmentKey   = "smartcli_proxy_id"
	accountProxyPublicType      = "https"
	accountProxyTransport       = "http-connect"
	accountProxyConnectivityURL = "https://auth.openai.com/"
)

var accountProxyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type accountProxy struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Username  string    `json:"username,omitempty"`
	Password  string    `json:"password,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type accountProxyStoreDocument struct {
	Version int            `json:"version"`
	Items   []accountProxy `json:"items"`
}

type accountProxyDTO struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Type                  string    `json:"type"`
	Transport             string    `json:"transport"`
	Endpoint              string    `json:"endpoint"`
	AuthConfigured        bool      `json:"auth_configured"`
	AssignedCodexAccounts int       `json:"assigned_codex_accounts"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type accountProxyInput struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Username *string `json:"username"`
	Password *string `json:"password"`
}

func (h *Handler) ListAccountProxies(c *gin.Context) {
	if err := h.ensureAccountProxies(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load account proxies"})
		return
	}
	h.accountProxyMu.RLock()
	items := make([]accountProxyDTO, 0, len(h.accountProxies))
	for _, proxy := range h.accountProxies {
		items = append(items, h.accountProxyDTO(proxy))
	}
	h.accountProxyMu.RUnlock()
	sort.Slice(items, func(i, j int) bool {
		if items[i].Name == items[j].Name {
			return items[i].ID < items[j].ID
		}
		return items[i].Name < items[j].Name
	})
	c.JSON(http.StatusOK, gin.H{"data": items, "observed_at": time.Now().UTC()})
}

func (h *Handler) CreateAccountProxy(c *gin.Context) {
	input, ok := readAccountProxyInput(c)
	if !ok {
		return
	}
	if err := h.ensureAccountProxies(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load account proxies"})
		return
	}
	h.accountProxyMu.Lock()
	defer h.accountProxyMu.Unlock()
	if input.ID == "" {
		id, err := randomAccountProxyID()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create proxy id"})
			return
		}
		input.ID = id
	}
	if _, exists := h.accountProxies[input.ID]; exists {
		c.JSON(http.StatusConflict, gin.H{"error": "account proxy already exists"})
		return
	}
	proxy, err := normalizeAccountProxy(input, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.accountProxies[proxy.ID] = proxy
	if err := h.saveAccountProxiesLocked(); err != nil {
		delete(h.accountProxies, proxy.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist account proxy"})
		return
	}
	c.JSON(http.StatusCreated, h.accountProxyDTO(proxy))
}

func (h *Handler) PatchAccountProxy(c *gin.Context) {
	input, ok := readAccountProxyInput(c)
	if !ok {
		return
	}
	if err := h.ensureAccountProxies(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load account proxies"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	h.accountProxyMu.Lock()
	existing, exists := h.accountProxies[id]
	if !exists {
		h.accountProxyMu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "account proxy not found"})
		return
	}
	input.ID = id
	proxy, err := normalizeAccountProxy(input, &existing)
	if err != nil {
		h.accountProxyMu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.accountProxies[id] = proxy
	if err := h.saveAccountProxiesLocked(); err != nil {
		h.accountProxies[id] = existing
		h.accountProxyMu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist account proxy"})
		return
	}
	h.accountProxyMu.Unlock()
	if accountProxyURL(existing) != accountProxyURL(proxy) {
		if err := h.syncAssignedCodexProxy(c.Request.Context(), proxy); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "proxy saved but account update failed"})
			return
		}
	}
	c.JSON(http.StatusOK, h.accountProxyDTO(proxy))
}

func (h *Handler) DeleteAccountProxy(c *gin.Context) {
	if err := h.ensureAccountProxies(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load account proxies"})
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if h.assignedCodexProxyCount(id) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "proxy is assigned to Codex accounts"})
		return
	}
	h.accountProxyMu.Lock()
	proxy, exists := h.accountProxies[id]
	if !exists {
		h.accountProxyMu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "account proxy not found"})
		return
	}
	delete(h.accountProxies, id)
	if err := h.saveAccountProxiesLocked(); err != nil {
		h.accountProxies[id] = proxy
		h.accountProxyMu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist account proxies"})
		return
	}
	h.accountProxyMu.Unlock()
	c.Status(http.StatusNoContent)
}

func (h *Handler) TestAccountProxy(c *gin.Context) {
	proxy, ok := h.accountProxyByID(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "account proxy not found"})
		return
	}
	transport, _, err := proxyutil.BuildHTTPTransport(accountProxyURL(proxy))
	if err != nil || transport == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account proxy configuration"})
		return
	}
	started := time.Now()
	request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodHead, accountProxyConnectivityURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create proxy test"})
		return
	}
	client := &http.Client{Transport: transport, Timeout: accountProxyTestTimeout}
	response, err := client.Do(request)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"id":         proxy.ID,
			"ok":         false,
			"latency_ms": time.Since(started).Milliseconds(),
			"error":      "proxy connection failed",
		})
		return
	}
	defer func() { _ = response.Body.Close() }()
	ok = response.StatusCode != http.StatusProxyAuthRequired && response.StatusCode < http.StatusInternalServerError
	c.JSON(http.StatusOK, gin.H{
		"id":          proxy.ID,
		"ok":          ok,
		"status_code": response.StatusCode,
		"latency_ms":  time.Since(started).Milliseconds(),
	})
}

func (h *Handler) PatchCodexAccountProxy(c *gin.Context) {
	if !h.codexFingerprintReady(c) {
		return
	}
	var request struct {
		ProxyID *string `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.ProxyID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "proxy_id is required; use an empty string for Direct"})
		return
	}
	auth := h.findCodexAuthByFingerprint(c.Param("fingerprint"))
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Codex account not found"})
		return
	}
	proxyID := strings.TrimSpace(*request.ProxyID)
	proxyURL := "direct"
	if proxyID != "" {
		proxy, exists := h.accountProxyByID(proxyID)
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "account proxy not found"})
			return
		}
		proxyURL = accountProxyURL(proxy)
	}
	setCodexAuthProxy(auth, proxyID, proxyURL)
	if _, err := h.persistCodexAuth(c.Request.Context(), auth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist account proxy assignment"})
		return
	}
	c.JSON(http.StatusOK, h.codexAccountDTO(auth))
}

func (h *Handler) accountProxyByID(id string) (accountProxy, bool) {
	if err := h.ensureAccountProxies(); err != nil {
		return accountProxy{}, false
	}
	h.accountProxyMu.RLock()
	defer h.accountProxyMu.RUnlock()
	proxy, ok := h.accountProxies[strings.TrimSpace(id)]
	return proxy, ok
}

func (h *Handler) accountProxyDTO(proxy accountProxy) accountProxyDTO {
	return accountProxyDTO{
		ID:                    proxy.ID,
		Name:                  proxy.Name,
		Type:                  accountProxyPublicType,
		Transport:             accountProxyTransport,
		Endpoint:              proxy.Host + ":" + strconv.Itoa(proxy.Port),
		AuthConfigured:        proxy.Username != "",
		AssignedCodexAccounts: h.assignedCodexProxyCount(proxy.ID),
		CreatedAt:             proxy.CreatedAt,
		UpdatedAt:             proxy.UpdatedAt,
	}
}

func (h *Handler) assignedCodexProxyCount(proxyID string) int {
	count := 0
	for _, auth := range h.codexAuths() {
		if codexMetadataString(auth.Metadata, accountProxyAssignmentKey) == proxyID {
			count++
		}
	}
	return count
}

func (h *Handler) syncAssignedCodexProxy(ctx context.Context, proxy accountProxy) error {
	for _, auth := range h.codexAuths() {
		if codexMetadataString(auth.Metadata, accountProxyAssignmentKey) != proxy.ID {
			continue
		}
		setCodexAuthProxy(auth, proxy.ID, accountProxyURL(proxy))
		if _, err := h.persistCodexAuth(ctx, auth); err != nil {
			return err
		}
	}
	return nil
}

func setCodexAuthProxy(auth *coreauth.Auth, proxyID, proxyURL string) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.ProxyURL = strings.TrimSpace(proxyURL)
	auth.Metadata["proxy_url"] = auth.ProxyURL
	if proxyID = strings.TrimSpace(proxyID); proxyID == "" {
		delete(auth.Metadata, accountProxyAssignmentKey)
	} else {
		auth.Metadata[accountProxyAssignmentKey] = proxyID
	}
	auth.UpdatedAt = time.Now().UTC()
}

func accountProxyURL(proxy accountProxy) string {
	value := &url.URL{
		Scheme: "http",
		Host:   proxy.Host + ":" + strconv.Itoa(proxy.Port),
	}
	if proxy.Username != "" {
		value.User = url.UserPassword(proxy.Username, proxy.Password)
	}
	return value.String()
}

func normalizeAccountProxy(input accountProxyInput, existing *accountProxy) (accountProxy, error) {
	now := time.Now().UTC()
	value := accountProxy{CreatedAt: now, UpdatedAt: now}
	if existing != nil {
		value = *existing
		value.UpdatedAt = now
	}
	if input.ID != "" {
		value.ID = strings.ToLower(strings.TrimSpace(input.ID))
	}
	if !accountProxyIDPattern.MatchString(value.ID) {
		return accountProxy{}, errors.New("invalid account proxy id")
	}
	if input.Name != "" || existing == nil {
		value.Name = strings.TrimSpace(input.Name)
	}
	if len(value.Name) < 1 || len(value.Name) > 100 {
		return accountProxy{}, errors.New("proxy name must contain 1 to 100 characters")
	}
	proxyType := strings.ToLower(strings.TrimSpace(input.Type))
	if proxyType != "" && proxyType != "https" && proxyType != "https-connect" && proxyType != "http-connect" {
		return accountProxy{}, errors.New("only HTTPS CONNECT proxies are supported")
	}
	if input.Host != "" || existing == nil {
		value.Host = strings.TrimSpace(input.Host)
	}
	if value.Host == "" || len(value.Host) > 253 || strings.ContainsAny(value.Host, "/@ \t\r\n") {
		return accountProxy{}, errors.New("invalid proxy host")
	}
	if input.Port != 0 || existing == nil {
		value.Port = input.Port
	}
	if value.Port < 1 || value.Port > 65535 {
		return accountProxy{}, errors.New("proxy port must be between 1 and 65535")
	}
	if input.Username != nil {
		value.Username = strings.TrimSpace(*input.Username)
	}
	if input.Password != nil {
		value.Password = *input.Password
	}
	if len(value.Username) > 500 || len(value.Password) > 500 {
		return accountProxy{}, errors.New("proxy credentials are too long")
	}
	if (value.Username == "") != (value.Password == "") {
		return accountProxy{}, errors.New("proxy username and password must be provided together")
	}
	return value, nil
}

func readAccountProxyInput(c *gin.Context) (accountProxyInput, bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, accountProxyRequestLimit+1))
	if err != nil || len(body) > accountProxyRequestLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid or oversized proxy body"})
		return accountProxyInput{}, false
	}
	var input accountProxyInput
	if err := json.Unmarshal(body, &input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proxy body"})
		return accountProxyInput{}, false
	}
	return input, true
}

func (h *Handler) ensureAccountProxies() error {
	if h == nil || h.cfg == nil {
		return errors.New("handler configuration is unavailable")
	}
	h.accountProxyLoadOnce.Do(func() {
		path, err := h.accountProxyStorePath()
		if err != nil {
			h.accountProxyLoadErr = err
			return
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			h.accountProxyLoadErr = err
			return
		}
		var document accountProxyStoreDocument
		if err := json.Unmarshal(data, &document); err != nil {
			h.accountProxyLoadErr = err
			return
		}
		if document.Version != accountProxyStoreVersion {
			h.accountProxyLoadErr = errors.New("unsupported account proxy store version")
			return
		}
		h.accountProxyMu.Lock()
		for _, proxy := range document.Items {
			h.accountProxies[proxy.ID] = proxy
		}
		h.accountProxyMu.Unlock()
	})
	return h.accountProxyLoadErr
}

func (h *Handler) saveAccountProxiesLocked() error {
	path, err := h.accountProxyStorePath()
	if err != nil {
		return err
	}
	items := make([]accountProxy, 0, len(h.accountProxies))
	for _, proxy := range h.accountProxies {
		items = append(items, proxy)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	data, err := json.MarshalIndent(accountProxyStoreDocument{
		Version: accountProxyStoreVersion,
		Items:   items,
	}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0o600)
}

func (h *Handler) accountProxyStorePath() (string, error) {
	authDir := strings.TrimSpace(h.cfg.AuthDir)
	if authDir == "" {
		return "", errors.New("auth directory is not configured")
	}
	if !filepath.IsAbs(authDir) {
		absolute, err := filepath.Abs(authDir)
		if err != nil {
			return "", err
		}
		authDir = absolute
	}
	return filepath.Join(filepath.Clean(authDir), accountProxyStoreFile), nil
}

func randomAccountProxyID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "px_" + hex.EncodeToString(value[:]), nil
}
