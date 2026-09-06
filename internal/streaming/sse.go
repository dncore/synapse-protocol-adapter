// Package streaming implements a streaming SSE reader and writer robust
// against arbitrary TCP segmentation: events split across reads, very long
// data lines, multi-line data fields, and interleaved comments.
package streaming

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// ErrDone is returned by Reader.Next when the upstream stream ended with
// the [DONE] sentinel. A stream that ends without the sentinel (or with an
// I/O error) surfaces that error instead.
var ErrDone = errors.New("sse: stream done")

// Reader incrementally parses an SSE byte stream into data payloads.
// It never buffers more than one event at a time.
type Reader struct {
	sc *bufio.Scanner
	// pending accumulates the data lines of the event currently being read.
	pending []string
}

// NewReader wraps r. maxLine bounds the largest accepted data line; SSE
// payloads from LLM providers carry whole JSON chunks and can reach
// hundreds of kilobytes when tool arguments are large.
func NewReader(r io.Reader, maxLine int) *Reader {
	sc := bufio.NewScanner(r)
	start := 64 * 1024
	if maxLine < start {
		start = maxLine
	}
	sc.Buffer(make([]byte, 0, start), maxLine)
	sc.Split(splitLines) // tolerate \r\n and bare \r
	return &Reader{sc: sc}
}

// Next returns the joined data payload of the next event. It returns
// ErrDone on the [DONE] sentinel, io.EOF on clean EOF without the sentinel,
// and the underlying error on malformed overlong lines.
func (r *Reader) Next() (string, error) {
	for {
		ok := r.sc.Scan()
		if !ok {
			if err := r.sc.Err(); err != nil {
				return "", err
			}
			// EOF: flush any trailing event that lacked a blank line.
			if len(r.pending) > 0 {
				data := strings.Join(r.pending, "\n")
				r.pending = nil
				if data == "[DONE]" {
					return "", ErrDone
				}
				return data, nil
			}
			return "", io.EOF
		}

		line := r.sc.Text()
		switch {
		case line == "":
			// Event boundary: emit if we have data.
			if len(r.pending) > 0 {
				data := strings.Join(r.pending, "\n")
				r.pending = nil
				if data == "[DONE]" {
					return "", ErrDone
				}
				return data, nil
			}
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive; ignore.
		case strings.HasPrefix(line, "data:"):
			r.pending = append(r.pending, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"),
			strings.HasPrefix(line, "id:"),
			strings.HasPrefix(line, "retry:"):
			// chat completions streams are self-describing JSON; we do not
			// need the SSE event name, only the data payload.
		default:
			// Unknown field per the SSE spec is ignored.
		}
	}
}

// splitLines is a bufio.SplitFunc splitting on \n, \r\n, or lone \r.
func splitLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, trimCR(data[:i]), nil
	}
	if i := bytes.IndexByte(data, '\r'); i >= 0 {
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, trimCR(data[:i]), nil
			}
			return i + 1, trimCR(data[:i]), nil
		}
		// \r is the last byte; decide at EOF.
		if atEOF {
			return i + 1, trimCR(data[:i]), nil
		}
		return 0, nil, nil
	}
	if atEOF {
		return len(data), trimCR(data), nil
	}
	return 0, nil, nil
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}
