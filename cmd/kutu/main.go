package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rakunlabs/into"
	"github.com/rakunlabs/logi"
	"github.com/rakunlabs/tell"

	"github.com/rakunlabs/kutu/internal/config"
	"github.com/rakunlabs/kutu/internal/secret/keymgr"
	"github.com/rakunlabs/kutu/internal/server"
	"github.com/rakunlabs/kutu/internal/server/api"
	"github.com/rakunlabs/kutu/internal/service"
	"github.com/rakunlabs/kutu/internal/storage"
)

// Injected at build time via -ldflags.
var (
	version = "v0.0.0"
	commit  = "-"
	date    = "-"
)

func main() {
	config.Version = version
	config.Service += "/" + version

	into.Init(run,
		into.WithLogger(logi.InitializeLog(logi.WithCaller(false))),
		into.WithMsgf("%s version:[%s] commit:[%s] date:[%s]",
			config.ServiceName, version, commit, date),
	)
}

func run(ctx context.Context) error {
	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}

	// Telemetry first so everything downstream is observable.
	collector, err := tell.New(ctx, cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("init telemetry; %w", err)
	}
	defer collector.Shutdown()

	// At-rest encryption: the key manager starts LOCKED with no key in
	// memory. The storage layer seals/unseals secret columns (registry
	// repository credentials) under the live key.
	mgr := keymgr.New()

	// Storage (PostgreSQL relational config store).
	store, err := storage.New(ctx, &cfg.Storage, mgr)
	if err != nil {
		return fmt.Errorf("init storage; %w", err)
	}
	defer store.Close()

	// Service core.
	svc := service.New(store)
	svc.SetKeyManager(mgr)

	// Bootstrap: if a verifier already exists on disk, engage the
	// lockgate so the server starts locked (awaiting unlock).
	st, stErr := svc.GetKeyStatus(ctx)
	if stErr == nil && st.Initialized {
		mgr.MarkInitialized()
		slog.Info("server started; encryption enabled, awaiting unlock", "initialized", true)
	} else if stErr == nil {
		slog.Info("server started; encryption not yet enabled", "initialized", false)
	}

	// Optional auto-unlock / auto-initialize from config.
	encryptionConfigInvalid := false
	if cfg.Encryption.Password != "" && stErr == nil {
		switch {
		case st.Initialized:
			if err := svc.UnlockServerKey(ctx, cfg.Encryption.Password); err != nil {
				slog.Warn("auto-unlock with config password failed; server remains locked", "err", err)
				encryptionConfigInvalid = true
			} else {
				slog.Info("server auto-unlocked using encryption.password from config")
			}
		default:
			if err := svc.InitializeServerKey(ctx, cfg.Encryption.Password); err != nil {
				slog.Error("auto-initialize with config password failed; encryption stays disabled", "err", err)
			} else {
				slog.Info("server auto-initialized using encryption.password from config")
			}
		}
	}

	info := api.Info{
		Name:                    config.ServiceName,
		Version:                 version,
		Commit:                  commit,
		Date:                    date,
		EncryptionConfigInvalid: encryptionConfigInvalid,
	}

	if err := server.Start(ctx, cfg, svc, info); err != nil {
		return fmt.Errorf("start server; %w", err)
	}

	return nil
}
