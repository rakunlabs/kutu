package nuget

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxNuspec caps the size of a .nuspec extracted from a package.
const maxNuspec = 10 << 20

// deprecatedLicenseURL is the placeholder licenseUrl NuGet writes when
// a package declares <license>.
const deprecatedLicenseURL = "https://aka.ms/deprecateLicenseUrl"

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_]+([.-][A-Za-z0-9_]+)*$`)

var prereleasePattern = regexp.MustCompile(`^[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*$`)

// ValidID reports whether id is a valid NuGet package id.
func ValidID(id string) bool { return len(id) <= 100 && idPattern.MatchString(id) }

// NormalizeVersion applies NuGet version normalization: build metadata
// is dropped, numeric parts lose leading zeros, missing minor/patch
// become 0 and a zero fourth (revision) part is removed. The
// pre-release label keeps its case.
func NormalizeVersion(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	core, pre := v, ""
	if i := strings.IndexByte(v, '-'); i >= 0 {
		core, pre = v[:i], v[i+1:]
		if !prereleasePattern.MatchString(pre) {
			return "", false
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) > 4 {
		return "", false
	}
	nums := make([]string, 0, 4)
	for _, p := range parts {
		if p == "" {
			return "", false
		}
		n, err := strconv.ParseUint(p, 10, 63)
		if err != nil {
			return "", false
		}
		nums = append(nums, strconv.FormatUint(n, 10))
	}
	for len(nums) < 3 {
		nums = append(nums, "0")
	}
	if len(nums) == 4 && nums[3] == "0" {
		nums = nums[:3]
	}
	out := strings.Join(nums, ".")
	if pre != "" {
		out += "-" + pre
	}
	return out, true
}

// lowerVersion normalizes and lowercases v for use in paths.
func lowerVersion(v string) (string, bool) {
	n, ok := NormalizeVersion(v)
	return strings.ToLower(n), ok
}

func isPrerelease(v string) bool { return strings.Contains(v, "-") }

type nuspecDependency struct {
	ID      string `xml:"id,attr"`
	Version string `xml:"version,attr"`
	Exclude string `xml:"exclude,attr"`
}

type nuspecGroup struct {
	TargetFramework string             `xml:"targetFramework,attr"`
	Dependencies    []nuspecDependency `xml:"dependency"`
}

type nuspecLicense struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

type nuspecDoc struct {
	Metadata struct {
		MinClientVersion         string        `xml:"minClientVersion,attr"`
		ID                       string        `xml:"id"`
		Version                  string        `xml:"version"`
		Title                    string        `xml:"title"`
		Authors                  string        `xml:"authors"`
		Owners                   string        `xml:"owners"`
		Description              string        `xml:"description"`
		Summary                  string        `xml:"summary"`
		ReleaseNotes             string        `xml:"releaseNotes"`
		Copyright                string        `xml:"copyright"`
		Language                 string        `xml:"language"`
		Tags                     string        `xml:"tags"`
		ProjectURL               string        `xml:"projectUrl"`
		IconURL                  string        `xml:"iconUrl"`
		LicenseURL               string        `xml:"licenseUrl"`
		License                  nuspecLicense `xml:"license"`
		RequireLicenseAcceptance string        `xml:"requireLicenseAcceptance"`
		Dependencies             struct {
			Groups       []nuspecGroup      `xml:"group"`
			Dependencies []nuspecDependency `xml:"dependency"`
		} `xml:"dependencies"`
	} `xml:"metadata"`
}

// Dependency is one dependency of a package version.
type Dependency struct {
	ID    string `json:"id"`
	Range string `json:"range,omitempty"`
}

// DependencyGroup groups dependencies by target framework.
type DependencyGroup struct {
	TargetFramework string       `json:"targetFramework,omitempty"`
	Dependencies    []Dependency `json:"dependencies,omitempty"`
}

// Meta is the per-version metadata kutu stores next to each nupkg.
type Meta struct {
	ID                       string            `json:"id"`
	Version                  string            `json:"version"`
	Title                    string            `json:"title,omitempty"`
	Authors                  string            `json:"authors,omitempty"`
	Owners                   string            `json:"owners,omitempty"`
	Description              string            `json:"description,omitempty"`
	Summary                  string            `json:"summary,omitempty"`
	ReleaseNotes             string            `json:"releaseNotes,omitempty"`
	Copyright                string            `json:"copyright,omitempty"`
	Language                 string            `json:"language,omitempty"`
	Tags                     []string          `json:"tags,omitempty"`
	ProjectURL               string            `json:"projectUrl,omitempty"`
	IconURL                  string            `json:"iconUrl,omitempty"`
	LicenseURL               string            `json:"licenseUrl,omitempty"`
	LicenseExpression        string            `json:"licenseExpression,omitempty"`
	RequireLicenseAcceptance bool              `json:"requireLicenseAcceptance,omitempty"`
	MinClientVersion         string            `json:"minClientVersion,omitempty"`
	DependencyGroups         []DependencyGroup `json:"dependencyGroups,omitempty"`
	Listed                   bool              `json:"listed"`
	Published                time.Time         `json:"published,omitzero"`
	Size                     int64             `json:"size,omitempty"`
	SHA512                   string            `json:"sha512,omitempty"`
}

// License returns the SPDX expression, or the license URL when the
// package predates license expressions.
func (m *Meta) License() string {
	if m.LicenseExpression != "" {
		return m.LicenseExpression
	}
	if m.LicenseURL != "" && m.LicenseURL != deprecatedLicenseURL {
		return m.LicenseURL
	}
	return ""
}

// ExtractNuspec returns the raw .nuspec bytes at the root of a nupkg.
func ExtractNuspec(nupkg []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(nupkg), int64(len(nupkg)))
	if err != nil {
		return nil, fmt.Errorf("invalid nupkg: %w", err)
	}
	for _, f := range zr.File {
		if strings.Contains(f.Name, "/") || !strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, maxNuspec+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxNuspec {
			return nil, errors.New("nuspec too large")
		}
		return b, nil
	}
	return nil, errors.New("nupkg contains no .nuspec")
}

// ParseNuspec decodes a .nuspec into Meta (id + normalized version
// are validated).
func ParseNuspec(raw []byte) (*Meta, error) {
	var doc nuspecDoc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid nuspec: %w", err)
	}
	md := doc.Metadata
	id := strings.TrimSpace(md.ID)
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid package id %q", id)
	}
	ver, ok := NormalizeVersion(md.Version)
	if !ok {
		return nil, fmt.Errorf("invalid package version %q", md.Version)
	}
	m := &Meta{
		ID:               id,
		Version:          ver,
		Title:            strings.TrimSpace(md.Title),
		Authors:          strings.TrimSpace(md.Authors),
		Owners:           strings.TrimSpace(md.Owners),
		Description:      strings.TrimSpace(md.Description),
		Summary:          strings.TrimSpace(md.Summary),
		ReleaseNotes:     strings.TrimSpace(md.ReleaseNotes),
		Copyright:        strings.TrimSpace(md.Copyright),
		Language:         strings.TrimSpace(md.Language),
		Tags:             splitTags(md.Tags),
		ProjectURL:       strings.TrimSpace(md.ProjectURL),
		IconURL:          strings.TrimSpace(md.IconURL),
		LicenseURL:       strings.TrimSpace(md.LicenseURL),
		MinClientVersion: md.MinClientVersion,
		Listed:           true,
	}
	m.RequireLicenseAcceptance, _ = strconv.ParseBool(strings.TrimSpace(md.RequireLicenseAcceptance))
	if strings.EqualFold(md.License.Type, "expression") {
		m.LicenseExpression = strings.TrimSpace(md.License.Value)
	}
	for _, g := range md.Dependencies.Groups {
		m.DependencyGroups = append(m.DependencyGroups, DependencyGroup{
			TargetFramework: strings.TrimSpace(g.TargetFramework),
			Dependencies:    convertDeps(g.Dependencies),
		})
	}
	if len(md.Dependencies.Dependencies) > 0 {
		m.DependencyGroups = append(m.DependencyGroups, DependencyGroup{Dependencies: convertDeps(md.Dependencies.Dependencies)})
	}
	return m, nil
}

func convertDeps(in []nuspecDependency) []Dependency {
	out := make([]Dependency, 0, len(in))
	for _, d := range in {
		if strings.TrimSpace(d.ID) == "" {
			continue
		}
		out = append(out, Dependency{ID: strings.TrimSpace(d.ID), Range: strings.TrimSpace(d.Version)})
	}
	return out
}

func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ';' || r == '\t' || r == '\n' })
}
