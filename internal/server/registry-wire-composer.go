package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/composer"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeComposer,
		Local:   composer.NewLocalFactory(),
		Remote:  composer.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return composer.NewVirtualFactory(m) },
	})
}
