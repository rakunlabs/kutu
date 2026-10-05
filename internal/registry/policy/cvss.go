package policy

import (
	"math"
	"strings"
)

// Severity buckets, ordered.
const (
	SeverityUnknown  = "unknown"
	SeverityLow      = "low"
	SeverityModerate = "moderate"
	SeverityHigh     = "high"
	SeverityCritical = "critical"
)

func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "low":
		return 1
	case "moderate", "medium":
		return 2
	case "high":
		return 3
	case "critical":
		return 4
	}
	return 0
}

// NormalizeSeverity maps free-form severity labels to the buckets
// ("medium" → "moderate"); unrecognized values become "unknown".
func NormalizeSeverity(s string) string {
	switch severityRank(s) {
	case 1:
		return SeverityLow
	case 2:
		return SeverityModerate
	case 3:
		return SeverityHigh
	case 4:
		return SeverityCritical
	}
	return SeverityUnknown
}

// SeverityFromScore buckets a CVSS base score.
func SeverityFromScore(score float64) string {
	switch {
	case score >= 9:
		return SeverityCritical
	case score >= 7:
		return SeverityHigh
	case score >= 4:
		return SeverityModerate
	case score > 0:
		return SeverityLow
	}
	return SeverityUnknown
}

// CVSS3BaseScore computes the CVSS v3.0/v3.1 base score of a vector
// such as "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H". ok=false for
// non-v3 or malformed vectors.
func CVSS3BaseScore(vector string) (float64, bool) {
	parts := strings.Split(strings.TrimSpace(vector), "/")
	if len(parts) == 0 || !strings.HasPrefix(parts[0], "CVSS:3.") {
		return 0, false
	}
	m := make(map[string]string, 8)
	for _, p := range parts[1:] {
		k, v, ok := strings.Cut(p, ":")
		if !ok {
			return 0, false
		}
		m[k] = v
	}
	changed := false
	switch m["S"] {
	case "U":
	case "C":
		changed = true
	default:
		return 0, false
	}
	av, ok1 := pick(m["AV"], map[string]float64{"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.2})
	ac, ok2 := pick(m["AC"], map[string]float64{"L": 0.77, "H": 0.44})
	prTable := map[string]float64{"N": 0.85, "L": 0.62, "H": 0.27}
	if changed {
		prTable = map[string]float64{"N": 0.85, "L": 0.68, "H": 0.5}
	}
	pr, ok3 := pick(m["PR"], prTable)
	ui, ok4 := pick(m["UI"], map[string]float64{"N": 0.85, "R": 0.62})
	cia := map[string]float64{"H": 0.56, "L": 0.22, "N": 0}
	c, ok5 := pick(m["C"], cia)
	i, ok6 := pick(m["I"], cia)
	a, ok7 := pick(m["A"], cia)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6 && ok7) {
		return 0, false
	}
	iss := 1 - (1-c)*(1-i)*(1-a)
	var impact float64
	if changed {
		impact = 7.52*(iss-0.029) - 3.25*math.Pow(iss-0.02, 15)
	} else {
		impact = 6.42 * iss
	}
	if impact <= 0 {
		return 0, true
	}
	expl := 8.22 * av * ac * pr * ui
	if changed {
		return roundUp(math.Min(1.08*(impact+expl), 10)), true
	}
	return roundUp(math.Min(impact+expl, 10)), true
}

func pick(v string, table map[string]float64) (float64, bool) {
	f, ok := table[v]
	return f, ok
}

// roundUp is the CVSS v3.1 Roundup function.
func roundUp(x float64) float64 {
	n := int64(math.Round(x * 100000))
	if n%10000 == 0 {
		return float64(n) / 100000
	}
	return float64(n/10000+1) / 10
}
