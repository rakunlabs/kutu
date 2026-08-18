package s3serve

import (
	"io"
	"strings"
	"testing"
)

func TestChunkedReaderSigned(t *testing.T) {
	// Two data chunks + zero terminator, signatures present.
	raw := "5;chunk-signature=aaaa\r\n" +
		"hello\r\n" +
		"6;chunk-signature=bbbb\r\n" +
		" world\r\n" +
		"0;chunk-signature=cccc\r\n" +
		"\r\n"

	got, err := io.ReadAll(newChunkedReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

func TestChunkedReaderWithTrailers(t *testing.T) {
	// aws cli v2 style: unsigned chunks with a checksum trailer.
	raw := "b\r\n" +
		"hello world\r\n" +
		"0\r\n" +
		"x-amz-checksum-crc32:sOO8/Q==\r\n" +
		"\r\n"

	got, err := io.ReadAll(newChunkedReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

func TestChunkedReaderSmallReads(t *testing.T) {
	raw := "3;chunk-signature=aa\r\nabc\r\n" +
		"3;chunk-signature=bb\r\ndef\r\n" +
		"0;chunk-signature=cc\r\n\r\n"

	r := newChunkedReader(strings.NewReader(raw))
	var out []byte
	buf := make([]byte, 2) // force reads across chunk boundaries
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if string(out) != "abcdef" {
		t.Fatalf("got %q, want %q", out, "abcdef")
	}
}

func TestChunkedReaderTruncated(t *testing.T) {
	raw := "a;chunk-signature=aa\r\nhello" // promises 10 bytes, gives 5

	_, err := io.ReadAll(newChunkedReader(strings.NewReader(raw)))
	if err == nil {
		t.Fatal("truncated stream should error")
	}
}

func TestChunkedReaderInvalidSize(t *testing.T) {
	raw := "zz;chunk-signature=aa\r\nhello\r\n"

	_, err := io.ReadAll(newChunkedReader(strings.NewReader(raw)))
	if err == nil {
		t.Fatal("invalid chunk size should error")
	}
}
