package docker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/blobstore"
)

const ociManifest = "application/vnd.oci.image.manifest.v1+json"

// pushImage pushes config + layer blobs and an image manifest tagged
// tag, returning the manifest body and digest.
func pushImage(t *testing.T, l *Local, name, tag, seed string) ([]byte, string, []blobstore.Digest) {
	t.Helper()
	cfg := pushBlob(t, l, name, []byte(`{"seed":"`+seed+`"}`))
	layer := pushBlob(t, l, name, []byte("layer-"+seed))
	body := []byte(`{"schemaVersion":2,"mediaType":"` + ociManifest + `",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cfg.String() + `","size":1},` +
		`"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + layer.String() + `","size":1}]}`)
	dgst := pushManifestBody(t, l, name, tag, body, ociManifest)
	return body, dgst, []blobstore.Digest{cfg, layer}
}

func signatureBody(subject string, subjectSize int, artifactType string) []byte {
	return []byte(`{"schemaVersion":2,"mediaType":"` + ociManifest + `","artifactType":"` + artifactType + `",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},` +
		`"layers":[],"subject":{"mediaType":"` + ociManifest + `","digest":"` + subject + `","size":` + jsonInt(subjectSize) + `}}`)
}

func TestDockerLocal_ListPackagesDeleteVersion(t *testing.T) {
	l := newDockerLocal(t, true)
	pushImage(t, l, "lib/a", "v1", "a1")
	pushImage(t, l, "lib/a", "v2", "a2")
	pushImage(t, l, "lib/b", "latest", "b")
	ctx := context.Background()

	pkgs, err := l.ListPackages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || pkgs[0].Name != "lib/a" || strings.Join(pkgs[0].Versions, ",") != "v1,v2" {
		t.Fatalf("pkgs %+v", pkgs)
	}
	if err := l.DeleteVersion(ctx, "lib/a", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := l.DeleteVersion(ctx, "lib/a", "v1"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	w := do(l, http.MethodGet, "/v2/lib/a/tags/list", nil, nil)
	if strings.Contains(w.Body.String(), `"v1"`) {
		t.Fatalf("tags after delete: %s", w.Body.String())
	}
}

func TestDockerClassifyRequest(t *testing.T) {
	l := newDockerLocal(t, false)
	dg := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		method, path, name, version string
		ok                          bool
	}{
		{http.MethodGet, "/v2/lib/foo/manifests/v1.2", "lib/foo", "v1.2", true},
		{http.MethodHead, "/v2/lib/foo/manifests/" + dg, "lib/foo", dg, true},
		{http.MethodGet, "/v2/lib/foo/blobs/" + dg, "lib/foo", "", true},
		{http.MethodGet, "/v2/lib/foo/tags/list", "lib/foo", "", true},
		{http.MethodGet, "/v2/", "", "", false},
		{http.MethodGet, "/v2/_catalog", "", "", false},
		{http.MethodGet, "/nope", "", "", false},
	}
	for _, c := range cases {
		ref, ok := l.ClassifyRequest(httptest.NewRequest(c.method, c.path, nil))
		if ok != c.ok || ref.Name != c.name || ref.Version != c.version {
			t.Errorf("%s %s: %+v %v", c.method, c.path, ref, ok)
		}
	}
}

func TestDockerLocal_HasSignature(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		sign func(t *testing.T, l *Local, subject string, size int)
		want bool
	}{
		{"unsigned", func(*testing.T, *Local, string, int) {}, false},
		{"cosign-referrer", func(t *testing.T, l *Local, s string, n int) {
			b := signatureBody(s, n, "application/vnd.dev.cosign.artifact.sig.v1+json")
			pushManifestBody(t, l, "lib/foo", digestOf(b), b, ociManifest)
		}, true},
		{"notation-referrer", func(t *testing.T, l *Local, s string, n int) {
			b := signatureBody(s, n, "application/vnd.cncf.notary.signature")
			pushManifestBody(t, l, "lib/foo", digestOf(b), b, ociManifest)
		}, true},
		{"sbom-only", func(t *testing.T, l *Local, s string, n int) {
			b := signatureBody(s, n, "application/spdx+json")
			pushManifestBody(t, l, "lib/foo", digestOf(b), b, ociManifest)
		}, false},
		{"cosign-tag", func(t *testing.T, l *Local, s string, _ int) {
			b := []byte(`{"schemaVersion":2,"layers":[]}`)
			pushManifestBody(t, l, "lib/foo", strings.Replace(s, ":", "-", 1)+".sig", b, ociManifest)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newDockerLocal(t, true)
			body, dgst, _ := pushImage(t, l, "lib/foo", "v1", "x")
			tc.sign(t, l, dgst, len(body))
			for _, ref := range []string{"v1", dgst} {
				got, err := l.HasSignature(ctx, "lib/foo", ref)
				if err != nil || got != tc.want {
					t.Fatalf("ref %s: got %v err %v, want %v", ref, got, err, tc.want)
				}
			}
		})
	}
	l := newDockerLocal(t, true)
	if _, err := l.HasSignature(ctx, "lib/foo", "missing"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing tag: %v", err)
	}
}

func TestDockerLocal_PromoteVersion(t *testing.T) {
	ctx := context.Background()
	src := newDockerLocal(t, true)
	dst := newDockerLocal(t, true)
	name := "lib/foo"

	body, dgst, blobs := pushImage(t, src, name, "v1", "p")
	sig := signatureBody(dgst, len(body), "application/vnd.dev.cosign.artifact.sig.v1+json")
	sigDigest := pushManifestBody(t, src, name, digestOf(sig), sig, ociManifest)
	tagSig := []byte(`{"schemaVersion":2,"layers":[],"annotations":{"k":"v"}}`)
	sigTag := strings.Replace(dgst, ":", "-", 1) + ".sig"
	pushManifestBody(t, src, name, sigTag, tagSig, ociManifest)

	// Multi-arch index over two child manifests.
	_, child1, _ := pushImage(t, src, name, "c1", "c1")
	_, child2, child2Blobs := pushImage(t, src, name, "c2", "c2")
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"mediaType":"` + ociManifest + `","digest":"` + child1 + `","size":1,"platform":{"os":"linux","architecture":"amd64"}},` +
		`{"mediaType":"` + ociManifest + `","digest":"` + child2 + `","size":1,"platform":{"os":"linux","architecture":"arm64"}}]}`)
	indexDigest := pushManifestBody(t, src, name, "multi", index, "application/vnd.oci.image.index.v1+json")

	if err := src.PromoteVersion(ctx, dst, name, "v1"); err != nil {
		t.Fatal(err)
	}
	w := do(dst, http.MethodGet, "/v2/"+name+"/manifests/v1", nil, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("dst manifest: %d", w.Code)
	}
	for _, b := range blobs {
		if _, err := dst.store.Blobs().Stat(b); err != nil {
			t.Fatalf("blob %s not copied: %v", b, err)
		}
	}
	if w := do(dst, http.MethodGet, "/v2/"+name+"/manifests/"+sigDigest, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("referrer manifest not copied: %d", w.Code)
	}
	if w := do(dst, http.MethodGet, "/v2/"+name+"/manifests/"+sigTag, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("cosign tag not copied: %d", w.Code)
	}
	if has, err := dst.HasSignature(ctx, name, "v1"); err != nil || !has {
		t.Fatalf("dst signature: %v %v", has, err)
	}

	if err := src.PromoteVersion(ctx, dst, name, "multi"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{indexDigest, child1, child2} {
		if w := do(dst, http.MethodGet, "/v2/"+name+"/manifests/"+d, nil, nil); w.Code != http.StatusOK {
			t.Fatalf("manifest %s not copied: %d", d, w.Code)
		}
	}
	for _, b := range child2Blobs {
		if _, err := dst.store.Blobs().Stat(b); err != nil {
			t.Fatalf("child blob %s not copied", b)
		}
	}
	tags, _ := dst.store.ListTags(name)
	if strings.Join(tags, ",") != "multi,"+sigTag+",v1" && strings.Join(tags, ",") != sigTag+",multi,v1" {
		t.Fatalf("dst tags %v", tags)
	}

	if err := src.PromoteVersion(ctx, dst, name, "nope"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := src.PromoteVersion(ctx, &Virtual{}, name, "v1"); err == nil {
		t.Fatal("expected error for non-local dst")
	}
}

func TestDockerRemote_PrefetchAndSignature(t *testing.T) {
	fu := newFakeDockerUpstream()
	defer fu.Close()
	name := "lib/foo"
	cfg := []byte(`{"cfg":1}`)
	layer := []byte("layer-bytes")
	cfgD, layerD := digestOf(cfg), digestOf(layer)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"` + ociManifest + `",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cfgD + `","size":1},` +
		`"layers":[{"digest":"` + layerD + `","size":1}]}`)
	mD := digestOf(manifest)
	fu.ServeManifest(name, "v1", ociManifest, manifest)
	fu.ServeManifest(name, mD, ociManifest, manifest)
	fu.ServeBlob(name, cfgD, cfg)
	fu.ServeBlob(name, layerD, layer)

	unsigned := []byte(`{"schemaVersion":2,"layers":[],"x":1}`)
	uD := digestOf(unsigned)
	fu.ServeManifest(name, "v2", ociManifest, unsigned)
	fu.ServeManifest(name, uD, ociManifest, unsigned)

	sigTagManifest := []byte(`{"schemaVersion":2,"layers":[],"sig":1}`)
	fu.ServeManifest(name, strings.Replace(mD, ":", "-", 1)+".sig", ociManifest, sigTagManifest)

	rr := newDockerRemote(t, fu.URL())
	ctx := context.Background()

	if err := rr.Prefetch(ctx, name, "v1"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cfgD, layerD} {
		pd, _ := blobstore.ParseDigest(d)
		if _, err := rr.store.Blobs().Stat(pd); err != nil {
			t.Fatalf("blob %s not cached: %v", d, err)
		}
	}
	pkgs, err := rr.ListPackages(ctx)
	if err != nil || len(pkgs) != 1 || pkgs[0].Versions[0] != "v1" {
		t.Fatalf("pkgs %+v %v", pkgs, err)
	}
	if err := rr.Prefetch(ctx, name, "missing"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("missing prefetch: %v", err)
	}

	has, err := rr.HasSignature(ctx, name, "v1")
	if err != nil || !has {
		t.Fatalf("v1 signature (cosign tag fallback): %v %v", has, err)
	}
	has, err = rr.HasSignature(ctx, name, "v2")
	if err != nil || has {
		t.Fatalf("v2 signature: %v %v", has, err)
	}
	if ref, ok := rr.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/v2/"+name+"/manifests/v1", nil)); !ok || ref.Version != "v1" {
		t.Fatalf("classify %+v", ref)
	}
}

func TestDockerRemote_HasSignatureUpstreamReferrers(t *testing.T) {
	fu := newFakeDockerUpstream()
	defer fu.Close()
	name := "lib/foo"
	manifest := []byte(`{"schemaVersion":2,"layers":[]}`)
	mD := digestOf(manifest)
	fu.ServeManifest(name, "v1", ociManifest, manifest)
	fu.mux.HandleFunc("/v2/"+name+"/referrers/"+mD, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		_, _ = w.Write([]byte(`{"schemaVersion":2,"manifests":[{"mediaType":"` + ociManifest +
			`","artifactType":"application/vnd.cncf.notary.signature","digest":"sha256:` + strings.Repeat("b", 64) + `","size":1}]}`))
	})
	rr := newDockerRemote(t, fu.URL())
	has, err := rr.HasSignature(context.Background(), name, "v1")
	if err != nil || !has {
		t.Fatalf("got %v %v", has, err)
	}
}

func TestDockerVirtual_ListPackagesAndSignature(t *testing.T) {
	ctx := context.Background()
	a := newDockerLocal(t, true)
	b := newDockerLocal(t, true)
	pushImage(t, a, "lib/foo", "v1", "a")
	body, dgst, _ := pushImage(t, b, "lib/foo", "v2", "b")
	sig := signatureBody(dgst, len(body), "application/vnd.dev.cosign.artifact.sig.v1+json")
	pushManifestBody(t, b, "lib/foo", digestOf(sig), sig, ociManifest)

	v := newVirtual(t, []string{"a", "b"}, &stubResolver{regs: map[string]registry.Registry{"a": a, "b": b}})
	pkgs, err := v.ListPackages(ctx)
	if err != nil || len(pkgs) != 1 || strings.Join(pkgs[0].Versions, ",") != "v1,v2" {
		t.Fatalf("pkgs %+v %v", pkgs, err)
	}
	if has, err := v.HasSignature(ctx, "lib/foo", "v2"); err != nil || !has {
		t.Fatalf("v2: %v %v", has, err)
	}
	if has, err := v.HasSignature(ctx, "lib/foo", "v1"); err != nil || has {
		t.Fatalf("v1: %v %v", has, err)
	}
	if _, err := v.HasSignature(ctx, "lib/foo", "ghost"); !errors.Is(err, registry.ErrPackageNotFound) {
		t.Fatalf("ghost: %v", err)
	}
	if ref, ok := v.ClassifyRequest(httptest.NewRequest(http.MethodGet, "/v2/lib/foo/manifests/v2", nil)); !ok || ref.Name != "lib/foo" {
		t.Fatalf("classify %+v", ref)
	}
}
