package composer

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9]([_.-]?[a-z0-9]+)*/[a-z0-9](([_.]|-{1,2})?[a-z0-9]+)*$`)
	numericRe = regexp.MustCompile(`^(\d+)(?:\.(\d+|[xX*]))?(?:\.(\d+|[xX*]))?(?:\.(\d+|[xX*]))?`)
	stabRe    = regexp.MustCompile(`(?i)^[._-]?(stable|beta|b|rc|alpha|a|patch|pl|p)((?:[.-]?\d+)*)$`)
)

// ValidName reports whether name is a valid "vendor/package" name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

func validVersion(v string) bool {
	if v == "" || len(v) > 200 || strings.Contains(v, "..") || strings.HasPrefix(v, "/") || strings.HasSuffix(v, "/") {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r == '\\' || r == 0x7f {
			return false
		}
	}
	return true
}

// IsDevVersion reports whether v belongs in the "~dev" metadata file.
func IsDevVersion(v string) bool {
	lv := strings.ToLower(v)
	return strings.HasPrefix(lv, "dev-") || strings.HasSuffix(lv, "-dev") || strings.HasSuffix(lv, ".x-dev")
}

// NormalizeVersion approximates Composer's VersionParser::normalize.
func NormalizeVersion(v string) string {
	orig := strings.TrimSpace(v)
	v = orig
	lv := strings.ToLower(v)
	if strings.HasPrefix(lv, "dev-") {
		return "dev-" + v[4:]
	}
	switch lv {
	case "master", "trunk", "default":
		return "dev-" + v
	}
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	dev := false
	if l := strings.ToLower(v); strings.HasSuffix(l, "-dev") || strings.HasSuffix(l, ".dev") {
		dev = true
		v = v[:len(v)-4]
	}
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	m := numericRe.FindStringSubmatch(v)
	if m == nil {
		return orig
	}
	parts := make([]string, 4)
	for i := 0; i < 4; i++ {
		switch p := m[i+1]; p {
		case "":
			parts[i] = "0"
			if dev && i > 0 && strings.ContainsAny(m[0], "xX*") {
				parts[i] = "9999999"
			}
		case "x", "X", "*":
			parts[i] = "9999999"
		default:
			parts[i] = p
		}
	}
	out := strings.Join(parts, ".")
	if rest := v[len(m[0]):]; rest != "" {
		sm := stabRe.FindStringSubmatch(rest)
		if sm == nil {
			return orig
		}
		mod := expandStability(sm[1])
		if mod != "stable" {
			out += "-" + mod + strings.TrimLeft(sm[2], ".-")
		}
	}
	if dev {
		out += "-dev"
	}
	return out
}

func expandStability(s string) string {
	switch strings.ToLower(s) {
	case "a", "alpha":
		return "alpha"
	case "b", "beta":
		return "beta"
	case "p", "pl", "patch":
		return "patch"
	case "rc":
		return "RC"
	}
	return "stable"
}

// escVersion is the storage / URL form of a version.
func escVersion(v string) string { return url.PathEscape(v) }

func unescVersion(v string) string {
	if u, err := url.PathUnescape(v); err == nil {
		return u
	}
	return v
}

// distPath is the client-facing path (below the prefix) of a dist zip.
func distPath(name, version string) string {
	return "/dist/" + name + "/" + escVersion(version) + ".zip"
}

// ReadComposerJSON extracts composer.json from a package zip (root or
// single top-level directory).
func ReadComposerJSON(body []byte) (map[string]any, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}
	var best *zip.File
	bestDepth := 99
	for _, f := range zr.File {
		n := strings.TrimPrefix(f.Name, "./")
		if path.Base(n) != "composer.json" || f.FileInfo().IsDir() {
			continue
		}
		if d := strings.Count(n, "/"); d <= 1 && d < bestDepth {
			best, bestDepth = f, d
		}
	}
	if best == nil {
		return nil, errors.New("composer.json not found in archive")
	}
	rc, err := best.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("composer.json: %w", err)
	}
	return m, nil
}

// droppedKeys are root-only composer.json fields that never appear in
// repository metadata.
var droppedKeys = []string{"repositories", "config", "minimum-stability", "prefer-stable", "dist", "_comment"}

// buildVersion turns a composer.json into a version metadata object.
func buildVersion(cj map[string]any, name, version string, published time.Time) map[string]any {
	out := make(map[string]any, len(cj)+4)
	for k, v := range cj {
		out[k] = v
	}
	for _, k := range droppedKeys {
		delete(out, k)
	}
	out["name"] = name
	out["version"] = version
	out["version_normalized"] = NormalizeVersion(version)
	if _, ok := out["time"].(string); !ok {
		out["time"] = published.UTC().Format(time.RFC3339)
	}
	return out
}

// p2Doc is a decoded Composer v2 metadata document.
type p2Doc map[string][]map[string]any

// parseP2 decodes a p2 metadata document, expanding the minified form.
func parseP2(body []byte) (p2Doc, error) {
	var raw struct {
		Packages json.RawMessage `json:"packages"`
		Minified string          `json:"minified"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := p2Doc{}
	if len(raw.Packages) == 0 || bytes.HasPrefix(bytes.TrimSpace(raw.Packages), []byte("[")) {
		return out, nil
	}
	if err := json.Unmarshal(raw.Packages, &out); err != nil {
		return nil, err
	}
	if raw.Minified != "" {
		for k, vs := range out {
			out[k] = expandMinified(vs)
		}
	}
	return out, nil
}

// expandMinified mirrors Composer's MetadataMinifier::expand.
func expandMinified(vs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	var cur map[string]any
	for _, v := range vs {
		if cur == nil {
			cur = cloneMap(v)
			out = append(out, cloneMap(cur))
			continue
		}
		for k, val := range v {
			if s, ok := val.(string); ok && s == "__unset" {
				delete(cur, k)
				continue
			}
			cur[k] = val
		}
		out = append(out, cloneMap(cur))
	}
	return out
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// licenseOf renders the "license" field as an SPDX expression.
func licenseOf(m map[string]any) string {
	switch l := m["license"].(type) {
	case string:
		return l
	case []any:
		var parts []string
		for _, x := range l {
			if s, ok := x.(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 1 {
			return "(" + strings.Join(parts, " OR ") + ")"
		}
		return strings.Join(parts, "")
	}
	return ""
}

func timeOf(m map[string]any) time.Time {
	s := str(m, "time")
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func sortVersionsDesc(vs []map[string]any) {
	sort.SliceStable(vs, func(i, j int) bool {
		return pkgbase.CompareVersions(str(vs[i], "version"), str(vs[j], "version")) > 0
	})
}

// detailFrom builds a generic package detail from version objects.
func detailFrom(name string, vs []map[string]any, sizes map[string]int64) *registry.PackageDetail {
	sortVersionsDesc(vs)
	d := &registry.GenericPackageDetail{}
	var stable []string
	for _, v := range vs {
		if ver := str(v, "version"); !IsDevVersion(ver) {
			stable = append(stable, ver)
		}
	}
	d.LatestVersion = pkgbase.Latest(stable)
	if d.LatestVersion == "" && len(vs) > 0 {
		d.LatestVersion = str(vs[0], "version")
	}
	for _, v := range vs {
		ver := str(v, "version")
		if ver == d.LatestVersion {
			d.Description = str(v, "description")
			d.Homepage = str(v, "homepage")
			d.License = licenseOf(v)
			if t := str(v, "type"); t != "" {
				d.Metadata = map[string]string{"type": t}
			}
		}
		row := registry.GenericVersionDetail{Version: ver, Size: sizes[ver]}
		if t := timeOf(v); !t.IsZero() {
			row.PublishedAt = t.UTC().Format(time.RFC3339)
		}
		md := map[string]string{"version_normalized": str(v, "version_normalized")}
		if l := licenseOf(v); l != "" {
			md["license"] = l
		}
		if dist, ok := v["dist"].(map[string]any); ok {
			if s := str(dist, "shasum"); s != "" {
				md["shasum"] = s
			}
		}
		if req, ok := v["require"].(map[string]any); ok && len(req) > 0 {
			keys := make([]string, 0, len(req))
			for k := range req {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			md["require"] = strings.Join(keys, ", ")
		}
		row.Metadata = md
		d.Versions = append(d.Versions, row)
	}
	return &registry.PackageDetail{Type: typ, Name: name, Generic: d}
}

// splitP2 parses "/p2/{vendor}/{name}.json" or "...~dev.json".
func splitP2(p string) (name string, dev, ok bool) {
	rest, found := strings.CutPrefix(p, "/p2/")
	if !found {
		return "", false, false
	}
	rest, found = strings.CutSuffix(rest, ".json")
	if !found {
		return "", false, false
	}
	if n, isDev := strings.CutSuffix(rest, "~dev"); isDev {
		rest, dev = n, true
	}
	rest = strings.ToLower(rest)
	if !ValidName(rest) {
		return "", false, false
	}
	return rest, dev, true
}

// splitDist parses "/dist/{vendor}/{name}/{version}.zip".
func splitDist(p string) (name, version string, ok bool) {
	rest, found := strings.CutPrefix(p, "/dist/")
	if !found {
		return "", "", false
	}
	rest, found = strings.CutSuffix(rest, ".zip")
	if !found {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	name = strings.ToLower(parts[0] + "/" + parts[1])
	version = unescVersion(parts[2])
	if !ValidName(name) || !validVersion(version) {
		return "", "", false
	}
	return name, version, true
}

// splitProviders parses "/providers/{vendor}/{name}.json".
func splitProviders(p string) (string, bool) {
	rest, found := strings.CutPrefix(p, "/providers/")
	if !found {
		return "", false
	}
	rest, found = strings.CutSuffix(rest, ".json")
	if !found || !ValidName(strings.ToLower(rest)) {
		return "", false
	}
	return strings.ToLower(rest), true
}

func classify(p string) (registry.ArtifactRef, bool) {
	if name, _, ok := splitP2(p); ok {
		return registry.ArtifactRef{Name: name}, true
	}
	if name, ver, ok := splitDist(p); ok {
		return registry.ArtifactRef{Name: name, Version: ver}, true
	}
	return registry.ArtifactRef{}, false
}

// rootDoc is the synthesized packages.json.
func rootDoc(prefix string, available []string) map[string]any {
	doc := map[string]any{
		"packages":      map[string]any{},
		"metadata-url":  prefix + "/p2/%package%.json",
		"search":        prefix + "/search.json?q=%query%&type=%type%",
		"providers-api": prefix + "/providers/%package%.json",
		"list":          prefix + "/list.json",
	}
	if available != nil {
		doc["available-packages"] = available
	}
	return doc
}

type searchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

func matchSearch(q, typFilter string, v map[string]any) bool {
	if typFilter != "" && str(v, "type") != typFilter {
		return false
	}
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	if strings.Contains(strings.ToLower(str(v, "name")), q) || strings.Contains(strings.ToLower(str(v, "description")), q) {
		return true
	}
	if kws, ok := v["keywords"].([]any); ok {
		for _, k := range kws {
			if s, ok := k.(string); ok && strings.Contains(strings.ToLower(s), q) {
				return true
			}
		}
	}
	return false
}

// provides reports whether version object v provides / replaces target.
func provides(v map[string]any, target string) bool {
	for _, key := range []string{"provide", "replace"} {
		if m, ok := v[key].(map[string]any); ok {
			if _, ok := m[target]; ok {
				return true
			}
		}
	}
	return false
}
