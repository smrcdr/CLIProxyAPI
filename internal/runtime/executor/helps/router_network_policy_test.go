package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type routerResolverStub struct {
	addresses []netip.Addr
	err       error
}

func (r routerResolverStub) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r.addresses...), r.err
}

type routerRoundTripperStub struct {
	called bool
}

func (r *routerRoundTripperStub) RoundTrip(*http.Request) (*http.Response, error) {
	r.called = true
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

func TestRouterNetworkPolicyRejectsPrivateDNSAnswer(t *testing.T) {
	target, _ := url.Parse("https://upstream.example.com/v1")
	err := validateRouterRuntimeTarget(
		context.Background(),
		target,
		config.RouterNetworkPolicy{},
		routerResolverStub{addresses: []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
	)
	if err == nil || !strings.Contains(err.Error(), "disallowed address") {
		t.Fatalf("validateRouterRuntimeTarget() error = %v", err)
	}
}

func TestRouterNetworkPolicyAllowsExplicitPrivateHost(t *testing.T) {
	target, _ := url.Parse("http://codex-pool:8317/healthz")
	policy := config.RouterNetworkPolicy{
		AllowHTTP:           true,
		AllowedPrivateHosts: []string{"codex-pool"},
	}
	err := validateRouterRuntimeTarget(
		context.Background(),
		target,
		policy,
		routerResolverStub{err: errors.New("must not resolve explicit private host")},
	)
	if err != nil {
		t.Fatalf("validateRouterRuntimeTarget() error = %v", err)
	}
}

func TestRouterNetworkPolicyRoundTripperStopsBlockedRequest(t *testing.T) {
	base := &routerRoundTripperStub{}
	transport := routerNetworkPolicyRoundTripper{
		base:     base,
		resolver: routerResolverStub{addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
	}
	request, _ := http.NewRequest(http.MethodGet, "https://upstream.example.com/v1", nil)
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip() error = nil")
	}
	if base.called {
		t.Fatal("blocked request reached base RoundTripper")
	}
}

func TestRouterRedirectPolicy(t *testing.T) {
	origin := &http.Request{URL: &url.URL{Scheme: "https", Host: "upstream.example.com", Path: "/v1"}}
	policy := config.RouterNetworkPolicy{AllowedRedirectHosts: []string{"login.example.com"}}
	check := routerRedirectPolicy(policy)

	allowed := &http.Request{URL: &url.URL{Scheme: "https", Host: "login.example.com", Path: "/next"}}
	if err := check(allowed, []*http.Request{origin}); err != nil {
		t.Fatalf("allowed redirect error = %v", err)
	}
	blocked := &http.Request{URL: &url.URL{Scheme: "https", Host: "metadata.internal", Path: "/"}}
	if err := check(blocked, []*http.Request{origin}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("blocked redirect error = %v", err)
	}
	downgrade := &http.Request{URL: &url.URL{Scheme: "http", Host: "login.example.com", Path: "/"}}
	if err := check(downgrade, []*http.Request{origin}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("downgrade redirect error = %v", err)
	}
}
