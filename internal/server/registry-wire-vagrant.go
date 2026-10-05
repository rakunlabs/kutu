package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/vagrant"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeVagrant,
		Local:   vagrant.NewLocalFactory(),
		Remote:  vagrant.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return vagrant.NewVirtualFactory(m) },
	})
}
