package converter

import (
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// The Sol ids reject reasoning_effort "none" outright ("must be low, medium,
// high, xhigh, or max") and 400 on any sampling parameter while tools are in
// the body, so the family-wide "tools ⇒ none" rule cannot serve them. A tool
// turn must instead carry an explicit effort from the model's own set, with
// the client's value kept whenever the upstream accepts it.
func TestGPT6SolToolTurnShape(t *testing.T) {
	effortCases := []struct {
		name string
		want string
		out  string
	}{
		{name: "accepted value is kept", want: "high", out: "high"},
		{name: "max is accepted", want: "max", out: "max"},
		{name: "none is clamped to the floor", want: "none", out: "low"},
		{name: "minimal is clamped to the floor", want: "minimal", out: "low"},
		{name: "omitted effort becomes explicit", want: "", out: "low"},
	}
	for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		for _, tc := range effortCases {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				if got := NormalizeGPT6Effort(model, tc.want, true); got != tc.out {
					t.Fatalf("tools + effort %q: got %q, want %q", tc.want, got, tc.out)
				}
			})
		}
	}
}

// Without tools the Sol ids reason normally: an accepted effort passes
// through, an unaccepted one is dropped rather than sent to a 400.
func TestGPT6SolNoToolTurnShape(t *testing.T) {
	if got := NormalizeGPT6Effort("gpt-6-sol", "high", false); got != "high" {
		t.Fatalf("no tools, accepted effort: got %q, want high", got)
	}
	if got := NormalizeGPT6Effort("gpt-6-sol", "none", false); got != "" {
		t.Fatalf("no tools, rejected effort must be dropped: got %q", got)
	}
}

// Sol drops sampling on tool turns only — without tools it accepts what the
// client sends, so the parameter must survive there.
func TestGPT6SolSamplingPolicy(t *testing.T) {
	if !GPT6StripsSampling("gpt-6-sol", true, "low") {
		t.Fatal("tools present: sampling must be dropped")
	}
	if GPT6StripsSampling("gpt-6-sol", false, "low") {
		t.Fatal("no tools: sampling must survive")
	}
	// luna keeps the older effort-based rule: sampling survives while the
	// effort is an explicit "none" (tools force it), and is dropped otherwise.
	if GPT6StripsSampling("gpt-6-luna", true, "none") {
		t.Fatal("luna with tools forces none — sampling must survive, measured 2026-10-10")
	}
	if !GPT6StripsSampling("gpt-6-luna", false, "high") {
		t.Fatal("luna reasoning on: sampling must be dropped")
	}
}

// Ids the table does not name (a future member, a dated alias) take the
// conservative default: an explicit floor effort on tool turns and sampling
// dropped there, which every measured member accepts.
func TestGPT6UnknownIDFallsBackToSafeShape(t *testing.T) {
	if got := NormalizeGPT6Effort("gpt-6-nebula-2027", "none", true); got != "low" {
		t.Fatalf("unknown gpt-6 id with tools: got %q, want low", got)
	}
	if !GPT6StripsSampling("gpt-6-nebula-2027", true, "low") {
		t.Fatal("unknown gpt-6 id with tools: sampling must be dropped")
	}
	if !IsGPT6("GPT-6-Sol") {
		t.Fatal("id matching stays case-insensitive")
	}
}

// End to end through the Responses route: the shape a Codex-style client
// produces for a Sol model must carry max_completion_tokens, an accepted
// effort and no sampling parameters.
func TestConvertRequest_GPT6SolToolTurn(t *testing.T) {
	temp, topP, maxTok := 0.7, 0.9, 4096
	req := &responses.Request{
		Model:           "gpt-6.1-sol",
		Input:           responses.Input{String: "hi"},
		Tools:           []responses.Tool{{Type: "function", Name: "get_time"}},
		Temperature:     &temp,
		TopP:            &topP,
		MaxOutputTokens: &maxTok,
		Reasoning:       &responses.Reasoning{Effort: "none"},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningEffort != "low" {
		t.Fatalf("effort: got %q, want low (none is rejected by the Sol backend)", out.ReasoningEffort)
	}
	if out.Temperature != nil || out.TopP != nil {
		t.Fatalf("sampling must be dropped on a Sol tool turn, got temp=%v top_p=%v", out.Temperature, out.TopP)
	}
	if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != maxTok || out.MaxTokens != nil {
		t.Fatalf("max_output_tokens must land in max_completion_tokens only: %+v", out)
	}
}
