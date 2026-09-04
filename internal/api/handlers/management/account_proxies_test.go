package management

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountProxyStoreAndDTORedactCredentials(t *testing.T) {
	handler := &Handler{
		cfg:            &config.Config{AuthDir: t.TempDir()},
		accountProxies: make(map[string]accountProxy),
	}
	username := "proxy-user"
	password := "proxy-password"
	proxy, err := normalizeAccountProxy(accountProxyInput{
		ID:       "px_test",
		Name:     "Test proxy",
		Type:     "https",
		Host:     "proxy.example.test",
		Port:     1234,
		Username: &username,
		Password: &password,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler.accountProxies[proxy.ID] = proxy
	if err := handler.saveAccountProxiesLocked(); err != nil {
		t.Fatal(err)
	}
	path, err := handler.accountProxyStorePath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy store mode = %o, want 600", info.Mode().Perm())
	}
	raw, err := json.Marshal(handler.accountProxyDTO(proxy))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{username, password} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("proxy DTO leaked %q: %s", secret, raw)
		}
	}
}

func TestSetCodexAuthProxySupportsProxyAndDirect(t *testing.T) {
	auth := &coreauth.Auth{Metadata: make(map[string]any)}
	setCodexAuthProxy(auth, "px_test", "http://user:pass@proxy.example.test:1234")
	if auth.ProxyURL == "" || codexMetadataString(auth.Metadata, accountProxyAssignmentKey) != "px_test" {
		t.Fatalf("proxy assignment was not applied: %#v", auth)
	}
	setCodexAuthProxy(auth, "", "direct")
	if auth.ProxyURL != "direct" {
		t.Fatalf("direct assignment = %q, want direct", auth.ProxyURL)
	}
	if codexMetadataString(auth.Metadata, accountProxyAssignmentKey) != "" {
		t.Fatal("direct assignment retained proxy id")
	}
}
