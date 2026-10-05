package pypi

import (
	"encoding/json"
	"net/url"
	"sort"
	"time"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

// jsonFile is one file entry of the warehouse JSON API.
type jsonFile struct {
	Filename          string            `json:"filename"`
	URL               string            `json:"url"`
	Digests           map[string]string `json:"digests"`
	Size              int64             `json:"size"`
	UploadTime        string            `json:"upload_time,omitempty"`
	UploadTimeISO8601 string            `json:"upload_time_iso_8601,omitempty"`
	RequiresPython    *string           `json:"requires_python"`
	PackageType       string            `json:"packagetype,omitempty"`
	PythonVersion     string            `json:"python_version,omitempty"`
	Yanked            bool              `json:"yanked"`
	YankedReason      *string           `json:"yanked_reason"`
	HasSig            bool              `json:"has_sig"`
}

type jsonInfo struct {
	Name           string  `json:"name"`
	Version        string  `json:"version"`
	Summary        string  `json:"summary"`
	License        string  `json:"license"`
	RequiresPython *string `json:"requires_python"`
	HomePage       string  `json:"home_page"`
	Author         string  `json:"author"`
	AuthorEmail    string  `json:"author_email"`
	PackageURL     string  `json:"package_url"`
	ReleaseURL     string  `json:"release_url"`
	Yanked         bool    `json:"yanked"`
	YankedReason   *string `json:"yanked_reason"`
}

type jsonDoc struct {
	Info     jsonInfo              `json:"info"`
	Releases map[string][]jsonFile `json:"releases,omitempty"`
	URLs     []jsonFile            `json:"urls"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// buildJSONAPI renders the warehouse JSON document for a local
// package. version == "" selects the latest non-yanked version and
// includes every release. Returns nil when version is unknown.
func buildJSONAPI(publicBase, name, version string, metas []*FileMeta) *jsonDoc {
	byVer := map[string][]*FileMeta{}
	var versions, live []string
	for _, fm := range metas {
		if _, ok := byVer[fm.Version]; !ok {
			versions = append(versions, fm.Version)
		}
		byVer[fm.Version] = append(byVer[fm.Version], fm)
	}
	for _, v := range versions {
		for _, fm := range byVer[v] {
			if !fm.Yanked {
				live = append(live, v)
				break
			}
		}
	}
	project := version == ""
	if project {
		version = pkgbase.Latest(live)
		if version == "" {
			version = pkgbase.Latest(versions)
		}
	}
	files := byVer[version]
	if len(files) == 0 {
		return nil
	}
	conv := func(fm *FileMeta) jsonFile {
		f := jsonFile{
			Filename:       fm.Filename,
			URL:            publicBase + "/packages/" + url.PathEscape(name) + "/" + url.PathEscape(fm.Filename),
			Digests:        map[string]string{"sha256": fm.SHA256},
			Size:           fm.Size,
			RequiresPython: strPtr(fm.RequiresPython),
			PackageType:    fm.PackageType,
			PythonVersion:  fm.PythonVersion,
			Yanked:         fm.Yanked,
		}
		if fm.Yanked {
			f.YankedReason = strPtr(fm.YankedReason)
		}
		if !fm.UploadTime.IsZero() {
			f.UploadTime = fm.UploadTime.UTC().Format("2006-01-02T15:04:05")
			f.UploadTimeISO8601 = formatUploadTime(fm.UploadTime)
		}
		return f
	}
	doc := &jsonDoc{URLs: []jsonFile{}}
	info := &doc.Info
	info.Name, info.Version = name, version
	info.PackageURL = publicBase + "/pypi/" + url.PathEscape(name) + "/"
	info.ReleaseURL = info.PackageURL + url.PathEscape(version) + "/"
	allYanked := true
	for _, fm := range files {
		doc.URLs = append(doc.URLs, conv(fm))
		pick := func(dst *string, v string) {
			if *dst == "" {
				*dst = v
			}
		}
		pick(&info.Summary, fm.Summary)
		pick(&info.License, fm.License)
		pick(&info.HomePage, fm.HomePage)
		pick(&info.Author, fm.Author)
		pick(&info.AuthorEmail, fm.AuthorEmail)
		if info.RequiresPython == nil {
			info.RequiresPython = strPtr(fm.RequiresPython)
		}
		if !fm.Yanked {
			allYanked = false
		} else if info.YankedReason == nil {
			info.YankedReason = strPtr(fm.YankedReason)
		}
	}
	info.Yanked = allYanked
	if !allYanked {
		info.YankedReason = nil
	}
	if project {
		doc.Releases = map[string][]jsonFile{}
		for _, v := range versions {
			for _, fm := range byVer[v] {
				doc.Releases[v] = append(doc.Releases[v], conv(fm))
			}
		}
	}
	sort.Slice(doc.URLs, func(i, j int) bool { return doc.URLs[i].Filename < doc.URLs[j].Filename })
	return doc
}

// upstreamDoc is the subset of an upstream JSON API document used for
// policy metadata.
type upstreamDoc struct {
	Info struct {
		Version         string   `json:"version"`
		License         string   `json:"license"`
		LicenseExpr     string   `json:"license_expression"`
		Classifiers     []string `json:"classifiers"`
		RequiresPython  string   `json:"requires_python"`
		Summary         string   `json:"summary"`
		ProjectHomePage string   `json:"home_page"`
	} `json:"info"`
	Releases map[string][]struct {
		UploadTime string `json:"upload_time_iso_8601"`
	} `json:"releases"`
	URLs []struct {
		UploadTime string `json:"upload_time_iso_8601"`
	} `json:"urls"`
}

func decodeJSONAPI(body []byte) (*upstreamDoc, error) {
	var d upstreamDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *upstreamDoc) license() string {
	if d.Info.LicenseExpr != "" {
		return d.Info.LicenseExpr
	}
	if l := d.Info.License; l != "" && len(l) < 128 {
		return l
	}
	return ""
}

func (d *upstreamDoc) published(version string) time.Time {
	var best time.Time
	consider := func(s string) {
		if t, err := time.Parse(time.RFC3339, s); err == nil && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	if version == "" || version == d.Info.Version {
		for _, u := range d.URLs {
			consider(u.UploadTime)
		}
	}
	if best.IsZero() {
		for _, f := range d.Releases[version] {
			consider(f.UploadTime)
		}
	}
	return best
}

// rewriteJSONAPI points every file URL in an upstream JSON API
// document back at kutu's /_remote/ cache.
func rewriteJSONAPI(body []byte, publicBase string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	rewrite := func(v any) {
		files, _ := v.([]any)
		for _, f := range files {
			m, ok := f.(map[string]any)
			if !ok {
				continue
			}
			u, _ := m["url"].(string)
			fn, _ := m["filename"].(string)
			if u == "" {
				continue
			}
			if fn == "" {
				fn = pathBase(u)
			}
			m["url"] = publicBase + "/_remote/" + encodeKey(u) + "/" + url.PathEscape(fn)
		}
	}
	rewrite(doc["urls"])
	if rel, ok := doc["releases"].(map[string]any); ok {
		for _, files := range rel {
			rewrite(files)
		}
	}
	if info, ok := doc["info"].(map[string]any); ok {
		for _, k := range []string{"package_url", "release_url", "project_url"} {
			delete(info, k)
		}
	}
	return pkgbase.MarshalJSON(doc)
}
