package helps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type routerHostResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type routerNetworkPolicyRoundTripper struct {
	base     http.RoundTripper
	policy   config.RouterNetworkPolicy
	resolver routerHostResolver
}

func (r routerNetworkPolicyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("smart router network policy: request URL is missing")
	}
	if err := validateRouterRuntimeTarget(request.Context(), request.URL, r.policy, r.resolver); err != nil {
		return nil, err
	}
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(request)
}

func applyRouterNetworkPolicy(client *http.Client, cfg *config.Config) {
	if client == nil || cfg == nil || cfg.ServiceRole != config.ServiceRoleRouter {
		return
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = routerNetworkPolicyRoundTripper{
		base:     base,
		policy:   cfg.Router.NetworkPolicy,
		resolver: net.DefaultResolver,
	}
	client.CheckRedirect = routerRedirectPolicy(cfg.Router.NetworkPolicy)
}

func validateRouterRuntimeTarget(ctx context.Context, target *url.URL, policy config.RouterNetworkPolicy, resolver routerHostResolver) error {
	if err := policy.ValidateTargetURL(target); err != nil {
		return fmt.Errorf("smart router network policy: %w", err)
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if address, err := netip.ParseAddr(host); err == nil {
		if !policy.AllowsResolvedAddress(host, address) {
			return fmt.Errorf("smart router network policy: target address %q is not allowed", host)
		}
		return nil
	}
	if policy.AllowsPrivateHost(host) {
		return nil
	}
	if resolver == nil {
		return errors.New("smart router network policy: DNS resolver is unavailable")
	}
	addresses, errLookup := resolver.LookupNetIP(ctx, "ip", host)
	if errLookup != nil || len(addresses) == 0 {
		return fmt.Errorf("smart router network policy: target hostname %q could not be resolved", host)
	}
	for _, address := range addresses {
		if !policy.AllowsResolvedAddress(host, address) {
			return fmt.Errorf("smart router network policy: target hostname %q resolved to a disallowed address", host)
		}
	}
	return nil
}

func routerRedirectPolicy(policy config.RouterNetworkPolicy) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if request == nil || request.URL == nil || len(via) == 0 || via[0] == nil || via[0].URL == nil {
			return nil
		}
		origin := via[0].URL
		if sameRouterOrigin(request.URL, origin) {
			return nil
		}
		if strings.EqualFold(origin.Scheme, "https") && strings.EqualFold(request.URL.Scheme, "http") {
			return http.ErrUseLastResponse
		}
		if !policy.AllowsRedirectHost(request.URL.Hostname()) {
			return http.ErrUseLastResponse
		}
		if err := policy.ValidateTargetURL(request.URL); err != nil {
			return http.ErrUseLastResponse
		}
		return nil
	}
}

func sameRouterOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(canonicalRouterHost(left), canonicalRouterHost(right))
}

func canonicalRouterHost(target *url.URL) string {
	if target == nil {
		return ""
	}
	host := strings.ToLower(target.Hostname())
	port := target.Port()
	if port == "" {
		switch strings.ToLower(target.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return net.JoinHostPort(host, port)
}
