package config

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/rakunlabs/chu"
	"github.com/rakunlabs/chu/loader/loaderenv"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"

	"github.com/rakunlabs/kutu/internal/storage"
)

var (
	// ServiceName is the canonical name used for telemetry and logs.
	ServiceName = "kutu"
	// Service is ServiceName plus the running version (filled in main).
	Service = ServiceName
	// Version is the build version (overwritten from ldflags in main).
	Version = "v0.0.0"
)

// Config is the whole kutu configuration. It is loaded by chu in the
// order default -> file -> env (KUTU_ prefix).
type Config struct {
	LogLevel string `cfg:"log_level" default:"info"`

	Storage    storage.Config `cfg:"storage"`
	Server     Server         `cfg:"server"`
	Encryption Encryption     `cfg:"encryption"`

	// Telemetry is configured from the same pipeline. An empty OTLP
	// endpoint yields no-op providers.
	Telemetry tell.Config `cfg:"telemetry"`
}

// Encryption carries the optional at-rest key passphrase. Behaviour at
// boot (see cmd/kutu/main.go):
//
//   - Empty: if a verifier exists on disk the server starts LOCKED and
//     the operator must enter the key through the UI unlock screen on
//     every restart. A fresh install runs in plaintext-at-rest mode.
//   - Set + already-initialized: the server auto-unlocks with the
//     supplied passphrase. Wrong passphrase → stays locked + an
//     "encryption_config_invalid" warning on /api/v1/info.
//   - Set + not yet initialized: the server auto-initializes using this
//     passphrase as the at-rest key.
//
// SECURITY: when populated this puts the key on disk / in the process
// environment. It is masked from the loaded-config log line via
// log:"-" but is readable to anyone who can read the config or env.
type Encryption struct {
	Password string `cfg:"password" log:"-"`
}

// Server is the admin/data HTTP listener configuration.
type Server struct {
	Host string `cfg:"host"`
	Port string `cfg:"port" default:"8080"`
}

// Addr returns the host:port the HTTP server binds to.
func (s Server) Addr() string {
	return net.JoinHostPort(s.Host, s.Port)
}

// Load reads configuration through chu and applies the log level.
func Load(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := chu.Load(ctx, ServiceName, &cfg,
		chu.WithLoaderOption(loaderenv.New(
			loaderenv.WithPrefix("KUTU_"),
		)),
		chu.WithVersion(Version),
	); err != nil {
		return nil, err
	}

	if err := logi.SetLogLevel(cfg.LogLevel); err != nil {
		return nil, fmt.Errorf("set log level %s: %w", cfg.LogLevel, err)
	}

	slog.Info("loaded configuration", "config", chu.MarshalMap(cfg))

	return &cfg, nil
}
