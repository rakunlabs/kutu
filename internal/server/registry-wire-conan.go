package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/conan"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeConan,
		Local:   conan.NewLocalFactory(),
		Remote:  conan.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return conan.NewVirtualFactory(m) },
	})
}
