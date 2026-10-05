package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/p2"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeP2,
		Local:   p2.NewLocalFactory(),
		Remote:  p2.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return p2.NewVirtualFactory(m) },
	})
}
