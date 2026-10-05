package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/apt"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeAPT,
		Local:   apt.NewLocalFactory(),
		Remote:  apt.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return apt.NewVirtualFactory(m) },
	})
}
