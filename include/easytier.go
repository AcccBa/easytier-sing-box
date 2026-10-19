//go:build with_easytier

package include

import (
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/protocol/easytier"
)

func registerEasyTierOutbound(registry *outbound.Registry) {
	easytier.RegisterOutbound(registry)
}
