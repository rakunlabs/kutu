package s3serve

import (
	"crypto/md5" //nolint:gosec // S3 ETags are MD5 by protocol definition
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/rawfs"
	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
	"github.com/rakunlabs/kutu/internal/service"
)

const (
	maxListKeys    = 1000
	maxXMLBodySize = 4 << 20 // 4 MB cap for XML request bodies
)

// ── Service level ──

type bucketInfo struct {
	Name         string    `xml:"Name"`
	CreationDate time.Time `xml:"CreationDate"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name     `xml:"ListAllMyBucketsResult"`
	Xmlns   string       `xml:"xmlns,attr"`
	Owner   ownerInfo    `xml:"Owner"`
	Buckets bucketsField `xml:"Buckets"`
}

type bucketsField struct {
	Bucket []bucketInfo `xml:"Bucket"`
}

type ownerInfo struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

const xmlnsS3 = "http://s3.amazonaws.com/doc/2006-03-01/"

// listBuckets handles GET / — every share visible to the user is a bucket.
func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	shares := s.userShares(rc.user)

	res := &listAllMyBucketsResult{
		Xmlns: xmlnsS3,
		Owner: ownerInfo{ID: rc.user.Username, DisplayName: rc.user.Username},
	}
	for _, sh := range shares {
		res.Buckets.Bucket = append(res.Buckets.Bucket, bucketInfo{
			Name:         sh.Name,
			CreationDate: time.Unix(0, 0).UTC(),
		})
	}
	writeXMLResponse(w, http.StatusOK, res)
}

// ── Bucket level ──

// handleBucket dispatches bucket-scoped requests (no object key).
func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	share := s.findShare(rc.user, rc.bucket)
	q := r.URL.Query()

	switch r.Method {
	case http.MethodHead:
		if share == nil {
			writeS3Error(w, r, errNoSuchBucket())
			return
		}
		w.Header().Set("X-Amz-Bucket-Region", s.region)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if share == nil {
			writeS3Error(w, r, errNoSuchBucket())
			return
		}
		switch {
		case q.Has("location"):
			s.getBucketLocation(w)
		case q.Has("uploads"):
			s.listMultipartUploads(w, rc)
		case q.Has("versioning"):
			writeXMLResponse(w, http.StatusOK, &versioningConfiguration{Xmlns: xmlnsS3})
		case q.Has("acl"), q.Has("policy"), q.Has("lifecycle"), q.Has("cors"),
			q.Has("encryption"), q.Has("website"), q.Has("replication"),
			q.Has("tagging"), q.Has("object-lock"), q.Has("logging"), q.Has("notification"):
			writeS3Error(w, r, errNotImplemented("bucket sub-resources are not supported"))
		default:
			s.listObjects(w, r, rc, share)
		}

	case http.MethodPut:
		if len(q) > 0 {
			writeS3Error(w, r, errNotImplemented("bucket sub-resources are not supported"))
			return
		}
		// Buckets are managed as kutu shares; treat CreateBucket on an
		// existing share as idempotent success.
		if share == nil {
			writeS3Error(w, r, errNotImplemented("buckets are managed as kutu serve shares"))
			return
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodPost:
		if share == nil {
			writeS3Error(w, r, errNoSuchBucket())
			return
		}
		if q.Has("delete") {
			s.deleteObjects(w, r, rc, share)
			return
		}
		writeS3Error(w, r, errMethodNotAllowed())

	case http.MethodDelete:
		writeS3Error(w, r, errNotImplemented("buckets are managed as kutu serve shares"))

	default:
		writeS3Error(w, r, errMethodNotAllowed())
	}
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	Xmlns   string   `xml:"xmlns,attr"`
	Value   string   `xml:",chardata"`
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	Xmlns   string   `xml:"xmlns,attr"`
}

func (s *Server) getBucketLocation(w http.ResponseWriter) {
	value := s.region
	if value == "us-east-1" {
		value = "" // AWS returns an empty LocationConstraint for us-east-1
	}
	writeXMLResponse(w, http.StatusOK, &locationConstraint{Xmlns: xmlnsS3, Value: value})
}

// ── Object listing ──

type listContents struct {
	Key          string    `xml:"Key"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
	StorageClass string    `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResultV2 struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Xmlns                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []listContents `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type listBucketResultV1 struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	Xmlns          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	IsTruncated    bool           `xml:"IsTruncated"`
	Contents       []listContents `xml:"Contents"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
}

// listObjects handles GET /bucket (ListObjects V1 and V2).
func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share) {
	q := r.URL.Query()
	isV2 := q.Get("list-type") == "2"

	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	encodingType := q.Get("encoding-type")

	if delimiter != "" && delimiter != "/" {
		writeS3Error(w, r, errNotImplemented("only '/' is supported as a delimiter"))
		return
	}

	maxKeys := maxListKeys
	if mk := q.Get("max-keys"); mk != "" {
		v, err := strconv.Atoi(mk)
		if err != nil || v < 0 {
			writeS3Error(w, r, errInvalidArgument("invalid max-keys"))
			return
		}
		if v < maxKeys {
			maxKeys = v
		}
	}

	var after string
	var contToken, startAfter, marker string
	if isV2 {
		contToken = q.Get("continuation-token")
		startAfter = q.Get("start-after")
		if contToken != "" {
			decoded, err := base64.StdEncoding.DecodeString(contToken)
			if err != nil {
				writeS3Error(w, r, errInvalidArgument("invalid continuation-token"))
				return
			}
			after = string(decoded)
		} else {
			after = startAfter
		}
	} else {
		marker = q.Get("marker")
		after = marker
	}

	if cleanPfx, ok := cleanKeyPrefix(prefix); ok {
		prefix = cleanPfx
	} else {
		writeS3Error(w, r, errInvalidArgument("invalid prefix"))
		return
	}

	entries, truncated := listShare(share, prefix, delimiter, after, maxKeys)

	encode := func(v string) string {
		if encodingType == "url" {
			return s3URIEncode(v, true)
		}
		return v
	}

	var contents []listContents
	var prefixes []commonPrefix
	var lastKey string
	for _, e := range entries {
		lastKey = e.key
		if e.isPrefix {
			prefixes = append(prefixes, commonPrefix{Prefix: encode(e.key)})
			continue
		}
		modTime := time.Unix(0, 0).UTC()
		size := e.size
		if fi, err := e.src.FS.Stat(sourceFSPath(e.src, e.key)); err == nil {
			modTime = fi.ModTime.UTC()
			size = fi.Size
		}
		contents = append(contents, listContents{
			Key:          encode(e.key),
			LastModified: modTime,
			ETag:         `"` + pseudoETag(e.key, size, modTime) + `"`,
			Size:         size,
			StorageClass: "STANDARD",
		})
	}

	if isV2 {
		res := &listBucketResultV2{
			Xmlns:             xmlnsS3,
			Name:              rc.bucket,
			Prefix:            encode(prefix),
			StartAfter:        encode(startAfter),
			ContinuationToken: contToken,
			KeyCount:          len(contents) + len(prefixes),
			MaxKeys:           maxKeys,
			Delimiter:         encode(delimiter),
			EncodingType:      encodingType,
			IsTruncated:       truncated,
			Contents:          contents,
			CommonPrefixes:    prefixes,
		}
		if truncated && lastKey != "" {
			res.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(lastKey))
		}
		writeXMLResponse(w, http.StatusOK, res)
		return
	}

	res := &listBucketResultV1{
		Xmlns:          xmlnsS3,
		Name:           rc.bucket,
		Prefix:         encode(prefix),
		Marker:         encode(marker),
		MaxKeys:        maxKeys,
		Delimiter:      encode(delimiter),
		EncodingType:   encodingType,
		IsTruncated:    truncated,
		Contents:       contents,
		CommonPrefixes: prefixes,
	}
	if truncated && lastKey != "" {
		res.NextMarker = encode(lastKey)
	}
	writeXMLResponse(w, http.StatusOK, res)
}

// cleanKeyPrefix normalizes a list prefix (which may legitimately end
// with "/" or be empty) while rejecting traversal.
func cleanKeyPrefix(prefix string) (string, bool) {
	if prefix == "" {
		return "", true
	}
	trailing := strings.HasSuffix(prefix, "/")
	cleaned, ok := cleanKey(prefix)
	if !ok {
		return "", false
	}
	if trailing && cleaned != "" {
		cleaned += "/"
	}
	return cleaned, true
}

// ── Object level ──

// handleObject dispatches object-scoped requests.
func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	share := s.findShare(rc.user, rc.bucket)
	if share == nil {
		writeS3Error(w, r, errNoSuchBucket())
		return
	}

	trailingSlash := strings.HasSuffix(rc.key, "/")
	key, ok := cleanKey(rc.key)
	if !ok || key == "" {
		writeS3Error(w, r, errInvalidArgument("invalid object key"))
		return
	}

	q := r.URL.Query()

	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("uploadId"):
			s.listParts(w, r, rc, key)
		case q.Has("tagging"):
			writeXMLResponse(w, http.StatusOK, &taggingResult{Xmlns: xmlnsS3})
		case q.Has("acl"):
			writeS3Error(w, r, errNotImplemented("object ACLs are not supported"))
		default:
			s.getObject(w, r, share, key, trailingSlash)
		}

	case http.MethodHead:
		s.headObject(w, r, share, key, trailingSlash)

	case http.MethodPut:
		switch {
		case r.Header.Get("X-Amz-Copy-Source") != "":
			if q.Has("partNumber") || q.Has("uploadId") {
				writeS3Error(w, r, errNotImplemented("UploadPartCopy is not supported"))
				return
			}
			s.copyObject(w, r, rc, share, key)
		case q.Has("partNumber") && q.Has("uploadId"):
			s.uploadPart(w, r, rc, share, key)
		case q.Has("tagging"), q.Has("acl"):
			w.WriteHeader(http.StatusOK) // accepted and ignored
		default:
			s.putObject(w, r, rc, share, key, trailingSlash)
		}

	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			s.createMultipartUpload(w, r, rc, share, key)
		case q.Has("uploadId"):
			s.completeMultipartUpload(w, r, rc, share, key)
		default:
			writeS3Error(w, r, errMethodNotAllowed())
		}

	case http.MethodDelete:
		switch {
		case q.Has("uploadId"):
			s.uploads.remove(q.Get("uploadId"))
			w.WriteHeader(http.StatusNoContent)
		case q.Has("tagging"):
			w.WriteHeader(http.StatusNoContent)
		default:
			s.deleteObject(w, r, rc, share, key)
		}

	default:
		writeS3Error(w, r, errMethodNotAllowed())
	}
}

// getObject handles GET /bucket/key with Range support.
func (s *Server) getObject(w http.ResponseWriter, r *http.Request, share *ftpserve.Share, key string, trailingSlash bool) {
	src, err := findInSources(share, key)
	if err != nil {
		writeS3Error(w, r, errNoSuchKey())
		return
	}

	fi, err := src.FS.Stat(sourceFSPath(src, key))
	if err != nil {
		writeS3Error(w, r, errNoSuchKey())
		return
	}

	if fi.IsDir {
		if !trailingSlash {
			writeS3Error(w, r, errNoSuchKey())
			return
		}
		// Directory placeholder object ("key/"): empty body.
		w.Header().Set("ETag", `"`+emptyMD5+`"`)
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Last-Modified", fi.ModTime.UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		return
	}

	reader, fi, err := src.FS.Open(sourceFSPath(src, key))
	if err != nil {
		writeS3Error(w, r, errInternal(""))
		return
	}
	defer reader.Close() //nolint:errcheck

	setResponseOverrides(w, r)
	w.Header().Set("ETag", `"`+pseudoETag(key, fi.Size, fi.ModTime.UTC())+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", contentTypeFor(key))
	}

	http.ServeContent(w, r, path.Base(key), fi.ModTime, reader)
}

// headObject handles HEAD /bucket/key.
func (s *Server) headObject(w http.ResponseWriter, r *http.Request, share *ftpserve.Share, key string, trailingSlash bool) {
	src, err := findInSources(share, key)
	if err != nil {
		writeS3Error(w, r, errNoSuchKey())
		return
	}

	fi, err := src.FS.Stat(sourceFSPath(src, key))
	if err != nil {
		writeS3Error(w, r, errNoSuchKey())
		return
	}

	if fi.IsDir && !trailingSlash {
		writeS3Error(w, r, errNoSuchKey())
		return
	}

	size := fi.Size
	etag := pseudoETag(key, size, fi.ModTime.UTC())
	if fi.IsDir {
		size = 0
		etag = emptyMD5
	}

	w.Header().Set("Content-Type", contentTypeFor(key))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Last-Modified", fi.ModTime.UTC().Format(http.TimeFormat))
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
}

// putObject handles PUT /bucket/key.
func (s *Server) putObject(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share, key string, trailingSlash bool) {
	if isReadOnly(rc.user, share) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}

	body, size, s3err := requestBody(r)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	// Zero-byte "key/" put = directory marker.
	if trailingSlash && size == 0 {
		_, wfs, err := firstWritableSource(share)
		if err != nil {
			writeS3Error(w, r, errAccessDenied("no writable source in share"))
			return
		}
		if err := wfs.MkDir(sourceFSPathW(share, key)); err != nil {
			writeS3Error(w, r, errInternal(err.Error()))
			return
		}
		w.Header().Set("ETag", `"`+emptyMD5+`"`)
		w.WriteHeader(http.StatusOK)
		return
	}

	wfs, fsPath, s3err := writeTarget(share, key)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	h := md5.New() //nolint:gosec
	if err := wfs.Write(fsPath, io.TeeReader(body, h), size); err != nil {
		writeS3Error(w, r, errInternal(err.Error()))
		return
	}

	w.Header().Set("ETag", `"`+hex.EncodeToString(h.Sum(nil))+`"`)
	w.WriteHeader(http.StatusOK)
}

// copyObject handles PUT /bucket/key with x-amz-copy-source.
type copyObjectResult struct {
	XMLName      xml.Name  `xml:"CopyObjectResult"`
	Xmlns        string    `xml:"xmlns,attr"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, rc *reqCtx, dstShare *ftpserve.Share, dstKey string) {
	if isReadOnly(rc.user, dstShare) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}

	srcBucket, srcKey, s3err := parseCopySource(r.Header.Get("X-Amz-Copy-Source"))
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	srcShare := s.findShare(rc.user, srcBucket)
	if srcShare == nil {
		writeS3Error(w, r, errNoSuchBucket())
		return
	}

	cleanSrc, ok := cleanKey(srcKey)
	if !ok || cleanSrc == "" {
		writeS3Error(w, r, errInvalidArgument("invalid copy source key"))
		return
	}

	src, err := findInSources(srcShare, cleanSrc)
	if err != nil {
		writeS3Error(w, r, errNoSuchKey())
		return
	}

	wfs, dstPath, s3err := writeTarget(dstShare, dstKey)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	srcPath := sourceFSPath(src, cleanSrc)

	// Same backend + native copy support: server-side copy.
	if cfs, ok := src.FS.(rawfs.CopyableRawFS); ok && sameFS(src.FS, rawfs.RawFS(wfs)) {
		err = cfs.Copy(srcPath, dstPath)
	} else {
		err = rawfs.GenericCopy(src.FS, srcPath, wfs, dstPath)
	}
	if err != nil {
		writeS3Error(w, r, errInternal(err.Error()))
		return
	}

	modTime := time.Now().UTC()
	var size int64
	if fi, statErr := wfs.Stat(dstPath); statErr == nil {
		modTime = fi.ModTime.UTC()
		size = fi.Size
	}

	writeXMLResponse(w, http.StatusOK, &copyObjectResult{
		Xmlns:        xmlnsS3,
		LastModified: modTime,
		ETag:         `"` + pseudoETag(dstKey, size, modTime) + `"`,
	})
}

func sameFS(a, b rawfs.RawFS) bool { return a == b }

// parseCopySource parses "/bucket/key" (URL-encoded, optional
// ?versionId suffix) from the x-amz-copy-source header.
func parseCopySource(src string) (bucket, key string, s3err *s3Error) {
	if i := strings.IndexByte(src, '?'); i >= 0 {
		src = src[:i]
	}
	unescaped, err := url.PathUnescape(src)
	if err != nil {
		unescaped = src
	}
	unescaped = strings.TrimPrefix(unescaped, "/")
	i := strings.IndexByte(unescaped, '/')
	if i <= 0 || i == len(unescaped)-1 {
		return "", "", errInvalidArgument("invalid x-amz-copy-source")
	}
	return unescaped[:i], unescaped[i+1:], nil
}

// deleteObject handles DELETE /bucket/key. Deleting a missing key is a
// success per S3 semantics.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share, key string) {
	if isReadOnly(rc.user, share) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}

	if s3err := deleteFromShare(share, key); s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func deleteFromShare(share *ftpserve.Share, key string) *s3Error {
	src, err := findInSources(share, key)
	if err != nil {
		return nil // already gone
	}

	wfs, ok := src.FS.(rawfs.WritableRawFS)
	if !ok {
		return errAccessDenied("source is not writable")
	}

	if err := wfs.Delete(sourceFSPath(src, key)); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return nil
		}
		return errInternal(err.Error())
	}
	return nil
}

// ── Batch delete ──

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

type deleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr"`
	Deleted []deletedObject `xml:"Deleted"`
	Errors  []deleteError   `xml:"Error"`
}

type deletedObject struct {
	Key string `xml:"Key"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// deleteObjects handles POST /bucket?delete.
func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share) {
	if isReadOnly(rc.user, share) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}

	body, _, s3err := requestBody(r)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	var req deleteRequest
	if err := xml.NewDecoder(io.LimitReader(body, maxXMLBodySize)).Decode(&req); err != nil {
		writeS3Error(w, r, errInvalidRequest("malformed delete request body"))
		return
	}

	res := &deleteResult{Xmlns: xmlnsS3}
	for _, obj := range req.Objects {
		key, ok := cleanKey(obj.Key)
		if !ok || key == "" {
			res.Errors = append(res.Errors, deleteError{Key: obj.Key, Code: "InvalidArgument", Message: "invalid key"})
			continue
		}
		if delErr := deleteFromShare(share, key); delErr != nil {
			res.Errors = append(res.Errors, deleteError{Key: obj.Key, Code: delErr.Code, Message: delErr.Message})
			continue
		}
		if !req.Quiet {
			res.Deleted = append(res.Deleted, deletedObject{Key: obj.Key})
		}
	}

	writeXMLResponse(w, http.StatusOK, res)
}

// ── Multipart upload ──

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

func (s *Server) createMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share, key string) {
	if isReadOnly(rc.user, share) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}
	if _, _, s3err := writeTarget(share, key); s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	up, err := s.uploads.create(rc.bucket, key)
	if err != nil {
		writeS3Error(w, r, errInternal(err.Error()))
		return
	}

	writeXMLResponse(w, http.StatusOK, &initiateMultipartUploadResult{
		Xmlns:    xmlnsS3,
		Bucket:   rc.bucket,
		Key:      key,
		UploadID: up.id,
	})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share, key string) {
	if isReadOnly(rc.user, share) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}

	q := r.URL.Query()
	up := s.uploads.get(q.Get("uploadId"), rc.bucket, key)
	if up == nil {
		writeS3Error(w, r, errNoSuchUpload())
		return
	}

	partNumber, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		writeS3Error(w, r, errInvalidArgument("invalid partNumber"))
		return
	}

	body, _, s3err := requestBody(r)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	pi, err := up.putPart(partNumber, body)
	if err != nil {
		writeS3Error(w, r, errInvalidArgument(err.Error()))
		return
	}

	w.Header().Set("ETag", `"`+pi.etag+`"`)
	w.WriteHeader(http.StatusOK)
}

type completedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadRequest struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Xmlns    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqCtx, share *ftpserve.Share, key string) {
	if isReadOnly(rc.user, share) {
		writeS3Error(w, r, errAccessDenied("share is read-only"))
		return
	}

	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	up := s.uploads.get(uploadID, rc.bucket, key)
	if up == nil {
		writeS3Error(w, r, errNoSuchUpload())
		return
	}

	var req completeMultipartUploadRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxXMLBodySize)).Decode(&req); err != nil {
		writeS3Error(w, r, errInvalidRequest("malformed complete request body"))
		return
	}

	parts, total, etag, err := up.assemble(req.Parts)
	if err != nil {
		writeS3Error(w, r, errInvalidRequest(err.Error()))
		return
	}

	wfs, fsPath, s3err := writeTarget(share, key)
	if s3err != nil {
		writeS3Error(w, r, s3err)
		return
	}

	reader := partsReader(parts)
	defer reader.Close() //nolint:errcheck

	if err := wfs.Write(fsPath, reader, total); err != nil {
		writeS3Error(w, r, errInternal(err.Error()))
		return
	}

	s.uploads.remove(uploadID)

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}

	writeXMLResponse(w, http.StatusOK, &completeMultipartUploadResult{
		Xmlns:    xmlnsS3,
		Location: fmt.Sprintf("%s://%s/%s/%s", scheme, r.Host, rc.bucket, key),
		Bucket:   rc.bucket,
		Key:      key,
		ETag:     `"` + etag + `"`,
	})
}

type listPartsResult struct {
	XMLName     xml.Name   `xml:"ListPartsResult"`
	Xmlns       string     `xml:"xmlns,attr"`
	Bucket      string     `xml:"Bucket"`
	Key         string     `xml:"Key"`
	UploadID    string     `xml:"UploadId"`
	MaxParts    int        `xml:"MaxParts"`
	IsTruncated bool       `xml:"IsTruncated"`
	Parts       []partItem `xml:"Part"`
}

type partItem struct {
	PartNumber   int       `xml:"PartNumber"`
	LastModified time.Time `xml:"LastModified"`
	ETag         string    `xml:"ETag"`
	Size         int64     `xml:"Size"`
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, rc *reqCtx, key string) {
	up := s.uploads.get(r.URL.Query().Get("uploadId"), rc.bucket, key)
	if up == nil {
		writeS3Error(w, r, errNoSuchUpload())
		return
	}

	res := &listPartsResult{
		Xmlns:    xmlnsS3,
		Bucket:   rc.bucket,
		Key:      key,
		UploadID: up.id,
		MaxParts: 10000,
	}
	for _, p := range up.listParts() {
		res.Parts = append(res.Parts, partItem{
			PartNumber:   p.number,
			LastModified: up.started.UTC(),
			ETag:         `"` + p.etag + `"`,
			Size:         p.size,
		})
	}
	writeXMLResponse(w, http.StatusOK, res)
}

type listMultipartUploadsResult struct {
	XMLName     xml.Name `xml:"ListMultipartUploadsResult"`
	Xmlns       string   `xml:"xmlns,attr"`
	Bucket      string   `xml:"Bucket"`
	IsTruncated bool     `xml:"IsTruncated"`
}

func (s *Server) listMultipartUploads(w http.ResponseWriter, rc *reqCtx) {
	writeXMLResponse(w, http.StatusOK, &listMultipartUploadsResult{Xmlns: xmlnsS3, Bucket: rc.bucket})
}

type taggingResult struct {
	XMLName xml.Name `xml:"Tagging"`
	Xmlns   string   `xml:"xmlns,attr"`
	TagSet  struct{} `xml:"TagSet"`
}

// ── Shared helpers ──

const emptyMD5 = "d41d8cd98f00b204e9800998ecf8427e"

// requestBody returns the (possibly aws-chunked decoded) request body
// and its decoded size (-1 when unknown).
func requestBody(r *http.Request) (io.Reader, int64, *s3Error) {
	if isStreamingPayload(r) {
		size := int64(-1)
		if v := r.Header.Get("X-Amz-Decoded-Content-Length"); v != "" {
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, 0, errInvalidRequest("invalid x-amz-decoded-content-length")
			}
			size = parsed
		}
		return newChunkedReader(r.Body), size, nil
	}
	return r.Body, r.ContentLength, nil
}

// writeTarget picks the rawfs backend + path a new/updated object goes
// to: an existing source when the key already exists, otherwise the
// first writable source.
func writeTarget(share *ftpserve.Share, key string) (rawfs.WritableRawFS, string, *s3Error) {
	if src, err := findInSources(share, key); err == nil {
		wfs, ok := src.FS.(rawfs.WritableRawFS)
		if !ok {
			return nil, "", errAccessDenied("existing source is not writable")
		}
		return wfs, sourceFSPath(src, key), nil
	}

	wsrc, wfs, err := firstWritableSource(share)
	if err != nil {
		return nil, "", errAccessDenied("no writable source in share")
	}
	return wfs, sourceFSPath(wsrc, key), nil
}

// sourceFSPathW resolves the write path against the first writable
// source (used for directory markers).
func sourceFSPathW(share *ftpserve.Share, key string) string {
	wsrc, _, err := firstWritableSource(share)
	if err != nil {
		return key
	}
	return sourceFSPath(wsrc, key)
}

// pseudoETag derives a stable ETag surrogate for backends that do not
// expose content hashes. It is not a content MD5.
func pseudoETag(key string, size int64, modTime time.Time) string {
	h := md5.New() //nolint:gosec
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d", key, size, modTime.Unix())
	return hex.EncodeToString(h.Sum(nil))
}

// contentTypeFor guesses a MIME type from the key's extension.
func contentTypeFor(key string) string {
	if ct := mime.TypeByExtension(path.Ext(key)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// setResponseOverrides applies the standard response-* query overrides
// (mainly used with presigned GETs).
func setResponseOverrides(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	overrides := map[string]string{
		"response-content-type":        "Content-Type",
		"response-content-language":    "Content-Language",
		"response-expires":             "Expires",
		"response-cache-control":       "Cache-Control",
		"response-content-disposition": "Content-Disposition",
		"response-content-encoding":    "Content-Encoding",
	}
	for param, header := range overrides {
		if v := q.Get(param); v != "" {
			w.Header().Set(header, v)
		}
	}
}
