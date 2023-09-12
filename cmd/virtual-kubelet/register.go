package main

import (
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/commands/root"
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider"
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider/mock"
	"github.com/virtual-kubelet/virtual-kubelet/cmd/virtual-kubelet/internal/provider/sina"
)

func registerMock(s *provider.Store) {
	/* #nosec */
	s.Register("mock", func(cfg provider.InitConfig) (provider.Provider, error) { //nolint:errcheck
		return mock.NewMockProvider(
			cfg.ConfigPath,
			cfg.NodeName,
			cfg.OperatingSystem,
			cfg.InternalIP,
			cfg.DaemonPort,
		)
	})
}

func registerSina(s *provider.Store) {
	/* #nosec */
	var cc sina.ClientConfig
	var o root.Opts
	s.Register("sina", func(cfg provider.InitConfig) (provider.Provider, error) { //nolint:errcheck
		return sina.NewSinaProvider(
			cfg,
			&cc,
			"",
			false,
			&o,
		)
	})
}
