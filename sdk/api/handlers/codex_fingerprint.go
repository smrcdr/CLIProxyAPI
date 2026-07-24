package handlers

import (
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexaccount"
	"golang.org/x/net/context"
)

const SmartCLICodexAccountFingerprintHeader = "X-SmartCLI-Account-Fingerprint"

// WithCodexAccountFingerprint captures the selected Codex auth as an opaque
// HMAC value. The getter is safe to call after execution has selected an auth.
func WithCodexAccountFingerprint(ctx context.Context) (context.Context, func() string) {
	var mu sync.RWMutex
	fingerprint := ""
	child := WithSelectedAuthIDCallback(ctx, func(authID string) {
		value := codexaccount.FingerprintForAuthID(authID)
		if value == "" {
			return
		}
		mu.Lock()
		fingerprint = value
		mu.Unlock()
	})
	return child, func() string {
		mu.RLock()
		defer mu.RUnlock()
		return fingerprint
	}
}

// WriteCodexAccountFingerprint adds the internal provenance header when the
// selected auth was a Codex credential.
func WriteCodexAccountFingerprint(dst http.Header, fingerprint func() string) {
	if dst == nil || fingerprint == nil {
		return
	}
	if value := fingerprint(); value != "" {
		dst.Set(SmartCLICodexAccountFingerprintHeader, value)
	}
}
