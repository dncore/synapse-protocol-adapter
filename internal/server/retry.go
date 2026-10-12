package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/middleware"
)

// retryAttempt issues one upstream request. It must be safe to call
// several times, which requires a replayable body — see doUpstreamRetry's
// replayable parameter.
type retryAttempt func() (*http.Response, error)

// transientUpstreamPatterns are the gateway's channel-rotation errors.
//
// They are anchored at the START of the upstream's own error message, with
// only the model-id slot left free, because the body is client-influenced:
// the gateway echoes model ids and parameter values into its error text, and
// a match here flips process-global state (model_health marks the model
// unavailable, which can switch it to emulation or a fallback for EVERY
// user). A client must not be able to forge a "the channel is broken"
// verdict by naming a model or a field after a marker phrase, so the match
// is structural — parse the envelope, take the gateway's message, require
// the gateway's template — rather than a substring scan over raw bytes.
//
// Measured samples (2026-10-10/12, all of them start the message):
//
//	GPT-6: gpt-6.1-sol tool calling requires /v1/responses or configured Chat-to-Responses routing  (type: api_upstream_error) (request_id: …)
//	GPT-6: gpt-6.1-sol reasoning effort must be low, medium, high, xhigh, or max  (type: api_upstream_error) (request_id: …)
//	Unsupported value: 'reasoning_effort' does not support 'none' with this model. Supported values are: 'low', …
//	Unsupported value: 'none' is not supported with the 'gpt-6.1-sol-2026-09-29' model. …
//	Model resources are currently busy. Please try again later. (request_id: …)
//	Insufficient quota available for instant inference. (type: deployment_disabled) (request_id: …)
//	Provider returned 400   ← only inside a 200 SSE frame; no trailing text (measured 2026-10-12)
var transientUpstreamPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^GPT-6: [A-Za-z0-9._\-/]{1,128} tool calling requires /v1/responses or configured Chat-to-Responses routing`),
	regexp.MustCompile(`^GPT-6: [A-Za-z0-9._\-/]{1,128} reasoning effort must be low, medium, high, xhigh, or max`),
	regexp.MustCompile(`^Unsupported value: 'reasoning_effort' does not support 'none' with this model`),
	regexp.MustCompile(`^Unsupported value: 'none' is not supported with the '[A-Za-z0-9._\-/]{1,128}' model`),
	regexp.MustCompile(`^Model resources are currently busy`),
	regexp.MustCompile(`^Insufficient quota available for instant inference`),
	// The streaming presentation of any of the failures above: the gateway has
	// already sent 200 + text/event-stream headers when its upstream fails, so
	// the error arrives as the first SSE frame with this generic message and
	// the real text is lost. Full-match: the entire message is only this
	// sentence, nothing client-supplied can ride along.
	regexp.MustCompile(`^Provider returned [45]\d\d$`),
}

// upstreamErrorText pulls the human message out of an upstream error body.
// Gateways nest it differently per mount ({"detail":{"message":…}},
// {"error":{"message":…}}, and the Anthropic mount wraps a JSON string), so
// the extraction is bounded and recursive rather than a scan.
func upstreamErrorText(body []byte) string {
	const maxDepth = 3
	text := string(body)
	for depth := 0; depth < maxDepth; depth++ {
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, "{") {
			return trimmed
		}
		var env struct {
			Detail struct {
				Message string `json:"message"`
			} `json:"detail"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
			return trimmed
		}
		next := env.Detail.Message
		if next == "" {
			next = env.Error.Message
		}
		if next == "" {
			next = env.Message
		}
		if next == "" {
			return trimmed
		}
		text = next
	}
	return strings.TrimSpace(text)
}

// isCandidateTransientStatus reports whether a status can carry a
// channel-rotation error at all. Kept separate so the (blocking) body peek
// runs for error responses only.
func isCandidateTransientStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusServiceUnavailable
}

// isTransientUpstreamResponse reports whether a non-2xx upstream response is
// worth retrying with the identical body, and whether it may be attributed to
// the model as a gateway/channel failure.
func isTransientUpstreamResponse(status int, body []byte) bool {
	if !isCandidateTransientStatus(status) {
		return false
	}
	message := upstreamErrorText(body)
	if message == "" {
		return false
	}
	for _, pattern := range transientUpstreamPatterns {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// startSSEFrameClassifier watches a streaming 2xx response for the gateway's
// streamed channel-rotation error.
//
// The gateway reports an upstream failure that happens after the stream's
// 200 headers already went out as the stream's FIRST frame — data:
// {"error":{"message":"Provider returned 400",…}} — which the status-based
// classification can never see (measured 2026-10-12: the identical
// non-streaming request got a 400 with the gateway's detailed message while
// streaming clients got this generic frame, so every streamed request
// bypassed retries, health marks and emulation entirely).
//
// The body is pumped in a goroutine that owns it from here on: every byte is
// forwarded through a pipe unchanged, the first frame is classified on the
// way, and the verdict is delivered on the returned channel (false on the
// closed channel for clean streams and non-SSE responses). Callers wait for
// the verdict with a short bound: the error frame arrives immediately, while
// a real stream's first frame is simply forwarded as it comes — a slow first
// token is never held back.
func startSSEFrameClassifier(resp *http.Response) <-chan bool {
	verdict := make(chan bool, 1)
	if resp == nil || resp.Body == nil ||
		!strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		close(verdict)
		return verdict
	}
	orig := resp.Body
	pr, pw := io.Pipe()
	resp.Body = struct {
		io.Reader
		io.Closer
	}{pr, orig}
	go func() {
		defer pw.Close()
		br := bufio.NewReaderSize(orig, 16<<10)
		var frame bytes.Buffer
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if int64(frame.Len()+len(line)) > 4<<10 {
					break // first frame implausibly large: not the error frame
				}
				frame.Write(line)
				if len(bytes.TrimRight(line, "\r\n")) == 0 {
					break // blank line: end of the first frame
				}
			}
			if err != nil {
				break
			}
		}
		// The verdict is sent for every non-empty first frame — true for the
		// gateway's error frame, false for a real one — and the channel closes
		// right away (not at stream end), so a watcher resolves the moment the
		// frame is classified. An empty frame (stream aborted before any
		// bytes) sends nothing: there is no verdict to record.
		if frame.Len() > 0 {
			verdict <- isTransientStreamError(frame.Bytes())
		}
		close(verdict)
		if _, err := pw.Write(frame.Bytes()); err != nil {
			return // consumer went away
		}
		_, _ = io.Copy(pw, br)
	}()
	return verdict
}

// watchStreamVerdict records the classifier's verdict whenever it lands —
// without waiting for it. Measured 2026-10-12: the gateway's error frame can
// take several seconds to arrive (its headers are immediate; its body waits
// on the gateway's own upstream timeout), while a reasoning model's first
// real token can take a minute. Waiting before returning the response would
// stall the first group of streams and still miss the second — recording
// late is enough, because the mark decides the NEXT request (which is how
// the non-streaming path recovers too).
func (s *Server) watchStreamVerdict(verdict <-chan bool, route, model string, r *http.Request) {
	if model == "" {
		return // nothing to record against
	}
	go func() {
		bad, ok := <-verdict
		if !ok {
			// Stream ended (or aborted) before any frame: no verdict to record.
			return
		}
		if bad {
			if s.health.noteFailure(model) {
				s.logger.Warn("model marked unavailable after repeated channel-rotation failures (streamed frame)",
					"request_id", middleware.RequestIDOf(r), "route", route, "model", model,
					"threshold", s.cfg.ModelHealth.FailureThreshold,
					"cooldown", s.cfg.ModelHealth.Cooldown.String())
			}
			return
		}
		s.health.noteSuccess(model)
	}()
}

// isTransientStreamError reports whether a peeked SSE frame carries the
// gateway's streamed channel-rotation error. Classification reuses the
// envelope extraction and pattern set of isTransientUpstreamResponse; the
// 400 is only the pattern-enablement gate (the classification is about the
// message text, which is status-independent).
func isTransientStreamError(frame []byte) bool {
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if isTransientUpstreamResponse(http.StatusBadRequest, []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))) {
			return true
		}
	}
	return false
}

// peekBody reads up to limit bytes of the response body and restores the
// stream, so callers can inspect an error body without consuming it.
func peekBody(resp *http.Response, limit int64) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		// Could not read: hand the caller what we got and let it stream on.
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix), resp.Body), resp.Body}
		return prefix
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix), resp.Body), resp.Body}
	return prefix
}

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
func (s *Server) doUpstreamRetry(ctx context.Context, r *http.Request, route string, replayable bool, model string, attempt retryAttempt) (*http.Response, error) {
	rc := s.cfg.UpstreamRetry
	// A model the gateway has been failing gets one attempt per request
	// instead of the full retry budget: inside a failure window the retries
	// cannot succeed (measured windows last 15+ minutes), and that single
	// attempt doubles as the recovery probe.
	canRetry := rc.Enabled && replayable && rc.MaxAttempts > 1 &&
		!s.health.unavailable(model) && !s.health.inProbation(model)
	if model != "" && s.health.unavailable(model) {
		// Served while the model is still marked unavailable: exactly one
		// upstream attempt instead of the retry budget. Emulated and
		// fallback-substituted requests carry an empty or different model
		// here, so this counts the fast-fail path only.
		s.metrics.ModelUnavailableTotal.With("single_attempt").Inc()
	}

	resp, err := attempt()
	if err != nil {
		return resp, err
	}
	// Error statuses are classified from their body. A streaming 2xx is
	// additionally watched for the gateway's streamed error frame (see
	// startSSEFrameClassifier): the first frame is classified while a pump
	// goroutine forwards every byte unchanged, and the verdict is recorded
	// asynchronously (watchStreamVerdict) — headers and slow first tokens are
	// never held back, and the streamed failure still marks the model for the
	// next request. Non-SSE 2xx bodies are never read here.
	if resp.StatusCode != http.StatusTooManyRequests {
		transient := isCandidateTransientStatus(resp.StatusCode) &&
			isTransientUpstreamResponse(resp.StatusCode, peekBody(resp, 4<<10))
		if !transient && resp.StatusCode < http.StatusBadRequest &&
			strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			s.watchStreamVerdict(startSSEFrameClassifier(resp), route, model, r)
			return resp, nil
		}
		if !transient {
			if resp.StatusCode < http.StatusBadRequest {
				s.health.noteSuccess(model)
			}
			return resp, nil
		}
		// Channel-rotation failure (see transientUpstreamMarkers): identical
		// request, identical body — only the channel behind it differs.
		if s.health.noteFailure(model) {
			s.logger.Warn("model marked unavailable after repeated channel-rotation failures",
				"request_id", middleware.RequestIDOf(r), "route", route, "model", model,
				"threshold", s.cfg.ModelHealth.FailureThreshold,
				"cooldown", s.cfg.ModelHealth.Cooldown.String())
		}
		if !canRetry {
			return resp, nil
		}
		// retryTransientUpstream accounts for the final response itself
		// (success — immediate or streamed-verdict — or nothing).
		return s.retryTransientUpstream(ctx, r, route, model, attempt, resp)
	}
	s.metrics.Upstream429Total.With(route).Inc()
	if !canRetry {
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

// retryTransientUpstream re-sends a request whose first response was a
// channel-rotation failure. Same budget/backoff machinery as the 429 path:
// the retry happens while the caller still holds its queue slots (backpressure,
// not piling on load), and the last response is returned untouched for the
// caller to relay. The final response is accounted for here: an immediate
// success is recorded now, a streaming success is recorded when its first
// frame is classified (watchStreamVerdict), and an exhausted budget records
// nothing further — the first attempt already counted the failure.
func (s *Server) retryTransientUpstream(ctx context.Context, r *http.Request, route, model string, attempt retryAttempt, resp *http.Response) (*http.Response, error) {
	rc := s.cfg.UpstreamRetry
	backoff := rc.InitialBackoff
	attempts := 1
	start := time.Now()
	for {
		delay := backoff
		if delay > rc.MaxBackoff {
			delay = rc.MaxBackoff
		}
		if attempts >= rc.MaxAttempts || time.Since(start)+delay > rc.Budget {
			s.metrics.RetryExhaustedTotal.Inc()
			s.logger.Warn("upstream returned a channel-rotation error; retry budget exhausted",
				"request_id", middleware.RequestIDOf(r), "route", route,
				"status", resp.StatusCode, "attempts", attempts)
			return resp, nil
		}
		drainAndClose(resp)
		s.metrics.RetriesAbsorbedTotal.Inc()
		s.logger.Warn("upstream returned a channel-rotation error; retrying the same body",
			"request_id", middleware.RequestIDOf(r), "route", route,
			"status", resp.StatusCode, "attempt", attempts, "delay_ms", delay.Milliseconds())
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
		resp, err := attempt()
		if err != nil {
			return nil, err
		}
		if isCandidateTransientStatus(resp.StatusCode) &&
			isTransientUpstreamResponse(resp.StatusCode, peekBody(resp, 4<<10)) {
			continue // still transient: spend more of the budget
		}
		if resp.StatusCode < http.StatusBadRequest {
			if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
				s.watchStreamVerdict(startSSEFrameClassifier(resp), route, model, r)
			} else {
				s.health.noteSuccess(model)
			}
		}
		return resp, nil
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
