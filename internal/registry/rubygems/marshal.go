package rubygems

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"sort"
	"strings"
)

// Minimal Ruby Marshal 4.8 encoder: just enough to emit the legacy
// index files (specs.4.8.gz & co), quick/Marshal.4.8 gemspecs and the
// Marshal variant of /api/v1/dependencies. The output only uses
// classes Gem::SafeMarshal permits.

type (
	rbSym     string
	rbBinary  []byte
	rbHash    [][2]any
	rbUserMar struct {
		Class string
		Data  any
	}
	rbUserDef struct {
		Class string
		Data  []byte
	}
	rbObject struct {
		Class string
		Ivars [][2]any // [rbSym("@name"), value]
	}
)

type marshaler struct {
	buf  bytes.Buffer
	syms map[string]int
}

func newMarshaler() *marshaler {
	m := &marshaler{syms: map[string]int{}}
	m.buf.Write([]byte{4, 8})
	return m
}

// rubyMarshal encodes v as a complete Marshal stream.
func rubyMarshal(v any) []byte {
	m := newMarshaler()
	m.encode(v)
	return m.buf.Bytes()
}

func (m *marshaler) long(n int) {
	switch {
	case n == 0:
		m.buf.WriteByte(0)
	case n > 0 && n < 123:
		m.buf.WriteByte(byte(n + 5))
	case n < 0 && n > -124:
		m.buf.WriteByte(byte((n - 5) & 0xff))
	default:
		var b [4]byte
		cnt := 0
		x := n
		for i := 0; i < 4; i++ {
			b[i] = byte(x & 0xff)
			x >>= 8
			cnt = i + 1
			if (n >= 0 && x == 0) || (n < 0 && x == -1) {
				break
			}
		}
		if n < 0 {
			m.buf.WriteByte(byte(-cnt))
		} else {
			m.buf.WriteByte(byte(cnt))
		}
		m.buf.Write(b[:cnt])
	}
}

func (m *marshaler) bytesRaw(b []byte) {
	m.long(len(b))
	m.buf.Write(b)
}

func (m *marshaler) symbol(s string) {
	if idx, ok := m.syms[s]; ok {
		m.buf.WriteByte(';')
		m.long(idx)
		return
	}
	m.syms[s] = len(m.syms)
	m.buf.WriteByte(':')
	m.bytesRaw([]byte(s))
}

func (m *marshaler) encode(v any) {
	switch x := v.(type) {
	case nil:
		m.buf.WriteByte('0')
	case bool:
		if x {
			m.buf.WriteByte('T')
		} else {
			m.buf.WriteByte('F')
		}
	case int:
		if x >= -(1<<30) && x < 1<<30 {
			m.buf.WriteByte('i')
			m.long(x)
		} else {
			m.encode(fmt.Sprint(x))
		}
	case string:
		// UTF-8 string: I"..." with ivar E=true.
		m.buf.WriteByte('I')
		m.buf.WriteByte('"')
		m.bytesRaw([]byte(x))
		m.long(1)
		m.symbol("E")
		m.buf.WriteByte('T')
	case rbBinary:
		m.buf.WriteByte('"')
		m.bytesRaw(x)
	case rbSym:
		m.symbol(string(x))
	case []string:
		m.buf.WriteByte('[')
		m.long(len(x))
		for _, e := range x {
			m.encode(e)
		}
	case []any:
		m.buf.WriteByte('[')
		m.long(len(x))
		for _, e := range x {
			m.encode(e)
		}
	case rbHash:
		m.buf.WriteByte('{')
		m.long(len(x))
		for _, kv := range x {
			m.encode(kv[0])
			m.encode(kv[1])
		}
	case rbUserMar:
		m.buf.WriteByte('U')
		m.symbol(x.Class)
		m.encode(x.Data)
	case rbUserDef:
		m.buf.WriteByte('u')
		m.symbol(x.Class)
		m.bytesRaw(x.Data)
	case rbObject:
		m.buf.WriteByte('o')
		m.symbol(x.Class)
		m.long(len(x.Ivars))
		for _, kv := range x.Ivars {
			m.symbol(string(kv[0].(rbSym)))
			m.encode(kv[1])
		}
	default:
		m.encode(fmt.Sprint(x))
	}
}

func gemVersionObj(v string) any { return rbUserMar{Class: "Gem::Version", Data: []any{v}} }

func splitReq(r string) (op, ver string) {
	r = strings.TrimSpace(r)
	for _, o := range []string{">=", "<=", "~>", "!=", ">", "<", "="} {
		if strings.HasPrefix(r, o) {
			return o, strings.TrimSpace(r[len(o):])
		}
	}
	return "=", r
}

func gemRequirementObj(reqs []string) any {
	if len(reqs) == 0 {
		reqs = []string{">= 0"}
	}
	list := make([]any, 0, len(reqs))
	for _, r := range reqs {
		op, ver := splitReq(r)
		list = append(list, []any{op, gemVersionObj(ver)})
	}
	return rbUserMar{Class: "Gem::Requirement", Data: []any{list}}
}

func strOrList(v []string) any {
	switch len(v) {
	case 0:
		return nil
	case 1:
		return v[0]
	}
	return v
}

// gemspecMarshal returns Marshal.dump(spec) for m, mirroring
// Gem::Specification#_dump. The date is emitted as nil (rubygems
// substitutes today's date).
func gemspecMarshal(m *GemMeta) []byte {
	deps := make([]any, 0, len(m.Dependencies))
	for _, d := range m.Dependencies {
		req := gemRequirementObj(d.Requirements)
		typ := strings.TrimPrefix(d.Type, ":")
		if typ != "development" {
			typ = "runtime"
		}
		deps = append(deps, rbObject{Class: "Gem::Dependency", Ivars: [][2]any{
			{rbSym("@name"), d.Name},
			{rbSym("@requirement"), req},
			{rbSym("@type"), rbSym(typ)},
			{rbSym("@prerelease"), false},
			{rbSym("@version_requirements"), req},
		}})
	}
	meta := rbHash{}
	keys := make([]string, 0, len(m.Metadata))
	for k := range m.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		meta = append(meta, [2]any{k, m.Metadata[k]})
	}
	rgv := m.RubygemsVersion
	if rgv == "" {
		rgv = "3.5.0"
	}
	authors := m.Authors
	if authors == nil {
		authors = []string{}
	}
	licenses := m.Licenses
	if licenses == nil {
		licenses = []string{}
	}
	inner := rubyMarshal([]any{
		rgv,
		4,
		m.Name,
		gemVersionObj(m.Version),
		nil,
		m.Summary,
		gemRequirementObj(m.RequiredRuby),
		gemRequirementObj(m.RequiredRubygems),
		m.platform(),
		deps,
		"",
		strOrList(m.Email),
		authors,
		m.Description,
		m.Homepage,
		true,
		m.platform(),
		licenses,
		meta,
	})
	return rubyMarshal(rbUserDef{Class: "Gem::Specification", Data: inner})
}

// quickSpec returns the zlib-deflated gemspec served at
// /quick/Marshal.4.8/{name}-{version}.gemspec.rz.
func quickSpec(m *GemMeta) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(gemspecMarshal(m))
	_ = zw.Close()
	return buf.Bytes()
}

// specsRaw returns the Marshal array of [name, Gem::Version, platform]
// tuples.
func specsRaw(metas []*GemMeta) []byte {
	list := make([]any, 0, len(metas))
	for _, m := range metas {
		list = append(list, []any{m.Name, gemVersionObj(m.Version), m.platform()})
	}
	return rubyMarshal(list)
}

// specsIndex is the gzipped specsRaw.
func specsIndex(metas []*GemMeta) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(specsRaw(metas))
	_ = zw.Close()
	return buf.Bytes()
}
