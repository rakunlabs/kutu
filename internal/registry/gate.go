package registry

import (
	"net/http"
	"sync/atomic"
)

// RequestGate decides whether a data-plane request against reg may
// proceed. It must not write to the response; it returns the HTTP
// status and message to reject with (ok=false) or ok=true to allow.
//
// The server installs one gate (policy: include/exclude, OSV,
// license, quarantine, signatures). Virtual registries consult it for
// every member they dispatch to so a member's policy cannot be
// bypassed through a virtual.
type RequestGate func(reg Registry, r *http.Request) (status int, msg string, ok bool)

var gate atomic.Pointer[RequestGate]

// SetRequestGate installs the process-wide gate (nil clears it).
func SetRequestGate(g RequestGate) {
	if g == nil {
		gate.Store(nil)
		return
	}
	gate.Store(&g)
}

// CheckGate runs the installed gate. Allows when none is installed.
func CheckGate(reg Registry, r *http.Request) (int, string, bool) {
	g := gate.Load()
	if g == nil || *g == nil {
		return 0, "", true
	}
	return (*g)(reg, r)
}
