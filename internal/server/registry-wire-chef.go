package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/chef"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeChef,
		Local:   chef.NewLocalFactory(),
		Remote:  chef.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return chef.NewVirtualFactory(m) },
	})
}
