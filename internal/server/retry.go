package server

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/middleware"
)

// retryAttempt issues one upstream request. It must be safe to call
// several times, which requires a replayable body — see doUpstreamRetry's
// replayable parameter.
type retryAttempt func() (*http.Response, error)

// doUpstreamRetry issues the upstream attempt and absorbs transient 429s
// ("too many concurrent requests" at the gateway) with bounded retries.
//
// The wait happens while the caller still holds its user-queue and global
// slots, so backoff applies backpressure instead of piling more load on
// the upstream. Only 429 responses are retried, and only before any byte
// reached the client — inherent, because a 429 arrives in place of the
// response; once a different status is received the caller starts
// streaming and this function has returned.
//
// replayable is false when the request body cannot be re-sent (passthrough
// requests whose bodies were streamed through): a 429 is then counted and
// returned as-is. route labels metrics and logs ("convert" | "passthrough").
//
// The final response is always returned with its body untouched; the
// caller owns closing it. On context cancellation during a backoff the
// context error is returned.
func (s *Server) doUpstreamRetry(ctx context.Context, r *http.Request, route string, replayable bool, attempt retryAttempt) (*http.Response, error) {
	rc := s.cfg.UpstreamRetry

	resp, err := attempt()
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	s.metrics.Upstream429Total.With(route).Inc()
	if !rc.Enabled || !replayable || rc.MaxAttempts <= 1 {
		return resp, nil
	}

	backoff := rc.InitialBackoff
	attempts := 1
	start := time.Now()
	for {
		// Wait before retrying: Retry-After wins when the upstream sends
		// one, otherwise the exponential backoff, both capped.
		delay := backoff
		if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			delay = ra
		}
		if delay > rc.MaxBackoff {
			delay = rc.MaxBackoff
		}

		if attempts >= rc.MaxAttempts || time.Since(start)+delay > rc.Budget {
			s.metrics.RetryExhaustedTotal.Inc()
			s.logger.Warn("upstream 429; retry budget exhausted",
				"request_id", middleware.RequestIDOf(r), "route", route,
				"attempts", attempts, "waited_ms", time.Since(start).Milliseconds())
			return resp, nil
		}

		drainAndClose(resp)
		s.metrics.RetriesAbsorbedTotal.Inc()
		s.logger.Warn("upstream 429; backing off",
			"request_id", middleware.RequestIDOf(r), "route", route,
			"attempt", attempts, "delay_ms", delay.Milliseconds(),
			"retry_after", resp.Header.Get("Retry-After"))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		attempts++
		if backoff < rc.MaxBackoff {
			backoff *= 2
			if backoff > rc.MaxBackoff {
				backoff = rc.MaxBackoff
			}
		}
		resp, err = attempt()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			s.logger.Info("upstream 429 absorbed",
				"request_id", middleware.RequestIDOf(r), "route", route,
				"attempts", attempts, "waited_ms", time.Since(start).Milliseconds())
			return resp, nil
		}
		s.metrics.Upstream429Total.With(route).Inc()
	}
}

// drainAndClose reads a small amount of a discarded response body so the
// connection can be reused, then closes it.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// parseRetryAfter understands both Retry-After forms (delta-seconds and
// HTTP-date). Clamping to the configured cap happens at the call site.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// rejectUpstream429 answers a 429 that survived the retry budget with the
// same client contract as queue rejections — 429 plus Retry-After, so SDK
// retry logic engages — while preserving the upstream's message for
// humans. The upstream's own Retry-After is forwarded when present.
func (s *Server) rejectUpstream429(w http.ResponseWriter, resp *http.Response) {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		ra = "1"
	}
	w.Header().Set("Retry-After", ra)
	msg := upstreamErrorMessage(resp)
	if msg == "" {
		msg = "upstream concurrency limit reached; retry after the advertised delay"
	}
	s.writeProxyError(w, http.StatusTooManyRequests, msg)
}
