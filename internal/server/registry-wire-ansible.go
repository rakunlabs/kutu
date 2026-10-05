package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/ansible"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeAnsible,
		Local:   ansible.NewLocalFactory(),
		Remote:  ansible.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return ansible.NewVirtualFactory(m) },
	})
}
