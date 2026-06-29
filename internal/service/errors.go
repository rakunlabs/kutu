package service

import "errors"

// Sentinel errors mapped to HTTP statuses by the api error handler.
// Handlers wrap these with %w so the boundary can switch on them.
var (
	ErrNotFound     = errors.New("not found")
	ErrBadRequest   = errors.New("bad request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrConflict     = errors.New("conflict")
	ErrInternal     = errors.New("internal error")
)
