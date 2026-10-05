package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/puppet"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypePuppet,
		Local:   puppet.NewLocalFactory(),
		Remote:  puppet.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return puppet.NewVirtualFactory(m) },
	})
}
