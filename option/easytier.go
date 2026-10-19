package option

import (
	"github.com/sagernet/sing/common/json/badoption"
)

// EasyTierOutboundOptions configures an outbound that embeds the EasyTier core
// (Rust core compiled to WebAssembly, hosted by easytier-go) and reaches
// overlay addresses through its socket data plane, without a system TUN.
//
// `detour` / `bind_interface` are intentionally absent: the embedded core
// creates its own underlay sockets (peering, hole punching, listeners), which
// do not go through sing-box dialers yet.
// TODO(underlay): implement easytier-go platform.Services.SocketFactory on top
// of a sing-box dialer so underlay traffic honors detour and, on Android, the
// selected underlying network.
type EasyTierOutboundOptions struct {
	// NetworkName is the EasyTier network identifier (required unless
	// ConfigTOML is used).
	NetworkName string `json:"network_name,omitempty"`
	// NetworkSecret is the network key. Kept as a plain string so existing
	// EasyTier tooling can paste it verbatim.
	NetworkSecret string `json:"network_secret,omitempty"`
	// Hostname is advertised to other overlay members.
	Hostname string `json:"hostname,omitempty"`
	// IPv4Prefix is this instance's overlay address, e.g. "10.0.0.5/24".
	// Empty asks the core to allocate one.
	IPv4Prefix string `json:"ipv4,omitempty"`
	// Peers are entry-point URIs, e.g. "udp://198.51.100.10:11010".
	Peers []string `json:"peers,omitempty"`
	// Listeners make this instance reachable for direct connections,
	// e.g. "udp://0.0.0.0:11010".
	Listeners []string `json:"listeners,omitempty"`
	// Encryption defaults to the core default (enabled).
	Encryption *bool `json:"encryption,omitempty"`
	// ConfigTOML is an escape hatch: a full EasyTier instance TOML document
	// (mtu, proxy_network, exit_nodes, socks5, port_forward, ...) that
	// overrides the fields above.
	ConfigTOML string `json:"config_toml,omitempty"`
	// RouteWaitTimeout bounds the wait for an overlay route to the destination
	// before dialing, so a cold start does not look like a hang. Default 10s.
	RouteWaitTimeout badoption.Duration `json:"route_wait_timeout,omitempty"`
}
