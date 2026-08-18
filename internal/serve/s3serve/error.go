package s3serve

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"net/http"
)

// s3Error is an S3 API error with its wire representation.
type s3Error struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *s3Error) Error() string { return e.Code + ": " + e.Message }

func newS3Error(code, message string, status int) *s3Error {
	return &s3Error{Code: code, Message: message, HTTPStatus: status}
}

func errNoSuchBucket() *s3Error {
	return newS3Error("NoSuchBucket", "The specified bucket does not exist", http.StatusNotFound)
}

func errNoSuchKey() *s3Error {
	return newS3Error("NoSuchKey", "The specified key does not exist", http.StatusNotFound)
}

func errNoSuchUpload() *s3Error {
	return newS3Error("NoSuchUpload", "The specified multipart upload does not exist", http.StatusNotFound)
}

func errAccessDenied(msg string) *s3Error {
	if msg == "" {
		msg = "Access Denied"
	}
	return newS3Error("AccessDenied", msg, http.StatusForbidden)
}

func errInvalidAccessKey() *s3Error {
	return newS3Error("InvalidAccessKeyId", "The AWS access key ID you provided does not exist in our records", http.StatusForbidden)
}

func errSignatureMismatch() *s3Error {
	return newS3Error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided", http.StatusForbidden)
}

func errAuthMalformed(msg string) *s3Error {
	return newS3Error("AuthorizationHeaderMalformed", msg, http.StatusBadRequest)
}

func errMissingAuth() *s3Error {
	return newS3Error("AccessDenied", "Anonymous access is not allowed; sign the request with SigV4", http.StatusForbidden)
}

func errTimeSkewed() *s3Error {
	return newS3Error("RequestTimeTooSkewed", "The difference between the request time and the server's time is too large", http.StatusForbidden)
}

func errExpiredPresign() *s3Error {
	return newS3Error("AccessDenied", "Request has expired", http.StatusForbidden)
}

func errInvalidArgument(msg string) *s3Error {
	return newS3Error("InvalidArgument", msg, http.StatusBadRequest)
}

func errInvalidRequest(msg string) *s3Error {
	return newS3Error("InvalidRequest", msg, http.StatusBadRequest)
}

func errMethodNotAllowed() *s3Error {
	return newS3Error("MethodNotAllowed", "The specified method is not allowed against this resource", http.StatusMethodNotAllowed)
}

func errNotImplemented(msg string) *s3Error {
	if msg == "" {
		msg = "A header or query you provided requested a function that is not implemented"
	}
	return newS3Error("NotImplemented", msg, http.StatusNotImplemented)
}

func errInternal(msg string) *s3Error {
	if msg == "" {
		msg = "We encountered an internal error. Please try again."
	}
	return newS3Error("InternalError", msg, http.StatusInternalServerError)
}

// errorResponse is the S3 XML error document.
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

// requestID generates a random request identifier.
func requestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeS3Error writes an S3 XML error response.
func writeS3Error(w http.ResponseWriter, r *http.Request, err *s3Error) {
	reqID := requestID()
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Amz-Request-Id", reqID)
	w.WriteHeader(err.HTTPStatus)

	if r.Method == http.MethodHead {
		return
	}

	writeXML(w, &errorResponse{
		Code:      err.Code,
		Message:   err.Message,
		Resource:  r.URL.Path,
		RequestID: reqID,
	})
}

// writeXMLResponse writes a 200 XML response document.
func writeXMLResponse(w http.ResponseWriter, status int, doc any) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("X-Amz-Request-Id", requestID())
	w.WriteHeader(status)
	writeXML(w, doc)
}

func writeXML(w http.ResponseWriter, doc any) {
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	_ = enc.Encode(doc)
}
