package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/alpine"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeAlpine,
		Local:   alpine.NewLocalFactory(),
		Remote:  alpine.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return alpine.NewVirtualFactory(m) },
	})
}
