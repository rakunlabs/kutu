package alpine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

// record is one package entry of an APKINDEX.
type record struct {
	Checksum         string   `json:"checksum"`
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	Arch             string   `json:"arch"`
	Size             int64    `json:"size"`
	InstalledSize    int64    `json:"installed_size,omitempty"`
	Description      string   `json:"description,omitempty"`
	URL              string   `json:"url,omitempty"`
	License          string   `json:"license,omitempty"`
	Origin           string   `json:"origin,omitempty"`
	Maintainer       string   `json:"maintainer,omitempty"`
	BuildDate        int64    `json:"build_date,omitempty"`
	Commit           string   `json:"commit,omitempty"`
	ProviderPriority string   `json:"provider_priority,omitempty"`
	Depends          []string `json:"depends,omitempty"`
	Provides         []string `json:"provides,omitempty"`
	InstallIf        []string `json:"install_if,omitempty"`
	Packager         string   `json:"packager,omitempty"`
	DataHash         string   `json:"datahash,omitempty"`
}

func (r *record) fileName() string { return r.Name + "-" + r.Version + ".apk" }

// lines renders r as an APKINDEX block (without the trailing blank line).
func (r *record) lines() string {
	var b strings.Builder
	add := func(k, v string) {
		if v != "" {
			b.WriteString(k + ":" + v + "\n")
		}
	}
	add("C", r.Checksum)
	add("P", r.Name)
	add("V", r.Version)
	add("A", r.Arch)
	add("S", strconv.FormatInt(r.Size, 10))
	add("I", strconv.FormatInt(r.InstalledSize, 10))
	add("T", r.Description)
	add("U", r.URL)
	add("L", r.License)
	add("o", r.Origin)
	add("m", r.Maintainer)
	if r.BuildDate > 0 {
		add("t", strconv.FormatInt(r.BuildDate, 10))
	}
	add("c", r.Commit)
	add("k", r.ProviderPriority)
	add("D", strings.Join(r.Depends, " "))
	add("p", strings.Join(r.Provides, " "))
	add("i", strings.Join(r.InstallIf, " "))
	return b.String()
}

// parseRecord parses one APKINDEX block.
func parseRecord(block string) record {
	var r record
	for _, line := range strings.Split(block, "\n") {
		if len(line) < 2 || line[1] != ':' {
			continue
		}
		v := line[2:]
		switch line[0] {
		case 'C':
			r.Checksum = v
		case 'P':
			r.Name = v
		case 'V':
			r.Version = v
		case 'A':
			r.Arch = v
		case 'S':
			r.Size, _ = strconv.ParseInt(v, 10, 64)
		case 'I':
			r.InstalledSize, _ = strconv.ParseInt(v, 10, 64)
		case 'T':
			r.Description = v
		case 'U':
			r.URL = v
		case 'L':
			r.License = v
		case 'o':
			r.Origin = v
		case 'm':
			r.Maintainer = v
		case 't':
			r.BuildDate, _ = strconv.ParseInt(v, 10, 64)
		case 'c':
			r.Commit = v
		case 'k':
			r.ProviderPriority = v
		case 'D':
			r.Depends = strings.Fields(v)
		case 'p':
			r.Provides = strings.Fields(v)
		case 'i':
			r.InstallIf = strings.Fields(v)
		}
	}
	return r
}

// splitBlocks splits APKINDEX text into non-empty record blocks.
func splitBlocks(text string) []string {
	var out []string
	for _, b := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		if b = strings.Trim(b, "\n"); b != "" {
			out = append(out, b)
		}
	}
	return out
}

func parseIndexText(text string) []record {
	blocks := splitBlocks(text)
	out := make([]record, 0, len(blocks))
	for _, b := range blocks {
		if r := parseRecord(b); r.Name != "" {
			out = append(out, r)
		}
	}
	return out
}

// gzipMember is one gzip stream of a concatenated archive.
type gzipMember struct {
	Start, End int
	Files      map[string][]byte // tar entries (content only for wanted names)
	Names      []string
}

// walkMembers iterates the gzip members of b, reading each as a
// (possibly end-block-less) tar segment. want selects entries whose
// contents are captured. Iteration stops when fn returns true.
//
// bytes.Reader implements io.ByteReader so neither gzip nor flate
// add read-ahead buffering: the consumed byte count after a member
// is its exact end offset.
func walkMembers(b []byte, want func(name string) bool, fn func(m gzipMember) bool) error {
	br := bytes.NewReader(b)
	var zr gzip.Reader
	for {
		start := len(b) - br.Len()
		if start >= len(b) {
			return nil
		}
		if err := zr.Reset(br); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("gzip member at %d: %w", start, err)
		}
		zr.Multistream(false)
		m := gzipMember{Start: start, Files: map[string][]byte{}}
		tr := tar.NewReader(&zr)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("tar in gzip member at %d: %w", start, err)
			}
			m.Names = append(m.Names, hdr.Name)
			if want != nil && want(hdr.Name) {
				body, err := io.ReadAll(io.LimitReader(tr, 64<<20))
				if err != nil {
					return err
				}
				m.Files[hdr.Name] = body
			}
		}
		if _, err := io.Copy(io.Discard, &zr); err != nil {
			return fmt.Errorf("gzip member at %d: %w", start, err)
		}
		m.End = len(b) - br.Len()
		if fn(m) {
			return nil
		}
	}
}

// parseAPK extracts the .PKGINFO metadata and the apk v2 identity
// (Q1 + base64 sha1 of the control gzip member) from an .apk file.
func parseAPK(b []byte) (record, error) {
	var (
		info    []byte
		control []byte
	)
	err := walkMembers(b, func(n string) bool { return n == ".PKGINFO" }, func(m gzipMember) bool {
		if body, ok := m.Files[".PKGINFO"]; ok {
			info = body
			control = b[m.Start:m.End]
			return true
		}
		return false
	})
	if err != nil {
		return record{}, err
	}
	if info == nil {
		return record{}, errors.New("not an apk: .PKGINFO not found")
	}
	r := parsePKGINFO(info)
	if r.Name == "" || r.Version == "" || r.Arch == "" {
		return record{}, errors.New(".PKGINFO missing pkgname, pkgver or arch")
	}
	sum := sha1.Sum(control)
	r.Checksum = "Q1" + base64.StdEncoding.EncodeToString(sum[:])
	r.Size = int64(len(b))
	return r, nil
}

func parsePKGINFO(b []byte) record {
	var r record
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "pkgname":
			r.Name = v
		case "pkgver":
			r.Version = v
		case "pkgdesc":
			r.Description = v
		case "url":
			r.URL = v
		case "builddate":
			r.BuildDate, _ = strconv.ParseInt(v, 10, 64)
		case "packager":
			r.Packager = v
		case "size":
			r.InstalledSize, _ = strconv.ParseInt(v, 10, 64)
		case "arch":
			r.Arch = v
		case "origin":
			r.Origin = v
		case "commit":
			r.Commit = v
		case "maintainer":
			r.Maintainer = v
		case "license":
			r.License = v
		case "depend":
			r.Depends = append(r.Depends, v)
		case "provides":
			r.Provides = append(r.Provides, v)
		case "install_if":
			r.InstallIf = append(r.InstallIf, strings.Fields(v)...)
		case "provider_priority":
			r.ProviderPriority = v
		case "datahash":
			r.DataHash = v
		}
	}
	return r
}

// readIndex returns the APKINDEX text of an (optionally signed)
// APKINDEX.tar.gz.
func readIndex(b []byte) (string, error) {
	var text []byte
	found := false
	err := walkMembers(b, func(n string) bool { return n == "APKINDEX" }, func(m gzipMember) bool {
		if body, ok := m.Files["APKINDEX"]; ok {
			text, found = body, true
			return true
		}
		return false
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", errors.New("APKINDEX not found in archive")
	}
	return string(text), nil
}

func tarEntry(tw *tar.Writer, name string, body []byte, mod time.Time) error {
	hdr := &tar.Header{
		Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: mod.Truncate(time.Second),
		Typeflag: tar.TypeReg, Uname: "root", Gname: "root", Format: tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortRecords(recs []record) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Name != recs[j].Name {
			return recs[i].Name < recs[j].Name
		}
		return pkgbase.CompareVersions(recs[i].Version, recs[j].Version) < 0
	})
}

// renderIndex renders records as APKINDEX text.
func renderIndex(recs []record) string {
	var b strings.Builder
	for i := range recs {
		b.WriteString(recs[i].lines())
		b.WriteString("\n")
	}
	return b.String()
}

// buildIndexArchive returns APKINDEX.tar.gz bytes for the given
// APKINDEX text, prefixed by an abuild-style signature stream when
// key is non-nil.
func buildIndexArchive(text, description string, key *rsa.PrivateKey, keyName string) ([]byte, error) {
	now := time.Now()
	var tbuf bytes.Buffer
	tw := tar.NewWriter(&tbuf)
	if err := tarEntry(tw, "DESCRIPTION", []byte(description), now); err != nil {
		return nil, err
	}
	if err := tarEntry(tw, "APKINDEX", []byte(text), now); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	index, err := gzipBytes(tbuf.Bytes())
	if err != nil {
		return nil, err
	}
	if key == nil {
		return index, nil
	}
	sig, err := signatureStream(index, key, keyName, now)
	if err != nil {
		return nil, err
	}
	return append(sig, index...), nil
}

// signatureStream builds the gzip'd ".SIGN.RSA.{key}.rsa.pub" tar
// segment (no end-of-archive blocks, like `abuild-tar --cut`)
// carrying an RSA PKCS#1 v1.5 SHA-1 signature over data.
func signatureStream(data []byte, key *rsa.PrivateKey, keyName string, mod time.Time) ([]byte, error) {
	digest := sha1.Sum(data)
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA1, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign APKINDEX: %w", err)
	}
	var tbuf bytes.Buffer
	tw := tar.NewWriter(&tbuf)
	if err := tarEntry(tw, ".SIGN.RSA."+keyName+".rsa.pub", sig, mod); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	raw := tbuf.Bytes()
	raw = raw[:len(raw)-1024]
	return gzipBytes(raw)
}

// splitFilename splits "{name}-{pkgver}-r{N}.apk" into name and
// version ("{pkgver}-r{N}").
func splitFilename(file string) (string, string, bool) {
	base, ok := strings.CutSuffix(file, ".apk")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndexByte(base, '-')
	if i <= 0 {
		return "", "", false
	}
	if rel := base[i+1:]; len(rel) > 1 && rel[0] == 'r' && isDigits(rel[1:]) {
		j := strings.LastIndexByte(base[:i], '-')
		if j <= 0 {
			return "", "", false
		}
		return base[:j], base[j+1:], true
	}
	return base[:i], base[i+1:], true
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
