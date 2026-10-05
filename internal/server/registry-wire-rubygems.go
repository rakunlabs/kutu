package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/rubygems"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeRubyGems,
		Local:   rubygems.NewLocalFactory(),
		Remote:  rubygems.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return rubygems.NewVirtualFactory(m) },
	})
}
