// Package s3serve implements an S3-compatible server that bridges rawfs
// backends to serve files over the S3 REST API. It reuses the FTP
// share/user model for access control: every share is exposed as a
// bucket and each user's username/password pair acts as the AWS
// access-key/secret-key pair for SigV4 authentication.
//
// Only path-style addressing is supported (http://host:port/bucket/key),
// which is what the AWS CLI / SDKs and MinIO clients use for custom
// endpoints.
package s3serve

import (
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
	"github.com/rakunlabs/kutu/internal/service"
)

// Server handles S3 API requests. It does not own a listener: the
// shared vhost layer (internal/server/vhost) binds the port — possibly
// shared with other virtual hosts — and dispatches matched requests to
// Handler(). TLS and the bind address are vhost concerns.
type Server struct {
	mu     sync.RWMutex
	shares []ftpserve.Share
	users  []ftpserve.User
	region string

	uploads *uploadManager
}

// NewServer creates a new S3 server with the given config, shares, and users.
func NewServer(cfg *service.S3ServeSettings, shares []ftpserve.Share, users []ftpserve.User) (*Server, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	return &Server{
		shares:  shares,
		users:   users,
		region:  region,
		uploads: newUploadManager(),
	}, nil
}

// Handler returns the S3 API entry handler for the vhost layer.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.handle)
}

// Stop releases per-instance resources (pending multipart uploads).
// The listener itself is owned and closed by the vhost layer.
func (s *Server) Stop() {
	s.uploads.cleanup()
}

// UpdateShares replaces the shares served by the S3 server.
func (s *Server) UpdateShares(shares []ftpserve.Share) {
	s.mu.Lock()
	s.shares = shares
	s.mu.Unlock()
}

// UpdateUsers replaces the user list for S3 auth.
func (s *Server) UpdateUsers(users []ftpserve.User) {
	s.mu.Lock()
	s.users = users
	s.mu.Unlock()
}

// lookupUser finds a user by access key (username). Returns nil if not found.
func (s *Server) lookupUser(accessKey string) *ftpserve.User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.users {
		if s.users[i].Username == accessKey {
			u := s.users[i]
			return &u
		}
	}
	return nil
}

// handle authenticates and dispatches every S3 API request.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	user, s3err := s.authenticate(r)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	bucket, key := splitBucketKey(r.URL.Path)

	rc := &reqCtx{server: s, user: user, bucket: bucket, key: key}

	if bucket == "" {
		// Service-level requests.
		if r.Method == http.MethodGet {
			s.listBuckets(w, r, rc)
			return
		}
		writeS3Error(w, r, errMethodNotAllowed())
		return
	}

	if key == "" {
		s.handleBucket(w, r, rc)
		return
	}

	s.handleObject(w, r, rc)
}

// reqCtx carries per-request resolved state.
type reqCtx struct {
	server *Server
	user   *ftpserve.User
	bucket string
	key    string
}

// splitBucketKey splits an escaped URL path into bucket and object key.
func splitBucketKey(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

// userShares returns shares visible to this user.
func (s *Server) userShares(user *ftpserve.User) []ftpserve.Share {
	s.mu.RLock()
	shares := s.shares
	s.mu.RUnlock()

	if user == nil || len(user.Shares) == 0 {
		return shares
	}

	allowed := make(map[string]bool, len(user.Shares))
	for _, sh := range user.Shares {
		allowed[sh] = true
	}

	var filtered []ftpserve.Share
	for _, sh := range shares {
		if allowed[sh.Name] {
			filtered = append(filtered, sh)
		}
	}
	return filtered
}

// findShare resolves a bucket name to a share visible to the user.
func (s *Server) findShare(user *ftpserve.User, bucket string) *ftpserve.Share {
	shares := s.userShares(user)
	for i := range shares {
		if shares[i].Name == bucket {
			return &shares[i]
		}
	}
	return nil
}

func isReadOnly(user *ftpserve.User, share *ftpserve.Share) bool {
	return (share != nil && share.ReadOnly) || (user != nil && user.ReadOnly)
}

// --- Helper functions (mirrored from ftpserve/webdavserve) ---

func sourceFSPath(s *ftpserve.ShareSource, relPath string) string {
	if s.Path == "" {
		return relPath
	}
	if relPath == "" {
		return s.Path
	}
	return s.Path + "/" + relPath
}

func findInSources(share *ftpserve.Share, relPath string) (*ftpserve.ShareSource, error) {
	for i := range share.Sources {
		src := &share.Sources[i]
		_, err := src.FS.Stat(sourceFSPath(src, relPath))
		if err == nil {
			return src, nil
		}
	}
	return nil, os.ErrNotExist
}

func firstWritableSource(share *ftpserve.Share) (*ftpserve.ShareSource, rawfs.WritableRawFS, error) {
	for i := range share.Sources {
		if wfs, ok := share.Sources[i].FS.(rawfs.WritableRawFS); ok {
			return &share.Sources[i], wfs, nil
		}
	}
	return nil, nil, fmt.Errorf("no writable source in share")
}

// cleanKey normalizes an object key into a rawfs-relative path. It
// rejects path traversal.
func cleanKey(key string) (string, bool) {
	key = strings.Trim(key, "/")
	if key == "" {
		return "", true
	}
	cleaned := path.Clean(key)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}
