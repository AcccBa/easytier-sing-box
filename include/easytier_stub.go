//go:build !with_easytier

package include

import (
	"context"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
)

func registerEasyTierOutbound(registry *outbound.Registry) {
	outbound.Register[option.EasyTierOutboundOptions](registry, C.TypeEasyTier, func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.EasyTierOutboundOptions) (adapter.Outbound, error) {
		return nil, E.New(`EasyTier is not included in this build, rebuild with -tags with_easytier`)
	})
}
