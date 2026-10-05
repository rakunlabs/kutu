package pypi

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

// PackageMeta is the per-package JSON sidecar stored at
// meta/{name}.json. It records per-file metadata captured at publish
// time (hashes, core metadata, yank state).
type PackageMeta struct {
	Name  string               `json:"name"`
	Files map[string]*FileMeta `json:"files"`
}

// FileMeta is the sidecar record of one distribution file.
type FileMeta struct {
	Filename           string    `json:"filename"`
	Version            string    `json:"version,omitempty"`
	SHA256             string    `json:"sha256,omitempty"`
	Size               int64     `json:"size,omitempty"`
	UploadTime         time.Time `json:"upload_time,omitempty"`
	RequiresPython     string    `json:"requires_python,omitempty"`
	Yanked             bool      `json:"yanked,omitempty"`
	YankedReason       string    `json:"yanked_reason,omitempty"`
	CoreMetadataSHA256 string    `json:"core_metadata_sha256,omitempty"`
	MetadataVersion    string    `json:"metadata_version,omitempty"`
	Summary            string    `json:"summary,omitempty"`
	License            string    `json:"license,omitempty"`
	HomePage           string    `json:"home_page,omitempty"`
	Author             string    `json:"author,omitempty"`
	AuthorEmail        string    `json:"author_email,omitempty"`
	PackageType        string    `json:"packagetype,omitempty"`
	PythonVersion      string    `json:"python_version,omitempty"`
}

func (s *Store) metaPath(name string) string { return s.join("meta", normalizeName(name)+".json") }

func (s *Store) metadataPath(name, filename string) string {
	return s.join("metadata", normalizeName(name), path.Base(filename)+".metadata")
}

// LoadMeta returns the sidecar for name (empty when missing).
func (s *Store) LoadMeta(name string) (*PackageMeta, error) {
	pm := &PackageMeta{Name: normalizeName(name), Files: map[string]*FileMeta{}}
	body, err := s.readRaw(s.metaPath(name))
	if err != nil {
		if isNotFound(err) {
			return pm, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(body, pm); err != nil {
		return nil, fmt.Errorf("pypi: decode sidecar %s: %w", name, err)
	}
	if pm.Files == nil {
		pm.Files = map[string]*FileMeta{}
	}
	return pm, nil
}

func (s *Store) saveMeta(name string, pm *PackageMeta) error {
	if len(pm.Files) == 0 {
		return s.deleteRaw(s.metaPath(name))
	}
	body, err := json.MarshalIndent(pm, "", "  ")
	if err != nil {
		return err
	}
	return s.writeRaw(s.metaPath(name), body)
}

// UpdateMeta applies fn to the sidecar of name under the store lock.
func (s *Store) UpdateMeta(name string, fn func(pm *PackageMeta) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pm, err := s.LoadMeta(name)
	if err != nil {
		return err
	}
	if err := fn(pm); err != nil {
		return err
	}
	return s.saveMeta(name, pm)
}

// fileVersion returns the recorded version of filename, falling back
// to inference from the filename.
func fileVersion(pm *PackageMeta, filename string) string {
	if pm != nil {
		if fm := pm.Files[filename]; fm != nil && fm.Version != "" {
			return fm.Version
		}
	}
	_, v := InferNameVersion(filename)
	return v
}

// coreMetadata is a parsed METADATA / PKG-INFO header block.
type coreMetadata map[string]string

func (m coreMetadata) get(key string) string { return m[strings.ToLower(key)] }

func parseCoreMetadata(body []byte) coreMetadata {
	out := coreMetadata{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var last string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break
		}
		if (line[0] == ' ' || line[0] == '\t') && last != "" {
			out[last] += "\n" + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if _, seen := out[k]; seen {
			last = ""
			continue
		}
		out[k] = strings.TrimSpace(v)
		last = k
	}
	return out
}

// extractWheelMetadata returns the *.dist-info/METADATA file of a wheel.
func extractWheelMetadata(body []byte) []byte {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil
	}
	for _, f := range zr.File {
		dir, base := path.Split(f.Name)
		if base != "METADATA" || strings.Count(f.Name, "/") != 1 || !strings.HasSuffix(dir, ".dist-info/") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil
		}
		out, err := io.ReadAll(io.LimitReader(rc, 16<<20))
		rc.Close()
		if err != nil {
			return nil
		}
		return out
	}
	return nil
}

// extractSdistPKGInfo returns {root}/PKG-INFO of a .tar.gz sdist.
func extractSdistPKGInfo(body []byte) []byte {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil
		}
		name := strings.TrimPrefix(h.Name, "./")
		if strings.Count(name, "/") == 1 && path.Base(name) == "PKG-INFO" {
			out, err := io.ReadAll(io.LimitReader(tr, 16<<20))
			if err != nil {
				return nil
			}
			return out
		}
	}
}

// buildFileMeta assembles the sidecar record for an upload. form holds
// the twine multipart fields (may be nil). The returned core metadata
// is non-nil only for wheels and is served as PEP 658 {file}.metadata.
func buildFileMeta(filename string, body []byte, form url.Values, now time.Time) (*FileMeta, []byte, error) {
	get := func(k string) string {
		if form == nil {
			return ""
		}
		return strings.TrimSpace(form.Get(k))
	}
	sum := pkgbase.SHA256Hex(body)
	if d := get("sha256_digest"); d != "" && !strings.EqualFold(d, sum) {
		return nil, nil, fmt.Errorf("sha256_digest mismatch: got %s, computed %s", d, sum)
	}
	fm := &FileMeta{
		Filename:        path.Base(filename),
		Version:         get("version"),
		SHA256:          sum,
		Size:            int64(len(body)),
		UploadTime:      now.UTC(),
		RequiresPython:  get("requires_python"),
		MetadataVersion: get("metadata_version"),
		Summary:         get("summary"),
		License:         get("license_expression"),
		HomePage:        get("home_page"),
		Author:          get("author"),
		AuthorEmail:     get("author_email"),
		PackageType:     get("filetype"),
		PythonVersion:   get("pyversion"),
	}
	if fm.License == "" {
		fm.License = get("license")
	}
	var core, info []byte
	switch {
	case strings.HasSuffix(filename, ".whl"):
		core = extractWheelMetadata(body)
		info = core
	case strings.HasSuffix(filename, ".tar.gz") || strings.HasSuffix(filename, ".tgz"):
		info = extractSdistPKGInfo(body)
	}
	if info != nil {
		md := parseCoreMetadata(info)
		fill := func(dst *string, keys ...string) {
			for _, k := range keys {
				if *dst != "" {
					return
				}
				*dst = md.get(k)
			}
		}
		fill(&fm.Version, "Version")
		fill(&fm.RequiresPython, "Requires-Python")
		fill(&fm.MetadataVersion, "Metadata-Version")
		fill(&fm.Summary, "Summary")
		fill(&fm.License, "License-Expression", "License")
		fill(&fm.HomePage, "Home-page")
		fill(&fm.Author, "Author")
		fill(&fm.AuthorEmail, "Author-email")
	}
	if strings.Contains(fm.License, "\n") {
		fm.License = strings.SplitN(fm.License, "\n", 2)[0]
	}
	if fm.Version == "" {
		_, fm.Version = InferNameVersion(filename)
	}
	if fm.PackageType == "" {
		if strings.HasSuffix(filename, ".whl") {
			fm.PackageType = "bdist_wheel"
		} else {
			fm.PackageType = "sdist"
		}
	}
	if fm.PythonVersion == "" {
		if fm.PackageType == "bdist_wheel" {
			parts := strings.Split(strings.TrimSuffix(path.Base(filename), ".whl"), "-")
			if len(parts) >= 5 {
				fm.PythonVersion = parts[len(parts)-3]
			}
		} else {
			fm.PythonVersion = "source"
		}
	}
	if core != nil {
		fm.CoreMetadataSHA256 = pkgbase.SHA256Hex(core)
	}
	return fm, core, nil
}
