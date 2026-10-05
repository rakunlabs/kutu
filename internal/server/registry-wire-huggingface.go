package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/huggingface"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeHuggingFace,
		Local:   huggingface.NewLocalFactory(),
		Remote:  huggingface.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return huggingface.NewVirtualFactory(m) },
	})
}
