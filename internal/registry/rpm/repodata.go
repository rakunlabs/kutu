package rpm

import (
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

const (
	nsCommon    = "http://linux.duke.edu/metadata/common"
	nsRPM       = "http://linux.duke.edu/metadata/rpm"
	nsFilelists = "http://linux.duke.edu/metadata/filelists"
	nsOther     = "http://linux.duke.edu/metadata/other"
	nsRepo      = "http://linux.duke.edu/metadata/repo"
)

// metaTypes are the repodata documents kutu generates / merges, in
// repomd.xml order.
var metaTypes = []string{"primary", "filelists", "other"}

type xmlVersion struct {
	Epoch string `xml:"epoch,attr"`
	Ver   string `xml:"ver,attr"`
	Rel   string `xml:"rel,attr"`
}

type xmlChecksum struct {
	Type  string `xml:"type,attr"`
	PkgID string `xml:"pkgid,attr,omitempty"`
	Value string `xml:",chardata"`
}

type xmlEntry struct {
	Name  string `xml:"name,attr"`
	Flags string `xml:"flags,attr,omitempty"`
	Epoch string `xml:"epoch,attr,omitempty"`
	Ver   string `xml:"ver,attr,omitempty"`
	Rel   string `xml:"rel,attr,omitempty"`
	Pre   string `xml:"pre,attr,omitempty"`
}

type xmlEntries struct {
	Entries []xmlEntry `xml:"rpm:entry"`
}

type xmlFile struct {
	Type string `xml:"type,attr,omitempty"`
	Path string `xml:",chardata"`
}

type xmlFormat struct {
	License     string `xml:"rpm:license"`
	Vendor      string `xml:"rpm:vendor"`
	Group       string `xml:"rpm:group"`
	BuildHost   string `xml:"rpm:buildhost"`
	SourceRPM   string `xml:"rpm:sourcerpm"`
	HeaderRange struct {
		Start int64 `xml:"start,attr"`
		End   int64 `xml:"end,attr"`
	} `xml:"rpm:header-range"`
	Provides    *xmlEntries `xml:"rpm:provides,omitempty"`
	Requires    *xmlEntries `xml:"rpm:requires,omitempty"`
	Conflicts   *xmlEntries `xml:"rpm:conflicts,omitempty"`
	Obsoletes   *xmlEntries `xml:"rpm:obsoletes,omitempty"`
	Suggests    *xmlEntries `xml:"rpm:suggests,omitempty"`
	Enhances    *xmlEntries `xml:"rpm:enhances,omitempty"`
	Recommends  *xmlEntries `xml:"rpm:recommends,omitempty"`
	Supplements *xmlEntries `xml:"rpm:supplements,omitempty"`
	Files       []xmlFile   `xml:"file"`
}

type xmlPrimary struct {
	XMLName     xml.Name    `xml:"package"`
	Type        string      `xml:"type,attr"`
	Name        string      `xml:"name"`
	Arch        string      `xml:"arch"`
	Version     xmlVersion  `xml:"version"`
	Checksum    xmlChecksum `xml:"checksum"`
	Summary     string      `xml:"summary"`
	Description string      `xml:"description"`
	Packager    string      `xml:"packager"`
	URL         string      `xml:"url"`
	Time        struct {
		File  int64 `xml:"file,attr"`
		Build int64 `xml:"build,attr"`
	} `xml:"time"`
	Size struct {
		Package   int64 `xml:"package,attr"`
		Installed int64 `xml:"installed,attr"`
		Archive   int64 `xml:"archive,attr"`
	} `xml:"size"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Format xmlFormat `xml:"format"`
}

type xmlFilelistPkg struct {
	XMLName xml.Name   `xml:"package"`
	PkgID   string     `xml:"pkgid,attr"`
	Name    string     `xml:"name,attr"`
	Arch    string     `xml:"arch,attr"`
	Version xmlVersion `xml:"version"`
	Files   []xmlFile  `xml:"file"`
}

type xmlChangelog struct {
	Author string `xml:"author,attr"`
	Date   int64  `xml:"date,attr"`
	Text   string `xml:",chardata"`
}

type xmlOtherPkg struct {
	XMLName    xml.Name       `xml:"package"`
	PkgID      string         `xml:"pkgid,attr"`
	Name       string         `xml:"name,attr"`
	Arch       string         `xml:"arch,attr"`
	Version    xmlVersion     `xml:"version"`
	Changelogs []xmlChangelog `xml:"changelog"`
}

func entries(ds []Dep) *xmlEntries {
	if len(ds) == 0 {
		return nil
	}
	out := &xmlEntries{Entries: make([]xmlEntry, 0, len(ds))}
	for _, d := range ds {
		e := xmlEntry{Name: d.Name, Flags: d.Flags, Epoch: d.Epoch, Ver: d.Ver, Rel: d.Rel}
		if d.Pre {
			e.Pre = "1"
		}
		out.Entries = append(out.Entries, e)
	}
	return out
}

func pkgVersion(p *Package) xmlVersion {
	return xmlVersion{Epoch: p.Epoch, Ver: p.Version, Rel: p.Release}
}

func primaryChunk(p *Package) ([]byte, error) {
	x := xmlPrimary{
		Type: "rpm", Name: p.Name, Arch: p.Arch, Version: pkgVersion(p),
		Checksum: xmlChecksum{Type: "sha256", PkgID: "YES", Value: p.SHA256},
		Summary:  p.Summary, Description: p.Description, Packager: p.Packager, URL: p.URL,
	}
	x.Time.File, x.Time.Build = p.FileTime, p.BuildTime
	x.Size.Package, x.Size.Installed, x.Size.Archive = p.PackageSize, p.InstalledSize, p.ArchiveSize
	x.Location.Href = p.Location
	f := &x.Format
	f.License, f.Vendor, f.Group, f.BuildHost, f.SourceRPM = p.License, p.Vendor, p.Group, p.BuildHost, p.SourceRPM
	f.HeaderRange.Start, f.HeaderRange.End = p.HeaderStart, p.HeaderEnd
	f.Provides, f.Requires, f.Conflicts, f.Obsoletes = entries(p.Provides), entries(p.Requires), entries(p.Conflicts), entries(p.Obsoletes)
	f.Suggests, f.Enhances, f.Recommends, f.Supplements = entries(p.Suggests), entries(p.Enhances), entries(p.Recommends), entries(p.Supplements)
	for _, fl := range p.Files {
		if fl.Type != "ghost" && isPrimaryFile(fl.Path) {
			f.Files = append(f.Files, xmlFile{Type: fl.Type, Path: fl.Path})
		}
	}
	return xml.Marshal(x)
}

func filelistsChunk(p *Package) ([]byte, error) {
	x := xmlFilelistPkg{PkgID: p.SHA256, Name: p.Name, Arch: p.Arch, Version: pkgVersion(p)}
	for _, f := range p.Files {
		x.Files = append(x.Files, xmlFile{Type: f.Type, Path: f.Path})
	}
	return xml.Marshal(x)
}

func otherChunk(p *Package) ([]byte, error) {
	x := xmlOtherPkg{PkgID: p.SHA256, Name: p.Name, Arch: p.Arch, Version: pkgVersion(p)}
	for _, c := range p.Changelogs {
		x.Changelogs = append(x.Changelogs, xmlChangelog{Author: c.Author, Date: c.Date, Text: c.Text})
	}
	return xml.Marshal(x)
}

// wrapDoc assembles a full repodata document of kind from package chunks.
func wrapDoc(kind string, chunks [][]byte) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	switch kind {
	case "primary":
		fmt.Fprintf(&b, "<metadata xmlns=%q xmlns:rpm=%q packages=\"%d\">\n", nsCommon, nsRPM, len(chunks))
	case "filelists":
		fmt.Fprintf(&b, "<filelists xmlns=%q packages=\"%d\">\n", nsFilelists, len(chunks))
	case "other":
		fmt.Fprintf(&b, "<otherdata xmlns=%q packages=\"%d\">\n", nsOther, len(chunks))
	}
	for _, c := range chunks {
		b.Write(c)
		b.WriteByte('\n')
	}
	switch kind {
	case "primary":
		b.WriteString("</metadata>\n")
	case "filelists":
		b.WriteString("</filelists>\n")
	case "other":
		b.WriteString("</otherdata>\n")
	}
	return b.Bytes()
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

// repodata is one generated metadata set: repodata/* files keyed by
// base-relative path, plus repomd.xml.
type repodata struct {
	Files  map[string][]byte
	Repomd []byte
}

// buildRepodata compresses the three documents and renders repomd.xml.
func buildRepodata(docs map[string][]byte, now time.Time) (*repodata, error) {
	out := &repodata{Files: map[string][]byte{}}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, "<repomd xmlns=%q xmlns:rpm=%q>\n", nsRepo, nsRPM)
	fmt.Fprintf(&b, "  <revision>%d</revision>\n", now.Unix())
	for _, kind := range metaTypes {
		open := docs[kind]
		gz, err := gzipBytes(open)
		if err != nil {
			return nil, err
		}
		sum := sha256Hex(gz)
		href := "repodata/" + sum + "-" + kind + ".xml.gz"
		out.Files[href] = gz
		fmt.Fprintf(&b, "  <data type=%q>\n", kind)
		fmt.Fprintf(&b, "    <checksum type=\"sha256\">%s</checksum>\n", sum)
		fmt.Fprintf(&b, "    <open-checksum type=\"sha256\">%s</open-checksum>\n", sha256Hex(open))
		fmt.Fprintf(&b, "    <location href=%q/>\n", href)
		fmt.Fprintf(&b, "    <timestamp>%d</timestamp>\n", now.Unix())
		fmt.Fprintf(&b, "    <size>%d</size>\n", len(gz))
		fmt.Fprintf(&b, "    <open-size>%d</open-size>\n", len(open))
		b.WriteString("  </data>\n")
	}
	b.WriteString("</repomd>\n")
	out.Repomd = b.Bytes()
	return out, nil
}

// generate renders the full metadata set for pkgs.
func generate(pkgs []*Package, now time.Time) (*repodata, error) {
	docs := map[string][]byte{}
	gens := map[string]func(*Package) ([]byte, error){"primary": primaryChunk, "filelists": filelistsChunk, "other": otherChunk}
	for _, kind := range metaTypes {
		chunks := make([][]byte, 0, len(pkgs))
		for _, p := range pkgs {
			c, err := gens[kind](p)
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, c)
		}
		docs[kind] = wrapDoc(kind, chunks)
	}
	return buildRepodata(docs, now)
}

// ── parsing (virtual merge, remote prefetch) ──

type repomdDoc struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

// repomdHrefs maps data type → location href.
func repomdHrefs(b []byte) (map[string]string, error) {
	var d repomdDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range d.Data {
		out[e.Type] = e.Location.Href
	}
	return out, nil
}

// decompress inflates gzip, zstd or xz repodata, or passes through
// plain XML.
func decompress(b []byte) ([]byte, error) {
	switch {
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, 1<<31))
	case bytes.HasPrefix(bytes.TrimSpace(b), []byte("<")):
		return b, nil
	case len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd:
		zr, err := zstd.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, 1<<31))
	case len(b) >= 4 && b[0] == 0xfd && b[1] == '7' && b[2] == 'z' && b[3] == 'X':
		xr, err := xz.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(xr, 1<<31))
	}
	return nil, errors.New("rpm: unknown repodata compression")
}

// packageChunks returns the raw <package …>…</package> elements of a
// repodata document.
func packageChunks(doc []byte) [][]byte {
	var out [][]byte
	end := []byte("</package>")
	for i := 0; i < len(doc); {
		j := bytes.Index(doc[i:], []byte("<package"))
		if j < 0 {
			break
		}
		start := i + j
		k := start + len("<package")
		if k >= len(doc) || (doc[k] != ' ' && doc[k] != '>' && doc[k] != '\n' && doc[k] != '\t' && doc[k] != '\r') {
			i = k
			continue
		}
		e := bytes.Index(doc[k:], end)
		if e < 0 {
			break
		}
		stop := k + e + len(end)
		out = append(out, doc[start:stop])
		i = stop
	}
	return out
}

// primaryIdent is the subset of a primary <package> needed for merging
// and prefetch.
type primaryIdent struct {
	Name     string     `xml:"name"`
	Arch     string     `xml:"arch"`
	Version  xmlVersion `xml:"version"`
	Checksum string     `xml:"checksum"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
}

func (p primaryIdent) vr() string { return p.Version.Ver + "-" + p.Version.Rel }

func (p primaryIdent) nevra() string {
	return p.Name + "-" + p.Version.Epoch + ":" + p.vr() + "." + p.Arch
}

func parsePrimaryIdent(chunk []byte) (primaryIdent, error) {
	var p primaryIdent
	err := xml.Unmarshal(chunk, &p)
	p.Checksum = strings.TrimSpace(p.Checksum)
	return p, err
}

func chunkPkgID(chunk []byte) string {
	var x struct {
		PkgID string `xml:"pkgid,attr"`
	}
	_ = xml.Unmarshal(chunk, &x)
	return x.PkgID
}

// remotePkg is the slim per-package record of an upstream primary.xml.
type remotePkg struct {
	Name      string
	Arch      string
	Epoch     string
	VR        string
	Href      string
	Size      int64
	License   string
	Summary   string
	URL       string
	BuildTime int64
	SHA256    string
}

// EVR mirrors Package.EVR.
func (p *remotePkg) EVR() string {
	if p.Epoch != "" && p.Epoch != "0" {
		return p.Epoch + ":" + p.VR
	}
	return p.VR
}

// parsePrimaryStream decodes a (decompressed) primary.xml stream.
func parsePrimaryStream(r io.Reader) ([]*remotePkg, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var out []*remotePkg
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "package" {
			continue
		}
		var x struct {
			Name     string     `xml:"name"`
			Arch     string     `xml:"arch"`
			Version  xmlVersion `xml:"version"`
			Checksum string     `xml:"checksum"`
			Summary  string     `xml:"summary"`
			URL      string     `xml:"url"`
			Time     struct {
				Build int64 `xml:"build,attr"`
			} `xml:"time"`
			Size struct {
				Package int64 `xml:"package,attr"`
			} `xml:"size"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
			License string `xml:"format>license"`
		}
		if err := dec.DecodeElement(&x, &se); err != nil {
			return out, err
		}
		out = append(out, &remotePkg{
			Name: x.Name, Arch: x.Arch, Epoch: x.Version.Epoch, VR: x.Version.Ver + "-" + x.Version.Rel,
			Href: x.Location.Href, Size: x.Size.Package, License: x.License, Summary: x.Summary,
			URL: x.URL, BuildTime: x.Time.Build, SHA256: strings.TrimSpace(x.Checksum),
		})
	}
}

// openDecompressed wraps a gzip or plain stream.
func openDecompressed(b []byte) (io.Reader, error) {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		return gzip.NewReader(bytes.NewReader(b))
	}
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("<")) {
		return bytes.NewReader(b), nil
	}
	out, err := decompress(b)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(out), nil
}

func sha256Hex(b []byte) string { return pkgbase.SHA256Hex(b) }
