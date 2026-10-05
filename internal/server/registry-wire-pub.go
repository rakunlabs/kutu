package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pub"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypePub,
		Local:   pub.NewLocalFactory(),
		Remote:  pub.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return pub.NewVirtualFactory(m) },
	})
}
