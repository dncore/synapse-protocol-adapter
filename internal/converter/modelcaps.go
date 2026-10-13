package converter

import "strings"

// ---------------------------------------------------------------------------
// gpt-6 family capabilities — measured per model, not per family
// ---------------------------------------------------------------------------
//
// The shared gateway serves the gpt-6 ids through different upstream channels,
// so a rule that holds for one member can hard-fail another. Everything below
// is a measurement, dated, with the shape that produced it; re-measure before
// changing it — one tool-bearing chat request per shape is enough to tell the
// members apart, and the upstream's own 400 bodies list the accepted effort
// values verbatim.
//
// 2026-10-10, relay /chat/completions, one function tool in the body,
// four attempts per shape:
//
//	gpt-6-luna    tools + none|low|high|omitted   → 200 tool_calls (4/4)
//	              tools + top_p                   → 200 (2/4), 400 (2/4)
//	gpt-6-sol     tools + low                     → 200 tool_calls (4/4)
//	              tools + omitted                 → 400 (3/4)  ← must be explicit
//	              tools + none                    → 400 "must be low, medium, high, xhigh, or max"
//	gpt-6.1-sol   tools + low                     → 200 tool_calls (4/4)
//	              tools + none                    → 400 (same as above)
//	all three     tools + temperature or top_p    → 400 (4/4 on both Sol ids)
//
// Accepted sets, verbatim from the upstream's own 400 bodies:
//
//	gpt-6-luna     none, low, medium, high, xhigh
//	gpt-6-sol      low, medium, high, xhigh   (the alias backend also lists max)
//	gpt-6.1-sol    low, medium, high, xhigh, max
//
// What the table encodes:
//
//  1. A tool-bearing Sol request must carry an explicit effort from that
//     model's set: an omitted effort is not accepted either (3/4 400s on
//     gpt-6-sol), so clients asking for "none"/"off" are clamped to the
//     model's floor ("low") instead of being rejected. This is the fix for
//     the family-wide "tools ⇒ none" rule that 400s every Sol tool turn.
//  2. A tool-bearing luna request keeps the client's own effort whenever the
//     model accepts it, and falls back to "none" only when it is absent or
//     outside luna's set. The older rule forced "none" on every tool turn,
//     which silently discarded what the client asked for — Codex sends
//     reasoning_effort=high on every turn, so luna never reasoned under it.
//     Re-measured 2026-10-13 against the shared gateway, one function tool
//     per request, four attempts per value: none / low / medium / high /
//     xhigh / omitted all returned 200 with finish_reason=tool_calls,
//     streaming and non-streaming alike. The sibling client-config projects
//     dropped the forcing on 2026-10-10; this table was the last one still
//     applying it.
//  3. A tool-bearing Sol request must not carry temperature/top_p; luna
//     keeps the older effort-based rule (sampling survives while reasoning
//     is explicitly off).
//
// Invalidation: when the gateway needs none of this — e.g. once its chat →
// responses routing covers every channel — empty the tables and the dialect
// collapses to the max_tokens rename. Re-measure per id, not per family: the
// members already disagree.

// Sampling-strip policies.
const (
	// samplingWhenReasoningOn drops temperature/top_p unless the request
	// turns reasoning off with an explicit "none".
	samplingWhenReasoningOn = iota
	// samplingOnTools drops them whenever the request carries tools, whatever
	// the effort says.
	samplingOnTools
)

// modelCaps is the measured chat-route capability set of one model id.
type modelCaps struct {
	// efforts is the reasoning_effort set the upstream accepts.
	efforts map[string]bool
	// toolsEffort is the effort a tool-bearing request carries: always, when
	// forceToolsEffort is set; otherwise whenever the client's value is
	// absent or outside efforts.
	toolsEffort string
	// forceToolsEffort makes toolsEffort win over an accepted client value.
	forceToolsEffort bool
	// stripSampling selects when temperature/top_p must be dropped.
	stripSampling int
}

// gpt6Caps is keyed by the exact, lowercased model id.
var gpt6Caps = map[string]modelCaps{
	"gpt-6-luna": {
		// toolsEffort is the fallback only: the client's own value wins
		// whenever luna accepts it (see the measurement note above).
		efforts:          effortSet("none", "low", "medium", "high", "xhigh"),
		toolsEffort:      "none",
		forceToolsEffort: false,
		stripSampling:    samplingWhenReasoningOn,
	},
	"gpt-6-sol": {
		efforts:       effortSet("low", "medium", "high", "xhigh", "max"),
		toolsEffort:   "low",
		stripSampling: samplingOnTools,
	},
	"gpt-6.1-sol": {
		efforts:       effortSet("low", "medium", "high", "xhigh", "max"),
		toolsEffort:   "low",
		stripSampling: samplingOnTools,
	},
}

// gpt6Default covers ids this table does not name yet (new members, dated and
// aliased ids). "low" is inside every measured member's set, so a tool turn
// always has a shape at least one channel accepts; sampling is dropped there
// because that is the stricter of the two measured policies.
var gpt6Default = modelCaps{
	efforts:       effortSet("low", "medium", "high", "xhigh"),
	toolsEffort:   "low",
	stripSampling: samplingOnTools,
}

func effortSet(values ...string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

// capsForGPT6 resolves the capability set for a gpt-6 model id; named ids win,
// everything else gets the conservative default.
func capsForGPT6(model string) modelCaps {
	if caps, ok := gpt6Caps[strings.ToLower(model)]; ok {
		return caps
	}
	return gpt6Default
}

// NormalizeGPT6Effort returns the reasoning_effort a gpt-6 chat request should
// carry. A tool-bearing request always ends up with an explicit value the
// model accepts (the client's own where that is accepted, the model's floor
// otherwise); a request without tools keeps an accepted value and drops one
// the upstream would reject.
//
// The empty string means "send no reasoning_effort at all".
func NormalizeGPT6Effort(model, want string, hasTools bool) string {
	caps := capsForGPT6(model)
	if hasTools {
		if caps.forceToolsEffort || !caps.efforts[want] {
			return caps.toolsEffort
		}
		return want
	}
	if want == "" || caps.efforts[want] {
		return want
	}
	return ""
}

// GPT6StripsSampling reports whether temperature/top_p have to be dropped for
// this request shape. effort is the effective effort after NormalizeGPT6Effort.
func GPT6StripsSampling(model string, hasTools bool, effort string) bool {
	switch capsForGPT6(model).stripSampling {
	case samplingOnTools:
		return hasTools
	default:
		return effort != "none"
	}
}
