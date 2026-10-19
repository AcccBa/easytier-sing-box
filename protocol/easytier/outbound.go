//go:build with_easytier

package easytier

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	corehost "github.com/easytier/easytier/easytier-go"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

const defaultRouteWaitTimeout = 10 * time.Second

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[option.EasyTierOutboundOptions](registry, C.TypeEasyTier, NewOutbound)
}

var (
	_ adapter.Outbound = (*Outbound)(nil)
)

// The EasyTier host owns one WebAssembly runtime shared by all instances, so
// several easytier outbounds in one configuration pay for a single runtime.
var (
	hostAccess   sync.Mutex
	hostInstance *corehost.Host
	hostRefCount int
)

func acquireHost(ctx context.Context) (*corehost.Host, func(), error) {
	hostAccess.Lock()
	defer hostAccess.Unlock()
	if hostInstance == nil {
		host, err := corehost.New(ctx, corehost.Options{})
		if err != nil {
			return nil, nil, E.Cause(err, "create easytier host")
		}
		hostInstance = host
	}
	hostRefCount++
	var closeOnce sync.Once
	return hostInstance, func() {
		closeOnce.Do(func() {
			hostAccess.Lock()
			defer hostAccess.Unlock()
			hostRefCount--
			if hostRefCount == 0 {
				closing := hostInstance
				hostInstance = nil
				go func() {
					_ = closing.Close(context.Background())
				}()
			}
		})
	}, nil
}

type Outbound struct {
	outbound.Adapter
	ctx          context.Context
	logger       logger.ContextLogger
	options      option.EasyTierOutboundOptions
	routeTimeout time.Duration
	access       sync.Mutex
	host         *corehost.Host
	releaseHost  func()
	instance     *corehost.Instance
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.EasyTierOutboundOptions) (adapter.Outbound, error) {
	if options.ConfigTOML == "" && options.NetworkName == "" {
		return nil, E.New("`network_name` is required unless `config_toml` is set")
	}
	if options.IPv4Prefix != "" {
		if _, err := netip.ParsePrefix(options.IPv4Prefix); err != nil {
			return nil, E.Cause(err, "parse `ipv4`")
		}
	}
	routeTimeout := time.Duration(options.RouteWaitTimeout)
	if routeTimeout == 0 {
		routeTimeout = defaultRouteWaitTimeout
	}
	return &Outbound{
		Adapter:      outbound.NewAdapter(C.TypeEasyTier, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		ctx:          ctx,
		logger:       logger,
		options:      options,
		routeTimeout: routeTimeout,
	}, nil
}

func (o *Outbound) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateInitialize {
		return nil
	}
	host, releaseHost, err := acquireHost(o.ctx)
	if err != nil {
		return err
	}
	o.access.Lock()
	o.host = host
	o.releaseHost = releaseHost
	o.access.Unlock()
	scope.Add(o.Close)

	instanceName := o.Tag()
	var instance *corehost.Instance
	if o.options.ConfigTOML != "" {
		instance, err = host.CreateInstanceTOML(scope.Context(), instanceName, "", o.options.ConfigTOML)
		if err != nil {
			return E.Cause(err, "create easytier instance from `config_toml`")
		}
	} else {
		builder := corehost.NewInstanceConfigBuilder(o.options.NetworkName).
			NetworkSecret(o.options.NetworkSecret)
		if o.options.Hostname != "" {
			builder = builder.Hostname(o.options.Hostname)
		}
		if o.options.IPv4Prefix != "" {
			prefix, err := netip.ParsePrefix(o.options.IPv4Prefix)
			if err != nil {
				return E.Cause(err, "parse `ipv4`")
			}
			builder = builder.IPv4(prefix)
		}
		if len(o.options.Peers) > 0 {
			builder = builder.AddPeers(o.options.Peers...)
		}
		if len(o.options.Listeners) > 0 {
			builder = builder.AddListeners(o.options.Listeners...)
		}
		if o.options.Encryption != nil {
			builder = builder.Encryption(*o.options.Encryption)
		}
		instanceConfig, err := builder.Build()
		if err != nil {
			return E.Cause(err, "build easytier instance config")
		}
		instance, err = host.CreateInstance(scope.Context(), instanceConfig)
		if err != nil {
			return E.Cause(err, "create easytier instance")
		}
	}
	if err = instance.Start(scope.Context()); err != nil {
		_ = instance.Close(scope.Context())
		return E.Cause(err, "start easytier instance")
	}
	o.access.Lock()
	o.instance = instance
	o.access.Unlock()

	if nodeInfo, infoErr := instance.ShowNodeInfo(scope.Context()); infoErr == nil {
		o.logger.Info("easytier instance started: ", nodeInfo.GetHostname(), " ipv4=", o.options.IPv4Prefix)
	}
	routes, routeErr := instance.ListRoute(scope.Context())
	if routeErr == nil {
		for _, route := range routes {
			if prefix, isParsed := routePrefix(route); isParsed {
				o.logger.Debug("easytier route: ", prefix.String(), " hostname=", route.GetHostname())
			}
		}
	}
	return nil
}

func (o *Outbound) Close() error {
	o.access.Lock()
	defer o.access.Unlock()
	if o.instance != nil {
		instance := o.instance
		o.instance = nil
		_ = instance.Close(context.Background())
	}
	if o.releaseHost != nil {
		releaseHost := o.releaseHost
		o.releaseHost = nil
		releaseHost()
	}
	return nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	switch N.NetworkName(network) {
	case N.NetworkTCP:
		conn, err := o.dial(ctx, destination, "tcp4")
		if err != nil {
			return nil, err
		}
		o.logger.InfoContext(ctx, "outbound connection to ", destination)
		return conn, nil
	case N.NetworkUDP:
		conn, err := o.dialUDP(ctx, destination)
		if err != nil {
			return nil, err
		}
		o.logger.InfoContext(ctx, "outbound packet connection to ", destination)
		return conn, nil
	default:
		return nil, E.New("unsupported network: ", network)
	}
}

func (o *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	conn, err := o.dialUDP(ctx, destination)
	if err != nil {
		return nil, err
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	o.logger.InfoContext(ctx, "outbound packet connection to ", destination)
	return conn, nil
}

func (o *Outbound) dial(ctx context.Context, destination M.Socksaddr, network string) (net.Conn, error) {
	instance, err := o.readyInstance()
	if err != nil {
		return nil, err
	}
	address, err := o.overlayAddress(destination)
	if err != nil {
		return nil, err
	}
	if err = o.waitRoute(ctx, destination.Addr); err != nil {
		return nil, err
	}
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	return instance.Dial(ctx, network, address)
}

func (o *Outbound) dialUDP(ctx context.Context, destination M.Socksaddr) (*datagramConn, error) {
	conn, err := o.dial(ctx, destination, "udp4")
	if err != nil {
		return nil, err
	}
	return &datagramConn{Conn: conn, peer: destination}, nil
}

func (o *Outbound) readyInstance() (*corehost.Instance, error) {
	o.access.Lock()
	instance := o.instance
	o.access.Unlock()
	if instance == nil {
		return nil, E.New("easytier instance is not started")
	}
	return instance, nil
}

// The socket data plane is overlay-IPv4 only and does not resolve names:
// routing must hand it literal overlay addresses.
func (o *Outbound) overlayAddress(destination M.Socksaddr) (string, error) {
	if !destination.IsIP() {
		return "", E.New("easytier outbound requires an overlay address, got domain ", destination.String())
	}
	if !destination.Addr.Is4() {
		return "", E.New("easytier socket data plane is IPv4-only, cannot reach ", destination.Addr.String())
	}
	return destination.String(), nil
}

// The overlay needs a few seconds after start before the first dial; waiting
// for a route turns that cold start into a log line instead of a timeout.
func (o *Outbound) waitRoute(ctx context.Context, destination netip.Addr) error {
	if o.routeTimeout <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, o.routeTimeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		routes, err := o.instance.ListRoute(ctx)
		if err != nil {
			// Route lookup is advisory: the dial below fails fast on its own.
			o.logger.WarnContext(ctx, "easytier route lookup failed: ", err)
			return nil
		}
		for _, route := range routes {
			if prefix, isParsed := routePrefix(route); isParsed && prefix.Contains(destination) {
				return nil
			}
			for _, proxyCIDR := range route.GetProxyCidrs() {
				prefix, err := netip.ParsePrefix(proxyCIDR)
				if err == nil && prefix.Contains(destination) {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return E.New("no easytier route to ", destination.String(), " within ", o.routeTimeout)
		case <-ticker.C:
		}
	}
}

func routePrefix(route *corehost.Route) (netip.Prefix, bool) {
	inet := route.GetIpv4Addr()
	if inet == nil {
		return netip.Prefix{}, false
	}
	bits := inet.GetAddress().GetAddr()
	length := inet.GetNetworkLength()
	if length > 32 {
		return netip.Prefix{}, false
	}
	address := netip.AddrFrom4([4]byte{
		byte(bits >> 24),
		byte(bits >> 16),
		byte(bits >> 8),
		byte(bits),
	})
	return netip.PrefixFrom(address, int(length)), true
}

// datagramConn adapts the connected overlay datagram socket returned by
// Instance.Dial to the packet-connection shape sing-box expects.
type datagramConn struct {
	net.Conn
	peer M.Socksaddr
}

func (c *datagramConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	n, err := c.Conn.Read(buffer)
	return n, net.UDPAddrFromAddrPort(c.peer.AddrPort()), err
}

func (c *datagramConn) WriteTo(buffer []byte, address net.Addr) (int, error) {
	if address.String() != c.peer.String() {
		return 0, E.New("easytier datagram socket is bound to ", c.peer.String(), ", cannot write to ", address.String())
	}
	return c.Conn.Write(buffer)
}
