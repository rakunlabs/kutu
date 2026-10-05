package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/rpm"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeRPM,
		Local:   rpm.NewLocalFactory(),
		Remote:  rpm.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return rpm.NewVirtualFactory(m) },
	})
}
