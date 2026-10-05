package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/nuget"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeNuGet,
		Local:   nuget.NewLocalFactory(),
		Remote:  nuget.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return nuget.NewVirtualFactory(m) },
	})
}
