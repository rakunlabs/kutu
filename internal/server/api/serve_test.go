package api

import (
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/service"
)

func user(name, pass string) service.FTPUserEntry {
	return service.FTPUserEntry{Username: name, Password: pass}
}

func share(name string) service.FTPShareEntry {
	return service.FTPShareEntry{Name: name, Paths: []string{"data/" + name}}
}

func TestValidateServeSettings_MultiInstance(t *testing.T) {
	cfg := &service.ServeSettings{
		Shares: []service.FTPShareEntry{share("pub"), share("fw")},
		Users:  []service.FTPUserEntry{user("deploy", "s3cret")},
		Servers: []service.ServeServerEntry{
			{Protocol: "ftp", Enabled: true, FTP: &service.FTPServeSettings{Port: 2121}},
			{Protocol: "ftp", Enabled: true, FTP: &service.FTPServeSettings{Port: 2122}},
			{Protocol: "s3", Enabled: true, S3: &service.S3ServeSettings{Port: 9000}},
			{Protocol: "s3", Enabled: true, S3: &service.S3ServeSettings{Port: 9001}, Shares: []string{"fw"}},
			{Protocol: "tftp", Enabled: true, Shares: []string{"fw"}},
		},
	}
	if err := validateServeSettings(cfg); err != nil {
		t.Fatalf("valid multi-instance config rejected: %v", err)
	}
	// IDs must have been assigned and be unique.
	seen := map[string]bool{}
	for _, sv := range cfg.Servers {
		if sv.ID == "" {
			t.Fatalf("server %q was not assigned an id", sv.Protocol)
		}
		if seen[sv.ID] {
			t.Fatalf("duplicate assigned id %q", sv.ID)
		}
		seen[sv.ID] = true
	}
}

func TestValidateServeSettings_PortConflict(t *testing.T) {
	cfg := &service.ServeSettings{
		Users: []service.FTPUserEntry{user("a", "b")},
		Servers: []service.ServeServerEntry{
			{Protocol: "ftp", Enabled: true, FTP: &service.FTPServeSettings{Port: 2121}},
			{Protocol: "webdav", Enabled: true, WebDAV: &service.WebDAVServeSettings{Port: 2121}},
		},
	}
	err := validateServeSettings(cfg)
	if err == nil || !strings.Contains(err.Error(), "port 2121") {
		t.Fatalf("expected tcp port conflict error, got: %v", err)
	}

	// UDP (tftp) does not conflict with TCP on the same port.
	cfg = &service.ServeSettings{
		Users: []service.FTPUserEntry{user("a", "b")},
		Servers: []service.ServeServerEntry{
			{Protocol: "ftp", Enabled: true, FTP: &service.FTPServeSettings{Port: 6969}},
			{Protocol: "tftp", Enabled: true, TFTP: &service.TFTPServeSettings{Port: 6969}},
		},
	}
	if err := validateServeSettings(cfg); err != nil {
		t.Fatalf("udp/tcp same port should not conflict: %v", err)
	}

	// Disabled servers do not occupy ports.
	cfg = &service.ServeSettings{
		Users: []service.FTPUserEntry{user("a", "b")},
		Servers: []service.ServeServerEntry{
			{Protocol: "ftp", Enabled: false, FTP: &service.FTPServeSettings{Port: 2121}},
			{Protocol: "ftp", Enabled: true, FTP: &service.FTPServeSettings{Port: 2121}},
		},
	}
	if err := validateServeSettings(cfg); err != nil {
		t.Fatalf("disabled server should not hold its port: %v", err)
	}

	// Default ports collide too (two ftp with port 0 → both 2121).
	cfg = &service.ServeSettings{
		Users: []service.FTPUserEntry{user("a", "b")},
		Servers: []service.ServeServerEntry{
			{Protocol: "ftp", Enabled: true},
			{Protocol: "ftp", Enabled: true},
		},
	}
	if err := validateServeSettings(cfg); err == nil {
		t.Fatal("expected default-port conflict error")
	}
}

func TestValidateServeSettings_ServerChecks(t *testing.T) {
	// Unknown protocol.
	cfg := &service.ServeSettings{
		Servers: []service.ServeServerEntry{{Protocol: "gopher", Enabled: false}},
	}
	if err := validateServeSettings(cfg); err == nil {
		t.Fatal("expected unknown protocol error")
	}

	// Unknown share reference.
	cfg = &service.ServeSettings{
		Shares:  []service.FTPShareEntry{share("pub")},
		Servers: []service.ServeServerEntry{{Protocol: "tftp", Enabled: false, Shares: []string{"nope"}}},
	}
	if err := validateServeSettings(cfg); err == nil {
		t.Fatal("expected unknown share reference error")
	}

	// Enabled authenticated protocol without users.
	cfg = &service.ServeSettings{
		Servers: []service.ServeServerEntry{{Protocol: "ftp", Enabled: true}},
	}
	if err := validateServeSettings(cfg); err == nil {
		t.Fatal("expected users-required error")
	}

	// TFTP is anonymous: fine without users.
	cfg = &service.ServeSettings{
		Servers: []service.ServeServerEntry{{Protocol: "tftp", Enabled: true}},
	}
	if err := validateServeSettings(cfg); err != nil {
		t.Fatalf("tftp should not require users: %v", err)
	}

	// S3 needs a password user (SigV4 secret).
	cfg = &service.ServeSettings{
		Users:   []service.FTPUserEntry{{Username: "keyonly", AuthorizedKeys: "ssh-ed25519 AAAA"}},
		Servers: []service.ServeServerEntry{{Protocol: "s3", Enabled: true}},
	}
	if err := validateServeSettings(cfg); err == nil {
		t.Fatal("expected s3 password-user error")
	}

	// Duplicate explicit IDs.
	cfg = &service.ServeSettings{
		Users: []service.FTPUserEntry{user("a", "b")},
		Servers: []service.ServeServerEntry{
			{ID: "x", Protocol: "ftp", Enabled: false},
			{ID: "x", Protocol: "sftp", Enabled: false},
		},
	}
	if err := validateServeSettings(cfg); err == nil {
		t.Fatal("expected duplicate id error")
	}
}
