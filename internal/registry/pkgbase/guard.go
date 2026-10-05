package pkgbase

import (
	"fmt"
	"net/http"

	"github.com/rakunlabs/kutu/internal/service"
)

// PublishGuard enforces the push-side policies shared by every local
// repository: ImmutableVersions and QuotaBytes.
type PublishGuard struct {
	Immutable  bool
	QuotaBytes int64
}

// GuardFor extracts the push-side policy from a repo row.
func GuardFor(r *service.RegistryRepository) PublishGuard {
	if r == nil || r.Policy == nil {
		return PublishGuard{}
	}
	return PublishGuard{Immutable: r.Policy.ImmutableVersions, QuotaBytes: r.Policy.QuotaBytes}
}

// Check returns an HTTP status + message when a publish of incoming
// bytes must be rejected. exists reports whether the target version is
// already stored.
func (g PublishGuard) Check(s *Store, exists bool, incoming int64) (int, error) {
	if g.Immutable && exists {
		return http.StatusConflict, fmt.Errorf("version already exists and the repository has immutable_versions enabled")
	}
	if g.QuotaBytes > 0 && s != nil {
		_, used := s.Usage("")
		if used+incoming > g.QuotaBytes {
			return http.StatusRequestEntityTooLarge, fmt.Errorf("repository quota exceeded (%d + %d > %d bytes)", used, incoming, g.QuotaBytes)
		}
	}
	return 0, nil
}

// Allow writes the rejection (if any) and reports whether to proceed.
func (g PublishGuard) Allow(w http.ResponseWriter, s *Store, exists bool, incoming int64) bool {
	if code, err := g.Check(s, exists, incoming); err != nil {
		Error(w, code, err.Error())
		return false
	}
	return true
}
