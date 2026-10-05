package api

import (
	"net/http"

	"github.com/rakunlabs/ada"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/metrics"
)

// serveRegistryMetered dispatches to reg while recording request /
// byte / download counters.
func (a *api) serveRegistryMetered(w http.ResponseWriter, r *http.Request, reg registry.Registry) {
	sw := &metrics.StatusWriter{ResponseWriter: w}
	reg.ServeHTTP(sw, r)
	status := sw.Status
	if status == 0 {
		status = http.StatusOK
	}
	metrics.Default.Observe(reg.Namespace(), reg.Name(), reg.Type(), status, sw.Bytes)
	if status == http.StatusOK && r.Method == http.MethodGet {
		if c, ok := reg.(registry.ArtifactClassifier); ok {
			if ref, ok := c.ClassifyRequest(r); ok && ref.Version != "" {
				metrics.Default.Download(reg.Namespace(), reg.Name(), reg.Type(), ref.Name)
			}
		}
	}
}

// getRegistryMetrics serves Prometheus text for every repo.
// GET /api/v1/registries/metrics (registry admin).
func (a *api) getRegistryMetrics(c *ada.Context) error {
	c.Response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.Response.WriteHeader(http.StatusOK)
	metrics.Default.WritePrometheus(c.Response)
	return nil
}

// getRegistryUsage returns per-repo counters + top downloads.
// GET /api/v1/registries/{type}/{ns}/{repo}/usage.
func (a *api) getRegistryUsageFor(typ string) func(*ada.Context) error {
	return func(c *ada.Context) error {
		_, ns, repo, err := a.resolveRegistry(c, typ)
		if err != nil {
			return err
		}
		return c.SetStatus(http.StatusOK).SendJSON(metrics.Default.Summary(ns, repo, 20))
	}
}
