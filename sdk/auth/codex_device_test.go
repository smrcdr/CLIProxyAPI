package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexDeviceAuthRecordHydratesCredentialMetadata(t *testing.T) {
	authenticator := NewCodexAuthenticator()
	service := codex.NewCodexAuth(&config.Config{})
	bundle := &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			AccessToken:  "access-token",
			AccountID:    "account-id",
			Email:        "person@example.com",
			Expire:       "2026-07-24T12:00:00Z",
			IDToken:      "id-token",
			RefreshToken: "refresh-token",
		},
		LastRefresh: "2026-07-24T11:00:00Z",
	}

	record, err := authenticator.buildAuthRecord(service, bundle)
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}

	expected := map[string]string{
		"access_token":  bundle.TokenData.AccessToken,
		"account_id":    bundle.TokenData.AccountID,
		"email":         bundle.TokenData.Email,
		"expired":       bundle.TokenData.Expire,
		"id_token":      bundle.TokenData.IDToken,
		"last_refresh":  bundle.LastRefresh,
		"refresh_token": bundle.TokenData.RefreshToken,
		"type":          "codex",
	}
	for key, want := range expected {
		if got, _ := record.Metadata[key].(string); got != want {
			t.Fatalf("metadata[%q] was not hydrated", key)
		}
	}
}
