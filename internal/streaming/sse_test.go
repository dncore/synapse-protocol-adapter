package streaming

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// collect drains a reader into payloads until ErrDone/EOF.
func collect(t *testing.T, r *Reader) ([]string, error) {
	t.Helper()
	var out []string
	for {
		s, err := r.Next()
		if err != nil {
			return out, err
		}
		out = append(out, s)
	}
}

func TestReader_BasicEvents(t *testing.T) {
	in := "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if err == nil || !errors.Is(err, ErrDone) {
		t.Fatalf("want ErrDone, got %v", err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"a":2}` {
		t.Fatalf("payloads wrong: %q", got)
	}
}

func TestReader_ByteAtATime(t *testing.T) {
	// Feed the stream through a reader that returns ONE byte per Read call,
	// simulating the most hostile TCP segmentation possible.
	in := "data: {\"delta\":\"你好\"}\n\r\n:data: keepalive\n\ndata: [DONE]\n\n"
	slow := &oneByteReader{r: strings.NewReader(in)}
	r := NewReader(slow, 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) {
		t.Fatalf("want ErrDone, got %v", err)
	}
	if len(got) != 1 || got[0] != `{"delta":"你好"}` {
		t.Fatalf("payloads wrong: %q", got)
	}
}

func TestReader_UTF8SplitAcrossReads(t *testing.T) {
	// Multi-byte UTF-8 inside the JSON payload must survive arbitrary
	// chunk boundaries (the parser works on whole lines only).
	in := "data: {\"t\":\"日本語テキスト\"}\n\ndata: [DONE]\n\n"
	r := NewReader(&oneByteReader{r: strings.NewReader(in)}, 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) {
		t.Fatalf("err: %v", err)
	}
	if got[0] != `{"t":"日本語テキスト"}` {
		t.Fatalf("utf8 mangled: %q", got[0])
	}
}

func TestReader_NoSpaceAfterColon(t *testing.T) {
	in := "data:{\"a\":1}\n\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) || len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("payloads wrong: %q err=%v", got, err)
	}
}

func TestReader_MultiLineData(t *testing.T) {
	in := "data: line1\ndata: line2\n\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0] != "line1\nline2" {
		t.Fatalf("multi-line join wrong: %q", got)
	}
}

func TestReader_CRLFAndCRLineEndings(t *testing.T) {
	in := "data: a\r\n\r\ndata: b\r\rdata: [DONE]\r\r"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("payloads wrong: %q", got)
	}
}

func TestReader_EventNameIgnored(t *testing.T) {
	in := "event: message\ndata: {\"a\":1}\n\nid: 7\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) || len(got) != 1 {
		t.Fatalf("payloads wrong: %q err=%v", got, err)
	}
}

func TestReader_EOFWithoutDone(t *testing.T) {
	in := "data: {\"a\":1}\n\n" // stream cut off
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("payloads wrong: %q", got)
	}
}

func TestReader_TrailingEventWithoutBlankLine(t *testing.T) {
	in := "data: {\"a\":1}\n\ndata: {\"a\":2}" // no final newline
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if len(got) != 2 || got[1] != `{"a":2}` {
		t.Fatalf("trailing event lost: %q", got)
	}
}

func TestReader_LongLineWithinLimit(t *testing.T) {
	big := strings.Repeat("x", 300*1024)
	in := "data: " + big + "\n\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) || len(got) != 1 || len(got[0]) != 300*1024 {
		t.Fatalf("long line mishandled: len=%d err=%v", len(got), err)
	}
}

func TestReader_OverlongLine(t *testing.T) {
	big := strings.Repeat("x", 200)
	in := "data: " + big + "\n\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 100)
	_, err := collect(t, r)
	if err == nil || errors.Is(err, ErrDone) {
		t.Fatalf("expected overlong-line error, got %v", err)
	}
}

func TestReader_CommentsAndKeepAlives(t *testing.T) {
	in := ": ping\n\n: ping\n\ndata: {\"a\":1}\n\n: ping\n\ndata: [DONE]\n\n"
	r := NewReader(strings.NewReader(in), 1<<20)
	got, err := collect(t, r)
	if !errors.Is(err, ErrDone) || len(got) != 1 {
		t.Fatalf("payloads wrong: %q err=%v", got, err)
	}
}

func TestWriter_EventFormat(t *testing.T) {
	rec := newRecorder()
	w := NewWriter(rec)
	w.Start()
	if err := w.WriteEvent("response.created", map[string]int{"sequence_number": 1}); err != nil {
		t.Fatal(err)
	}
	want := "event: response.created\ndata: {\"sequence_number\":1}\n\n"
	if rec.Body.String() != want {
		t.Fatalf("wire format wrong: %q", rec.Body.String())
	}
	if rec.flushes < 2 { // Start + event
		t.Fatalf("must flush per event, flushes=%d", rec.flushes)
	}
}

// oneByteReader returns at most one byte per Read, defeating any buffering
// assumptions.
type oneByteReader struct {
	r io.Reader
}

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
