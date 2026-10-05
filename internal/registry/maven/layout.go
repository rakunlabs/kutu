package maven

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // Maven checksum sidecar, not a security primitive
	"crypto/sha1" //nolint:gosec // Maven checksum sidecar, not a security primitive
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"hash"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

const (
	metadataFile   = "maven-metadata.xml"
	snapshotSuffix = "-SNAPSHOT"

	// SnapshotBuildsToKeep is the number of unique (timestamped)
	// builds retained per SNAPSHOT version on a local repository.
	// Older builds are pruned whenever a new build is uploaded.
	SnapshotBuildsToKeep = 10

	lastUpdatedLayout = "20060102150405"
)

// checksumAlgos are the sidecar checksums generated for every stored file.
var checksumAlgos = []string{"md5", "sha1", "sha256", "sha512"}

func checksumHex(algo string, b []byte) string {
	var h hash.Hash
	switch algo {
	case "md5":
		h = md5.New() //nolint:gosec
	case "sha1":
		h = sha1.New() //nolint:gosec
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return ""
	}
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// splitChecksum returns the checksummed path and algorithm when rel
// names a checksum sidecar; otherwise (rel, "").
func splitChecksum(rel string) (string, string) {
	for _, a := range checksumAlgos {
		if strings.HasSuffix(rel, "."+a) {
			return strings.TrimSuffix(rel, "."+a), a
		}
	}
	return rel, ""
}

// isPrimary reports whether a file name is a real artifact file (not a
// checksum, signature or metadata document).
func isPrimary(name string) bool {
	if strings.HasPrefix(name, metadataFile) {
		return false
	}
	if _, algo := splitChecksum(name); algo != "" {
		return false
	}
	return !strings.HasSuffix(name, ".asc")
}

func isSnapshot(version string) bool { return strings.HasSuffix(version, snapshotSuffix) }

func groupPath(g string) string { return strings.ReplaceAll(g, ".", "/") }

// coords is a parsed Maven repository path.
type coords struct {
	Group    string
	Artifact string
	Version  string
	File     string
}

func (c coords) Name() string        { return c.Group + ":" + c.Artifact }
func (c coords) artifactDir() string { return path.Join(groupPath(c.Group), c.Artifact) }
func (c coords) versionDir() string  { return path.Join(c.artifactDir(), c.Version) }

// parseArtifactPath parses group/artifact/version/file paths whose file
// name starts with "{artifact}-".
func parseArtifactPath(rel string) (coords, bool) {
	parts := strings.Split(cleanRel(rel), "/")
	if len(parts) < 4 {
		return coords{}, false
	}
	n := len(parts)
	file, version, artifact := parts[n-1], parts[n-2], parts[n-3]
	if strings.HasPrefix(file, metadataFile) || !strings.HasPrefix(file, artifact+"-") {
		return coords{}, false
	}
	return coords{Group: strings.Join(parts[:n-3], "."), Artifact: artifact, Version: version, File: file}, true
}

// parseMetadataPath parses an artifact-level ({g}/{a}/maven-metadata.xml)
// or snapshot version-level ({g}/{a}/{v}-SNAPSHOT/maven-metadata.xml)
// metadata path. Version is set only for the latter.
func parseMetadataPath(rel string) (coords, bool) {
	rel = cleanRel(rel)
	if path.Base(rel) != metadataFile {
		return coords{}, false
	}
	dir := path.Dir(rel)
	if dir == "." || dir == "" {
		return coords{}, false
	}
	parts := strings.Split(dir, "/")
	n := len(parts)
	if n < 2 {
		return coords{}, false
	}
	if isSnapshot(parts[n-1]) && n >= 3 {
		return coords{Group: strings.Join(parts[:n-2], "."), Artifact: parts[n-2], Version: parts[n-1], File: metadataFile}, true
	}
	return coords{Group: strings.Join(parts[:n-1], "."), Artifact: parts[n-1], File: metadataFile}, true
}

// snapshotFile is one parsed unique-snapshot file name
// ({artifact}-{base}-{yyyyMMdd.HHmmss}-{build}[-{classifier}].{ext}).
type snapshotFile struct {
	Timestamp  string
	Build      int
	Classifier string
	Ext        string
	Value      string // {base}-{timestamp}-{build}
}

var snapshotBuildRe = regexp.MustCompile(`^(\d{8}\.\d{6})-(\d+)(?:-([^.]+))?\.(.+)$`)

func parseSnapshotFile(artifact, version, name string) (snapshotFile, bool) {
	if !isSnapshot(version) {
		return snapshotFile{}, false
	}
	base := strings.TrimSuffix(version, snapshotSuffix)
	prefix := artifact + "-" + base + "-"
	if !strings.HasPrefix(name, prefix) {
		return snapshotFile{}, false
	}
	m := snapshotBuildRe.FindStringSubmatch(name[len(prefix):])
	if m == nil {
		return snapshotFile{}, false
	}
	num, err := strconv.Atoi(m[2])
	if err != nil {
		return snapshotFile{}, false
	}
	return snapshotFile{Timestamp: m[1], Build: num, Classifier: m[3], Ext: m[4], Value: base + "-" + m[1] + "-" + m[2]}, true
}

// aliasRef is a non-unique snapshot request ({artifact}-{v}-SNAPSHOT[-{classifier}].{ext}).
type aliasRef struct {
	coords
	Classifier string
	Ext        string
}

func (a aliasRef) target(value string) string {
	name := a.Artifact + "-" + value
	if a.Classifier != "" {
		name += "-" + a.Classifier
	}
	return path.Join(a.versionDir(), name+"."+a.Ext)
}

// parseSnapshotAlias parses rel (without checksum suffix) as a
// non-unique snapshot file name.
func parseSnapshotAlias(rel string) (aliasRef, bool) {
	c, ok := parseArtifactPath(rel)
	if !ok || !isSnapshot(c.Version) {
		return aliasRef{}, false
	}
	prefix := c.Artifact + "-" + c.Version
	if !strings.HasPrefix(c.File, prefix) {
		return aliasRef{}, false
	}
	rest := c.File[len(prefix):]
	var cls string
	switch {
	case strings.HasPrefix(rest, "."):
		rest = rest[1:]
	case strings.HasPrefix(rest, "-"):
		rest = rest[1:]
		i := strings.IndexByte(rest, '.')
		if i <= 0 {
			return aliasRef{}, false
		}
		cls, rest = rest[:i], rest[i+1:]
	default:
		return aliasRef{}, false
	}
	if rest == "" {
		return aliasRef{}, false
	}
	return aliasRef{coords: c, Classifier: cls, Ext: rest}, true
}

// ── maven-metadata.xml ──

type metadataDoc struct {
	XMLName      xml.Name    `xml:"metadata"`
	ModelVersion string      `xml:"modelVersion,attr,omitempty"`
	GroupID      string      `xml:"groupId,omitempty"`
	ArtifactID   string      `xml:"artifactId,omitempty"`
	Version      string      `xml:"version,omitempty"`
	Versioning   *versioning `xml:"versioning,omitempty"`
	Plugins      *pluginList `xml:"plugins,omitempty"`
}

type versioning struct {
	Latest           string               `xml:"latest,omitempty"`
	Release          string               `xml:"release,omitempty"`
	Snapshot         *snapshotInfo        `xml:"snapshot,omitempty"`
	Versions         *versionList         `xml:"versions,omitempty"`
	LastUpdated      string               `xml:"lastUpdated,omitempty"`
	SnapshotVersions *snapshotVersionList `xml:"snapshotVersions,omitempty"`
}

type snapshotInfo struct {
	Timestamp   string `xml:"timestamp,omitempty"`
	BuildNumber int    `xml:"buildNumber,omitempty"`
	LocalCopy   bool   `xml:"localCopy,omitempty"`
}

type versionList struct {
	Version []string `xml:"version"`
}

type snapshotVersionList struct {
	Items []snapshotVersion `xml:"snapshotVersion"`
}

type snapshotVersion struct {
	Classifier string `xml:"classifier,omitempty"`
	Extension  string `xml:"extension"`
	Value      string `xml:"value"`
	Updated    string `xml:"updated,omitempty"`
}

type pluginList struct {
	Plugin []plugin `xml:"plugin"`
}

type plugin struct {
	Name       string `xml:"name,omitempty"`
	Prefix     string `xml:"prefix"`
	ArtifactID string `xml:"artifactId"`
}

// decodeXML decodes Maven XML leniently (latin-1 declarations, HTML
// entities in POM descriptions).
func decodeXML(b []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	d.CharsetReader = func(label string, r io.Reader) (io.Reader, error) {
		switch strings.ToLower(label) {
		case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "windows-1252", "cp1252":
			raw, err := io.ReadAll(r)
			if err != nil {
				return nil, err
			}
			var sb strings.Builder
			for _, c := range raw {
				sb.WriteRune(rune(c))
			}
			return strings.NewReader(sb.String()), nil
		}
		return r, nil
	}
	return d.Decode(v)
}

func parseMetadata(b []byte) (*metadataDoc, error) {
	var doc metadataDoc
	if err := decodeXML(b, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func renderMetadata(doc *metadataDoc) []byte {
	doc.XMLName = xml.Name{Local: "metadata"}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil
	}
	buf := make([]byte, 0, len(xml.Header)+len(out)+1)
	buf = append(buf, xml.Header...)
	buf = append(buf, out...)
	return append(buf, '\n')
}

func (doc *metadataDoc) versions() []string {
	if doc == nil || doc.Versioning == nil || doc.Versioning.Versions == nil {
		return nil
	}
	return doc.Versioning.Versions.Version
}

func latestRelease(vs []string) string {
	var rel []string
	for _, v := range vs {
		if !isSnapshot(v) {
			rel = append(rel, v)
		}
	}
	return pkgbase.Latest(rel)
}

func nowStamp() string { return time.Now().UTC().Format(lastUpdatedLayout) }

// compactTimestamp turns a snapshot timestamp (yyyyMMdd.HHmmss) into
// the lastUpdated layout (yyyyMMddHHmmss).
func compactTimestamp(ts string) string { return strings.ReplaceAll(ts, ".", "") }

// mergeMetadata unions several maven-metadata.xml documents: versions
// are merged, latest/release recomputed, the newest snapshot and
// per-(classifier, extension) snapshotVersion entries win.
func mergeMetadata(bodies [][]byte) ([]byte, bool) {
	var out *metadataDoc
	seen := map[string]struct{}{}
	var versions, latests, releases []string
	var lastUpdated string
	var snap *snapshotInfo
	svs := map[string]snapshotVersion{}
	var svOrder []string
	plugins := map[string]plugin{}
	var pluginOrder []string
	hasVersioning := false
	for _, b := range bodies {
		doc, err := parseMetadata(b)
		if err != nil {
			continue
		}
		if out == nil {
			out = &metadataDoc{ModelVersion: doc.ModelVersion, GroupID: doc.GroupID, ArtifactID: doc.ArtifactID, Version: doc.Version}
		}
		if vg := doc.Versioning; vg != nil {
			hasVersioning = true
			for _, v := range doc.versions() {
				v = strings.TrimSpace(v)
				if _, ok := seen[v]; v != "" && !ok {
					seen[v] = struct{}{}
					versions = append(versions, v)
				}
			}
			if vg.Latest != "" {
				latests = append(latests, vg.Latest)
			}
			if vg.Release != "" {
				releases = append(releases, vg.Release)
			}
			if vg.LastUpdated > lastUpdated {
				lastUpdated = vg.LastUpdated
			}
			if s := vg.Snapshot; s != nil {
				if snap == nil || s.Timestamp > snap.Timestamp || (s.Timestamp == snap.Timestamp && s.BuildNumber > snap.BuildNumber) {
					cp := *s
					snap = &cp
				}
			}
			if vg.SnapshotVersions != nil {
				for _, sv := range vg.SnapshotVersions.Items {
					key := sv.Classifier + "\x00" + sv.Extension
					cur, ok := svs[key]
					if !ok {
						svOrder = append(svOrder, key)
					}
					if !ok || sv.Updated > cur.Updated || (sv.Updated == cur.Updated && sv.Value > cur.Value) {
						svs[key] = sv
					}
				}
			}
		}
		if doc.Plugins != nil {
			for _, p := range doc.Plugins.Plugin {
				if _, ok := plugins[p.Prefix]; !ok {
					pluginOrder = append(pluginOrder, p.Prefix)
					plugins[p.Prefix] = p
				}
			}
		}
	}
	if out == nil {
		return nil, false
	}
	if hasVersioning {
		pkgbase.SortVersions(versions)
		vg := &versioning{
			Latest:      pkgbase.Latest(append(append([]string{}, versions...), latests...)),
			Release:     latestRelease(append(append([]string{}, versions...), releases...)),
			Snapshot:    snap,
			LastUpdated: lastUpdated,
		}
		if len(versions) > 0 {
			vg.Versions = &versionList{Version: versions}
		}
		if out.Version != "" {
			// Version-level documents carry no latest/release.
			vg.Latest, vg.Release = "", ""
		}
		if len(svOrder) > 0 {
			sort.Strings(svOrder)
			list := &snapshotVersionList{}
			for _, k := range svOrder {
				list.Items = append(list.Items, svs[k])
			}
			vg.SnapshotVersions = list
		}
		out.Versioning = vg
	}
	if len(pluginOrder) > 0 {
		pl := &pluginList{}
		for _, p := range pluginOrder {
			pl.Plugin = append(pl.Plugin, plugins[p])
		}
		out.Plugins = pl
	}
	return renderMetadata(out), true
}

// ── POM ──

type pomDoc struct {
	Packaging string `xml:"packaging"`
	Licenses  []struct {
		Name string `xml:"name"`
	} `xml:"licenses>license"`
}

func parsePOM(b []byte) pomDoc {
	var p pomDoc
	_ = decodeXML(b, &p)
	return p
}

func (p pomDoc) license() string {
	var names []string
	for _, l := range p.Licenses {
		if n := strings.TrimSpace(l.Name); n != "" {
			names = append(names, n)
		}
	}
	return strings.Join(names, " OR ")
}

// mainExt returns the extension of the main artifact for a POM
// packaging, or "" when the packaging has no main artifact.
func mainExt(packaging string) string {
	switch strings.TrimSpace(packaging) {
	case "", "jar", "bundle", "maven-plugin", "eclipse-plugin", "java-source", "javadoc", "ejb", "test-jar":
		return "jar"
	case "pom":
		return ""
	default:
		return strings.TrimSpace(packaging)
	}
}
