package service

// settings_feature.go holds the feature-specific config entity types
// (raw mounts, FTP/SFTP/TFTP/WebDAV serve listeners, proxy listeners,
// registry tree). Each entity is persisted in its own relational table
// by internal/storage; document-shaped sub-fields are JSONB columns.

// RawMountEntry is a single raw mount configured via the UI.
type RawMountEntry struct {
	Prefix     string                 `json:"prefix"`
	Type       string                 `json:"type,omitempty"` // "local" (default), "s3", "ftp", "sftp", "webdav", "vercel-blob"
	Path       string                 `json:"path,omitempty"` // for type=local
	S3         *S3ConfigEntry         `json:"s3,omitempty"`
	FTP        *FTPConfigEntry        `json:"ftp,omitempty"`
	SFTP       *SFTPConfigEntry       `json:"sftp,omitempty"`
	WebDAV     *WebDAVConfigEntry     `json:"webdav,omitempty"`
	VercelBlob *VercelBlobConfigEntry `json:"vercelBlob,omitempty"`
}

// S3ConfigEntry holds S3 configuration stored in settings.
type S3ConfigEntry struct {
	Bucket    string `json:"bucket"`
	Region    string `json:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Secure    *bool  `json:"secure,omitempty"`
}

// FTPConfigEntry holds FTP configuration stored in settings.
type FTPConfigEntry struct {
	Host     string `json:"host"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	TLS      bool   `json:"tls,omitempty"`
	BasePath string `json:"base_path,omitempty"`
}

// SFTPConfigEntry holds SFTP (SSH) configuration stored in settings.
type SFTPConfigEntry struct {
	Host       string `json:"host"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
	BasePath   string `json:"base_path,omitempty"`
}

// WebDAVConfigEntry holds WebDAV configuration stored in settings.
type WebDAVConfigEntry struct {
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	BasePath string `json:"base_path,omitempty"`
}

// VercelBlobConfigEntry holds Vercel Blob configuration stored in settings.
type VercelBlobConfigEntry struct {
	Token   string `json:"token"`
	StoreID string `json:"store_id,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
}

// FTPServeSettings configures a built-in FTP server instance (stored in
// DB as part of a ServeServerEntry).
type FTPServeSettings struct {
	Port         int    `json:"port,omitempty"`
	Host         string `json:"host,omitempty"`
	PublicIP     string `json:"public_ip,omitempty"`
	PassivePorts string `json:"passive_ports,omitempty"`
	TLSCertFile  string `json:"tls_cert_file,omitempty"`
	TLSKeyFile   string `json:"tls_key_file,omitempty"`
	TLSCertPEM   string `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM    string `json:"tls_key_pem,omitempty"`
	TLSRequired  int    `json:"tls_required,omitempty"`
}

// SFTPServeSettings configures a built-in SFTP server instance (stored in DB).
type SFTPServeSettings struct {
	Port        int    `json:"port,omitempty"`
	Host        string `json:"host,omitempty"`
	HostKeyPath string `json:"host_key_path,omitempty"`
	HostKeyPEM  string `json:"host_key_pem,omitempty"`
}

// TFTPServeSettings configures a built-in TFTP server instance (stored in DB).
type TFTPServeSettings struct {
	Port int    `json:"port,omitempty"`
	Host string `json:"host,omitempty"`
}

// WebDAVServeSettings configures a built-in WebDAV server instance (stored in DB).
//
// Hostname (optional) scopes the instance to one virtual host: several
// WebDAV/S3/registry endpoints may then share the same port, routed by
// the request's Host header through the shared vhost layer. TLS is
// optional (PEM pair, both or neither) — behind a TLS-terminating
// reverse proxy the instance runs plain.
type WebDAVServeSettings struct {
	Port       int    `json:"port,omitempty"`
	Host       string `json:"host,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	TLSCertPEM string `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM  string `json:"tls_key_pem,omitempty"`
}

// S3ServeSettings configures a built-in S3-compatible server instance
// (stored in DB). Shares are exposed as buckets; users authenticate with
// SigV4 where the username is the access key and the password is the
// secret key.
// Hostname (optional) scopes the instance to one virtual host so it can
// share its port with other HTTP-based endpoints (see WebDAVServeSettings).
type S3ServeSettings struct {
	Port       int    `json:"port,omitempty"`
	Host       string `json:"host,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	Region     string `json:"region,omitempty"`
	TLSCertPEM string `json:"tls_cert_pem,omitempty"`
	TLSKeyPEM  string `json:"tls_key_pem,omitempty"`
}

// FTPUserEntry defines an FTP user account stored in settings.
type FTPUserEntry struct {
	Username       string   `json:"username"`
	Password       string   `json:"password,omitempty"`
	Shares         []string `json:"shares,omitempty"`
	AuthorizedKeys string   `json:"authorized_keys,omitempty"`
	ReadOnly       bool     `json:"read_only"`
}

// FTPShareEntry defines a folder shared via the built-in FTP server.
type FTPShareEntry struct {
	Name     string   `json:"name"`
	Paths    []string `json:"paths"`
	ReadOnly bool     `json:"read_only"`
	Root     bool     `json:"root,omitempty"`
}

// ServeProtocols is the closed set of protocols a ServeServerEntry may
// use, in the canonical display order.
var ServeProtocols = []string{"ftp", "sftp", "tftp", "webdav", "s3"}

// ServeServerEntry is one configured file-serving server instance. Any
// number of instances may exist, including several of the same protocol
// on different ports (e.g. two S3 endpoints, or an FTP per team).
//
// Shares lists the names of the global shares this instance exposes; an
// empty list means "all shares". Exactly one of the protocol-specific
// settings pointers matching Protocol should be set (a nil pointer means
// protocol defaults).
type ServeServerEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Protocol string   `json:"protocol"` // "ftp" | "sftp" | "tftp" | "webdav" | "s3"
	Enabled  bool     `json:"enabled"`
	Shares   []string `json:"shares,omitempty"` // share names served; empty = all

	FTP    *FTPServeSettings    `json:"ftp,omitempty"`
	SFTP   *SFTPServeSettings   `json:"sftp,omitempty"`
	TFTP   *TFTPServeSettings   `json:"tftp,omitempty"`
	WebDAV *WebDAVServeSettings `json:"webdav,omitempty"`
	S3     *S3ServeSettings     `json:"s3,omitempty"`
}

// ServeSettings is the aggregate configuration for kutu's built-in file
// serving. It is persisted as a single JSONB singleton in kutu_meta and
// edited as a whole from the UI. Servers is the list of independent
// server instances; the Users and Shares lists are shared pools: shares
// define which raw-mount paths can be exposed (each server picks its
// own subset) and users provide the credentials the FTP / SFTP /
// WebDAV / S3 servers authenticate against (TFTP is anonymous by
// protocol design). For S3, shares appear as buckets and a user's
// username/password act as the access/secret key pair.
type ServeSettings struct {
	Servers []ServeServerEntry `json:"servers,omitempty"`
	Users   []FTPUserEntry     `json:"users,omitempty"`
	Shares  []FTPShareEntry    `json:"shares,omitempty"`
}
