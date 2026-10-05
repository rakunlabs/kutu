package rpm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

const (
	leadSize = 96

	typeInt8        = 2
	typeInt16       = 3
	typeInt32       = 4
	typeInt64       = 5
	typeString      = 6
	typeBin         = 7
	typeStringArray = 8
	typeI18NString  = 9

	tagName           = 1000
	tagVersion        = 1001
	tagRelease        = 1002
	tagEpoch          = 1003
	tagSummary        = 1004
	tagDescription    = 1005
	tagBuildTime      = 1006
	tagBuildHost      = 1007
	tagSize           = 1009
	tagVendor         = 1011
	tagLicense        = 1014
	tagPackager       = 1015
	tagGroup          = 1016
	tagURL            = 1020
	tagArch           = 1022
	tagOldFilenames   = 1027
	tagFileModes      = 1030
	tagFileFlags      = 1037
	tagSourceRPM      = 1044
	tagArchiveSize    = 1046
	tagProvideName    = 1047
	tagRequireFlags   = 1048
	tagRequireName    = 1049
	tagRequireVersion = 1050
	tagConflictFlags  = 1053
	tagConflictName   = 1054
	tagConflictVer    = 1055
	tagChangelogTime  = 1080
	tagChangelogName  = 1081
	tagChangelogText  = 1082
	tagObsoleteName   = 1090
	tagSourcePackage  = 1106
	tagProvideFlags   = 1112
	tagProvideVersion = 1113
	tagObsoleteFlags  = 1114
	tagObsoleteVer    = 1115
	tagDirIndexes     = 1116
	tagBaseNames      = 1117
	tagDirNames       = 1118
	tagLongSize       = 5009
	tagRecommendName  = 5046
	tagRecommendVer   = 5047
	tagRecommendFlags = 5048
	tagSuggestName    = 5049
	tagSuggestVer     = 5050
	tagSuggestFlags   = 5051
	tagSupplementName = 5052
	tagSupplementVer  = 5053
	tagSupplementFlag = 5054
	tagEnhanceName    = 5055
	tagEnhanceVer     = 5056
	tagEnhanceFlags   = 5057

	sigTagPayloadSize = 1007

	fileFlagGhost = 1 << 6

	senseLess       = 1 << 1
	senseGreater    = 1 << 2
	senseEqual      = 1 << 3
	sensePrereq     = 1 << 6
	senseScriptPre  = 1 << 9
	senseScriptPost = 1 << 10
)

var (
	leadMagic   = []byte{0xed, 0xab, 0xee, 0xdb}
	headerMagic = []byte{0x8e, 0xad, 0xe8, 0x01}
)

type indexEntry struct {
	typ, off, count int32
}

type header struct {
	entries map[int32]indexEntry
	data    []byte
}

// parseHeader parses an RPM header structure starting at off and
// returns it together with the offset just past its data store.
func parseHeader(b []byte, off int) (*header, int, error) {
	if off+16 > len(b) || string(b[off:off+4]) != string(headerMagic) {
		return nil, 0, errors.New("rpm: bad header magic")
	}
	nindex := int(binary.BigEndian.Uint32(b[off+8:]))
	hsize := int(binary.BigEndian.Uint32(b[off+12:]))
	if nindex < 0 || hsize < 0 || nindex > 1<<16 || hsize > 1<<28 {
		return nil, 0, errors.New("rpm: header too large")
	}
	idx := off + 16
	dataStart := idx + nindex*16
	end := dataStart + hsize
	if end > len(b) {
		return nil, 0, errors.New("rpm: truncated header")
	}
	h := &header{entries: make(map[int32]indexEntry, nindex), data: b[dataStart:end]}
	for i := 0; i < nindex; i++ {
		e := b[idx+i*16:]
		tag := int32(binary.BigEndian.Uint32(e))
		ent := indexEntry{
			typ:   int32(binary.BigEndian.Uint32(e[4:])),
			off:   int32(binary.BigEndian.Uint32(e[8:])),
			count: int32(binary.BigEndian.Uint32(e[12:])),
		}
		if ent.off < 0 || int(ent.off) > hsize || ent.count < 0 {
			return nil, 0, fmt.Errorf("rpm: bad index entry for tag %d", tag)
		}
		h.entries[tag] = ent
	}
	return h, end, nil
}

func (h *header) strings(tag int32) []string {
	e, ok := h.entries[tag]
	if !ok {
		return nil
	}
	switch e.typ {
	case typeString, typeStringArray, typeI18NString:
	default:
		return nil
	}
	n := int(e.count)
	if e.typ == typeString {
		n = 1
	}
	out := make([]string, 0, n)
	p := int(e.off)
	for i := 0; i < n && p < len(h.data); i++ {
		j := p
		for j < len(h.data) && h.data[j] != 0 {
			j++
		}
		out = append(out, string(h.data[p:j]))
		p = j + 1
	}
	return out
}

func (h *header) str(tag int32) string {
	if s := h.strings(tag); len(s) > 0 {
		return s[0]
	}
	return ""
}

func (h *header) ints(tag int32) []int64 {
	e, ok := h.entries[tag]
	if !ok {
		return nil
	}
	var width int
	switch e.typ {
	case typeInt8:
		width = 1
	case typeInt16:
		width = 2
	case typeInt32:
		width = 4
	case typeInt64:
		width = 8
	default:
		return nil
	}
	p := int(e.off)
	n := int(e.count)
	if p+n*width > len(h.data) {
		return nil
	}
	out := make([]int64, n)
	for i := range out {
		q := h.data[p+i*width:]
		switch width {
		case 1:
			out[i] = int64(q[0])
		case 2:
			out[i] = int64(binary.BigEndian.Uint16(q))
		case 4:
			out[i] = int64(binary.BigEndian.Uint32(q))
		case 8:
			out[i] = int64(binary.BigEndian.Uint64(q))
		}
	}
	return out
}

func (h *header) int(tag int32) (int64, bool) {
	if v := h.ints(tag); len(v) > 0 {
		return v[0], true
	}
	return 0, false
}

// Dep is one dependency entry (provides/requires/…).
type Dep struct {
	Name  string `json:"name"`
	Flags string `json:"flags,omitempty"`
	Epoch string `json:"epoch,omitempty"`
	Ver   string `json:"ver,omitempty"`
	Rel   string `json:"rel,omitempty"`
	Pre   bool   `json:"pre,omitempty"`
}

// File is one packaged path.
type File struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"` // "", "dir", "ghost"
}

// Changelog is one changelog entry.
type Changelog struct {
	Author string `json:"author"`
	Date   int64  `json:"date"`
	Text   string `json:"text"`
}

// Package is the parsed metadata of one RPM file.
type Package struct {
	Name          string      `json:"name"`
	Arch          string      `json:"arch"`
	Epoch         string      `json:"epoch"`
	Version       string      `json:"version"`
	Release       string      `json:"release"`
	Summary       string      `json:"summary,omitempty"`
	Description   string      `json:"description,omitempty"`
	Packager      string      `json:"packager,omitempty"`
	URL           string      `json:"url,omitempty"`
	License       string      `json:"license,omitempty"`
	Vendor        string      `json:"vendor,omitempty"`
	Group         string      `json:"group,omitempty"`
	BuildHost     string      `json:"buildhost,omitempty"`
	SourceRPM     string      `json:"sourcerpm,omitempty"`
	BuildTime     int64       `json:"buildtime,omitempty"`
	FileTime      int64       `json:"filetime,omitempty"`
	PackageSize   int64       `json:"package_size"`
	InstalledSize int64       `json:"installed_size,omitempty"`
	ArchiveSize   int64       `json:"archive_size,omitempty"`
	SHA256        string      `json:"sha256"`
	Location      string      `json:"location"`
	HeaderStart   int64       `json:"header_start"`
	HeaderEnd     int64       `json:"header_end"`
	Provides      []Dep       `json:"provides,omitempty"`
	Requires      []Dep       `json:"requires,omitempty"`
	Conflicts     []Dep       `json:"conflicts,omitempty"`
	Obsoletes     []Dep       `json:"obsoletes,omitempty"`
	Recommends    []Dep       `json:"recommends,omitempty"`
	Suggests      []Dep       `json:"suggests,omitempty"`
	Supplements   []Dep       `json:"supplements,omitempty"`
	Enhances      []Dep       `json:"enhances,omitempty"`
	Files         []File      `json:"files,omitempty"`
	Changelogs    []Changelog `json:"changelogs,omitempty"`
}

// VR is "version-release".
func (p *Package) VR() string { return p.Version + "-" + p.Release }

// EVR is "version-release", prefixed with "epoch:" when epoch != 0.
func (p *Package) EVR() string {
	if p.Epoch != "" && p.Epoch != "0" {
		return p.Epoch + ":" + p.VR()
	}
	return p.VR()
}

// FileName is the canonical "{name}-{version}-{release}.{arch}.rpm".
func (p *Package) FileName() string {
	return p.Name + "-" + p.Version + "-" + p.Release + "." + p.Arch + ".rpm"
}

// maxChangelogs mirrors createrepo_c's default --changelog-limit.
const maxChangelogs = 10

// ParseRPM parses the lead, signature header and main header of an
// RPM file. Checksum, size and location are filled by the caller.
func ParseRPM(b []byte) (*Package, error) {
	if len(b) < leadSize || string(b[:4]) != string(leadMagic) {
		return nil, errors.New("rpm: not an rpm file (bad lead magic)")
	}
	sig, sigEnd, err := parseHeader(b, leadSize)
	if err != nil {
		return nil, fmt.Errorf("signature header: %w", err)
	}
	start := sigEnd
	if pad := (sigEnd - leadSize) % 8; pad != 0 {
		start += 8 - pad
	}
	h, end, err := parseHeader(b, start)
	if err != nil {
		return nil, fmt.Errorf("main header: %w", err)
	}
	p := &Package{
		Name:        h.str(tagName),
		Version:     h.str(tagVersion),
		Release:     h.str(tagRelease),
		Arch:        h.str(tagArch),
		Summary:     h.str(tagSummary),
		Description: h.str(tagDescription),
		Packager:    h.str(tagPackager),
		URL:         h.str(tagURL),
		License:     h.str(tagLicense),
		Vendor:      h.str(tagVendor),
		Group:       h.str(tagGroup),
		BuildHost:   h.str(tagBuildHost),
		SourceRPM:   h.str(tagSourceRPM),
		HeaderStart: int64(start),
		HeaderEnd:   int64(end),
		Epoch:       "0",
	}
	if p.Name == "" || p.Version == "" || p.Release == "" {
		return nil, errors.New("rpm: header lacks name/version/release")
	}
	if _, isSrc := h.entries[tagSourcePackage]; isSrc {
		p.Arch = "src"
	}
	if p.Arch == "" {
		p.Arch = "noarch"
	}
	if strings.ContainsAny(p.Name+p.Version+p.Release+p.Arch, "/\\\x00") {
		return nil, errors.New("rpm: invalid characters in name/version/release/arch")
	}
	if v, ok := h.int(tagEpoch); ok {
		p.Epoch = strconv.FormatInt(v, 10)
	}
	p.BuildTime, _ = h.int(tagBuildTime)
	if v, ok := h.int(tagLongSize); ok {
		p.InstalledSize = v
	} else {
		p.InstalledSize, _ = h.int(tagSize)
	}
	if v, ok := sig.int(sigTagPayloadSize); ok {
		p.ArchiveSize = v
	} else {
		p.ArchiveSize, _ = h.int(tagArchiveSize)
	}

	p.Provides = deps(h, tagProvideName, tagProvideFlags, tagProvideVersion, false)
	p.Requires = deps(h, tagRequireName, tagRequireFlags, tagRequireVersion, true)
	p.Conflicts = deps(h, tagConflictName, tagConflictFlags, tagConflictVer, false)
	p.Obsoletes = deps(h, tagObsoleteName, tagObsoleteFlags, tagObsoleteVer, false)
	p.Recommends = deps(h, tagRecommendName, tagRecommendFlags, tagRecommendVer, false)
	p.Suggests = deps(h, tagSuggestName, tagSuggestFlags, tagSuggestVer, false)
	p.Supplements = deps(h, tagSupplementName, tagSupplementFlag, tagSupplementVer, false)
	p.Enhances = deps(h, tagEnhanceName, tagEnhanceFlags, tagEnhanceVer, false)
	p.Files = files(h)

	times := h.ints(tagChangelogTime)
	names := h.strings(tagChangelogName)
	texts := h.strings(tagChangelogText)
	for i := 0; i < len(times) && i < len(names) && i < len(texts) && i < maxChangelogs; i++ {
		p.Changelogs = append(p.Changelogs, Changelog{Author: names[i], Date: times[i], Text: texts[i]})
	}
	return p, nil
}

func deps(h *header, nameTag, flagTag, verTag int32, requires bool) []Dep {
	names := h.strings(nameTag)
	flags := h.ints(flagTag)
	vers := h.strings(verTag)
	var out []Dep
	seen := map[string]bool{}
	for i, n := range names {
		if requires && strings.HasPrefix(n, "rpmlib(") {
			continue
		}
		d := Dep{Name: n}
		var f int64
		if i < len(flags) {
			f = flags[i]
		}
		switch f & (senseLess | senseGreater | senseEqual) {
		case senseLess:
			d.Flags = "LT"
		case senseGreater:
			d.Flags = "GT"
		case senseEqual:
			d.Flags = "EQ"
		case senseLess | senseEqual:
			d.Flags = "LE"
		case senseGreater | senseEqual:
			d.Flags = "GE"
		}
		if i < len(vers) && vers[i] != "" {
			d.Epoch, d.Ver, d.Rel = splitEVR(vers[i])
		}
		if requires && f&(sensePrereq|senseScriptPre|senseScriptPost) != 0 {
			d.Pre = true
		}
		key := d.Name + "\x00" + d.Flags + "\x00" + d.Epoch + d.Ver + "\x00" + d.Rel
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

// splitEVR splits "[epoch:]version[-release]".
func splitEVR(s string) (epoch, ver, rel string) {
	epoch = "0"
	if i := strings.IndexByte(s, ':'); i >= 0 {
		epoch, s = s[:i], s[i+1:]
	}
	ver = s
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		ver, rel = s[:i], s[i+1:]
	}
	return epoch, ver, rel
}

func files(h *header) []File {
	var paths []string
	if base := h.strings(tagBaseNames); len(base) > 0 {
		dirs := h.strings(tagDirNames)
		idx := h.ints(tagDirIndexes)
		for i, b := range base {
			d := ""
			if i < len(idx) && int(idx[i]) < len(dirs) {
				d = dirs[idx[i]]
			}
			paths = append(paths, d+b)
		}
	} else {
		paths = h.strings(tagOldFilenames)
	}
	modes := h.ints(tagFileModes)
	flags := h.ints(tagFileFlags)
	out := make([]File, 0, len(paths))
	for i, p := range paths {
		f := File{Path: path.Clean(p)}
		if i < len(flags) && flags[i]&fileFlagGhost != 0 {
			f.Type = "ghost"
		} else if i < len(modes) && modes[i]&0o170000 == 0o040000 {
			f.Type = "dir"
		}
		out = append(out, f)
	}
	return out
}

// isPrimaryFile mirrors createrepo_c's rule for files listed in
// primary.xml (the rest live only in filelists.xml).
func isPrimaryFile(p string) bool {
	return strings.Contains(p, "bin/") || strings.HasPrefix(p, "/etc/") || p == "/usr/lib/sendmail"
}

// parseFileName splits "{name}-{version}-{release}.{arch}.rpm".
func parseFileName(base string) (name, vr, arch string, ok bool) {
	base = strings.TrimSuffix(base, ".rpm")
	i := strings.LastIndexByte(base, '.')
	if i <= 0 {
		return "", "", "", false
	}
	arch, base = base[i+1:], base[:i]
	r := strings.LastIndexByte(base, '-')
	if r <= 0 {
		return "", "", "", false
	}
	v := strings.LastIndexByte(base[:r], '-')
	if v <= 0 {
		return "", "", "", false
	}
	return base[:v], base[v+1:], arch, true
}
