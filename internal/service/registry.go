package service

// Registry settings model — namespace + repository tree configured via UI.
//
// Pika's artifact registry feature is multi-tenant by design: a single
// deployment carries N "namespaces" (logical tenants / teams), and each
// namespace holds M "repositories". A repository has a type
// (go|npm|docker|helm|maven|pypi|cargo) and a kind
// (local|remote|virtual). The triple
// {namespace, repo-name, type} uniquely identifies a serving surface:
//
//   /registries/{namespace}/{repo}/{type-specific-path}
//
// Local repositories store artifacts in a user-selected RawMount under
// a per-repo base path. Remote repositories proxy an upstream URL with
// pull-through caching. Virtual repositories aggregate a list of other
// repos (local + remote) and expose them under one endpoint with a
// configurable lookup order.
//
// Why this lives in the service package (not internal/registry):
//
//   - The Settings row already lives here. Registry config is just
//     another sub-tree of Settings (alongside RawMounts, Hooks,
//     ProxyServers), so keeping the model definition adjacent avoids
//     an import cycle between service and the registry runtime.
//
//   - The HTTP layer and the registry runtime both need to read these
//     structs. Putting them in service keeps the dependency arrow
//     pointing in one direction (registry -> service, not the other
//     way around).
//
//   - Secret values inside RegistrySettings (upstream credentials)
//     piggyback on the existing settings_seal.go sealed-payload
//     mechanism — see internal/secret/settings_seal.go for the
//     extraction/injection pattern.

// RegistrySettings is the root of the registry config. It is referenced
// from Settings (settings.go) as an optional pointer so installations
// that don't use the feature carry zero bytes for it.
type RegistrySettings struct {
	// Disabled is the deployment-wide feature flag for the artifact
	// registry (zero-value = enabled, preserving backward
	// compatibility with pre-flag rows). When true:
	//
	//   - The SPA hides the Registries link from navigation.
	//   - /api/v1/registries/* admin endpoints respond 404.
	//   - The data plane /registries/{ns}/{repo}/... refuses every
	//     request with 404 — package managers see "not configured".
	//   - Existing namespace / repository configuration is preserved;
	//     flipping the flag back makes them serve again.
	//
	// This is a feature-flag, not a destructive action: no on-disk
	// artifacts are touched.
	Disabled bool `json:"disabled,omitempty"`

	// Namespaces is the ordered list of configured tenants. The order
	// is preserved across Save so the UI can present a stable list;
	// it carries no semantic meaning beyond display order.
	//
	// Pika auto-creates a namespace named "default" on first boot
	// (see internal/registry.bootstrap) so a fresh install has
	// somewhere to attach repositories without an extra setup step.
	Namespaces []RegistryNamespace `json:"namespaces,omitempty"`
}

// RegistryNamespace groups repositories. The name is the URL path
// segment ("/registries/{name}/...") and must be unique across the
// installation. Allowed characters: lowercase alphanumerics, hyphen,
// underscore — matches the constraints of every registry protocol
// we support (NPM scope-less names, Docker repo names, Go module
// path segments).
type RegistryNamespace struct {
	Name         string               `json:"name"`
	Description  string               `json:"description,omitempty"`
	Repositories []RegistryRepository `json:"repositories,omitempty"`
}

// RegistryRepository is one repo inside a namespace. The Type field
// determines which protocol handler will serve it; Kind determines
// the backing mode (local storage / remote proxy / virtual
// aggregation).
//
// Field validity by Kind:
//
//	local   -> Mount + BasePath required, AllowPush honored
//	remote  -> URL + cache Mount/BasePath required, Auth optional (raw:// / config:// refs)
//	virtual -> Members required, ordered list of sibling repo names
//
// The validator (Validate, below) rejects rows that mix fields across
// kinds so the runtime can trust the shape without re-checking.
type RegistryRepository struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"` // "go" | "npm" | "docker" | "helm" | "maven" | "pypi" | "cargo"
	Kind        string `json:"kind"` // "local" | "remote" | "virtual"

	// Local fields
	Mount     string `json:"mount,omitempty"`      // raw mount prefix
	BasePath  string `json:"base_path,omitempty"`  // e.g. "go/" or "npm/"
	AllowPush bool   `json:"allow_push,omitempty"` // publish/push enabled

	// Remote fields
	URL  string                `json:"url,omitempty"`  // upstream base URL (default upstream)
	Auth *RegistryUpstreamAuth `json:"auth,omitempty"` // optional, for the default upstream
	// Upstreams is an optional list of additional, prefix-routed
	// upstreams. Currently honored only by Go remote repos: when set,
	// the runtime picks the entry whose Prefix is the longest match for
	// the requested module path and falls back to URL/Auth (the default
	// upstream) when nothing matches. Empty = single upstream (URL/Auth).
	Upstreams []RegistryUpstream `json:"upstreams,omitempty"`
	// MutableTTL controls how long mutable upstream responses
	// (latest tag, version list) are cached before being refetched.
	// Empty -> default per registry type (e.g. 5m for Go @latest).
	// Format is a Go duration string ("5m", "1h", "0s" to disable).
	MutableTTL string `json:"mutable_ttl,omitempty"`
	// FloatingTags is the operator-provided list of Docker tag
	// names that should be treated as mutable — i.e. the tag →
	// digest pointer is re-resolved through upstream every TTL
	// window. Tags not in this list are cached forever after the
	// first successful resolve.
	//
	// This carves out the common semver convention: tags like
	// "v1.2.3", "1.2", "2024-05-19" are by convention immutable,
	// but pika has no way to know that ahead of time. The operator
	// declares the floaters explicitly.
	//
	// Empty list ⇒ default ("latest", "main", "master", "dev",
	// "develop", "nightly", "edge", "stable", "canary"). Set to a
	// single-element list ["*"] to force every tag to be
	// TTL-bounded (the pre-FloatingTags behaviour, useful for
	// upstreams that overwrite semver tags in flight).
	//
	// Only honored by Docker Remote — Go and NPM have a dedicated
	// @latest / packument endpoint so the floater is already
	// isolated and this list is silently ignored for them.
	FloatingTags []string `json:"floating_tags,omitempty"`
	// InsecureSkipVerify disables TLS cert verification for the
	// upstream. Off by default; only useful for internal mirrors
	// with self-signed certs.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`

	// Virtual fields
	// Members is an ordered list of sibling repository names (within
	// the same namespace) to aggregate. Lookup tries members in order
	// on every request; the first match wins. Writes to a virtual
	// repo are rejected — clients must address a concrete local repo.
	Members []string `json:"members,omitempty"`
	// DefaultLocal is an optional hint for the UI / clients about
	// which local member should receive writes. Not enforced by the
	// runtime.
	DefaultLocal string `json:"default_local,omitempty"`

	// Common per-repo overrides
	CORSOrigins   []string        `json:"cors_origins,omitempty"`
	MaxUploadSize int64           `json:"max_upload_size,omitempty"` // bytes; 0 = type default
	Policy        *RegistryPolicy `json:"policy,omitempty"`

	// SigningKey is an ASCII-armored OpenPGP private key (apt, rpm) or
	// a PEM RSA private key (alpine) used to sign generated repository
	// metadata. Sealed at rest like upstream credentials; never
	// returned to non-admin callers. Empty = unsigned metadata.
	SigningKey string `json:"signing_key,omitempty"`
	// SigningKeyName is the public key file name advertised to
	// clients (alpine: "{name}.rsa.pub"). Optional.
	SigningKeyName string `json:"signing_key_name,omitempty"`

	// Prefetch (remote kind) keeps the listed packages warm in the
	// cache: every Interval the server refreshes metadata and the
	// latest version (or the pinned "name@version").
	Prefetch *RegistryPrefetch `json:"prefetch,omitempty"`

	// Replication (local kind) pulls this repository's content from
	// another kutu instance's export endpoint on a schedule (mirror /
	// DR / air-gap staging).
	Replication *RegistryReplication `json:"replication,omitempty"`
}

// RegistryPrefetch configures scheduled cache warming.
type RegistryPrefetch struct {
	Packages []string `json:"packages,omitempty"` // "name" or "name@version"
	Interval string   `json:"interval,omitempty"` // Go duration; default 6h
}

// RegistryReplication configures a pull replica of another kutu repo.
type RegistryReplication struct {
	// SourceURL is the full export URL of the source repository, e.g.
	// https://kutu.example.com/api/v1/registries/npm/default/npm-local/export
	SourceURL string `json:"source_url"`
	// Token is a kutu API token for the source (sealed at rest).
	Token    string `json:"token,omitempty"`
	Interval string `json:"interval,omitempty"` // Go duration; default 1h
	Disabled bool   `json:"disabled,omitempty"`
}

// RegistryPolicy carries optional per-repository operational policy.
// The first enforced fields target Docker Local repositories because
// they already have mutable tags and an explicit GC flow. The struct
// is intentionally protocol-neutral so later phases can extend it to
// Go/NPM/Helm retention without changing the settings shape.
type RegistryPolicy struct {
	// ImmutableTags is a list of Docker tag patterns that may be created
	// once but cannot be moved to another digest or deleted. Patterns use
	// shell-style matching (`v*`, `prod`, `release-*`) against one tag.
	ImmutableTags []string                 `json:"immutable_tags,omitempty"`
	Retention     *RegistryRetentionPolicy `json:"retention,omitempty"`

	// Include / Exclude are shell-style package-name patterns
	// (path.Match, plus "**" suffix for "any depth"). When Include is
	// non-empty only matching packages are served; Exclude always
	// wins. Applies to every kind; most useful on remote/virtual repos
	// to block dependency-confusion attacks.
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`

	// ImmutableVersions rejects re-publishing an existing version on
	// local repos (all types).
	ImmutableVersions bool `json:"immutable_versions,omitempty"`

	// QuotaBytes caps the stored size of a local repo; pushes that
	// would exceed it are rejected with 413. 0 = unlimited.
	QuotaBytes int64 `json:"quota_bytes,omitempty"`

	// QuarantineDays hides versions first seen (remote) or published
	// (local) less than N days ago. 0 = disabled.
	QuarantineDays int `json:"quarantine_days,omitempty"`

	// Vulnerability policy backed by OSV.dev.
	//   BlockVulnerable — refuse downloads of versions with a known
	//                     OSV advisory.
	//   MinSeverity     — optional floor ("low"|"moderate"|"high"|
	//                     "critical"); advisories below are ignored.
	BlockVulnerable bool   `json:"block_vulnerable,omitempty"`
	MinSeverity     string `json:"min_severity,omitempty"`

	// License policy. AllowedLicenses (SPDX ids, case-insensitive)
	// when non-empty is an allowlist; DeniedLicenses always blocks.
	// Packages without license metadata pass unless
	// BlockUnknownLicense is set.
	AllowedLicenses     []string `json:"allowed_licenses,omitempty"`
	DeniedLicenses      []string `json:"denied_licenses,omitempty"`
	BlockUnknownLicense bool     `json:"block_unknown_license,omitempty"`

	// RequireSignature (docker only) refuses manifest pulls by tag
	// when no cosign/notation signature referrer exists.
	RequireSignature bool `json:"require_signature,omitempty"`
}

// RegistryRetentionPolicy stores default cleanup windows for the
// manual Docker GC estimate/apply buttons. Operators can still override
// these per request; the policy controls the repo's normal defaults.
type RegistryRetentionPolicy struct {
	// GCMinAgeSeconds protects recently-written unreferenced blobs and
	// manifests from GC. Zero means use the server default.
	GCMinAgeSeconds int64 `json:"gc_min_age_seconds,omitempty"`
	// AbandonedUploadMaxAgeSeconds controls stale upload tmp pruning.
	// Zero means use the server default.
	AbandonedUploadMaxAgeSeconds int64 `json:"abandoned_upload_max_age_seconds,omitempty"`

	// KeepLastVersions keeps only the newest N versions of each
	// package when retention is applied (all local types). 0 = off.
	KeepLastVersions int `json:"keep_last_versions,omitempty"`
	// MaxVersionAgeDays deletes versions older than N days, never
	// removing the newest version. 0 = off.
	MaxVersionAgeDays int `json:"max_version_age_days,omitempty"`
	// KeepPatterns are version patterns (path.Match) never deleted
	// by retention.
	KeepPatterns []string `json:"keep_patterns,omitempty"`
}

// RegistryUpstream is one prefix-routed upstream for a remote
// repository. The runtime selects the upstream whose Prefix is the
// longest match for the requested path (e.g. a Go module path) and
// falls back to the repo-level URL/Auth when no entry matches. Each
// upstream carries its own URL and optional auth.
//
// SSHKey is a PEM-encoded private key reserved for a future
// git-over-SSH fetch mode (private modules pulled directly from a VCS).
// It is persisted (sealed at rest) but NOT yet used by the runtime —
// the Go remote is still an HTTP GOPROXY pull-through cache.
type RegistryUpstream struct {
	Prefix string                `json:"prefix,omitempty"` // path prefix this upstream serves
	URL    string                `json:"url"`              // upstream base URL (http/https)
	Auth   *RegistryUpstreamAuth `json:"auth,omitempty"`   // optional, per-upstream
	SSHKey string                `json:"ssh_key,omitempty"` // PEM private key (store-only, future git+ssh)
}

// RegistryUpstreamAuth describes how to authenticate to an upstream
// registry when proxying remote repositories. Secret fields (Password,
// Token, Value) are entered directly and sealed with the at-rest
// encryption key by the storage layer before persistence; they are
// never returned to non-admin API callers.
type RegistryUpstreamAuth struct {
	Type     string `json:"type"` // "basic" | "bearer" | "header"
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // basic
	Token    string `json:"token,omitempty"`    // bearer
	Header   string `json:"header,omitempty"`   // header
	Value    string `json:"value,omitempty"`    // header
}

// Registry type / kind constants. Defined as strings (not enum-like
// types) because the JSON shape uses bare strings and re-tagging
// everywhere would be churn for no win.
const (
	RegistryTypeGo     = "go"
	RegistryTypeNPM    = "npm"
	RegistryTypeDocker = "docker"
	// RegistryTypeHelm is the pure-HTTP Helm chart repository
	// (`index.yaml` + `{chart}-{version}.tgz`). Distinct from
	// pushing Helm charts as OCI artifacts into a Docker registry;
	// the type is reserved for the classic Helm protocol so a
	// `helm repo add` against pika just works. The data plane
	// optionally accepts ChartMuseum-style writes (POST /api/charts)
	// for ergonomic publishing.
	RegistryTypeHelm  = "helm"
	RegistryTypeMaven = "maven"
	RegistryTypePyPI  = "pypi"
	RegistryTypeCargo = "cargo"

	RegistryTypeGeneric     = "generic"
	RegistryTypeNuGet       = "nuget"
	RegistryTypeRubyGems    = "rubygems"
	RegistryTypeComposer    = "composer"
	RegistryTypeTerraform   = "terraform"
	RegistryTypePub         = "pub"
	RegistryTypeSwift       = "swift"
	RegistryTypeAPT         = "apt"
	RegistryTypeRPM         = "rpm"
	RegistryTypeAlpine      = "alpine"
	RegistryTypeConda       = "conda"
	RegistryTypeHuggingFace = "huggingface"
	RegistryTypeConan       = "conan"
	RegistryTypeCRAN        = "cran"
	RegistryTypeVagrant     = "vagrant"
	RegistryTypeAnsible     = "ansible"
	RegistryTypePuppet      = "puppet"
	RegistryTypeChef        = "chef"
	RegistryTypeCocoaPods   = "cocoapods"
	RegistryTypeBower       = "bower"
	RegistryTypeGitLFS      = "gitlfs"
	RegistryTypeP2          = "p2"

	RegistryKindLocal   = "local"
	RegistryKindRemote  = "remote"
	RegistryKindVirtual = "virtual"

	RegistryAuthBasic  = "basic"
	RegistryAuthBearer = "bearer"
	RegistryAuthHeader = "header"
)

// KnownRegistryTypes is the canonical allowlist of registry types
// pika ships with. The validator and the factory wiring both read
// this slice — adding a new protocol means a single insertion here
// (plus the factory registration in registry_wire.go).
var KnownRegistryTypes = []string{
	RegistryTypeGo,
	RegistryTypeNPM,
	RegistryTypeDocker,
	RegistryTypeHelm,
	RegistryTypeMaven,
	RegistryTypePyPI,
	RegistryTypeCargo,
	RegistryTypeGeneric,
	RegistryTypeNuGet,
	RegistryTypeRubyGems,
	RegistryTypeComposer,
	RegistryTypeTerraform,
	RegistryTypePub,
	RegistryTypeSwift,
	RegistryTypeAPT,
	RegistryTypeRPM,
	RegistryTypeAlpine,
	RegistryTypeConda,
	RegistryTypeHuggingFace,
	RegistryTypeConan,
	RegistryTypeCRAN,
	RegistryTypeVagrant,
	RegistryTypeAnsible,
	RegistryTypePuppet,
	RegistryTypeChef,
	RegistryTypeCocoaPods,
	RegistryTypeBower,
	RegistryTypeGitLFS,
	RegistryTypeP2,
}

// RegistryTypesWithPrefixUpstreams lists the protocols whose remote
// kind honours RegistryRepository.Upstreams (longest-prefix routing
// on the package name).
var RegistryTypesWithPrefixUpstreams = []string{
	RegistryTypeGo,
	RegistryTypeNPM,
	RegistryTypeMaven,
}

// RegistryTypesWithSigning lists the protocols whose local kind
// signs generated repository metadata with RegistryRepository.SigningKey.
var RegistryTypesWithSigning = []string{
	RegistryTypeAPT,
	RegistryTypeRPM,
	RegistryTypeAlpine,
}

func registryTypeIn(t string, list []string) bool {
	for _, k := range list {
		if t == k {
			return true
		}
	}
	return false
}

// IsKnownRegistryType reports whether t is one of the registered
// protocol types. Used by the validator and by any future code path
// that needs to whitelist registry types without hard-coding the
// switch.
func IsKnownRegistryType(t string) bool {
	for _, k := range KnownRegistryTypes {
		if t == k {
			return true
		}
	}
	return false
}

// DefaultRegistryNamespace is the name of the namespace pika auto-
// creates on first boot when RegistrySettings is empty. The bootstrap
// is non-destructive: if any namespace exists already (including one
// named "default" that the operator deleted then recreated), the
// bootstrap step does nothing.
const DefaultRegistryNamespace = "default"
