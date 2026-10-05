package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/bower"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeBower,
		Local:   bower.NewLocalFactory(),
		Remote:  bower.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return bower.NewVirtualFactory(m) },
	})
}
