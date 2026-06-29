package hook

import (
	"fmt"
	"io"

	"github.com/rakunlabs/kutu/internal/rawfs"
)

// NewHookedFS wraps a rawfs.RawFS so that successful mutating
// operations (write/delete/mkdir/rename/copy) emit hook events through
// the dispatcher. Read operations (Stat/ReadDir/Open) are passed
// straight through.
//
// The returned filesystem implements the optional WritableRawFS,
// RenamableRawFS and CopyableRawFS interfaces by delegating to the
// underlying backend. When the underlying backend does not implement a
// given optional interface, the corresponding method returns an error
// rather than silently succeeding. Callers that need to distinguish a
// read-only mount up front should therefore prefer to wrap only mounts
// whose backend is writable, or pass a nil dispatcher (in which case
// the raw mount is used unwrapped).
//
// When dispatcher is nil the underlying filesystem is returned
// unchanged so no behaviour (including optional-interface detection)
// is altered.
func NewHookedFS(fs rawfs.RawFS, dispatcher *Dispatcher, mount, protocol string) rawfs.RawFS {
	if dispatcher == nil || fs == nil {
		return fs
	}
	return &hookedFS{
		RawFS:    fs,
		d:        dispatcher,
		mount:    mount,
		protocol: protocol,
	}
}

// hookedFS decorates a rawfs.RawFS with event emission. It exposes the
// full set of optional rawfs interfaces and guards each against the
// capabilities of the wrapped backend.
type hookedFS struct {
	rawfs.RawFS
	d        *Dispatcher
	mount    string
	protocol string
}

func (h *hookedFS) emit(t EventType, path string, size int64) {
	if h.d == nil {
		return
	}
	h.d.Emit(Event{
		Type:     t,
		Mount:    h.mount,
		Path:     path,
		Size:     size,
		Protocol: h.protocol,
	})
}

// Write implements rawfs.WritableRawFS.
func (h *hookedFS) Write(path string, r io.Reader, size int64) error {
	w, ok := h.RawFS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("mount is read-only: %w", errNotSupported)
	}
	// Determine create vs update before the write lands.
	_, statErr := h.RawFS.Stat(path)
	if err := w.Write(path, r, size); err != nil {
		return err
	}
	if statErr != nil {
		h.emit(EventFileCreated, path, size)
	} else {
		h.emit(EventFileUpdated, path, size)
	}
	return nil
}

// Delete implements rawfs.WritableRawFS.
func (h *hookedFS) Delete(path string) error {
	w, ok := h.RawFS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("mount is read-only: %w", errNotSupported)
	}
	if err := w.Delete(path); err != nil {
		return err
	}
	h.emit(EventFileDeleted, path, 0)
	return nil
}

// MkDir implements rawfs.WritableRawFS.
func (h *hookedFS) MkDir(path string) error {
	w, ok := h.RawFS.(rawfs.WritableRawFS)
	if !ok {
		return fmt.Errorf("mount is read-only: %w", errNotSupported)
	}
	if err := w.MkDir(path); err != nil {
		return err
	}
	h.emit(EventDirCreated, path, 0)
	return nil
}

// Rename implements rawfs.RenamableRawFS.
func (h *hookedFS) Rename(oldPath, newPath string) error {
	rn, ok := h.RawFS.(rawfs.RenamableRawFS)
	if !ok {
		return fmt.Errorf("mount does not support rename: %w", errNotSupported)
	}
	if err := rn.Rename(oldPath, newPath); err != nil {
		return err
	}
	h.d.Emit(Event{
		Type:     EventFileRenamed,
		Mount:    h.mount,
		Path:     newPath,
		OldPath:  oldPath,
		Protocol: h.protocol,
	})
	return nil
}

// Copy implements rawfs.CopyableRawFS.
func (h *hookedFS) Copy(srcPath, dstPath string) error {
	cp, ok := h.RawFS.(rawfs.CopyableRawFS)
	if !ok {
		return fmt.Errorf("mount does not support copy: %w", errNotSupported)
	}
	if err := cp.Copy(srcPath, dstPath); err != nil {
		return err
	}
	h.d.Emit(Event{
		Type:     EventFileCopied,
		Mount:    h.mount,
		Path:     dstPath,
		OldPath:  srcPath,
		Protocol: h.protocol,
	})
	return nil
}

var errNotSupported = fmt.Errorf("operation not supported")
