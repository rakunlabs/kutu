package pkgbase

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// CompareVersions is a tolerant, ecosystem-agnostic version
// comparator: numeric runs compare numerically, alpha runs
// lexically, and a pre-release suffix ("-rc1", ".beta") sorts below
// the bare release. Good enough for "newest first" across
// nuget/gem/deb/rpm style strings; not a constraint solver.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}
	a = strings.TrimPrefix(strings.TrimPrefix(a, "v"), "V")
	b = strings.TrimPrefix(strings.TrimPrefix(b, "v"), "V")
	if i := strings.IndexByte(a, '+'); i >= 0 {
		a = a[:i]
	}
	if i := strings.IndexByte(b, '+'); i >= 0 {
		b = b[:i]
	}
	ac, apre := splitPre(a)
	bc, bpre := splitPre(b)
	if c := compareTokens(tokenize(ac), tokenize(bc)); c != 0 {
		return c
	}
	switch {
	case apre == "" && bpre == "":
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	}
	return compareTokens(tokenize(apre), tokenize(bpre))
}

func splitPre(v string) (string, string) {
	if i := strings.IndexByte(v, '-'); i > 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

type token struct {
	num bool
	n   int64
	s   string
}

func tokenize(v string) []token {
	var out []token
	var cur strings.Builder
	curNum := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		t := token{num: curNum, s: cur.String()}
		if curNum {
			t.n, _ = strconv.ParseInt(t.s, 10, 64)
		}
		out = append(out, t)
		cur.Reset()
	}
	for _, r := range v {
		switch {
		case unicode.IsDigit(r):
			if cur.Len() > 0 && !curNum {
				flush()
			}
			curNum = true
			cur.WriteRune(r)
		case unicode.IsLetter(r):
			if cur.Len() > 0 && curNum {
				flush()
			}
			curNum = false
			cur.WriteRune(unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
	return out
}

func compareTokens(a, b []token) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		if i >= len(a) {
			if !b[i].num {
				return 1
			}
			return -1
		}
		if i >= len(b) {
			if !a[i].num {
				return -1
			}
			return 1
		}
		x, y := a[i], b[i]
		switch {
		case x.num && y.num:
			if x.n != y.n {
				if x.n < y.n {
					return -1
				}
				return 1
			}
		case x.num:
			return 1
		case y.num:
			return -1
		default:
			if c := strings.Compare(x.s, y.s); c != 0 {
				return c
			}
		}
	}
	return 0
}

// SortVersions sorts ascending (oldest first).
func SortVersions(vs []string) {
	sort.SliceStable(vs, func(i, j int) bool { return CompareVersions(vs[i], vs[j]) < 0 })
}

// Latest returns the highest version (or "").
func Latest(vs []string) string {
	var best string
	for _, v := range vs {
		if best == "" || CompareVersions(v, best) > 0 {
			best = v
		}
	}
	return best
}
