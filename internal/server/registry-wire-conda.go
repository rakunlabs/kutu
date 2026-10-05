package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/conda"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeConda,
		Local:   conda.NewLocalFactory(),
		Remote:  conda.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return conda.NewVirtualFactory(m) },
	})
}
