package api

import (
	"context"
	"log/slog"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/service"
)

// mountEntriesFromConfig materializes the configured raw mounts into the
// api package's internal mountEntry shape, building each backend
// filesystem via service.NewRawFS. Mounts whose backend fails to build
// are logged and skipped so one bad mount doesn't sink the server.
func mountEntriesFromConfig(mounts []service.RawMountEntry) []mountEntry {
	entries := make([]mountEntry, 0, len(mounts))
	for _, m := range mounts {
		fs, err := service.NewRawFS(m)
		if err != nil {
			slog.Warn("raw mount skipped", "prefix", m.Prefix, "type", m.Type, "error", err)
			continue
		}
		typ := m.Type
		if typ == "" {
			typ = "local"
		}
		entries = append(entries, mountEntry{
			Prefix:   m.Prefix,
			FS:       fs,
			Type:     typ,
			Writable: rawfs.IsWritable(fs),
		})
	}
	return entries
}

// NewRawHandlerFromMounts builds a RawHandler from the configured raw
// mounts. The dispatcher (may be nil) is used to emit file events for
// writable mounts.
func NewRawHandlerFromMounts(ctx context.Context, mounts []service.RawMountEntry, dispatcher *hook.Dispatcher) *RawHandler {
	return NewRawHandler(mountEntriesFromConfig(mounts), ctx, dispatcher)
}

// ReloadFromMounts swaps the live mount table to match new config.
func (h *RawHandler) ReloadFromMounts(mounts []service.RawMountEntry) {
	if h == nil {
		return
	}
	h.UpdateMounts(mountEntriesFromConfig(mounts))
}
