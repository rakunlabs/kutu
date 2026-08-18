package s3serve

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rakunlabs/kutu/internal/serve/ftpserve"
)

// AWS Signature Version 4 verification for the S3 service.
//
// Both header-based (Authorization: AWS4-HMAC-SHA256 ...) and presigned
// query-string requests are supported. The region and service inside the
// credential scope are taken from the request itself so clients work
// regardless of the region they were configured with.

const (
	sigV4Algorithm   = "AWS4-HMAC-SHA256"
	unsignedPayload  = "UNSIGNED-PAYLOAD"
	iso8601Format    = "20060102T150405Z"
	yyyymmddFormat   = "20060102"
	maxClockSkew     = 15 * time.Minute
	streamingSHAPref = "STREAMING-"
)

// authenticate verifies the request signature and returns the matching
// user. Returns an s3Error on failure.
func (s *Server) authenticate(r *http.Request) (*ftpserve.User, *s3Error) {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, sigV4Algorithm) {
		return s.verifyHeaderAuth(r, auth)
	}
	if r.URL.Query().Get("X-Amz-Algorithm") == sigV4Algorithm {
		return s.verifyPresignedAuth(r)
	}
	return nil, errMissingAuth()
}

type credentialScope struct {
	accessKey string
	date      string // yyyymmdd
	region    string
	service   string
}

// parseCredential parses "AKID/20130524/us-east-1/s3/aws4_request".
func parseCredential(cred string) (*credentialScope, *s3Error) {
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" {
		return nil, errAuthMalformed("invalid credential scope: " + cred)
	}
	return &credentialScope{
		accessKey: parts[0],
		date:      parts[1],
		region:    parts[2],
		service:   parts[3],
	}, nil
}

// verifyHeaderAuth validates an Authorization-header signed request.
func (s *Server) verifyHeaderAuth(r *http.Request, auth string) (*ftpserve.User, *s3Error) {
	fields := strings.TrimPrefix(auth, sigV4Algorithm)
	var credStr, signedHeaders, signature string
	for _, part := range strings.Split(fields, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			credStr = strings.TrimPrefix(part, "Credential=")
		case strings.HasPrefix(part, "SignedHeaders="):
			signedHeaders = strings.TrimPrefix(part, "SignedHeaders=")
		case strings.HasPrefix(part, "Signature="):
			signature = strings.TrimPrefix(part, "Signature=")
		}
	}
	if credStr == "" || signedHeaders == "" || signature == "" {
		return nil, errAuthMalformed("missing Credential, SignedHeaders or Signature")
	}

	cred, s3err := parseCredential(credStr)
	if s3err != nil {
		return nil, s3err
	}

	user := s.lookupUser(cred.accessKey)
	if user == nil {
		return nil, errInvalidAccessKey()
	}

	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.Header.Get("Date")
	}
	reqTime, err := time.Parse(iso8601Format, amzDate)
	if err != nil {
		return nil, errAuthMalformed("invalid X-Amz-Date")
	}
	if skew := time.Since(reqTime); skew > maxClockSkew || skew < -maxClockSkew {
		return nil, errTimeSkewed()
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}

	canonical := canonicalRequest(r, signedHeaders, payloadHash, false)
	expected := computeSignature(user.Password, cred, amzDate, canonical)

	if !constantTimeHexEquals(expected, signature) {
		return nil, errSignatureMismatch()
	}
	return user, nil
}

// verifyPresignedAuth validates a presigned-URL request.
func (s *Server) verifyPresignedAuth(r *http.Request) (*ftpserve.User, *s3Error) {
	q := r.URL.Query()

	cred, s3err := parseCredential(q.Get("X-Amz-Credential"))
	if s3err != nil {
		return nil, s3err
	}

	user := s.lookupUser(cred.accessKey)
	if user == nil {
		return nil, errInvalidAccessKey()
	}

	amzDate := q.Get("X-Amz-Date")
	reqTime, err := time.Parse(iso8601Format, amzDate)
	if err != nil {
		return nil, errAuthMalformed("invalid X-Amz-Date")
	}

	expires, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 1 {
		return nil, errInvalidArgument("invalid X-Amz-Expires")
	}
	if time.Now().UTC().After(reqTime.Add(time.Duration(expires) * time.Second)) {
		return nil, errExpiredPresign()
	}

	signedHeaders := q.Get("X-Amz-SignedHeaders")
	signature := q.Get("X-Amz-Signature")
	if signedHeaders == "" || signature == "" {
		return nil, errAuthMalformed("missing X-Amz-SignedHeaders or X-Amz-Signature")
	}

	canonical := canonicalRequest(r, signedHeaders, unsignedPayload, true)
	expected := computeSignature(user.Password, cred, amzDate, canonical)

	if !constantTimeHexEquals(expected, signature) {
		return nil, errSignatureMismatch()
	}
	return user, nil
}

// canonicalRequest builds the SigV4 canonical request string.
func canonicalRequest(r *http.Request, signedHeaders, payloadHash string, presigned bool) string {
	var sb strings.Builder
	sb.WriteString(r.Method)
	sb.WriteByte('\n')
	sb.WriteString(s3URIEncode(r.URL.Path, true))
	sb.WriteByte('\n')
	sb.WriteString(canonicalQueryString(r.URL.Query(), presigned))
	sb.WriteByte('\n')

	headers := strings.Split(signedHeaders, ";")
	for _, h := range headers {
		h = strings.ToLower(strings.TrimSpace(h))
		var value string
		if h == "host" {
			value = r.Host
		} else {
			value = strings.Join(r.Header.Values(h), ",")
		}
		sb.WriteString(h)
		sb.WriteByte(':')
		sb.WriteString(trimAll(value))
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	sb.WriteString(signedHeaders)
	sb.WriteByte('\n')
	sb.WriteString(payloadHash)
	return sb.String()
}

// canonicalQueryString builds the sorted, URI-encoded query string.
// X-Amz-Signature is excluded for presigned requests.
func canonicalQueryString(q url.Values, presigned bool) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if presigned && k == "X-Amz-Signature" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		ek := s3URIEncode(k, false)
		for _, v := range values {
			parts = append(parts, ek+"="+s3URIEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// computeSignature derives the SigV4 signature for the canonical request.
func computeSignature(secretKey string, cred *credentialScope, amzDate, canonical string) string {
	scope := cred.date + "/" + cred.region + "/" + cred.service + "/aws4_request"

	hashed := sha256.Sum256([]byte(canonical))

	stringToSign := sigV4Algorithm + "\n" +
		amzDate + "\n" +
		scope + "\n" +
		hex.EncodeToString(hashed[:])

	dateKey := hmacSHA256([]byte("AWS4"+secretKey), cred.date)
	regionKey := hmacSHA256(dateKey, cred.region)
	serviceKey := hmacSHA256(regionKey, cred.service)
	signingKey := hmacSHA256(serviceKey, "aws4_request")

	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func constantTimeHexEquals(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// trimAll trims leading/trailing whitespace and collapses internal
// whitespace runs into a single space, per the SigV4 spec.
func trimAll(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// s3URIEncode implements the AWS uriEncode function: unreserved
// characters stay literal, everything else is percent-encoded with
// uppercase hex. Slash is preserved when keepSlash is true (paths).
func s3URIEncode(s string, keepSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			sb.WriteByte(c)
		case c == '/' && keepSlash:
			sb.WriteByte(c)
		default:
			sb.WriteByte('%')
			sb.WriteByte(upperhex[c>>4])
			sb.WriteByte(upperhex[c&0xF])
		}
	}
	return sb.String()
}

// isStreamingPayload reports whether the request body uses the
// aws-chunked streaming encoding.
func isStreamingPayload(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), streamingSHAPref) {
		return true
	}
	for _, enc := range r.Header.Values("Content-Encoding") {
		for _, e := range strings.Split(enc, ",") {
			if strings.TrimSpace(e) == "aws-chunked" {
				return true
			}
		}
	}
	return false
}
