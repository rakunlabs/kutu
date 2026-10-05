package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/rakunlabs/kutu/internal/hook"
	"github.com/rakunlabs/kutu/internal/registry"
	"github.com/rakunlabs/kutu/internal/registry/blobstore"
	"github.com/rakunlabs/kutu/internal/registry/events"
	"github.com/rakunlabs/kutu/internal/registry/pkgbase"
	"github.com/rakunlabs/kutu/internal/registry/upstream"
)

var (
	_ registry.PackageLister      = (*Local)(nil)
	_ registry.VersionDeleter     = (*Local)(nil)
	_ registry.ArtifactClassifier = (*Local)(nil)
	_ registry.VersionPromoter    = (*Local)(nil)
	_ registry.SignatureChecker   = (*Local)(nil)
	_ registry.PackageLister      = (*Remote)(nil)
	_ registry.ArtifactClassifier = (*Remote)(nil)
	_ registry.Prefetcher         = (*Remote)(nil)
	_ registry.SignatureChecker   = (*Remote)(nil)
	_ registry.PackageLister      = (*Virtual)(nil)
	_ registry.ArtifactClassifier = (*Virtual)(nil)
	_ registry.SignatureChecker   = (*Virtual)(nil)
)

// signatureArtifactTypes are the referrer artifact types that count
// as a signature (cosign OCI 1.1 / simplesigning, notation).
var signatureArtifactTypes = map[string]struct{}{
	"application/vnd.dev.cosign.artifact.sig.v1+json":  {},
	"application/vnd.dev.cosign.simplesigning.v1+json": {},
	"application/vnd.cncf.notary.signature":            {},
}

// cosignTagSuffixes are the tag-based fallback artifacts cosign
// attaches next to an image ("sha256-<hex>.sig" etc.).
var cosignTagSuffixes = []string{".sig", ".att", ".sbom"}

func cosignTag(d blobstore.Digest, suffix string) string {
	return d.Algorithm + "-" + d.Hex + suffix
}

// manifestRefs is the subset of a manifest needed to walk its graph.
type manifestRefs struct {
	Config *struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"config,omitempty"`
	Layers []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
	} `json:"layers,omitempty"`
	Blobs []struct {
		Digest string `json:"digest"`
	} `json:"blobs,omitempty"`
	Manifests []struct {
		Digest string `json:"digest"`
	} `json:"manifests,omitempty"`
}

func parseManifestRefs(body []byte) (children, blobs []blobstore.Digest) {
	var m manifestRefs
	if json.Unmarshal(body, &m) != nil {
		return nil, nil
	}
	add := func(dst *[]blobstore.Digest, s string) {
		if d, err := blobstore.ParseDigest(s); err == nil {
			*dst = append(*dst, d)
		}
	}
	for _, c := range m.Manifests {
		add(&children, c.Digest)
	}
	if m.Config != nil {
		add(&blobs, m.Config.Digest)
	}
	for _, l := range m.Layers {
		add(&blobs, l.Digest)
	}
	for _, b := range m.Blobs {
		add(&blobs, b.Digest)
	}
	return children, blobs
}

// referrerDescriptor returns the referrers-index descriptor for a
// manifest that carries a subject, or ok=false.
func referrerDescriptor(body []byte, contentType string, dgst blobstore.Digest) (string, manifestDescriptor, bool) {
	insp := inspectManifest(body)
	if insp == nil || insp.Subject == nil || insp.Subject.Digest == "" {
		return "", manifestDescriptor{}, false
	}
	return insp.Subject.Digest, manifestDescriptor{
		MediaType:    contentType,
		ArtifactType: insp.effectiveArtifactType(),
		Digest:       dgst.String(),
		Size:         int64(len(body)),
		Annotations:  insp.Annotations,
	}, true
}

func isSignatureIndex(idx ociImageIndex) bool {
	for _, m := range idx.Manifests {
		if _, ok := signatureArtifactTypes[m.ArtifactType]; ok {
			return true
		}
	}
	return false
}

func notFound(err error) bool {
	return errors.Is(err, ErrTagUnknown) || errors.Is(err, ErrManifestUnknown) ||
		errors.Is(err, registry.ErrPackageNotFound) || errors.Is(err, upstream.ErrNotFound)
}

func wrapNotFound(err error) error {
	if err != nil && notFound(err) && !errors.Is(err, registry.ErrPackageNotFound) {
		return fmt.Errorf("%w: %w", err, registry.ErrPackageNotFound)
	}
	return err
}

func listPackages(s *Store) ([]registry.PackageSummary, error) {
	repos, err := s.ListRepositories()
	if err != nil {
		return nil, err
	}
	out := make([]registry.PackageSummary, 0, len(repos))
	for _, name := range repos {
		tags, err := s.ListTags(name)
		if err != nil {
			return nil, err
		}
		out = append(out, registry.PackageSummary{Name: name, Versions: tags})
	}
	return out, nil
}

// classifyRequest maps manifest requests to {name, ref}; other
// repository-scoped requests (blobs, tags, referrers, uploads) to the
// name only. Catalog and the version probe are not package-scoped.
func classifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	req, ok := classify(r.Method, r.URL.Path)
	if !ok || req.Name == "" {
		return registry.ArtifactRef{}, false
	}
	if req.Op == opManifest {
		return registry.ArtifactRef{Name: req.Name, Version: req.Ref}, true
	}
	return registry.ArtifactRef{Name: req.Name}, true
}

// resolveStored resolves a tag or digest against the store.
func resolveStored(s *Store, name, ref string) (blobstore.Digest, error) {
	if err := ValidateRepoName(name); err != nil {
		return blobstore.Digest{}, fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	if IsDigestReference(ref) {
		d, err := blobstore.ParseDigest(ref)
		if err != nil {
			return blobstore.Digest{}, fmt.Errorf("%w: %v: %w", ErrDigestInvalid, err, registry.ErrInvalidPackageName)
		}
		return d, nil
	}
	if err := ValidateTag(ref); err != nil {
		return blobstore.Digest{}, fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	d, err := s.ReadTag(name, ref)
	return d, wrapNotFound(err)
}

// storedSignature reports whether the store holds a signature for
// dgst: a signature referrer or a cosign "<alg>-<hex>.sig" tag.
func storedSignature(s *Store, name string, dgst blobstore.Digest) bool {
	if idx, err := s.ReadReferrers(name, dgst.String()); err == nil && isSignatureIndex(idx) {
		return true
	}
	_, err := s.ReadTag(name, cosignTag(dgst, ".sig"))
	return err == nil
}

// ─── Local ─────────────────────────────────────────────────────────

// ListPackages implements registry.PackageLister (repositories with
// their tags as versions).
func (l *Local) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	return listPackages(l.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (l *Local) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classifyRequest(r)
}

// DeleteVersion implements registry.VersionDeleter. version is a tag
// (or a digest); immutable-tag policy applies.
func (l *Local) DeleteVersion(_ context.Context, name, version string) error {
	err := l.DeleteReference(name, version)
	if errors.Is(err, ErrNameInvalid) || errors.Is(err, ErrDigestInvalid) {
		return fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	return wrapNotFound(err)
}

// HasSignature implements registry.SignatureChecker.
func (l *Local) HasSignature(_ context.Context, name, reference string) (bool, error) {
	dgst, err := resolveStored(l.store, name, reference)
	if err != nil {
		return false, err
	}
	return storedSignature(l.store, name, dgst), nil
}

// PromoteVersion implements registry.VersionPromoter: copies the
// manifest graph (index children, config, layers), its referrers and
// cosign fallback tags into dst, then points the tag at it.
func (l *Local) PromoteVersion(_ context.Context, dst registry.Registry, name, version string) error {
	d, ok := dst.(*Local)
	if !ok {
		return fmt.Errorf("docker: promote target %T is not a docker local registry", dst)
	}
	dgst, err := resolveStored(l.store, name, version)
	if err != nil {
		return err
	}
	if _, err := l.store.ReadManifest(name, dgst); err != nil {
		return wrapNotFound(err)
	}
	isTag := !IsDigestReference(version)
	if isTag && d.isImmutableTag(version) {
		existing, err := d.store.ReadTag(name, version)
		if err == nil && existing.String() != dgst.String() {
			return fmt.Errorf("tag %q matches immutable policy in target: %w", version, ErrTagImmutable)
		}
	}
	c := &manifestCopier{src: l.store, dst: d.store, name: name, visited: map[string]bool{}}
	if err := c.copy(dgst, true); err != nil {
		return err
	}
	subject := name + "@" + dgst.String()
	if isTag {
		if err := d.store.SetTag(name, version, dgst); err != nil {
			return err
		}
		subject = name + ":" + version
	}
	events.EmitSafe(d.emitter, hook.Event{
		Type:     hook.EventRegistryPublished,
		Mount:    d.namespace,
		Path:     d.name + "/" + subject,
		Protocol: "registry-docker",
	})
	return nil
}

// manifestCopier copies a manifest graph between two stores of the
// same repository name.
type manifestCopier struct {
	src, dst *Store
	name     string
	visited  map[string]bool
}

func (c *manifestCopier) copy(dgst blobstore.Digest, root bool) error {
	key := dgst.String()
	if c.visited[key] {
		return nil
	}
	c.visited[key] = true
	rec, err := c.src.ReadManifest(c.name, dgst)
	if err != nil {
		if !root && errors.Is(err, ErrManifestUnknown) {
			return nil
		}
		return err
	}
	children, blobs := parseManifestRefs(rec.Body)
	for _, child := range children {
		if err := c.copy(child, false); err != nil {
			return err
		}
	}
	for _, b := range blobs {
		if err := copyBlob(c.src.Blobs(), c.dst.Blobs(), b); err != nil {
			return fmt.Errorf("docker: copy blob %s: %w", b, err)
		}
	}
	if err := c.dst.WriteManifest(c.name, dgst, rec.Body, rec.ContentType); err != nil {
		return err
	}
	if subj, desc, ok := referrerDescriptor(rec.Body, rec.ContentType, dgst); ok {
		if err := c.dst.AddReferrer(c.name, subj, desc); err != nil {
			return err
		}
	}
	if idx, err := c.src.ReadReferrers(c.name, key); err == nil {
		for _, m := range idx.Manifests {
			rd, err := blobstore.ParseDigest(m.Digest)
			if err != nil {
				continue
			}
			if err := c.copy(rd, false); err != nil {
				return err
			}
		}
	}
	for _, suffix := range cosignTagSuffixes {
		tag := cosignTag(dgst, suffix)
		td, err := c.src.ReadTag(c.name, tag)
		if err != nil {
			continue
		}
		if err := c.copy(td, false); err != nil {
			return err
		}
		if _, err := c.dst.ReadManifest(c.name, td); err == nil {
			if err := c.dst.SetTag(c.name, tag, td); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyBlob copies d from src to dst unless dst already has it. Blobs
// absent from src are skipped (dst mirrors src).
func copyBlob(src, dst blobstore.BlobStore, d blobstore.Digest) error {
	if _, err := dst.Stat(d); err == nil {
		return nil
	}
	rc, _, err := src.Get(d)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			return nil
		}
		return err
	}
	defer rc.Close()
	sess, err := dst.StartUpload()
	if err != nil {
		return err
	}
	if _, err := io.Copy(sess, rc); err != nil {
		_ = sess.Cancel()
		return err
	}
	_, err = sess.Commit(d)
	return err
}

// ─── Remote ────────────────────────────────────────────────────────

// ListPackages implements registry.PackageLister over the cache.
func (rr *Remote) ListPackages(_ context.Context) ([]registry.PackageSummary, error) {
	return listPackages(rr.store)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (rr *Remote) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classifyRequest(r)
}

// resolveRef resolves a tag or digest to a cached manifest digest,
// pulling the manifest from upstream with the same cache rules the
// data plane applies.
func (rr *Remote) resolveRef(ctx context.Context, name, ref string) (blobstore.Digest, error) {
	if err := ValidateRepoName(name); err != nil {
		return blobstore.Digest{}, fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	if IsDigestReference(ref) {
		dgst, err := blobstore.ParseDigest(ref)
		if err != nil {
			return blobstore.Digest{}, fmt.Errorf("%w: %v: %w", ErrDigestInvalid, err, registry.ErrInvalidPackageName)
		}
		if _, err := rr.store.ReadManifest(name, dgst); err == nil {
			return dgst, nil
		}
		_, err, _ = rr.sf.Do("manifest:"+name+":"+dgst.String(), func() (any, error) {
			return nil, rr.refetchManifest(ctx, name, dgst.String())
		})
		if _, rerr := rr.store.ReadManifest(name, dgst); rerr != nil {
			if err == nil {
				err = rerr
			}
			return blobstore.Digest{}, wrapNotFound(err)
		}
		return dgst, nil
	}
	if err := ValidateTag(ref); err != nil {
		return blobstore.Digest{}, fmt.Errorf("%w: %w", err, registry.ErrInvalidPackageName)
	}
	tagPath := rr.store.tagPath(name, ref)
	usable := rr.tagPointerExists(tagPath)
	if rr.isFloatingTag(ref) {
		usable = rr.cachedFresh(tagPath)
	}
	if usable {
		if dgst, err := rr.store.ReadTag(name, ref); err == nil {
			if _, err := rr.store.ReadManifest(name, dgst); err == nil {
				return dgst, nil
			}
		}
	}
	_, err, _ := rr.sf.Do("manifest:"+name+":tag:"+ref, func() (any, error) {
		return nil, rr.refetchManifest(ctx, name, ref)
	})
	dgst, rerr := rr.store.ReadTag(name, ref)
	if rerr == nil {
		if _, rerr = rr.store.ReadManifest(name, dgst); rerr == nil {
			return dgst, nil
		}
	}
	if err == nil {
		err = rerr
	}
	return blobstore.Digest{}, wrapNotFound(err)
}

// Prefetch implements registry.Prefetcher: resolves the tag (default
// "latest") and caches the manifest graph and every referenced blob.
func (rr *Remote) Prefetch(ctx context.Context, name, version string) error {
	if version == "" {
		version = "latest"
	}
	dgst, err := rr.resolveRef(ctx, name, version)
	if err != nil {
		return err
	}
	return rr.prefetchTree(ctx, name, dgst, map[string]bool{})
}

func (rr *Remote) prefetchTree(ctx context.Context, name string, dgst blobstore.Digest, visited map[string]bool) error {
	if visited[dgst.String()] {
		return nil
	}
	visited[dgst.String()] = true
	if _, err := rr.resolveRef(ctx, name, dgst.String()); err != nil {
		return err
	}
	rec, err := rr.store.ReadManifest(name, dgst)
	if err != nil {
		return err
	}
	children, blobs := parseManifestRefs(rec.Body)
	for _, child := range children {
		if err := rr.prefetchTree(ctx, name, child, visited); err != nil {
			return err
		}
	}
	for _, b := range blobs {
		if _, err := rr.store.Blobs().Stat(b); err == nil {
			continue
		}
		_, err, _ := rr.sf.Do("blob:"+name+":"+b.String(), func() (any, error) {
			return nil, rr.refetchBlob(ctx, name, b)
		})
		if _, serr := rr.store.Blobs().Stat(b); serr != nil {
			if err == nil {
				err = serr
			}
			return fmt.Errorf("docker: prefetch blob %s: %w", b, err)
		}
	}
	return nil
}

// HasSignature implements registry.SignatureChecker. The cache is
// consulted first, then the upstream referrers API and finally the
// cosign "<alg>-<hex>.sig" tag fallback.
func (rr *Remote) HasSignature(ctx context.Context, name, reference string) (bool, error) {
	dgst, err := rr.resolveRef(ctx, name, reference)
	if err != nil {
		return false, err
	}
	if storedSignature(rr.store, name, dgst) {
		return true, nil
	}
	up := rr.upstreamName(name)
	if resp, err := rr.upstreamGet(ctx, "/v2/"+up+"/referrers/"+dgst.String(), "repository:"+up+":pull"); err == nil {
		var idx ociImageIndex
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&idx)
		resp.Body.Close()
		if decErr == nil && isSignatureIndex(idx) {
			return true, nil
		}
	}
	if _, err := rr.resolveRef(ctx, name, cosignTag(dgst, ".sig")); err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ─── Virtual ───────────────────────────────────────────────────────

// ListPackages unions the members' listings.
func (v *Virtual) ListPackages(ctx context.Context) ([]registry.PackageSummary, error) {
	return (&pkgbase.Virtual{Base: v.Base, RepoType: v.Type()}).ListPackages(ctx)
}

// ClassifyRequest implements registry.ArtifactClassifier.
func (v *Virtual) ClassifyRequest(r *http.Request) (registry.ArtifactRef, bool) {
	return classifyRequest(r)
}

// HasSignature implements registry.SignatureChecker: true when any
// member reports a signature. Member errors are returned only when no
// member could answer.
func (v *Virtual) HasSignature(ctx context.Context, name, reference string) (bool, error) {
	var (
		found    bool
		answered bool
		firstErr error
	)
	v.ForEachMember(func(reg registry.Registry) bool {
		sc, ok := reg.(registry.SignatureChecker)
		if !ok {
			return false
		}
		has, err := sc.HasSignature(ctx, name, reference)
		if err != nil {
			if firstErr == nil && !notFound(err) {
				firstErr = err
			}
			return false
		}
		answered = true
		found = has
		return has
	})
	if found {
		return true, nil
	}
	if answered || firstErr == nil {
		if !answered {
			return false, registry.ErrPackageNotFound
		}
		return false, nil
	}
	return false, firstErr
}
