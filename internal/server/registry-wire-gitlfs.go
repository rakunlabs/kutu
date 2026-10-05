package server

import (
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/gitlfs"
	"github.com/rakunlabs/kutu/internal/service"
)

func init() {
	registerProtocol(protocolFactory{
		Type:    service.RegistryTypeGitLFS,
		Local:   gitlfs.NewLocalFactory(),
		Remote:  gitlfs.NewRemoteFactory(),
		Virtual: func(m *registry.Manager) registry.Factory { return gitlfs.NewVirtualFactory(m) },
	})
}
