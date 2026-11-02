package lds

import (
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"

	"github.com/bpalermo/redoma/xds/internal/modelauth"
)

// envoyVirtualHost is the name of the virtual host Envoy picks for a
// request's authority, following RouteMatcher (Envoy v1.40.0,
// source/common/router/config_impl.cc): the domains are lower-cased when the
// route table is built (2062-2063) and so is the authority (2128-2138); an
// exact domain wins (2139-2142), then the longest suffix wildcard whose
// "*" stands for at least one byte, any byte, dots and colons included
// (2143-2150, findWildcardVirtualHost 2012-2032: wildcard_length >=
// host.size() is skipped), then prefix wildcards, then "*" (2159). The
// listener sets neither ignore_port_in_host_matching nor vhost_header, so
// the authority is matched whole, port included.
func envoyVirtualHost(t *testing.T, rc *routev3.RouteConfiguration, authority string) string {
	t.Helper()
	if rc.GetIgnorePortInHostMatching() || rc.GetVhostHeader() != "" {
		t.Fatalf("the egress route table changed how Envoy reads the authority; update this model")
	}
	exact := map[string]string{}
	suffixes := map[string]string{}
	deflt := ""
	for _, vh := range rc.GetVirtualHosts() {
		for _, d := range vh.GetDomains() {
			d = strings.ToLower(d)
			switch {
			case d == "*":
				deflt = vh.GetName()
			case strings.HasPrefix(d, "*"):
				suffixes[d[1:]] = vh.GetName()
			case strings.HasSuffix(d, "*"):
				t.Fatalf("prefix wildcard %q: the egress listener has none; update this model", d)
			default:
				exact[d] = vh.GetName()
			}
		}
	}
	host := strings.ToLower(authority)
	if vh, ok := exact[host]; ok {
		return vh
	}
	keys := make([]string, 0, len(suffixes))
	for s := range suffixes {
		keys = append(keys, s)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, s := range keys {
		if len(s) < len(host) && strings.HasSuffix(host, s) {
			return suffixes[s]
		}
	}
	return deflt
}

// dfpTarget is the name and port the dynamic forward proxy resolves and
// connects to for a CONNECT's authority: Http::Utility::parseAuthority
// (Envoy v1.40.0, source/common/http/utility.cc 1403-1443) splits at the
// last ':' when what follows is a port, and otherwise resolves the whole
// authority; a bracketed IPv6 literal loses its brackets.
func dfpTarget(authority string) (string, int, bool) {
	host, port := authority, 443
	if i := strings.LastIndexByte(authority, ':'); i >= 0 && !strings.HasSuffix(authority, "]") {
		if p, err := strconv.Atoi(authority[i+1:]); err == nil && p >= 0 && p <= 65535 && authority[i+1:] != "" {
			host, port = authority[:i], p
		}
	}
	ip := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	_, err := netip.ParseAddr(ip)
	return host, port, err == nil
}

func egressRoutes(t *testing.T, hosts []string) *routev3.RouteConfiguration {
	t.Helper()
	l := Listeners("/run/netns/redoma-a", "a", modelauth.APIKey, hosts)[1]
	hcm := &hcmv3.HttpConnectionManager{}
	if err := l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(hcm); err != nil {
		t.Fatal(err)
	}
	return hcm.GetRouteConfig()
}

// TestEgressAuthorities: which CONNECT authorities the egress listener
// tunnels, under Envoy's own virtual host selection, for an allowlist of an
// exact host and a suffix wildcard. Every one it tunnels resolves, through
// the forward proxy, to the exact host or to a name below the wildcard's
// domain, on 443, and never to an IP literal.
func TestEgressAuthorities(t *testing.T) {
	rc := egressRoutes(t, []string{"github.com", "*.example.com"})
	for _, tc := range []struct {
		authority string
		allowed   bool
	}{
		{"github.com:443", true},
		{"GitHub.COM:443", true},
		{"a.example.com:443", true},
		{"a.b.example.com:443", true},
		{"A.Example.COM:443", true},
		{"1.2.3.4.example.com:443", true}, // a name in example.com's zone, not an address

		{"example.com:443", false}, // the wildcard is below the domain, not the domain
		{".example.com:443", false},
		{"xexample.com:443", false},
		{"example.com.evil.com:443", false},
		{"a.example.com.evil.com:443", false},
		{"a.example.com.:443", false}, // a trailing dot is not stripped, as for exact hosts
		{"github.com.:443", false},
		{"a.example.com:80", false},
		{"a.example.com:0443", false},
		{"a.example.com", false},
		{"a.example.com:443:443", false},
		{"a.example.co:443", false},
		{"sub.github.com:443", false}, // an exact host has no subdomains
		{"1.2.3.4:443", false},
		{"[::1]:443", false},
		{"[::1].example.com", false},

		// Odd bytes before the domain: Envoy's wildcard takes them, and the
		// forward proxy resolves a name that still ends in .example.com, which
		// only example.com's DNS can answer.
		{"evil.com#.example.com:443", true},
		{"evil.com:443.example.com:443", true},
		{"evil.com/.example.com:443", true},
		{"user@a.example.com:443", true},
	} {
		vh := envoyVirtualHost(t, rc, tc.authority)
		if got := vh == "allowed"; got != tc.allowed {
			t.Errorf("%q: virtual host %q; allowed = %v, want %v", tc.authority, vh, got, tc.allowed)
		}
		if vh != "allowed" {
			continue
		}
		host, port, ip := dfpTarget(tc.authority)
		host = strings.ToLower(host)
		if ip || port != 443 || (host != "github.com" && !strings.HasSuffix(host, ".example.com")) {
			t.Errorf("%q is tunnelled to %q port %d (IP literal %v): outside the allowlist", tc.authority, host, port, ip)
		}
	}
}

// TestEgressDomains: an exact host is that host on 443, and a wildcard
// Envoy's suffix wildcard over the same domain on 443, "*" and its dot
// kept.
func TestEgressDomains(t *testing.T) {
	rc := egressRoutes(t, []string{"github.com", "*.example.com"})
	got := rc.GetVirtualHosts()[0].GetDomains()
	if want := []string{"github.com:443", "*.example.com:443"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("domains %q, want %q", got, want)
	}
}
