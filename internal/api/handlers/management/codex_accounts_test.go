package management

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestParseCodexQuotaAllowlist(t *testing.T) {
	measuredAt := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	body := []byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": 12, "reset_at": 1784811600, "limit_window_seconds": 18000},
			"secondary_window": {"used_percent": 69, "reset_at": 1785254400, "limit_window_seconds": 604800},
			"secret": "must-not-leak"
		},
		"access_token": "must-not-leak"
	}`)
	snapshot, err := parseCodexQuota(body, measuredAt)
	if err != nil {
		t.Fatalf("parseCodexQuota() error = %v", err)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 12 {
		t.Fatalf("primary quota = %#v", snapshot.Primary)
	}
	if snapshot.Secondary == nil || snapshot.Secondary.WindowSeconds != 604800 {
		t.Fatalf("secondary quota = %#v", snapshot.Secondary)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "access_token") {
		t.Fatalf("safe quota DTO leaked upstream fields: %s", raw)
	}
}

func TestCodexAccountDTORedactsCredentialSecrets(t *testing.T) {
	handler := &Handler{
		codexFingerprintSecret: []byte("0123456789abcdef0123456789abcdef"),
		codexQuota:             make(map[string]codexQuotaSnapshot),
	}
	auth := &coreauth.Auth{
		ID:       "codex-person@example.com-plus.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			"email":         "person@example.com",
			"account_id":    "account-secret",
			"access_token":  "access-secret",
			"refresh_token": "refresh-secret",
			"id_token":      "id-secret",
		},
		Attributes: map[string]string{"plan_type": "plus"},
	}
	raw, err := json.Marshal(handler.codexAccountDTO(auth))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"access-secret", "refresh-secret", "id-secret", "account-secret", auth.ID} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("Codex account DTO leaked %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), "person@example.com") {
		t.Fatalf("live DTO must contain email: %s", raw)
	}
}

func TestCodexFingerprintStableAndSecretScoped(t *testing.T) {
	first := &Handler{codexFingerprintSecret: []byte("0123456789abcdef")}
	second := &Handler{codexFingerprintSecret: []byte("fedcba9876543210")}
	got := first.codexFingerprint("Account-ID")
	if got != first.codexFingerprint(" account-id ") {
		t.Fatal("fingerprint must be stable across identity casing and whitespace")
	}
	if got == second.codexFingerprint("account-id") {
		t.Fatal("fingerprint must be scoped to its dedicated secret")
	}
	if len(got) != 24 {
		t.Fatalf("fingerprint length = %d, want 24", len(got))
	}
}

func TestCodexFingerprintSeparatesTeamMembersWithSharedAccountID(t *testing.T) {
	handler := &Handler{codexFingerprintSecret: []byte("0123456789abcdef")}
	teamAccountID := "shared-workspace-account"
	first := handler.codexFingerprint(codexIdentityForCredential(
		teamAccountID,
		"team-a@example.com",
		testCodexIDToken("user-team-a"),
	))
	second := handler.codexFingerprint(codexIdentityForCredential(
		teamAccountID,
		"team-b@example.com",
		testCodexIDToken("user-team-b"),
	))
	if first == second {
		t.Fatalf("Team members with shared account_id received the same fingerprint: %s", first)
	}
}

func testCodexIDToken(userID string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_user_id": userID},
	})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestParseCodexQuotaRejectsUnsupportedPayload(t *testing.T) {
	if _, err := parseCodexQuota([]byte(`{"token":"secret"}`), time.Now()); err == nil {
		t.Fatal("parseCodexQuota() accepted payload without supported windows")
	}
}

func TestPruneCodexDeviceSessionsRemovesExpiredCredentialsFromMemory(t *testing.T) {
	now := time.Now()
	handler := &Handler{
		codexDeviceSessions: map[string]*codexDeviceSession{
			"expired": {
				ID:           "expired",
				DeviceAuthID: "secret-device-auth",
				UserCode:     "secret-user-code",
				ExpiresAt:    now.Add(-time.Second),
			},
			"active": {
				ID:           "active",
				DeviceAuthID: "active-device-auth",
				UserCode:     "active-user-code",
				ExpiresAt:    now.Add(time.Minute),
			},
		},
	}

	handler.pruneCodexDeviceSessions()

	if _, exists := handler.codexDeviceSessions["expired"]; exists {
		t.Fatal("expired device session was retained in memory")
	}
	if _, exists := handler.codexDeviceSessions["active"]; !exists {
		t.Fatal("active device session was removed")
	}
}

func TestImportedCodexCandidatePreservesManualDisableWithoutMutatingExisting(t *testing.T) {
	handler := &Handler{}
	existing := &coreauth.Auth{
		ID:       "existing.json",
		FileName: "existing.json",
		Provider: "codex",
		Disabled: true,
		Attributes: map[string]string{
			"plan_type": "plus",
			"existing":  "keep",
		},
		Metadata: map[string]any{
			codexManualDisabledKey: true,
		},
		CreatedAt: time.Now().Add(-time.Hour),
	}
	record := importedCodexCredential{
		Email:        "person@example.com",
		AccountID:    "account-id",
		RefreshToken: "refresh-token",
		Type:         "codex",
	}

	candidate := handler.authFromImportedCodex(record, existing)

	if candidate.ID != existing.ID || candidate.FileName != existing.FileName {
		t.Fatal("re-import candidate did not retain stable credential identity")
	}
	if !codexMetadataBool(candidate.Metadata, codexManualDisabledKey) {
		t.Fatal("re-import candidate lost manual disable state")
	}
	candidate.Attributes["existing"] = "changed"
	if existing.Attributes["existing"] != "keep" {
		t.Fatal("candidate attributes alias and mutate the routable existing credential")
	}
}
