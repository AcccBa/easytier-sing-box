//go:build with_easytier

package easytier

import (
	"context"
	"net"
	"net/netip"
	"strings"
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

// coldRouteWait 是实例刚刚创建时额外给的路由等待预算：刚入网时路由表还是空的。
const coldRouteWait = 20 * time.Second

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
	// hostCacheOff 由任一显式设 cache_runtime=false 的 outbound 置位：
	// 这是进程级扶择，显式意图优先于别人的默认值。
	hostCacheOff bool
)

func acquireHost(ctx context.Context, cacheRuntime bool) (*corehost.Host, func(), error) {
	hostAccess.Lock()
	defer hostAccess.Unlock()
	if !cacheRuntime {
		hostCacheOff = true
	}
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
			if hostRefCount > 0 || hostInstance == nil {
				return
			}
			if !hostCacheOff {
				// 保留 WebAssembly 运行时：它持有已编译的核心，下次启动免重编译
				// （实测每次重建多付 ~266MB 峰值与 15 秒 JIT，空闲驻留代价 +39MB）。
				return
			}
			closing := hostInstance
			hostInstance = nil
			go func() {
				_ = closing.Close(context.Background())
			}()
		})
	}, nil
}

type Outbound struct {
	outbound.Adapter
	ctx          context.Context
	logger       logger.ContextLogger
	options      option.EasyTierOutboundOptions
	routeTimeout time.Duration
	cacheRuntime bool
	access       sync.Mutex
	createAccess sync.Mutex
	host         *corehost.Host
	releaseHost  func()
	instance     *corehost.Instance
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.EasyTierOutboundOptions) (adapter.Outbound, error) {
	if options.ConfigTOML == "" && options.NetworkName == "" {
		return nil, E.New("`network_name` is required unless `config_toml` is set")
	}
	// `config_toml` 是整份配置直通，会静默吞掉 builder 字段（曾导致入网参数全部丢失的哑故障），
	// 因此两者同时出现时直接报错，不猜意图。
	if options.ConfigTOML != "" {
		var conflict []string
		if options.NetworkName != "" {
			conflict = append(conflict, "network_name")
		}
		if options.NetworkSecret != "" {
			conflict = append(conflict, "network_secret")
		}
		if options.Hostname != "" {
			conflict = append(conflict, "hostname")
		}
		if options.IPv4Prefix != "" {
			conflict = append(conflict, "ipv4")
		}
		if len(options.Peers) > 0 {
			conflict = append(conflict, "peers")
		}
		if len(options.Listeners) > 0 {
			conflict = append(conflict, "listeners")
		}
		if options.Encryption != nil {
			conflict = append(conflict, "encryption")
		}
		if len(conflict) > 0 {
			return nil, E.New("`config_toml` 与这些字段冲突且会覆盖它们: ", strings.Join(conflict, ", "), "；只保留 `config_toml` 或只保留结构化字段")
		}
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
	cacheRuntime := true
	if options.CacheRuntime != nil {
		cacheRuntime = *options.CacheRuntime
	}
	return &Outbound{
		Adapter:      outbound.NewAdapter(C.TypeEasyTier, tag, []string{N.NetworkTCP, N.NetworkUDP}, nil),
		ctx:          ctx,
		logger:       logger,
		options:      options,
		routeTimeout: routeTimeout,
		cacheRuntime: cacheRuntime,
	}, nil
}

func (o *Outbound) Start(stage adapter.StartStage, scope *adapter.Scope) error {
	if stage != adapter.StartStateInitialize {
		return nil
	}
	scope.Add(o.Close)
	if o.options.Lazy {
		o.logger.Info("easytier outbound 懒启动：首次拨向 overlay 目的地时才创建实例")
		return nil
	}
	if _, err := o.ensureInstance(); err != nil {
		return err
	}
	return nil
}

// ensureInstance 创建并启动内嵌核实例，可重复调用；已存在则直接返回。
// 返回 created=true 表示本次调用完成了创建，调用方据此给首次路由等待更多预算。
// 用服务级 ctx 创建：拨号方超时退出不该把实例创建打断在中途。
func (o *Outbound) ensureInstance() (created bool, err error) {
	o.access.Lock()
	ready := o.instance
	o.access.Unlock()
	if ready != nil {
		return false, nil
	}
	// 单次飞行：首个拨号付编译成本，其余并发拨号排在后面复用同一实例
	o.createAccess.Lock()
	defer o.createAccess.Unlock()
	o.access.Lock()
	ready = o.instance
	o.access.Unlock()
	if ready != nil {
		return false, nil
	}

	if o.options.Lazy {
		o.logger.Info("首次访问 overlay，正在创建 EasyTier 实例：首次需把 WebAssembly 核心编译到本机码，可能要十几到几十秒")
	}
	host, releaseHost, err := acquireHost(o.ctx, o.cacheRuntime)
	if err != nil {
		return false, err
	}
	o.access.Lock()
	o.host = host
	o.releaseHost = releaseHost
	o.access.Unlock()

	instance, err := o.startInstance(host)
	if err != nil {
		o.access.Lock()
		o.releaseHost = nil
		o.access.Unlock()
		releaseHost()
		return false, err
	}
	o.access.Lock()
	o.instance = instance
	o.access.Unlock()
	return true, nil
}

// startInstance 只负责实例的创建/启动/日志，并发与引用计数由调用方管。
func (o *Outbound) startInstance(host *corehost.Host) (*corehost.Instance, error) {
	ctx := o.ctx
	instanceName := o.Tag()
	var instance *corehost.Instance
	var err error
	if o.options.ConfigTOML != "" {
		instance, err = host.CreateInstanceTOML(ctx, instanceName, "", o.options.ConfigTOML)
		if err != nil {
			return nil, E.Cause(err, "create easytier instance from `config_toml`")
		}
	} else {
		builder := corehost.NewInstanceConfigBuilder(o.options.NetworkName).
			NetworkSecret(o.options.NetworkSecret)
		if o.options.Hostname != "" {
			builder = builder.Hostname(o.options.Hostname)
		}
		if o.options.IPv4Prefix != "" {
			prefix, parseErr := netip.ParsePrefix(o.options.IPv4Prefix)
			if parseErr != nil {
				return nil, E.Cause(parseErr, "parse `ipv4`")
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
		instanceConfig, buildErr := builder.Build()
		if buildErr != nil {
			return nil, E.Cause(buildErr, "build easytier instance config")
		}
		instance, err = host.CreateInstance(ctx, instanceConfig)
		if err != nil {
			return nil, E.Cause(err, "create easytier instance")
		}
	}
	if err = instance.Start(ctx); err != nil {
		_ = instance.Close(ctx)
		return nil, E.Cause(err, "start easytier instance")
	}

	if nodeInfo, infoErr := instance.ShowNodeInfo(ctx); infoErr == nil {
		o.logger.Info("easytier instance started: ", nodeInfo.GetHostname(), " ipv4=", o.options.IPv4Prefix)
	}
	routes, routeErr := instance.ListRoute(ctx)
	if routeErr == nil {
		for _, route := range routes {
			if prefix, isParsed := routePrefix(route); isParsed {
				o.logger.Debug("easytier route: ", prefix.String(), " hostname=", route.GetHostname())
			}
		}
	}
	return instance, nil
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
	instance, routeBudget, err := o.readyInstance()
	if err != nil {
		return nil, err
	}
	address, err := o.overlayAddress(destination)
	if err != nil {
		return nil, err
	}
	if err = o.waitRoute(ctx, instance, destination.Addr, routeBudget); err != nil {
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

// readyInstance 返回可用实例与本次的路由等待预算；懒启动时在这里完成创建。
func (o *Outbound) readyInstance() (*corehost.Instance, time.Duration, error) {
	o.access.Lock()
	instance := o.instance
	o.access.Unlock()
	if instance != nil {
		return instance, o.routeTimeout, nil
	}
	if !o.options.Lazy {
		return nil, 0, E.New("easytier instance is not started")
	}
	created, err := o.ensureInstance()
	if err != nil {
		return nil, 0, err
	}
	o.access.Lock()
	instance = o.instance
	o.access.Unlock()
	if instance == nil {
		return nil, 0, E.New("easytier instance is not started")
	}
	if created {
		// 刚建好就拨：路由表还是空的，多给冷启动预算
		return instance, o.routeTimeout + coldRouteWait, nil
	}
	return instance, o.routeTimeout, nil
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
func (o *Outbound) waitRoute(ctx context.Context, instance *corehost.Instance, destination netip.Addr, budget time.Duration) error {
	if budget <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		routes, err := instance.ListRoute(ctx)
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
