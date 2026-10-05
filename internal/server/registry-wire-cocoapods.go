package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/cocoapods"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeCocoaPods,
		Local:   cocoapods.NewLocalFactory(),
		Remote:  cocoapods.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return cocoapods.NewVirtualFactory(m) },
	})
}
