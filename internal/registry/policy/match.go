package policy

import (
	"path"
	"strings"
)

// NameAllowed reports whether name passes the include/exclude lists.
// Exclude always wins; a non-empty include list requires a match.
func NameAllowed(include, exclude []string, name string) bool {
	for _, p := range exclude {
		if MatchName(p, name) {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, p := range include {
		if MatchName(p, name) {
			return true
		}
	}
	return false
}

// MatchName matches a package name against a pattern. "**" matches any
// run of characters including "/"; everything else follows path.Match
// ("*" does not cross "/"). So "@acme/**" matches every package in the
// @acme scope and "com/acme/**" every name below com/acme/. Malformed
// patterns never match.
func MatchName(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "**" {
		return true
	}
	return globMatch(pattern, name)
}

func globMatch(pattern, name string) bool {
	if strings.HasPrefix(pattern, "**") {
		rest := strings.TrimLeft(pattern, "*")
		if rest == "" {
			return true
		}
		for i := 0; i <= len(name); i++ {
			if globMatch(rest, name[i:]) {
				return true
			}
		}
		return false
	}
	k := strings.Index(pattern, "**")
	if k < 0 {
		ok, err := path.Match(pattern, name)
		return err == nil && ok
	}
	head, tail := pattern[:k], pattern[k:]
	for i := 0; i <= len(name); i++ {
		if ok, err := path.Match(head, name[:i]); err == nil && ok && globMatch(tail, name[i:]) {
			return true
		}
	}
	return false
}
