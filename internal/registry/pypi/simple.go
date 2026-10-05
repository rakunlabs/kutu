package pypi

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

// Simple API media types (PEP 691).
const (
	mediaJSON   = "application/vnd.pypi.simple.v1+json"
	mediaV1HTML = "application/vnd.pypi.simple.v1+html"
	mediaHTML   = "text/html; charset=utf-8"
	apiVersion  = "1.1"
	acceptAll   = mediaJSON + ", " + mediaV1HTML + ";q=0.2, text/html;q=0.01"
)

type format int

const (
	fmtHTML format = iota
	fmtV1HTML
	fmtJSON
)

// negotiate picks the Simple API response format from ?format= or the
// Accept header. Unknown or missing preferences fall back to text/html.
func negotiate(r *http.Request) format {
	if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format"))); q != "" {
		switch {
		case strings.Contains(q, "json"):
			return fmtJSON
		case strings.Contains(q, "vnd.pypi.simple"):
			return fmtV1HTML
		default:
			return fmtHTML
		}
	}
	best, bestQ := fmtHTML, -1.0
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, p := range fields[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		var f format
		switch mt {
		case mediaJSON, "application/vnd.pypi.simple.latest+json":
			f = fmtJSON
		case mediaV1HTML, "application/vnd.pypi.simple.latest+html":
			f = fmtV1HTML
		case "text/html", "text/*", "*/*":
			f = fmtHTML
		default:
			continue
		}
		if q <= 0 {
			continue
		}
		if q > bestQ || (q == bestQ && f > best) {
			best, bestQ = f, q
		}
	}
	return best
}

// index is the protocol-neutral form of a Simple API page. A root page
// carries Projects; a project page carries Files and Versions.
type index struct {
	Name     string
	LinkBase string
	Projects []string
	Files    []indexFile
	Versions []string
}

type indexFile struct {
	Filename        string
	URL             string
	Hashes          map[string]string
	RequiresPython  string
	Yanked          bool
	YankedReason    string
	HasCoreMetadata bool
	CoreMetadata    map[string]string
	Size            int64
	UploadTime      string
}

// ── wire (PEP 691 JSON) ──

type wireMeta struct {
	APIVersion string `json:"api-version"`
}

type wireProject struct {
	Name string `json:"name"`
}

type wireRoot struct {
	Meta     wireMeta      `json:"meta"`
	Projects []wireProject `json:"projects"`
}

type wireFile struct {
	Filename         string            `json:"filename"`
	URL              string            `json:"url"`
	Hashes           map[string]string `json:"hashes"`
	RequiresPython   string            `json:"requires-python,omitempty"`
	Yanked           any               `json:"yanked,omitempty"`
	CoreMetadata     any               `json:"core-metadata,omitempty"`
	DistInfoMetadata any               `json:"dist-info-metadata,omitempty"`
	Size             *int64            `json:"size,omitempty"`
	UploadTime       string            `json:"upload-time,omitempty"`
}

type wirePage struct {
	Meta     wireMeta   `json:"meta"`
	Name     string     `json:"name"`
	Files    []wireFile `json:"files"`
	Versions []string   `json:"versions"`
}

type wireFileIn struct {
	Filename         string            `json:"filename"`
	URL              string            `json:"url"`
	Hashes           map[string]string `json:"hashes"`
	RequiresPython   *string           `json:"requires-python"`
	Yanked           json.RawMessage   `json:"yanked"`
	CoreMetadata     json.RawMessage   `json:"core-metadata"`
	DistInfoMetadata json.RawMessage   `json:"dist-info-metadata"`
	DataDistInfoMeta json.RawMessage   `json:"data-dist-info-metadata"`
	Size             *int64            `json:"size"`
	UploadTime       *string           `json:"upload-time"`
}

type wireIn struct {
	Name     string        `json:"name"`
	Projects []wireProject `json:"projects"`
	Files    []wireFileIn  `json:"files"`
	Versions []string      `json:"versions"`
}

func (idx *index) isRoot() bool { return idx.Name == "" }

func (idx *index) toWire() any {
	if idx.isRoot() {
		out := wireRoot{Meta: wireMeta{APIVersion: apiVersion}, Projects: []wireProject{}}
		for _, p := range idx.Projects {
			out.Projects = append(out.Projects, wireProject{Name: p})
		}
		return out
	}
	out := wirePage{Meta: wireMeta{APIVersion: apiVersion}, Name: idx.Name, Files: []wireFile{}, Versions: idx.Versions}
	if out.Versions == nil {
		out.Versions = []string{}
	}
	for _, f := range idx.Files {
		wf := wireFile{Filename: f.Filename, URL: f.URL, Hashes: f.Hashes, RequiresPython: f.RequiresPython, UploadTime: f.UploadTime}
		if wf.Hashes == nil {
			wf.Hashes = map[string]string{}
		}
		if f.Yanked {
			if f.YankedReason != "" {
				wf.Yanked = f.YankedReason
			} else {
				wf.Yanked = true
			}
		}
		if f.HasCoreMetadata {
			var cm any = true
			if len(f.CoreMetadata) > 0 {
				cm = f.CoreMetadata
			}
			wf.CoreMetadata, wf.DistInfoMetadata = cm, cm
		}
		if f.Size > 0 {
			sz := f.Size
			wf.Size = &sz
		}
		out.Files = append(out.Files, wf)
	}
	return out
}

func (idx *index) marshalJSON() []byte {
	b, _ := pkgbase.MarshalJSON(idx.toWire())
	return b
}

// parseIndexJSON decodes a PEP 691 JSON document (root or project).
func parseIndexJSON(body []byte) (*index, error) {
	var in wireIn
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("pypi: decode simple json: %w", err)
	}
	idx := &index{Name: normalizeName(in.Name), Versions: in.Versions}
	if in.Name == "" {
		idx.Name = ""
		for _, p := range in.Projects {
			idx.Projects = append(idx.Projects, p.Name)
		}
		return idx, nil
	}
	for _, f := range in.Files {
		out := indexFile{Filename: f.Filename, URL: f.URL, Hashes: f.Hashes}
		if f.RequiresPython != nil {
			out.RequiresPython = *f.RequiresPython
		}
		if f.Size != nil {
			out.Size = *f.Size
		}
		if f.UploadTime != nil {
			out.UploadTime = *f.UploadTime
		}
		out.Yanked, out.YankedReason = decodeYanked(f.Yanked)
		for _, raw := range []json.RawMessage{f.CoreMetadata, f.DistInfoMetadata, f.DataDistInfoMeta} {
			if ok, hashes := decodeCoreMetadata(raw); ok {
				out.HasCoreMetadata, out.CoreMetadata = true, hashes
				break
			}
		}
		idx.Files = append(idx.Files, out)
	}
	if len(idx.Versions) == 0 {
		idx.Versions = versionsOf(idx.Files)
	}
	return idx, nil
}

func decodeYanked(raw json.RawMessage) (bool, string) {
	if len(raw) == 0 {
		return false, ""
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return true, s
	}
	return false, ""
}

func decodeCoreMetadata(raw json.RawMessage) (bool, map[string]string) {
	if len(raw) == 0 {
		return false, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b, nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		return true, m
	}
	return false, nil
}

func versionsOf(files []indexFile) []string {
	seen := map[string]struct{}{}
	for _, f := range files {
		if _, v := InferNameVersion(f.Filename); v != "" {
			seen[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	pkgbase.SortVersions(out)
	return out
}

// ── HTML (PEP 503 / 592 / 658 / 714) ──

func (idx *index) marshalHTML() []byte {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n")
	fmt.Fprintf(&b, `<meta name="pypi:repository-version" content="%s">`+"\n", apiVersion)
	if idx.isRoot() {
		b.WriteString("<title>Simple index</title>\n</head>\n<body>\n")
		for _, name := range idx.Projects {
			fmt.Fprintf(&b, `<a href="%s">%s</a><br/>`+"\n", html.EscapeString(idx.LinkBase+url.PathEscape(normalizeName(name))+"/"), html.EscapeString(name))
		}
		b.WriteString("</body>\n</html>\n")
		return []byte(b.String())
	}
	name := html.EscapeString(idx.Name)
	fmt.Fprintf(&b, "<title>Links for %s</title>\n</head>\n<body>\n<h1>Links for %s</h1>\n", name, name)
	for _, f := range idx.Files {
		href := f.URL
		if h := f.Hashes["sha256"]; h != "" {
			href += "#sha256=" + h
		}
		fmt.Fprintf(&b, `<a href="%s"`, html.EscapeString(href))
		if f.RequiresPython != "" {
			fmt.Fprintf(&b, ` data-requires-python="%s"`, html.EscapeString(f.RequiresPython))
		}
		if f.Yanked {
			fmt.Fprintf(&b, ` data-yanked="%s"`, html.EscapeString(f.YankedReason))
		}
		if f.HasCoreMetadata {
			v := "true"
			if h := f.CoreMetadata["sha256"]; h != "" {
				v = "sha256=" + h
			}
			fmt.Fprintf(&b, ` data-dist-info-metadata="%s" data-core-metadata="%s"`, v, v)
		}
		fmt.Fprintf(&b, ">%s</a><br/>\n", html.EscapeString(f.Filename))
	}
	b.WriteString("</body>\n</html>\n")
	return []byte(b.String())
}

var (
	anchorRE = regexp.MustCompile(`(?is)<a\s([^>]*)>(.*?)</a>`)
	attrRE   = regexp.MustCompile(`([^\s=/>]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?`)
	tagRE    = regexp.MustCompile(`<[^>]*>`)
)

// parseIndexHTML decodes a PEP 503 HTML page. name is "" for the root.
// base resolves relative hrefs; resulting URLs are absolute.
func parseIndexHTML(body []byte, name string, base *url.URL) *index {
	idx := &index{Name: normalizeName(name)}
	if name == "" {
		idx.Name = ""
	}
	for _, m := range anchorRE.FindAllSubmatch(body, -1) {
		attrs := map[string]string{}
		present := map[string]bool{}
		for _, a := range attrRE.FindAllSubmatch(m[1], -1) {
			k := strings.ToLower(string(a[1]))
			present[k] = true
			v := string(a[2])
			if v == "" {
				v = string(a[3])
			}
			if v == "" {
				v = string(a[4])
			}
			attrs[k] = html.UnescapeString(v)
		}
		text := strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(string(m[2]), "")))
		if idx.isRoot() {
			if text != "" {
				idx.Projects = append(idx.Projects, text)
			}
			continue
		}
		href := attrs["href"]
		if href == "" {
			continue
		}
		u, err := url.Parse(href)
		if err != nil {
			continue
		}
		if base != nil {
			u = base.ResolveReference(u)
		}
		f := indexFile{Hashes: map[string]string{}}
		if algo, val, ok := strings.Cut(u.Fragment, "="); ok && algo != "" && val != "" {
			f.Hashes[algo] = val
		}
		u.Fragment = ""
		f.URL = u.String()
		f.Filename = text
		if f.Filename == "" {
			f.Filename = pathBase(u.Path)
		}
		f.RequiresPython = attrs["data-requires-python"]
		if present["data-yanked"] {
			f.Yanked, f.YankedReason = true, attrs["data-yanked"]
		}
		for _, k := range []string{"data-core-metadata", "data-dist-info-metadata"} {
			if !present[k] {
				continue
			}
			v := attrs[k]
			if strings.EqualFold(v, "false") {
				break
			}
			f.HasCoreMetadata = true
			if algo, val, ok := strings.Cut(v, "="); ok {
				f.CoreMetadata = map[string]string{algo: val}
			}
			break
		}
		idx.Files = append(idx.Files, f)
	}
	if !idx.isRoot() {
		idx.Versions = versionsOf(idx.Files)
	}
	return idx
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if u, err := url.PathUnescape(p); err == nil {
		return u
	}
	return p
}

// writeIndex renders idx in the negotiated format.
func writeIndex(w http.ResponseWriter, r *http.Request, idx *index) {
	w.Header().Set("Vary", "Accept")
	switch negotiate(r) {
	case fmtJSON:
		pkgbase.WriteBytes(w, r, http.StatusOK, mediaJSON, idx.marshalJSON())
	case fmtV1HTML:
		pkgbase.WriteBytes(w, r, http.StatusOK, mediaV1HTML, idx.marshalHTML())
	default:
		pkgbase.WriteBytes(w, r, http.StatusOK, mediaHTML, idx.marshalHTML())
	}
}

// mergeIndexes unions member pages: first member wins per filename,
// versions and projects are unioned.
func mergeIndexes(name string, pages []*index) *index {
	out := &index{Name: name}
	seenFile := map[string]bool{}
	seenProj := map[string]bool{}
	seenVer := map[string]bool{}
	for _, p := range pages {
		for _, proj := range p.Projects {
			if k := normalizeName(proj); !seenProj[k] {
				seenProj[k] = true
				out.Projects = append(out.Projects, proj)
			}
		}
		for _, f := range p.Files {
			if !seenFile[f.Filename] {
				seenFile[f.Filename] = true
				out.Files = append(out.Files, f)
			}
		}
		for _, v := range p.Versions {
			if !seenVer[v] {
				seenVer[v] = true
				out.Versions = append(out.Versions, v)
			}
		}
	}
	sort.Slice(out.Projects, func(i, j int) bool { return normalizeName(out.Projects[i]) < normalizeName(out.Projects[j]) })
	sort.SliceStable(out.Files, func(i, j int) bool { return out.Files[i].Filename < out.Files[j].Filename })
	pkgbase.SortVersions(out.Versions)
	return out
}
