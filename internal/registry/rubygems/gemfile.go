package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// GemMeta is the JSON metadata stored next to every pushed gem.
type GemMeta struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	Platform         string            `json:"platform,omitempty"`
	Summary          string            `json:"summary,omitempty"`
	Description      string            `json:"description,omitempty"`
	Homepage         string            `json:"homepage,omitempty"`
	Authors          []string          `json:"authors,omitempty"`
	Email            []string          `json:"email,omitempty"`
	Licenses         []string          `json:"licenses,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Dependencies     []Dependency      `json:"dependencies,omitempty"`
	RequiredRuby     []string          `json:"required_ruby_version,omitempty"`
	RequiredRubygems []string          `json:"required_rubygems_version,omitempty"`
	RubygemsVersion  string            `json:"rubygems_version,omitempty"`
	SHA256           string            `json:"sha256"`
	Size             int64             `json:"size"`
	CreatedAt        time.Time         `json:"created_at"`
}

// Dependency is one gemspec dependency.
type Dependency struct {
	Name         string   `json:"name"`
	Requirements []string `json:"requirements"`
	Type         string   `json:"type"` // runtime | development
}

func (m *GemMeta) platform() string {
	if m.Platform == "" {
		return "ruby"
	}
	return m.Platform
}

// versionPlatform is "1.0.0" or "1.0.0-x86_64-linux".
func (m *GemMeta) versionPlatform() string {
	if p := m.platform(); p != "ruby" {
		return m.Version + "-" + p
	}
	return m.Version
}

// FullName is "{name}-{version}[-{platform}]".
func (m *GemMeta) FullName() string { return m.Name + "-" + m.versionPlatform() }

func (m *GemMeta) prerelease() bool { return isPrerelease(m.Version) }

func isPrerelease(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }) >= 0
}

func (m *GemMeta) runtimeDeps() []Dependency {
	var out []Dependency
	for _, d := range m.Dependencies {
		if d.Type != "development" {
			out = append(out, d)
		}
	}
	return out
}

// ── YAML gemspec parsing ──

var rubyTagRe = regexp.MustCompile(`(^|[\s:\-\[,])!ruby/[A-Za-z_]+(:[A-Za-z0-9_:]+)?`)

func stripRubyTags(s string) string { return rubyTagRe.ReplaceAllString(s, "$1") }

// rawScalar keeps the literal YAML text of a scalar so "1.10" is not
// turned into the float 1.1.
type rawScalar string

func (r *rawScalar) UnmarshalYAML(b []byte) error {
	s := strings.TrimSpace(string(b))
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		if s[0] == '"' {
			var out string
			if err := yaml.Unmarshal(b, &out); err == nil {
				s = out
			} else {
				s = s[1 : len(s)-1]
			}
		} else {
			s = strings.ReplaceAll(s[1:len(s)-1], "''", "'")
		}
	}
	if s == "~" || s == "null" {
		s = ""
	}
	*r = rawScalar(s)
	return nil
}

// reqItem is either the operator string or a {version: x} mapping.
type reqItem struct{ s string }

func (r *reqItem) UnmarshalYAML(b []byte) error {
	var v struct {
		Version rawScalar `yaml:"version"`
	}
	if strings.Contains(string(b), "version") && yaml.Unmarshal(b, &v) == nil && v.Version != "" {
		r.s = string(v.Version)
		return nil
	}
	var s rawScalar
	_ = s.UnmarshalYAML(b)
	r.s = string(s)
	return nil
}

type yamlRequirement struct {
	Requirements [][]reqItem `yaml:"requirements"`
}

func (y yamlRequirement) strings() []string {
	var out []string
	for _, pair := range y.Requirements {
		if len(pair) != 2 {
			continue
		}
		out = append(out, pair[0].s+" "+pair[1].s)
	}
	return out
}

// platformField accepts a platform string or a Gem::Platform mapping.
type platformField string

func (p *platformField) UnmarshalYAML(b []byte) error {
	var m struct {
		CPU     rawScalar `yaml:"cpu"`
		OS      rawScalar `yaml:"os"`
		Version rawScalar `yaml:"version"`
	}
	if strings.Contains(string(b), ":") && yaml.Unmarshal(b, &m) == nil && m.OS != "" {
		parts := []string{}
		for _, s := range []rawScalar{m.CPU, m.OS, m.Version} {
			if s != "" {
				parts = append(parts, string(s))
			}
		}
		*p = platformField(strings.Join(parts, "-"))
		return nil
	}
	var s rawScalar
	_ = s.UnmarshalYAML(b)
	*p = platformField(s)
	return nil
}

type yamlSpec struct {
	Name    string `yaml:"name"`
	Version struct {
		Version rawScalar `yaml:"version"`
	} `yaml:"version"`
	Platform     platformField  `yaml:"platform"`
	Summary      string         `yaml:"summary"`
	Description  string         `yaml:"description"`
	Homepage     string         `yaml:"homepage"`
	Authors      []string       `yaml:"authors"`
	Email        any            `yaml:"email"`
	Licenses     []string       `yaml:"licenses"`
	Metadata     map[string]any `yaml:"metadata"`
	Dependencies []struct {
		Name        string          `yaml:"name"`
		Requirement yamlRequirement `yaml:"requirement"`
		Type        string          `yaml:"type"`
	} `yaml:"dependencies"`
	RequiredRuby     yamlRequirement `yaml:"required_ruby_version"`
	RequiredRubygems yamlRequirement `yaml:"required_rubygems_version"`
	RubygemsVersion  rawScalar       `yaml:"rubygems_version"`
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if e != nil {
				out = append(out, fmt.Sprint(e))
			}
		}
		return out
	}
	return []string{fmt.Sprint(v)}
}

// parseGemspecYAML decodes a gemspec YAML document (Ruby tags ignored).
func parseGemspecYAML(doc []byte) (*GemMeta, error) {
	var y yamlSpec
	if err := yaml.Unmarshal([]byte(stripRubyTags(string(doc))), &y); err != nil {
		return nil, fmt.Errorf("parse gemspec: %w", err)
	}
	m := &GemMeta{
		Name:             strings.TrimSpace(y.Name),
		Version:          string(y.Version.Version),
		Platform:         string(y.Platform),
		Summary:          y.Summary,
		Description:      y.Description,
		Homepage:         y.Homepage,
		Authors:          y.Authors,
		Email:            toStrings(y.Email),
		Licenses:         y.Licenses,
		RequiredRuby:     y.RequiredRuby.strings(),
		RequiredRubygems: y.RequiredRubygems.strings(),
		RubygemsVersion:  string(y.RubygemsVersion),
	}
	if m.Platform == "ruby" {
		m.Platform = ""
	}
	if len(y.Metadata) > 0 {
		m.Metadata = map[string]string{}
		for k, v := range y.Metadata {
			if v != nil {
				m.Metadata[k] = fmt.Sprint(v)
			}
		}
	}
	for _, d := range y.Dependencies {
		typ := strings.TrimPrefix(strings.TrimSpace(d.Type), ":")
		if typ != "development" {
			typ = "runtime"
		}
		reqs := d.Requirement.strings()
		if len(reqs) == 0 {
			reqs = []string{">= 0"}
		}
		m.Dependencies = append(m.Dependencies, Dependency{Name: d.Name, Requirements: reqs, Type: typ})
	}
	sort.SliceStable(m.Dependencies, func(i, j int) bool { return m.Dependencies[i].Name < m.Dependencies[j].Name })
	if !validName(m.Name) {
		return nil, fmt.Errorf("invalid gem name %q", m.Name)
	}
	if !validVersion(m.Version) {
		return nil, fmt.Errorf("invalid gem version %q", m.Version)
	}
	if m.Platform != "" && !validVersion(m.Platform) {
		return nil, fmt.Errorf("invalid gem platform %q", m.Platform)
	}
	return m, nil
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)
)

func validName(s string) bool    { return s != "" && s != "." && s != ".." && nameRe.MatchString(s) }
func validVersion(s string) bool { return s != "" && s != "." && s != ".." && versionRe.MatchString(s) }

// parseGem extracts the gemspec from a .gem (POSIX tar) archive.
func parseGem(body []byte) (*GemMeta, error) {
	tr := tar.NewReader(bytes.NewReader(body))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read gem archive: %w", err)
		}
		switch h.Name {
		case "metadata.gz":
			zr, err := gzip.NewReader(tr)
			if err != nil {
				return nil, fmt.Errorf("metadata.gz: %w", err)
			}
			doc, err := io.ReadAll(io.LimitReader(zr, 16<<20))
			if err != nil {
				return nil, fmt.Errorf("metadata.gz: %w", err)
			}
			return parseGemspecYAML(doc)
		case "metadata":
			doc, err := io.ReadAll(io.LimitReader(tr, 16<<20))
			if err != nil {
				return nil, err
			}
			return parseGemspecYAML(doc)
		}
	}
	return nil, errors.New("gem archive has no metadata.gz")
}

// splitGemFile splits "{name}-{version}[-{platform}]" heuristically:
// the version starts at the first "-" followed by a digit.
func splitGemFile(full string) (name, version, platform string, ok bool) {
	for i := 1; i < len(full)-1; i++ {
		if full[i] == '-' && full[i+1] >= '0' && full[i+1] <= '9' {
			name, rest := full[:i], full[i+1:]
			version, platform, _ = strings.Cut(rest, "-")
			return name, version, platform, validName(name) && validVersion(version)
		}
	}
	return "", "", "", false
}
