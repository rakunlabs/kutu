package rubygems

import (
	"net/http"
	"sort"
	"strings"

	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
)

// Virtual merges the compact index across members; everything else is
// first-hit.
type Virtual struct {
	*pkgbase.Virtual
}

func (v *Virtual) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !pkgbase.IsRead(r) {
		pkgbase.Error(w, http.StatusMethodNotAllowed, "virtual repositories are read-only; publish to a local member")
		return
	}
	p := r.URL.Path
	switch {
	case p == "/versions":
		serveCompact(w, r, v.mergedVersions(r))
	case p == "/names":
		serveCompact(w, r, buildNames(v.mergedNames(r)))
	case strings.HasPrefix(p, "/info/"):
		if _, ok := singleSegment(p, "/info/", ""); !ok {
			pkgbase.NotFound(w)
			return
		}
		bodies := v.collect(r)
		if len(bodies) == 0 {
			pkgbase.NotFound(w)
			return
		}
		serveCompact(w, r, mergeInfo(bodies))
	default:
		if !v.ServeFirstHit(w, r) {
			pkgbase.NotFound(w)
		}
	}
}

// collect returns member 200 bodies; conditional / range headers are
// stripped so every member answers with its full document.
func (v *Virtual) collect(r *http.Request) [][]byte {
	r2 := r.Clone(r.Context())
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "Range", "If-Range"} {
		r2.Header.Del(h)
	}
	r2.Method = http.MethodGet
	return v.CollectMembers(r2)
}

// mergeInfo unions info lines, first member wins per version-platform.
func mergeInfo(bodies [][]byte) []byte {
	seen := map[string]bool{}
	var lines []string
	for _, b := range bodies {
		for _, line := range infoLines(b) {
			tok, _, _ := strings.Cut(line, " ")
			if tok == "" || seen[tok] {
				continue
			}
			seen[tok] = true
			lines = append(lines, line)
		}
	}
	var out strings.Builder
	out.WriteString("---\n")
	for _, l := range lines {
		out.WriteString(l)
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

// parseVersionsFile returns name → version tokens (appended entries
// for the same name are concatenated; "-v" removals applied).
func parseVersionsFile(b []byte, into map[string][]string, order *[]string, md5s map[string]string) {
	started := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if !started {
			if line == "---" {
				started = true
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if _, ok := into[name]; !ok {
			*order = append(*order, name)
			into[name] = nil
		}
		for _, tok := range strings.Split(fields[1], ",") {
			if strings.HasPrefix(tok, "-") {
				into[name] = removeToken(into[name], tok[1:])
				continue
			}
			if tok != "" {
				into[name] = append(into[name], tok)
			}
		}
		if len(fields) >= 3 && md5s != nil {
			md5s[name] = fields[2]
		}
	}
}

func removeToken(list []string, tok string) []string {
	out := list[:0]
	for _, t := range list {
		if t != tok {
			out = append(out, t)
		}
	}
	return out
}

// mergedVersions combines member /versions files. Names present in a
// single member keep that member's md5; names present in several are
// re-hashed over the merged /info document the virtual serves.
func (v *Virtual) mergedVersions(r *http.Request) []byte {
	bodies := v.collect(r)
	type perMember struct {
		versions map[string][]string
		md5s     map[string]string
	}
	var members []perMember
	count := map[string]int{}
	var order []string
	seenOrder := map[string]bool{}
	for _, b := range bodies {
		pm := perMember{versions: map[string][]string{}, md5s: map[string]string{}}
		var o []string
		parseVersionsFile(b, pm.versions, &o, pm.md5s)
		for _, n := range o {
			if len(pm.versions[n]) == 0 {
				continue
			}
			count[n]++
			if !seenOrder[n] {
				seenOrder[n] = true
				order = append(order, n)
			}
		}
		members = append(members, pm)
	}
	sort.Strings(order)
	entries := map[string]versionsEntry{}
	for _, n := range order {
		if count[n] == 1 {
			for _, pm := range members {
				if vs := pm.versions[n]; len(vs) > 0 {
					entries[n] = versionsEntry{versions: vs, md5: pm.md5s[n]}
				}
			}
			continue
		}
		ir := r.Clone(r.Context())
		ir.URL.Path = "/info/" + n
		ir.URL.RawPath = ""
		ir.URL.RawQuery = ""
		infos := v.collect(ir)
		if len(infos) == 0 {
			continue
		}
		merged := mergeInfo(infos)
		entries[n] = versionsEntry{versions: infoVersions(merged), md5: md5Hex(merged)}
	}
	return buildVersions(order, entries)
}

func (v *Virtual) mergedNames(r *http.Request) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range v.collect(r) {
		started := false
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if !started {
				if line == "---" {
					started = true
				}
				continue
			}
			if line != "" && !seen[line] {
				seen[line] = true
				out = append(out, line)
			}
		}
	}
	sort.Strings(out)
	return out
}
