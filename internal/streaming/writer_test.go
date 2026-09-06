package streaming

import "net/http/httptest"

// recorder is a ResponseWriter capturing output and flushes.
type recorder struct {
	httptest.ResponseRecorder
	flushes int
}

func (r *recorder) Flush() { r.flushes++ }

func newRecorder() *recorder {
	rr := httptest.NewRecorder() // initializes Body and headers
	return &recorder{ResponseRecorder: *rr}
}

