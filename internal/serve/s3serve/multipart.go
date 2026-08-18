package s3serve

import (
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 by protocol definition
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// uploadManager tracks in-progress multipart uploads. Part data is
// buffered in per-upload temp directories until CompleteMultipartUpload
// streams the concatenation into the destination rawfs backend.
type uploadManager struct {
	mu      sync.Mutex
	uploads map[string]*multipartUpload
	root    string
}

type multipartUpload struct {
	id      string
	bucket  string
	key     string
	dir     string
	started time.Time

	mu    sync.Mutex
	parts map[int]*partInfo
}

type partInfo struct {
	number int
	path   string
	size   int64
	etag   string // hex MD5 of the part body
}

func newUploadManager() *uploadManager {
	return &uploadManager{
		uploads: map[string]*multipartUpload{},
		root:    filepath.Join(os.TempDir(), "kutu-s3serve"),
	}
}

// create registers a new multipart upload and returns its ID.
func (m *uploadManager) create(bucket, key string) (*multipartUpload, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b[:])

	dir := filepath.Join(m.root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	up := &multipartUpload{
		id:      id,
		bucket:  bucket,
		key:     key,
		dir:     dir,
		started: time.Now(),
		parts:   map[int]*partInfo{},
	}

	m.mu.Lock()
	m.uploads[id] = up
	m.mu.Unlock()

	return up, nil
}

// get returns the upload for id and validates bucket/key.
func (m *uploadManager) get(id, bucket, key string) *multipartUpload {
	m.mu.Lock()
	up := m.uploads[id]
	m.mu.Unlock()
	if up == nil || up.bucket != bucket || up.key != key {
		return nil
	}
	return up
}

// remove deletes an upload's state and temp files.
func (m *uploadManager) remove(id string) {
	m.mu.Lock()
	up := m.uploads[id]
	delete(m.uploads, id)
	m.mu.Unlock()
	if up != nil {
		os.RemoveAll(up.dir) //nolint:errcheck
	}
}

// cleanup removes all in-progress uploads (server shutdown).
func (m *uploadManager) cleanup() {
	m.mu.Lock()
	uploads := m.uploads
	m.uploads = map[string]*multipartUpload{}
	m.mu.Unlock()
	for _, up := range uploads {
		os.RemoveAll(up.dir) //nolint:errcheck
	}
}

// putPart stores one part's body on disk and records its MD5 ETag.
func (up *multipartUpload) putPart(number int, body io.Reader) (*partInfo, error) {
	if number < 1 || number > 10000 {
		return nil, fmt.Errorf("part number must be between 1 and 10000")
	}

	path := filepath.Join(up.dir, fmt.Sprintf("part-%05d", number))
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}

	h := md5.New() //nolint:gosec
	size, err := io.Copy(io.MultiWriter(f, h), body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(path) //nolint:errcheck
		return nil, err
	}
	if closeErr != nil {
		os.Remove(path) //nolint:errcheck
		return nil, closeErr
	}

	pi := &partInfo{
		number: number,
		path:   path,
		size:   size,
		etag:   hex.EncodeToString(h.Sum(nil)),
	}

	up.mu.Lock()
	if old := up.parts[number]; old != nil && old.path != path {
		os.Remove(old.path) //nolint:errcheck
	}
	up.parts[number] = pi
	up.mu.Unlock()

	return pi, nil
}

// listParts returns the stored parts sorted by part number.
func (up *multipartUpload) listParts() []*partInfo {
	up.mu.Lock()
	defer up.mu.Unlock()
	out := make([]*partInfo, 0, len(up.parts))
	for _, p := range up.parts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].number < out[j].number })
	return out
}

// assemble validates the requested part list against stored parts and
// returns them in order together with the total size and the S3-style
// multipart ETag ("md5-of-part-md5s"-N).
func (up *multipartUpload) assemble(requested []completedPart) ([]*partInfo, int64, string, error) {
	up.mu.Lock()
	defer up.mu.Unlock()

	if len(requested) == 0 {
		return nil, 0, "", fmt.Errorf("part list is empty")
	}

	parts := make([]*partInfo, 0, len(requested))
	var total int64
	etagHash := md5.New() //nolint:gosec

	prev := 0
	for _, rp := range requested {
		if rp.PartNumber <= prev {
			return nil, 0, "", fmt.Errorf("parts must be listed in ascending order")
		}
		prev = rp.PartNumber

		pi := up.parts[rp.PartNumber]
		if pi == nil {
			return nil, 0, "", fmt.Errorf("part %d was not uploaded", rp.PartNumber)
		}
		if want := trimETag(rp.ETag); want != "" && want != pi.etag {
			return nil, 0, "", fmt.Errorf("part %d ETag mismatch", rp.PartNumber)
		}

		raw, err := hex.DecodeString(pi.etag)
		if err != nil {
			return nil, 0, "", err
		}
		etagHash.Write(raw)

		parts = append(parts, pi)
		total += pi.size
	}

	etag := fmt.Sprintf("%s-%d", hex.EncodeToString(etagHash.Sum(nil)), len(parts))
	return parts, total, etag, nil
}

// partsReader streams the concatenation of the given parts.
func partsReader(parts []*partInfo) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		for _, p := range parts {
			f, err := os.Open(p.path)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			_, err = io.Copy(pw, f)
			f.Close() //nolint:errcheck,gosec
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close() //nolint:errcheck
	}()
	return pr
}

// trimETag strips surrounding quotes from an ETag value.
func trimETag(s string) string {
	s = trimQuote(s)
	return s
}

func trimQuote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
