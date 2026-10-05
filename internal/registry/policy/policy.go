// Package policy implements kutu's data-plane request policy: package
// include/exclude filters, version quarantine, license allow/deny
// lists, OSV.dev vulnerability blocking and Docker signature
// enforcement. The server installs it process-wide with
//
//	registry.SetRequestGate(policy.New(lookup, opts).Gate)
//
// Only read requests (GET/HEAD) are fully evaluated; write requests
// are checked against Include/Exclude only, so a repository cannot
// receive packages outside its allowed name space. Every rejection is
// a 403 with a human-readable reason.
package policy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/service"
)

// RepoLookup returns the repository row backing (ns, repo), or nil.
type RepoLookup func(ctx context.Context, ns, repo string) *service.RegistryRepository

// Options configures an Engine. Zero values select defaults.
type Options struct {
	OSVBaseURL string        // default https://api.osv.dev
	HTTPClient *http.Client  // default client with a 10s timeout
	CacheTTL   time.Duration // OSV result cache TTL; default 6h
	Now        func() time.Time
}

// Decision is the outcome of evaluating one artifact request.
type Decision struct {
	Allowed bool
	Status  int
	Reason  string
}

var allow = Decision{Allowed: true}

func deny(format string, args ...any) Decision {
	return Decision{Status: http.StatusForbidden, Reason: fmt.Sprintf(format, args...)}
}

// Engine evaluates repository policies. Safe for concurrent use.
type Engine struct {
	lookup RepoLookup
	now    func() time.Time
	osv    *osvClient
}

// New builds an Engine.
func New(lookup RepoLookup, opts Options) *Engine {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{
		lookup: lookup,
		now:    opts.Now,
		osv:    newOSVClient(opts),
	}
}

// Gate is a registry.RequestGate.
func (e *Engine) Gate(reg registry.Registry, r *http.Request) (int, string, bool) {
	p := e.policyFor(r.Context(), reg)
	if p == nil {
		return 0, "", true
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !read && (r.Method == http.MethodOptions || (len(p.Include) == 0 && len(p.Exclude) == 0)) {
		return 0, "", true
	}
	if read && !readActive(p) {
		return 0, "", true
	}
	cl, ok := reg.(registry.ArtifactClassifier)
	if !ok {
		return 0, "", true
	}
	ref, ok := cl.ClassifyRequest(r)
	if !ok {
		return 0, "", true
	}
	var d Decision
	if read {
		d = e.evaluate(r.Context(), reg, p, ref)
	} else {
		d = checkName(p, ref.Name)
	}
	if d.Allowed {
		return 0, "", true
	}
	return d.Status, d.Reason, false
}

// Evaluate applies the read policy of reg's repository to ref.
func (e *Engine) Evaluate(ctx context.Context, reg registry.Registry, ref registry.ArtifactRef) Decision {
	p := e.policyFor(ctx, reg)
	if p == nil || !readActive(p) {
		return allow
	}
	return e.evaluate(ctx, reg, p, ref)
}

// Vulnerabilities returns every known OSV advisory for the version,
// unfiltered. Returns nil when the registry type has no OSV ecosystem.
func (e *Engine) Vulnerabilities(ctx context.Context, regType, name, version string) ([]Vuln, error) {
	eco := registry.Ecosystem(regType)
	if eco == "" || name == "" || version == "" {
		return nil, nil
	}
	return e.osv.query(ctx, eco, name, version)
}

func (e *Engine) policyFor(ctx context.Context, reg registry.Registry) *service.RegistryPolicy {
	if e.lookup == nil || reg == nil {
		return nil
	}
	row := e.lookup(ctx, reg.Namespace(), reg.Name())
	if row == nil {
		return nil
	}
	return row.Policy
}

func readActive(p *service.RegistryPolicy) bool {
	return len(p.Include) > 0 || len(p.Exclude) > 0 || p.QuarantineDays > 0 ||
		p.BlockVulnerable || len(p.AllowedLicenses) > 0 || len(p.DeniedLicenses) > 0 ||
		p.BlockUnknownLicense || p.RequireSignature
}

func checkName(p *service.RegistryPolicy, name string) Decision {
	if !NameAllowed(p.Include, p.Exclude, name) {
		return deny("package %s is blocked by repository policy", name)
	}
	return allow
}

func (e *Engine) evaluate(ctx context.Context, reg registry.Registry, p *service.RegistryPolicy, ref registry.ArtifactRef) Decision {
	if d := checkName(p, ref.Name); !d.Allowed {
		return d
	}
	if ref.Version == "" {
		return allow
	}
	label := ref.Name + "@" + ref.Version

	if p.QuarantineDays > 0 || len(p.AllowedLicenses) > 0 || len(p.DeniedLicenses) > 0 || p.BlockUnknownLicense {
		var meta registry.ArtifactMeta
		known := false
		if ip, ok := reg.(registry.ArtifactInfoProvider); ok {
			m, err := ip.ArtifactInfo(ctx, ref)
			if err == nil {
				meta, known = m, true
			}
		}
		if p.QuarantineDays > 0 && known && !meta.PublishedAt.IsZero() {
			until := meta.PublishedAt.Add(time.Duration(p.QuarantineDays) * 24 * time.Hour)
			if e.now().Before(until) {
				return deny("version %s is quarantined until %s", label, until.UTC().Format(time.RFC3339))
			}
		}
		if d := checkLicense(p, label, meta.License); !d.Allowed {
			return d
		}
	}

	if p.BlockVulnerable {
		if eco := registry.Ecosystem(reg.Type()); eco != "" {
			vulns, err := e.osv.query(ctx, eco, ref.Name, ref.Version)
			if err == nil {
				if ids := blocking(vulns, p.MinSeverity); len(ids) > 0 {
					return deny("version %s has known vulnerabilities: %s", label, summarizeIDs(ids))
				}
			}
		}
	}

	if p.RequireSignature && reg.Type() == service.RegistryTypeDocker && !strings.HasPrefix(ref.Version, "sha256:") {
		sc, ok := reg.(registry.SignatureChecker)
		if !ok {
			return deny("image %s:%s is not signed", ref.Name, ref.Version)
		}
		signed, err := sc.HasSignature(ctx, ref.Name, ref.Version)
		if err != nil && !errors.Is(err, registry.ErrPackageNotFound) {
			return deny("image %s:%s signature could not be verified: %v", ref.Name, ref.Version, err)
		}
		if !signed {
			return deny("image %s:%s is not signed", ref.Name, ref.Version)
		}
	}
	return allow
}

func checkLicense(p *service.RegistryPolicy, label, license string) Decision {
	if len(p.AllowedLicenses) == 0 && len(p.DeniedLicenses) == 0 && !p.BlockUnknownLicense {
		return allow
	}
	expr := ParseLicense(license)
	if expr == nil {
		if p.BlockUnknownLicense {
			return deny("license of %s is unknown", label)
		}
		return allow
	}
	if !LicensePermitted(expr, p.AllowedLicenses, p.DeniedLicenses) {
		return deny("license %q of %s is not permitted by repository policy", strings.TrimSpace(license), label)
	}
	return allow
}

func summarizeIDs(ids []string) string {
	const max = 6
	if len(ids) <= max {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:max], ", ") + fmt.Sprintf(" and %d more", len(ids)-max)
}
