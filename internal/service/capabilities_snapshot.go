package service

// capabilities_snapshot.go is a reference list of the capability
// keys related to the features extracted from pika (raw files,
// proxy graphs, package registries). When kutu wires up its own
// permission/capability layer it should fold these into the local
// vocabulary; consumers of /api/v1/info in pika consumed the same
// strings, so re-using them keeps token scopes round-trippable.

const (
	// Raw file capabilities cover the rawfs-backed file browser
	// (local mounts, S3, FTP, SFTP, WebDAV, Vercel Blob).
	SnapshotCapRawRead  = "raw.read"
	SnapshotCapRawWrite = "raw.write"

	// Proxy capabilities cover the user-built Proxy Servers graph
	// (listeners, switches, middleware, handlers).
	SnapshotCapProxyRead   = "proxy.read"
	SnapshotCapProxyManage = "proxy.manage"

	// Registry capabilities cover the artifact-registry feature
	// (Go modules, NPM, Docker/OCI, Helm, Cargo, Maven, PyPI).
	SnapshotCapRegistryRead   = "registry.read"
	SnapshotCapRegistryWrite  = "registry.write"
	SnapshotCapRegistryDelete = "registry.delete"
	SnapshotCapRegistryAdmin  = "registry.admin"
)

// SnapshotCapability mirrors service.Capability from pika so kutu
// can keep the descriptive metadata next to the keys without
// pulling in the rest of the pika service package.
type SnapshotCapability struct {
	Key         string
	Name        string
	Description string
}

// SnapshotKnownCapabilities is the descriptive list of capability
// keys extracted from pika. Order matches what the pika UI used to
// render in its permission editor.
var SnapshotKnownCapabilities = []SnapshotCapability{
	{SnapshotCapRawRead, "Raw Files Read", "Browse and download raw files from mounted filesystems"},
	{SnapshotCapRawWrite, "Raw Files Write", "Upload, delete, rename, copy and move raw files"},
	{SnapshotCapProxyRead, "Proxy Read", "View configured proxy servers, their pipelines, live status and run test requests against them"},
	{SnapshotCapProxyManage, "Proxy Manage", "Create, edit and delete proxy server graphs (listeners, middleware, handlers)"},
	{SnapshotCapRegistryRead, "Registry Read", "Browse and pull artifacts from configured registries (Go modules, NPM packages, Docker/OCI images)"},
	{SnapshotCapRegistryWrite, "Registry Write", "Publish and push artifacts to local registries"},
	{SnapshotCapRegistryDelete, "Registry Delete", "Remove tags, versions and manifests from registries"},
	{SnapshotCapRegistryAdmin, "Registry Admin", "Manage namespaces and repositories, trigger maintenance actions (rebuild index, garbage collect)"},
}
