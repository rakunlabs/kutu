package s3serve

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// chunkedReader decodes the aws-chunked transfer encoding used by
// SigV4 streaming uploads (x-amz-content-sha256:
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD and the *-TRAILER variants).
//
// Wire format per chunk:
//
//	<hex-size>[;chunk-signature=<sig>]\r\n
//	<data>\r\n
//
// The stream ends with a zero-size chunk, optionally followed by
// trailer headers (x-amz-checksum-*) and a final CRLF. Chunk signatures
// are not re-verified here; the enclosing request is already
// authenticated via its seed signature.
type chunkedReader struct {
	br        *bufio.Reader
	remaining int64 // bytes left in the current chunk
	finished  bool
}

func newChunkedReader(r io.Reader) *chunkedReader {
	return &chunkedReader{br: bufio.NewReader(r)}
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	for {
		if c.finished {
			return 0, io.EOF
		}

		if c.remaining == 0 {
			if err := c.nextChunk(); err != nil {
				return 0, err
			}
			continue
		}

		if int64(len(p)) > c.remaining {
			p = p[:c.remaining]
		}
		n, err := c.br.Read(p)
		c.remaining -= int64(n)
		if c.remaining == 0 {
			// Consume the CRLF that terminates the chunk data.
			if crlfErr := c.expectCRLF(); crlfErr != nil && err == nil {
				err = crlfErr
			}
		}
		if err == io.EOF && !c.finished {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
}

// nextChunk reads the next chunk header. On the final (zero-size) chunk
// it drains any trailers and marks the stream finished.
func (c *chunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if line == "" {
		// Tolerate a stray blank line between chunks.
		line, err = c.readLine()
		if err != nil {
			return err
		}
	}

	sizeStr := line
	if i := strings.IndexByte(line, ';'); i >= 0 {
		sizeStr = line[:i]
	}

	size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("aws-chunked: invalid chunk size %q", sizeStr)
	}

	if size == 0 {
		// Drain trailers until an empty line or EOF.
		for {
			tline, terr := c.readLine()
			if terr != nil || tline == "" {
				break
			}
		}
		c.finished = true
		return nil
	}

	c.remaining = size
	return nil
}

func (c *chunkedReader) expectCRLF() error {
	b, err := c.br.ReadByte()
	if err != nil {
		return nil // tolerate missing terminator at EOF
	}
	if b == '\r' {
		if b2, err2 := c.br.ReadByte(); err2 == nil && b2 != '\n' {
			return fmt.Errorf("aws-chunked: malformed chunk terminator")
		}
		return nil
	}
	if b == '\n' {
		return nil
	}
	return fmt.Errorf("aws-chunked: malformed chunk terminator")
}

func (c *chunkedReader) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
