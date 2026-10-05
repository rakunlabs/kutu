package service

import (
	"context"
	"time"
)

// metaReplicationPrefix namespaces the per-repo replication watermark
// (the "since" timestamp of the last successful incremental pull).
const metaReplicationPrefix = "registry_replication/"

// ReplicationWatermark returns the last successful replication start
// time for ns/repo (zero when never replicated).
func (s *Service) ReplicationWatermark(ctx context.Context, ns, repo string) time.Time {
	var v string
	if ok, err := s.store.GetMeta(ctx, metaReplicationPrefix+ns+"/"+repo, &v); err != nil || !ok {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

// SetReplicationWatermark persists the replication watermark.
func (s *Service) SetReplicationWatermark(ctx context.Context, ns, repo string, t time.Time) error {
	return s.store.SetMeta(ctx, metaReplicationPrefix+ns+"/"+repo, t.UTC().Format(time.RFC3339))
}
