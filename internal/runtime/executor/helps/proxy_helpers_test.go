package helps

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestRejectCrossOriginRedirect(t *testing.T) {
	origin := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example.com", Path: "/v1"}}
	sameOrigin := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example.com", Path: "/healthz"}}
	if err := rejectCrossOriginRedirect(sameOrigin, []*http.Request{origin}); err != nil {
		t.Fatalf("same-origin redirect error = %v", err)
	}

	for _, target := range []*url.URL{
		{Scheme: "https", Host: "other.example.com", Path: "/healthz"},
		{Scheme: "http", Host: "upstream.example.com", Path: "/healthz"},
	} {
		err := rejectCrossOriginRedirect(&http.Request{URL: target}, []*http.Request{origin})
		if !errors.Is(err, http.ErrUseLastResponse) {
			t.Fatalf("redirect to %s error = %v", target, err)
		}
	}
}
