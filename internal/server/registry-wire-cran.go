package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/cran"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeCRAN,
		Local:   cran.NewLocalFactory(),
		Remote:  cran.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return cran.NewVirtualFactory(m) },
	})
}
