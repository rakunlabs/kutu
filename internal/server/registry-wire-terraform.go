package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/terraform"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeTerraform,
		Local:   terraform.NewLocalFactory(),
		Remote:  terraform.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return terraform.NewVirtualFactory(m) },
	})
}
