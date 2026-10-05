// Package ops implements cross-registry operations on top of the
// optional registry interfaces: package search across repositories,
// version retention, promotion between local repos, export / import
// of a repository's storage tree (offline transfer, backup and pull
// replication), cache prefetching and a small periodic job scheduler.
//
// The package is a library: HTTP endpoints and schedulers are wired by
// the server.
package ops

import "errors"

var (
	// ErrUnsupported is returned when a registry does not implement the
	// optional interface an operation needs.
	ErrUnsupported = errors.New("operation not supported by registry")
	// ErrIncompatible is returned when two registries cannot take part
	// in the same operation (kind or type mismatch).
	ErrIncompatible = errors.New("incompatible registries")
	// ErrInvalidArchive is returned by Import for malformed, unsafe or
	// corrupted export archives.
	ErrInvalidArchive = errors.New("invalid export archive")
)
