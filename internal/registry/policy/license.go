package policy

import (
	"strings"
)

// LicenseExpr is a parsed SPDX license expression.
//
// Leaves carry a normalized (lower-case) license id; Op is "or" or
// "and" for inner nodes. "WITH <exception>" is folded into its license
// leaf (the exception is ignored for policy purposes).
type LicenseExpr struct {
	ID   string
	Op   string
	Args []*LicenseExpr
}

var licenseAliases = map[string]string{
	"apache 2.0":                      "apache-2.0",
	"apache 2":                        "apache-2.0",
	"apache-2":                        "apache-2.0",
	"apache2":                         "apache-2.0",
	"apache license 2.0":              "apache-2.0",
	"apache license, version 2.0":     "apache-2.0",
	"apache software license":         "apache-2.0",
	"the apache license, version 2.0": "apache-2.0",
	"the apache software license, version 2.0": "apache-2.0",
	"mit license":                "mit",
	"the mit license":            "mit",
	"bsd":                        "bsd-3-clause",
	"new bsd license":            "bsd-3-clause",
	"bsd license":                "bsd-3-clause",
	"isc license":                "isc",
	"mozilla public license 2.0": "mpl-2.0",
	"gplv2":                      "gpl-2.0",
	"gplv3":                      "gpl-3.0",
	"lgplv3":                     "lgpl-3.0",
}

func normalizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if a, ok := licenseAliases[s]; ok {
		return a
	}
	return s
}

// ParseLicense parses an SPDX expression ("MIT", "(MIT OR Apache-2.0)",
// "GPL-2.0-only WITH Classpath-exception-2.0", legacy "MIT/Apache-2.0").
// Strings that are not valid SPDX syntax (e.g. Maven's "The Apache
// Software License, Version 2.0") become a single alias-normalized
// leaf. Returns nil for empty / unknown licenses ("", "NOASSERTION",
// "SEE LICENSE IN ...").
func ParseLicense(s string) *LicenseExpr {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	up := strings.ToUpper(s)
	if up == "NOASSERTION" || up == "UNKNOWN" || strings.HasPrefix(up, "SEE LICENSE IN") {
		return nil
	}
	if p := (&licParser{toks: tokenizeLicense(s)}); len(p.toks) > 0 {
		if e, ok := p.parseOr(); ok && p.pos == len(p.toks) {
			return e
		}
	}
	return &LicenseExpr{ID: normalizeID(s)}
}

func tokenizeLicense(s string) []string {
	var toks []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch r {
		case '(', ')':
			flush()
			toks = append(toks, string(r))
		case '/':
			flush()
			toks = append(toks, "OR")
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

type licParser struct {
	toks []string
	pos  int
}

func (p *licParser) peekOp(op string) bool {
	return p.pos < len(p.toks) && strings.EqualFold(p.toks[p.pos], op)
}

func (p *licParser) parseOr() (*LicenseExpr, bool) {
	return p.parseBinary("or", p.parseAnd)
}

func (p *licParser) parseAnd() (*LicenseExpr, bool) {
	return p.parseBinary("and", p.parseWith)
}

func (p *licParser) parseBinary(op string, next func() (*LicenseExpr, bool)) (*LicenseExpr, bool) {
	first, ok := next()
	if !ok {
		return nil, false
	}
	args := []*LicenseExpr{first}
	for p.peekOp(op) {
		p.pos++
		e, ok := next()
		if !ok {
			return nil, false
		}
		args = append(args, e)
	}
	if len(args) == 1 {
		return first, true
	}
	return &LicenseExpr{Op: op, Args: args}, true
}

func (p *licParser) parseWith() (*LicenseExpr, bool) {
	e, ok := p.parseAtom()
	if !ok {
		return nil, false
	}
	if p.peekOp("with") {
		p.pos++
		if _, ok := p.parseAtom(); !ok {
			return nil, false
		}
	}
	return e, true
}

func (p *licParser) parseAtom() (*LicenseExpr, bool) {
	if p.pos >= len(p.toks) {
		return nil, false
	}
	t := p.toks[p.pos]
	switch {
	case t == "(":
		p.pos++
		e, ok := p.parseOr()
		if !ok || p.pos >= len(p.toks) || p.toks[p.pos] != ")" {
			return nil, false
		}
		p.pos++
		return e, true
	case t == ")" || p.peekOp("or") || p.peekOp("and") || p.peekOp("with"):
		return nil, false
	}
	p.pos++
	return &LicenseExpr{ID: normalizeID(t)}, true
}

// LicensePermitted reports whether some way of satisfying expr uses
// only licenses that are on the allowlist (when non-empty) and not on
// the denylist. Thus "MIT OR GPL-3.0" passes a GPL-3.0 denylist (the
// consumer can pick MIT) while "MIT AND GPL-3.0" does not. Ids are
// compared case-insensitively; a trailing "+" on a leaf is also
// matched without it.
func LicensePermitted(expr *LicenseExpr, allowed, denied []string) bool {
	if expr == nil {
		return true
	}
	switch expr.Op {
	case "or":
		for _, a := range expr.Args {
			if LicensePermitted(a, allowed, denied) {
				return true
			}
		}
		return false
	case "and":
		for _, a := range expr.Args {
			if !LicensePermitted(a, allowed, denied) {
				return false
			}
		}
		return true
	}
	if licenseIn(expr.ID, denied) {
		return false
	}
	return len(allowed) == 0 || licenseIn(expr.ID, allowed)
}

func licenseIn(id string, list []string) bool {
	base := strings.TrimSuffix(id, "+")
	for _, l := range list {
		n := normalizeID(l)
		if n == id || n == base {
			return true
		}
	}
	return false
}
