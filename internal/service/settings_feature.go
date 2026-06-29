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

// FTPServeSettings configures the built-in FTP server (stored in DB).
type FTPServeSettings struct {
	Enabled      bool   `json:"enabled"`
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

// SFTPServeSettings configures the built-in SFTP server (stored in DB).
type SFTPServeSettings struct {
	Enabled     bool   `json:"enabled"`
	Port        int    `json:"port,omitempty"`
	Host        string `json:"host,omitempty"`
	HostKeyPath string `json:"host_key_path,omitempty"`
	HostKeyPEM  string `json:"host_key_pem,omitempty"`
}

// TFTPServeSettings configures the built-in TFTP server (stored in DB).
type TFTPServeSettings struct {
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port,omitempty"`
	Host    string `json:"host,omitempty"`
}

// WebDAVServeSettings configures the built-in WebDAV server (stored in DB).
type WebDAVServeSettings struct {
	Enabled bool   `json:"enabled"`
	Port    int    `json:"port,omitempty"`
	Host    string `json:"host,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
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

// ProxySettings is the deployment-wide feature flag for the user-built
// Proxy Servers. Kept for JSON round-trip compatibility; the live flag
// is stored in kutu_meta and read via Service.ProxyEnabled.
type ProxySettings struct {
	Disabled bool `json:"disabled"`
}
