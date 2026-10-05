package ops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

const (
	// ManifestName is the first entry of every export archive.
	ManifestName = "kutu-export.json"
	dataPrefix   = "data/"

	maxManifestSize  = 256 << 20
	memSpoolLimit    = 8 << 20
	exportFormatV1   = 1
	tarModeRegular   = 0o644
	importBufferSize = 32 << 10
)

// Tree is a path-keyed file tree a repository's storage can be
// exported from and imported into. Paths are slash-separated and
// relative to the tree root.
type Tree interface {
	// Walk visits every regular file under rel.
	Walk(rel string, fn func(rel string, size int64, modTime time.Time) error) error
	Open(rel string) (io.ReadCloser, error)
	Write(rel string, r io.Reader, size int64) error
}

// ExportMeta is the archive manifest.
type ExportMeta struct {
	Format    int          `json:"format"`
	Type      string       `json:"type"`
	Namespace string       `json:"namespace,omitempty"`
	Repo      string       `json:"repo,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	Since     time.Time    `json:"since,omitzero"`
	Files     []ExportFile `json:"files"`
}

// ExportFile describes one file in the archive.
type ExportFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ImportStats summarises an import.
type ImportStats struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped"`
}

// ── rawfs-backed tree ──

type rawTree struct {
	store *pkgbase.Store
}

// TreeFromRawFS returns a Tree rooted at basePath on fs. Writes
// require a rawfs.WritableRawFS.
func TreeFromRawFS(fs rawfs.RawFS, basePath string) Tree {
	return &rawTree{store: pkgbase.NewStore(fs, basePath)}
}

func (t *rawTree) Walk(rel string, fn func(rel string, size int64, modTime time.Time) error) error {
	return t.store.Walk(rel, func(child string, e rawfs.DirEntry) error {
		var mod time.Time
		if fi, err := t.store.Stat(child); err == nil {
			mod = fi.ModTime
		}
		return fn(strings.TrimPrefix(child, "/"), e.Size, mod)
	})
}

func (t *rawTree) Open(rel string) (io.ReadCloser, error) {
	rc, _, err := t.store.Open(rel)
	return rc, err
}

func (t *rawTree) Write(rel string, r io.Reader, size int64) error {
	return t.store.WriteStream(rel, r, size)
}

// ── export ──

// Export streams every file of t into w as a tar.gz archive whose
// first entry is the manifest and whose files live under "data/".
func Export(ctx context.Context, t Tree, meta ExportMeta, w io.Writer) error {
	return export(ctx, t, meta, time.Time{}, w)
}

// ExportSince is Export restricted to files modified after since.
func ExportSince(ctx context.Context, t Tree, meta ExportMeta, since time.Time, w io.Writer) error {
	return export(ctx, t, meta, since, w)
}

func export(ctx context.Context, t Tree, meta ExportMeta, since time.Time, w io.Writer) error {
	type entry struct {
		path string
		size int64
	}
	var entries []entry
	err := t.Walk("", func(rel string, size int64, mod time.Time) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !since.IsZero() && !mod.After(since) {
			return nil
		}
		if _, err := cleanRel(rel); err != nil {
			return nil
		}
		entries = append(entries, entry{rel, size})
		return nil
	})
	if err != nil && !pkgbase.IsNotFound(err) {
		return fmt.Errorf("walk: %w", err)
	}

	// First pass: hash every file so the manifest can lead the archive.
	files := make([]ExportFile, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum, n, err := hashFile(t, e.path)
		if err != nil {
			if pkgbase.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("hash %s: %w", e.path, err)
		}
		files = append(files, ExportFile{Path: e.path, Size: n, SHA256: sum})
	}

	meta.Format = exportFormatV1
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	meta.Since = since
	meta.Files = files
	manifest, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name:    ManifestName,
		Mode:    tarModeRegular,
		Size:    int64(len(manifest)),
		ModTime: meta.CreatedAt,
	}); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}

	// Second pass: stream contents, re-verifying the hash so a file
	// changing between passes fails the export instead of producing a
	// corrupt archive.
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:    dataPrefix + f.Path,
			Mode:    tarModeRegular,
			Size:    f.Size,
			ModTime: meta.CreatedAt,
		}); err != nil {
			return err
		}
		rc, err := t.Open(f.Path)
		if err != nil {
			return fmt.Errorf("open %s: %w", f.Path, err)
		}
		h := sha256.New()
		n, err := io.Copy(tw, io.TeeReader(io.LimitReader(rc, f.Size), h))
		rc.Close()
		if err != nil {
			return fmt.Errorf("copy %s: %w", f.Path, err)
		}
		if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return fmt.Errorf("file %s changed during export", f.Path)
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func hashFile(t Tree, rel string) (string, int64, error) {
	rc, err := t.Open(rel)
	if err != nil {
		return "", 0, err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ── import ──

// Import reads an archive produced by Export into t. The manifest type
// must equal expectType (when non-empty); every file is size- and
// sha256-verified before it is written. Existing files are skipped
// unless overwrite is set.
func Import(ctx context.Context, t Tree, r io.Reader, expectType string, overwrite bool) (ImportStats, error) {
	var stats ImportStats

	gz, err := gzip.NewReader(r)
	if err != nil {
		return stats, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil {
		return stats, fmt.Errorf("%w: missing manifest: %v", ErrInvalidArchive, err)
	}
	if hdr.Name != ManifestName || hdr.Typeflag != tar.TypeReg {
		return stats, fmt.Errorf("%w: first entry must be %s", ErrInvalidArchive, ManifestName)
	}
	if hdr.Size > maxManifestSize {
		return stats, fmt.Errorf("%w: manifest too large", ErrInvalidArchive)
	}
	var meta ExportMeta
	if err := json.NewDecoder(io.LimitReader(tr, hdr.Size)).Decode(&meta); err != nil {
		return stats, fmt.Errorf("%w: manifest: %v", ErrInvalidArchive, err)
	}
	if expectType != "" && meta.Type != expectType {
		return stats, fmt.Errorf("%w: archive type %q does not match repository type %q", ErrInvalidArchive, meta.Type, expectType)
	}

	expected := make(map[string]ExportFile, len(meta.Files))
	for _, f := range meta.Files {
		p, err := cleanRel(f.Path)
		if err != nil {
			return stats, fmt.Errorf("%w: manifest path %q: %v", ErrInvalidArchive, f.Path, err)
		}
		if _, dup := expected[p]; dup {
			return stats, fmt.Errorf("%w: duplicate path %q", ErrInvalidArchive, p)
		}
		f.Path = p
		expected[p] = f
	}

	existing := map[string]bool{}
	if !overwrite {
		err := t.Walk("", func(rel string, _ int64, _ time.Time) error {
			existing[strings.TrimPrefix(rel, "/")] = true
			return nil
		})
		if err != nil && !pkgbase.IsNotFound(err) {
			return stats, fmt.Errorf("walk destination: %w", err)
		}
	}

	seen := make(map[string]bool, len(expected))
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return stats, fmt.Errorf("%w: unsupported entry %q", ErrInvalidArchive, hdr.Name)
		}
		if !strings.HasPrefix(hdr.Name, dataPrefix) {
			return stats, fmt.Errorf("%w: unexpected entry %q", ErrInvalidArchive, hdr.Name)
		}
		rel, err := cleanRel(strings.TrimPrefix(hdr.Name, dataPrefix))
		if err != nil {
			return stats, fmt.Errorf("%w: entry %q: %v", ErrInvalidArchive, hdr.Name, err)
		}
		f, ok := expected[rel]
		if !ok {
			return stats, fmt.Errorf("%w: entry %q not in manifest", ErrInvalidArchive, rel)
		}
		if seen[rel] {
			return stats, fmt.Errorf("%w: duplicate entry %q", ErrInvalidArchive, rel)
		}
		seen[rel] = true
		if hdr.Size != f.Size {
			return stats, fmt.Errorf("%w: size mismatch for %q", ErrInvalidArchive, rel)
		}

		if existing[rel] {
			stats.Skipped++
			continue
		}

		if err := importFile(t, tr, f); err != nil {
			return stats, err
		}
		stats.Files++
		stats.Bytes += f.Size
	}

	for p := range expected {
		if !seen[p] {
			return stats, fmt.Errorf("%w: archive is missing %q", ErrInvalidArchive, p)
		}
	}
	return stats, nil
}

// importFile spools one entry (memory or temp file), verifies its
// checksum and only then writes it to the tree.
func importFile(t Tree, r io.Reader, f ExportFile) error {
	h := sha256.New()
	src := io.TeeReader(io.LimitReader(r, f.Size), h)

	var body io.Reader
	if f.Size <= memSpoolLimit {
		buf := bytes.NewBuffer(make([]byte, 0, f.Size))
		if _, err := io.Copy(buf, src); err != nil {
			return fmt.Errorf("%w: read %q: %v", ErrInvalidArchive, f.Path, err)
		}
		body = buf
	} else {
		tmp, err := os.CreateTemp("", "kutu-import-*")
		if err != nil {
			return err
		}
		defer func() {
			tmp.Close()
			os.Remove(tmp.Name())
		}()
		if _, err := io.CopyBuffer(tmp, src, make([]byte, importBufferSize)); err != nil {
			return fmt.Errorf("%w: read %q: %v", ErrInvalidArchive, f.Path, err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body = tmp
	}

	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, f.SHA256) {
		return fmt.Errorf("%w: sha256 mismatch for %q", ErrInvalidArchive, f.Path)
	}
	if err := t.Write(f.Path, body, f.Size); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	return nil
}

// cleanRel validates an archive-relative path: non-empty, relative,
// no ".." segments, no backslashes, already in clean form.
func cleanRel(p string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("empty path")
	case strings.HasPrefix(p, "/"):
		return "", errors.New("absolute path")
	case strings.ContainsAny(p, "\\\x00"):
		return "", errors.New("invalid character")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errors.New("path traversal")
		}
	}
	c := path.Clean(p)
	if c != p || c == "." {
		return "", errors.New("path not in canonical form")
	}
	return c, nil
}
