package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsModelsPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/models":                   true,
		"/v1/models/":                  true,
		"/api/v1/models":               true,
		"/api/anthropic/v1/models":     true,
		"/api/anthropic/v1/models/":    true,
		"/v1/chat/completions":         false,
		"/v1/models/Recommend/preview": false,
		"/api/anthropic/v1/messages":   false,
		"/models":                      true, // depth is the caller's business
	}
	for path, want := range cases {
		if got := isModelsPath(path); got != want {
			t.Errorf("isModelsPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func catalogue(t *testing.T) []byte {
	t.Helper()
	// Shape observed from the magene gateway: display_name is a description,
	// the id is the only identifying field, plus provider-specific extras.
	return []byte(`{"data":[` +
		`{"id":"Recommend","display_name":"按当前模型能力推荐使用的模型","type":"model","context_window":1000000},` +
		`{"id":"deepseek-v4.1-flash","display_name":"DeepSeek 快速版","type":"model"},` +
		`{"id":"no-name","type":"model"},` +
		`{"display_name":"no id at all"}` +
		`]}`)
}

func TestRewriteModelsDisplayID(t *testing.T) {
	out, ok := rewriteModelsDisplay(catalogue(t), "id")
	if !ok {
		t.Fatal("rewriteModelsDisplay returned not-ok")
	}
	var doc struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	want := map[string]string{
		"Recommend":           "Recommend",
		"deepseek-v4.1-flash": "deepseek-v4.1-flash",
		"no-name":             "no-name",
		"":                    "no id at all", // left alone: nothing to name it after
	}
	for _, e := range doc.Data {
		if e.DisplayName != want[e.ID] {
			t.Errorf("id=%q display_name = %q, want %q", e.ID, e.DisplayName, want[e.ID])
		}
	}
}

func TestRewriteModelsDisplayIDDesc(t *testing.T) {
	out, ok := rewriteModelsDisplay(catalogue(t), "id-desc")
	if !ok {
		t.Fatal("rewriteModelsDisplay returned not-ok")
	}
	var doc struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	want := map[string]string{
		"Recommend":           "Recommend — 按当前模型能力推荐使用的模型",
		"deepseek-v4.1-flash": "deepseek-v4.1-flash — DeepSeek 快速版",
		"no-name":             "no-name",
		"":                    "no id at all",
	}
	for _, e := range doc.Data {
		if e.DisplayName != want[e.ID] {
			t.Errorf("id=%q display_name = %q, want %q", e.ID, e.DisplayName, want[e.ID])
		}
	}
}

func TestRewriteModelsDisplayPreservesExtras(t *testing.T) {
	out, ok := rewriteModelsDisplay(catalogue(t), "id")
	if !ok {
		t.Fatal("rewriteModelsDisplay returned not-ok")
	}
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.Data[0]["context_window"] == nil {
		t.Error("provider-specific fields were dropped from the entry")
	}
	if doc.Data[0]["type"] != "model" {
		t.Errorf("type = %v, want model", doc.Data[0]["type"])
	}
}

func TestRewriteModelsDisplayRejectsNonCatalogue(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"object":"list"}`,
		`{"data":"not an array"}`,
		`[]`,
	} {
		if _, ok := rewriteModelsDisplay([]byte(body), "id"); ok {
			t.Errorf("rewriteModelsDisplay(%q) claimed success on a shape it does not understand", body)
		}
	}
}

func TestRewriteModelsDisplayIdempotent(t *testing.T) {
	once, ok := rewriteModelsDisplay(catalogue(t), "id-desc")
	if !ok {
		t.Fatal("first pass failed")
	}
	twice, ok := rewriteModelsDisplay(once, "id-desc")
	if !ok {
		t.Fatal("second pass failed")
	}
	if string(once) != string(twice) {
		t.Error("rewriting twice changed the result; the id prefix guard is not working")
	}
}

// --- model aliases ---------------------------------------------------------

func TestRewriteModelAliasesResolves(t *testing.T) {
	aliases := map[string]string{"claude-sonnet-4-5": "deepseek-v4.1-flash"}
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":32000,"messages":[{"role":"user","content":"hi"}]}`)

	out, from, to, ok := rewriteModelAliases(body, aliases)
	if !ok {
		t.Fatal("alias must resolve")
	}
	if from != "claude-sonnet-4-5" || to != "deepseek-v4.1-flash" {
		t.Fatalf("from/to = %q/%q", from, to)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	var model string
	if err := json.Unmarshal(doc["model"], &model); err != nil || model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q (err %v)", model, err)
	}
	// Everything the client sent that is not the model survives verbatim.
	var maxTokens int
	if err := json.Unmarshal(doc["max_tokens"], &maxTokens); err != nil || maxTokens != 32000 {
		t.Fatalf("max_tokens = %d (err %v)", maxTokens, err)
	}
	if string(doc["messages"]) != `[{"role":"user","content":"hi"}]` {
		t.Fatalf("messages changed: %s", doc["messages"])
	}
}

func TestRewriteModelAliasesLeavesUnknownModel(t *testing.T) {
	aliases := map[string]string{"claude-sonnet-4-5": "deepseek-v4.1-flash"}
	for _, body := range []string{
		`{"model":"deepseek-v4.1-flash","messages":[]}`, // a real id, not an alias
		`{"model":"kimi-k3.1-pro","messages":[]}`,       // anything else
		`{"messages":[]}`, // no model at all
		`{"model":42}`,    // wrong type
		`not json at all`, // not a body we understand
	} {
		if out, _, _, ok := rewriteModelAliases([]byte(body), aliases); ok {
			t.Errorf("%s: must not be rewritten, got %s", body, out)
		}
	}
	if _, _, _, ok := rewriteModelAliases([]byte(`{"model":"claude-sonnet-4-5"}`), nil); ok {
		t.Error("no aliases configured must be a no-op")
	}
}

func TestRewriteModelAliasesKeepsNumberFormatting(t *testing.T) {
	// A body member we do not touch must not be re-encoded through float64:
	// 2^53+1 is not representable as a float, and a chat body may carry a seed.
	body := []byte(`{"model":"claude-opus-4-5","seed":9007199254740993}`)
	out, _, _, ok := rewriteModelAliases(body, map[string]string{"claude-opus-4-5": "kimi-k3.1-pro"})
	if !ok {
		t.Fatal("alias must resolve")
	}
	if !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("seed lost precision: %s", out)
	}
}

func TestRewriteModelsDedupesRepeatedID(t *testing.T) {
	body := []byte(`{"data":[` +
		`{"id":"glm-5.3-flash","display_name":"a"},` +
		`{"id":"glm-5.3-flash","display_name":"b"},` +
		`{"id":"hy4","display_name":"c"}]}`)
	out, ok := rewriteModelsDisplay(body, "")
	if !ok {
		t.Fatal("dedupe must apply")
	}
	var doc struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Data) != 2 {
		t.Fatalf("want 2 rows, got %d: %s", len(doc.Data), out)
	}
	if doc.Data[0].DisplayName != "a" {
		t.Fatalf("the first row must win, got %q", doc.Data[0].DisplayName)
	}
}

func TestRewriteModelsAddsAliasRows(t *testing.T) {
	aliases := map[string]string{
		"claude-sonnet-4-5": "deepseek-v4.1-flash",
		"claude-haiku-4-5":  "glm-5.3-flash",
	}
	out, ok := rewriteModels(catalogueWithout(t), "id", aliases)
	if !ok {
		t.Fatal("alias rows must be added")
	}
	var doc struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
		} `json:"data"`
		FirstID string `json:"first_id"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, row := range doc.Data {
		seen[row.ID]++
		if row.Type != "model" {
			t.Errorf("row %q has type %q", row.ID, row.Type)
		}
	}
	if seen["claude-sonnet-4-5"] != 1 || seen["claude-haiku-4-5"] != 1 {
		t.Fatalf("alias rows missing: %s", out)
	}
	// The upstream's own rows are untouched and not duplicated.
	if seen["deepseek-v4.1-flash"] != 1 || seen["no-name"] != 1 {
		t.Fatalf("upstream rows disturbed: %s", out)
	}
	// Aliases lead, so they are the first thing a client shows.
	if doc.Data[0].ID != "claude-haiku-4-5" || doc.Data[1].ID != "claude-sonnet-4-5" {
		t.Fatalf("alias rows must be prepended in id order, got %q %q", doc.Data[0].ID, doc.Data[1].ID)
	}
	if doc.FirstID != "claude-haiku-4-5" {
		t.Fatalf("first_id not realigned: %q", doc.FirstID)
	}
}

func TestRewriteModelsAliasSkipsIDAlreadyPresent(t *testing.T) {
	body := []byte(`{"data":[{"id":"claude-sonnet-4-5","display_name":"from upstream"}]}`)
	out, ok := rewriteModels(body, "", map[string]string{"claude-sonnet-4-5": "deepseek-v4.1-flash"})
	if !ok {
		t.Fatal("second pass must be a no-op, not a failure")
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Data) != 1 {
		t.Fatalf("alias must not shadow an existing id: %s", out)
	}
}

// catalogueWithout is catalogue() minus the id we assert on, so the alias test
// does not depend on what the upstream happened to list.
func catalogueWithout(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"data":[` +
		`{"id":"deepseek-v4.1-flash","display_name":"DeepSeek fast","type":"model"},` +
		`{"id":"no-name","type":"model"}` +
		`],"first_id":"deepseek-v4.1-flash","last_id":"no-name","has_more":false}`)
}
