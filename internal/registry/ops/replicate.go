package ops

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/service"
)

// Replicate downloads an export from rep.SourceURL (incremental when
// since is non-zero) and imports it into t, overwriting existing files.
func Replicate(ctx context.Context, client *http.Client, rep service.RegistryReplication, t Tree, expectType string, since time.Time) (ImportStats, error) {
	if rep.SourceURL == "" {
		return ImportStats{}, fmt.Errorf("replication: source_url is required")
	}
	u, err := url.Parse(rep.SourceURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ImportStats{}, fmt.Errorf("replication: invalid source_url %q", rep.SourceURL)
	}
	if !since.IsZero() {
		q := u.Query()
		q.Set("since", since.UTC().Format(time.RFC3339))
		u.RawQuery = q.Encode()
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ImportStats{}, err
	}
	req.Header.Set("Accept", "application/gzip")
	if tok := strings.TrimSpace(rep.Token); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return ImportStats{}, fmt.Errorf("replication: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return ImportStats{}, fmt.Errorf("replication: source returned %s: %s", resp.Status, strings.TrimSpace(string(preview)))
	}

	stats, err := Import(ctx, t, resp.Body, expectType, true)
	if err != nil {
		return stats, fmt.Errorf("replication: %w", err)
	}
	return stats, nil
}
