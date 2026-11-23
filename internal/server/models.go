package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/dncore/synapse-protocol-adapter/internal/upstream"
)

// maxModelsBodyBytes caps how much of a models response is buffered for
// rewriting. A model catalogue is a few kilobytes; anything larger is not a
// catalogue we want to hold in memory, so it streams through untouched.
const maxModelsBodyBytes = 4 << 20 // 4 MiB

// isModelsPath reports whether p is a leaf models route under one of the
// served prefixes: /v1/models, /api/v1/models, /api/anthropic/v1/models.
func isModelsPath(p string) bool {
	return strings.HasSuffix(strings.TrimSuffix(p, "/"), "/models")
}

// rewriteModelsDisplay sets each entry's display_name so a client that renders
// that field — Claude Desktop among them — shows something identifying instead
// of an upstream description.
//
// mode is "id" or "id-desc". Entries without a usable id are left alone.
func rewriteModelsDisplay(body []byte, mode string) ([]byte, bool) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	data, ok := doc["data"].([]any)
	if !ok {
		return nil, false
	}

	changed := false
	for _, item := range data {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		if id == "" {
			continue
		}
		orig, _ := entry["display_name"].(string)
		switch mode {
		case "id":
			if orig != id {
				entry["display_name"] = id
				changed = true
			}
		case "id-desc":
			// Already in the target form: leave it, so a second pass is a no-op.
			want := orig
			switch {
			case orig == "":
				want = id
			case orig == id, strings.HasPrefix(orig, id+" — "):
				want = orig
			default:
				want = id + " — " + orig
			}
			if orig != want {
				entry["display_name"] = want
				changed = true
			}
		}
	}
	if !changed {
		return body, true
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// rewriteModelsResponse buffers a models response, rewrites it, and writes it
// to the client. It reports whether it handled the response; when it returns
// false the caller must continue with the normal byte-for-byte path, and
// nothing has been written to w.
//
// A body larger than maxModelsBodyBytes, or one that is not the JSON shape we
// understand, is passed through unchanged: this route is a workaround for a
// mislabelled catalogue, not a reason to break an upstream that behaves.
func (s *Server) rewriteModelsResponse(w http.ResponseWriter, resp *http.Response, mode string) bool {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBodyBytes+1))
	if err != nil {
		return false
	}
	if len(body) > maxModelsBodyBytes {
		// Too big to rewrite. Nothing has been written yet, but the bytes are
		// already read, so emit them and stream the remainder.
		upstream.ForwardResponseHeaders(w.Header(), resp.Header)
		if resp.Uncompressed {
			w.Header().Del("Content-Encoding")
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		if _, werr := w.Write(body); werr != nil {
			return true
		}
		_, _ = io.Copy(w, resp.Body)
		return true
	}

	rewritten, ok := rewriteModelsDisplay(body, mode)
	if !ok {
		rewritten = body
	}

	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	if resp.Uncompressed {
		w.Header().Del("Content-Encoding")
	}
	// The body changed length, so the upstream's Content-Length is wrong.
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rewritten)
	return true
}
