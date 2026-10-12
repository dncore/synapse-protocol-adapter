package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Per-model health, learned from the upstream's own answers.
//
// The shared gateway rotates a model across upstream channels, and only some
// channels are provisioned for every request shape. Measured 2026-10-10 with
// gpt-6.1-sol + tools: identical requests failed 0/8 in one stretch and
// succeeded 20/20 minutes later, the same body throughout; a live window
// lasted at least 15 minutes. During a window the retry budget is spent in
// full on every request and the client still ends up with the upstream's
// error, which is both slow and useless.
//
// So failures are remembered per model. Once a model has failed
// threshold times in a row, it is marked unavailable for the cooldown
// period; while marked, requests skip the retry budget (they still make one
// upstream attempt, which doubles as the recovery probe) and, when a
// fallback model is configured for it, are served by that model instead of
// failing. Success clears the mark immediately.
type modelHealth struct {
	mu        sync.Mutex
	failures  map[string]int
	unhealthy map[string]time.Time
	// probation marks a model whose cooldown just expired: the next request
	// tries the native path again, but with a single attempt and no retry
	// budget — a long window (measured: 1h45m and counting on 2026-10-12)
	// would otherwise pay the full budget every cooldown at every probe.
	probation map[string]bool
	threshold int
	cooldown  time.Duration
}

// maxTrackedModels bounds the health maps. Model ids come from client
// requests, so without a cap a client could grow these maps without limit by
// churning ids; ids beyond the cap are simply not tracked (fail-closed: no
// tracking means no marking, never a forged marking).
const maxTrackedModels = 512

// maxModelIDLen mirrors the length bound in the transient-error patterns.
const maxModelIDLen = 128

func newModelHealth(threshold int, cooldown time.Duration) *modelHealth {
	if threshold < 1 {
		threshold = 1
	}
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	return &modelHealth{
		failures:  make(map[string]int),
		unhealthy: make(map[string]time.Time),
		probation: make(map[string]bool),
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// noteSuccess clears any failure history for the model.
func (h *modelHealth) noteSuccess(model string) {
	if h == nil || model == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.failures, model)
	delete(h.unhealthy, model)
	delete(h.probation, model)
}

// noteFailure records one channel-rotation failure and marks the model
// unavailable once it has failed threshold times in a row. It reports
// whether this call is what flipped the model into the unavailable state.
func (h *modelHealth) noteFailure(model string) bool {
	if h == nil || model == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(model) > maxModelIDLen {
		return false
	}
	if _, tracked := h.failures[model]; !tracked && len(h.failures) >= maxTrackedModels {
		return false // fail closed rather than grow without bound
	}
	// A failing probe re-marks immediately: the model already proved it was
	// broken, so one failed attempt is enough evidence.
	probing := h.probation[model]
	delete(h.probation, model)
	h.failures[model]++
	if !probing && h.failures[model] < h.threshold {
		return false
	}
	_, already := h.unhealthy[model]
	h.unhealthy[model] = time.Now().Add(h.cooldown)
	return !already
}

// unavailable reports whether the model is in its cooldown window.
func (h *modelHealth) unavailable(model string) bool {
	if h == nil || model == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.unhealthy[model]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		// Cooldown elapsed: allow exactly one native attempt (no retries) to
		// see whether the upstream recovered.
		delete(h.unhealthy, model)
		h.probation[model] = true
		return false
	}
	return true
}

// inProbation reports whether the model's cooldown just expired and its next
// native attempt should be a single, un-retried probe.
func (h *modelHealth) inProbation(model string) bool {
	if h == nil || model == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probation[model]
}

// failureCount reports the current consecutive-failure count (for logs/tests).
func (h *modelHealth) failureCount(model string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failures[model]
}

// healthKey scopes the failure memory by request shape. The channel-rotation
// failures this package tracks are specific to tool-bearing requests — only
// the chat → responses routing those need is missing — so a tool-free
// request's success says nothing about tool-call health. Measured
// 2026-10-12 on the deployed proxy: a tool-free request succeeded mid-window
// and cleared the mark, and the next tool request paid the full retry budget
// into a 400 again. Separate buckets make each verdict depend only on
// requests of its own shape.
func healthKey(model string, hasTools bool) string {
	if model == "" {
		return ""
	}
	if hasTools {
		return model + " [tools]"
	}
	return model
}

// substitutedModel returns the model a request should actually use: the
// configured fallback when the requested model is marked unavailable in the
// request's own shape bucket, else the requested model unchanged. The second
// return value reports whether a substitution happened.
func (s *Server) substitutedModel(model string, hasTools bool) (string, bool) {
	fallbacks := s.cfg.ModelHealth.Fallbacks
	if len(fallbacks) == 0 || !s.health.unavailable(healthKey(model, hasTools)) {
		return model, false
	}
	to, ok := fallbacks[strings.ToLower(model)]
	if !ok || to == "" {
		return model, false
	}
	return to, true
}

// bodyModelShape reads the "model" field and whether the body carries a
// non-empty "tools" array out of a raw request body, for the health
// bookkeeping's shape bucket (see healthKey). It returns "" when the body is
// absent, unparsable, or carries no string model — the bookkeeping then
// simply has nothing to attribute the failure to.
func bodyModelShape(body []byte) (model string, hasTools bool) {
	if len(body) == 0 {
		return "", false
	}
	var doc struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", false
	}
	return doc.Model, hasNonEmptyArray(doc.Tools)
}

// shouldEmulate reports whether a tool-bearing request for this model should
// use the prompt protocol instead of the upstream's (currently failing)
// native tool calling. Emulation never replaces a configured fallback: a
// substitution keeps the request native.
func (s *Server) shouldEmulate(model string, hasTools bool) bool {
	if !s.cfg.ToolEmulation.Enabled || !hasTools || model == "" {
		return false
	}
	if !s.health.unavailable(healthKey(model, hasTools)) {
		return false
	}
	for _, m := range s.cfg.ToolEmulation.Models {
		if strings.EqualFold(strings.TrimSpace(m), model) {
			return true
		}
	}
	return false
}

// emulatedCallID mints a tool-call id for an emulated reply. The upstream
// never saw a call, so there is none to reuse; clients only need it to be
// stable within the conversation and distinct across calls.
func emulatedCallID() string {
	n := emulatedCallSeq.Add(1)
	return fmt.Sprintf("call_emu_%d_%d", time.Now().UnixNano(), n)
}

var emulatedCallSeq atomic.Uint64
