package service

import (
	"context"
	"fmt"
	"io"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/rawfs/localfs"
)

// fetchRawMountConfig reads a file from a raw mount and returns its contents.
// The mountPrefix identifies which raw mount to use, and path is the file path within it.
func (s *Service) fetchRawMountConfig(ctx context.Context, mountPrefix string, path string) ([]byte, error) {
	mountEntry, err := s.GetRawMount(ctx, mountPrefix)
	if err != nil {
		return nil, fmt.Errorf("raw mount %q: %w", mountPrefix, err)
	}

	fs, err := newRawFSFromMountEntry(*mountEntry)
	if err != nil {
		return nil, fmt.Errorf("creating filesystem for mount %q: %w", mountPrefix, err)
	}

	reader, _, err := fs.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading file %q from mount %q: %w", path, mountPrefix, err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("reading file %q from mount %q: %w", path, mountPrefix, err)
	}

	return data, nil
}

// NewRawFS builds a rawfs.RawFS backend from a persisted RawMountEntry.
// Exported so the server/api layer can materialize the configured raw
// mounts without re-implementing the backend switch.
func NewRawFS(m RawMountEntry) (rawfs.RawFS, error) {
	return newRawFSFromMountEntry(m)
}

// newRawFSFromMountEntry creates a RawFS instance from a RawMountEntry.
func newRawFSFromMountEntry(m RawMountEntry) (rawfs.RawFS, error) {
	mountType := m.Type
	if mountType == "" {
		mountType = "local"
	}

	switch mountType {
	case "local":
		if m.Path == "" {
			return nil, fmt.Errorf("path is required for local mount")
		}
		return localfs.New(m.Path)
	case "s3":
		if m.S3 == nil {
			return nil, fmt.Errorf("s3 config is required")
		}
		if rawfs.NewS3FSFunc == nil {
			return nil, fmt.Errorf("s3 backend not available")
		}
		return rawfs.NewS3FSFunc(m.S3.Bucket, m.S3.Region, m.S3.Endpoint, m.S3.AccessKey, m.S3.SecretKey, m.S3.Prefix, m.S3.PathStyle, m.S3.Secure)
	case "ftp":
		if m.FTP == nil {
			return nil, fmt.Errorf("ftp config is required")
		}
		if rawfs.NewFTPFSFunc == nil {
			return nil, fmt.Errorf("ftp backend not available")
		}
		return rawfs.NewFTPFSFunc(m.FTP.Host, m.FTP.Username, m.FTP.Password, m.FTP.BasePath, m.FTP.TLS)
	case "sftp":
		if m.SFTP == nil {
			return nil, fmt.Errorf("sftp config is required")
		}
		if rawfs.NewSFTPFSFunc == nil {
			return nil, fmt.Errorf("sftp backend not available")
		}
		return rawfs.NewSFTPFSFunc(m.SFTP.Host, m.SFTP.Username, m.SFTP.Password, m.SFTP.PrivateKey, m.SFTP.BasePath)
	case "webdav":
		if m.WebDAV == nil {
			return nil, fmt.Errorf("webdav config is required")
		}
		if rawfs.NewWebDAVFSFunc == nil {
			return nil, fmt.Errorf("webdav backend not available")
		}
		return rawfs.NewWebDAVFSFunc(m.WebDAV.URL, m.WebDAV.Username, m.WebDAV.Password, m.WebDAV.BasePath)
	case "vercel-blob":
		if m.VercelBlob == nil {
			return nil, fmt.Errorf("vercelBlob config is required")
		}
		if rawfs.NewVercelBlobFSFunc == nil {
			return nil, fmt.Errorf("vercel-blob backend not available")
		}
		return rawfs.NewVercelBlobFSFunc(m.VercelBlob.Token, m.VercelBlob.StoreID, m.VercelBlob.Prefix)
	default:
		return nil, fmt.Errorf("unknown mount type %q", mountType)
	}
}
