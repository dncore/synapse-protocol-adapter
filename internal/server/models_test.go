package server

import (
	"encoding/json"
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
