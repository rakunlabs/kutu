package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/swift"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeSwift,
		Local:   swift.NewLocalFactory(),
		Remote:  swift.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return swift.NewVirtualFactory(m) },
	})
}
