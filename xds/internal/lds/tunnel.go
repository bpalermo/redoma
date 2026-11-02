package lds

import (
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"

	"github.com/bpalermo/redoma/xds/internal/cds"
)

// TunnelSocketMode is the mode of a tunnel's socket. Envoy creates the
// socket, as root with only its supplementary groups' rights, so it cannot
// give the socket redoma xds's group: the socket is open to all, and the
// directory (0770, redoma-sds, made by redoma xds) is what keeps everyone
// but Envoy and redoma xds out. Envoy unlinks a stale socket at the path
// before it binds, and sets the mode after (Envoy v1.40.0,
// source/common/network/socket_impl.cc:67-75).
const TunnelSocketMode = 0o666

// TunnelListener is the host side of a tunnel to a Roles Anywhere endpoint
// (rolesanywhere.Tunnels): a unix socket at socket, in the host's namespace,
// whose every connection is carried as bytes to host:443
// (cds.TunnelCluster). It parses nothing: redoma xds does the TLS, so Envoy
// sees only ciphertext.
func TunnelListener(host, socket string) *listenerv3.Listener {
	name := cds.TunnelName(host)
	return &listenerv3.Listener{
		Name: name,
		Address: &corev3.Address{Address: &corev3.Address_Pipe{Pipe: &corev3.Pipe{
			Path: socket,
			Mode: TunnelSocketMode,
		}}},
		FilterChains: []*listenerv3.FilterChain{{
			Filters: []*listenerv3.Filter{{
				Name: "envoy.filters.network.tcp_proxy",
				ConfigType: &listenerv3.Filter_TypedConfig{TypedConfig: mustAny(&tcpproxyv3.TcpProxy{
					StatPrefix:       name,
					ClusterSpecifier: &tcpproxyv3.TcpProxy_Cluster{Cluster: name},
				})},
			}},
		}},
	}
}
