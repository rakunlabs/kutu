package apt

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// errUnsupportedCompression is returned for unknown control/index
// compressions.
var errUnsupportedCompression = errors.New("unsupported control compression")

var (
	reName    = regexp.MustCompile(`^[a-z0-9][a-z0-9+.\-]+$`)
	reVersion = regexp.MustCompile(`^[A-Za-z0-9.+~:\-]+$`)
	reArch    = regexp.MustCompile(`^[a-z0-9\-]+$`)
	reSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]*$`)
)

// field is one "Key: value" entry of a deb822 stanza. V holds the
// first line trimmed plus any continuation lines (with their leading
// whitespace) joined by "\n".
type field struct {
	K string `json:"k"`
	V string `json:"v"`
}

// stanza is an ordered deb822 paragraph.
type stanza []field

// Get returns the value of key k (case-insensitive) or "".
func (s stanza) Get(k string) string {
	for _, f := range s {
		if strings.EqualFold(f.K, k) {
			return f.V
		}
	}
	return ""
}

// without returns s minus the listed keys (case-insensitive).
func (s stanza) without(keys ...string) stanza {
	out := make(stanza, 0, len(s))
outer:
	for _, f := range s {
		for _, k := range keys {
			if strings.EqualFold(f.K, k) {
				continue outer
			}
		}
		out = append(out, f)
	}
	return out
}

func (s stanza) render(b *bytes.Buffer) {
	for _, f := range s {
		b.WriteString(f.K)
		b.WriteByte(':')
		if f.V != "" && f.V[0] != '\n' {
			b.WriteByte(' ')
		}
		b.WriteString(f.V)
		b.WriteByte('\n')
	}
}

// parseStanzas parses deb822 paragraphs (control files, Packages,
// Release).
func parseStanzas(data []byte) []stanza {
	var out []stanza
	var cur stanza
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
			continue
		}
		if line[0] == '#' {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(cur) > 0 {
				cur[len(cur)-1].V += "\n" + line
			}
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 {
			continue
		}
		cur = append(cur, field{K: line[:i], V: strings.TrimSpace(line[i+1:])})
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// firstLine returns the first line of a (possibly multi-line) value.
func firstLine(v string) string {
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return strings.TrimSpace(v)
}

// readAr returns the members of an ar archive in order.
func readAr(data []byte) (map[string][]byte, []string, error) {
	const magic = "!<arch>\n"
	if !bytes.HasPrefix(data, []byte(magic)) {
		return nil, nil, errors.New("not a debian package (missing ar magic)")
	}
	members := map[string][]byte{}
	var order []string
	off := len(magic)
	for off < len(data) {
		if off+60 > len(data) {
			return nil, nil, errors.New("truncated ar header")
		}
		hdr := data[off : off+60]
		if string(hdr[58:60]) != "`\n" {
			return nil, nil, errors.New("bad ar header")
		}
		name := strings.TrimRight(strings.TrimSpace(string(hdr[0:16])), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || size < 0 {
			return nil, nil, errors.New("bad ar member size")
		}
		off += 60
		if int64(off)+size > int64(len(data)) {
			return nil, nil, errors.New("truncated ar member")
		}
		members[name] = data[off : off+int(size)]
		order = append(order, name)
		off += int(size)
		if size%2 == 1 {
			off++
		}
	}
	return members, order, nil
}

// decompress returns a reader decoding data according to the file
// name suffix (.gz, .xz, .zst or none).
func decompress(name string, data []byte) (io.Reader, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		return gzip.NewReader(bytes.NewReader(data))
	case strings.HasSuffix(name, ".xz"):
		return xz.NewReader(bytes.NewReader(data))
	case strings.HasSuffix(name, ".zst"):
		d, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	case strings.HasSuffix(name, ".tar"), !strings.Contains(name[strings.LastIndexByte(name, '/')+1:], "."):
		return bytes.NewReader(data), nil
	}
	return nil, fmt.Errorf("%w: %s", errUnsupportedCompression, name)
}

// decompressAll fully decodes data.
func decompressAll(name string, data []byte) ([]byte, error) {
	r, err := decompress(name, data)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(r, 1<<30))
}

// parseDeb extracts the control stanza from a .deb.
func parseDeb(data []byte) (stanza, error) {
	members, order, err := readAr(data)
	if err != nil {
		return nil, err
	}
	if _, ok := members["debian-binary"]; !ok {
		return nil, errors.New("not a debian package (missing debian-binary)")
	}
	var ctlName string
	for _, n := range order {
		if strings.HasPrefix(n, "control.tar") {
			ctlName = n
			break
		}
	}
	if ctlName == "" {
		return nil, errors.New("control.tar member missing")
	}
	r, err := decompress(ctlName, members[ctlName])
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", ctlName, err)
		}
		if strings.TrimPrefix(h.Name, "./") != "control" {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(tr, 4<<20))
		if err != nil {
			return nil, err
		}
		st := parseStanzas(body)
		if len(st) == 0 {
			return nil, errors.New("empty control file")
		}
		return validateControl(st[0])
	}
	return nil, errors.New("control file missing from control archive")
}

func validateControl(s stanza) (stanza, error) {
	name, ver, arch := s.Get("Package"), s.Get("Version"), s.Get("Architecture")
	switch {
	case !reName.MatchString(name):
		return nil, fmt.Errorf("invalid Package %q", name)
	case !reVersion.MatchString(ver):
		return nil, fmt.Errorf("invalid Version %q", ver)
	case !reArch.MatchString(arch):
		return nil, fmt.Errorf("invalid Architecture %q", arch)
	}
	// Package first, as dpkg emits it.
	out := stanza{{K: "Package", V: name}}
	out = append(out, s.without("Package", "Filename", "Size", "MD5sum", "SHA1", "SHA256", "SHA512")...)
	return out, nil
}

// stripEpoch drops a leading "N:" epoch (pool file names omit it).
func stripEpoch(v string) string {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		return v[i+1:]
	}
	return v
}

// poolPrefix returns the pool sub-directory for a source/package name.
func poolPrefix(name string) string {
	if strings.HasPrefix(name, "lib") && len(name) > 3 {
		return name[:4]
	}
	return name[:1]
}

// poolPath returns the pool file path for a package.
func poolPath(component, name, version, arch string) string {
	return "pool/" + component + "/" + poolPrefix(name) + "/" + name + "/" + name + "_" + stripEpoch(version) + "_" + arch + ".deb"
}

// parseDebFilename splits "{name}_{version}_{arch}.deb".
func parseDebFilename(p string) (name, version, arch string, ok bool) {
	base := p[strings.LastIndexByte(p, '/')+1:]
	if !strings.HasSuffix(base, ".deb") {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(base, ".deb"), "_")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], strings.ReplaceAll(parts[1], "%3a", ":"), parts[2], true
}

// releaseFiles parses the SHA256 section of a Release document into
// path → sha256.
func releaseFiles(release []byte) map[string]string {
	out := map[string]string{}
	st := parseStanzas(release)
	if len(st) == 0 {
		return out
	}
	for _, line := range strings.Split(st[0].Get("SHA256"), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 {
			out[f[2]] = f[0]
		}
	}
	return out
}

func splitFields(v string) []string {
	f := strings.Fields(v)
	sort.Strings(f)
	return f
}
