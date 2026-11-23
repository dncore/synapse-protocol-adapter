package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
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

// rewriteModelAliases replaces a request's model with the upstream id its
// alias names. Resolution is a single hop and an id that is not a key is left
// untouched, so a client that selected a real catalogue id keeps talking to
// the same model. Everything else in the body is carried through as raw bytes:
// only the model member is re-encoded, so upstream-specific fields and their
// number formatting survive.
func rewriteModelAliases(body []byte, aliases map[string]string) (out []byte, from, to string, ok bool) {
	if len(aliases) == 0 || len(body) == 0 {
		return nil, "", "", false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", "", false
	}
	raw, present := doc["model"]
	if !present {
		return nil, "", "", false
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, "", "", false
	}
	target, hit := aliases[model]
	if !hit || target == "" || target == model {
		return nil, "", "", false
	}
	enc, err := json.Marshal(target)
	if err != nil {
		return nil, "", "", false
	}
	doc["model"] = enc
	out, err = json.Marshal(doc)
	if err != nil {
		return nil, "", "", false
	}
	return out, model, target, true
}

// aliasEntry builds the catalogue row advertised for one alias. It follows the
// Anthropic models shape; the numeric fields an upstream may add are unknown
// here, so they are omitted rather than guessed.
func aliasEntry(id string) map[string]any {
	return map[string]any{
		"type":         "model",
		"id":           id,
		"display_name": id,
		"created_at":   "1970-01-01T00:00:00Z",
	}
}

// rewriteModelsDisplay sets each entry's display_name so a client that renders
// that field — Claude Desktop among them — shows something identifying instead
// of an upstream description. See rewriteModels.
func rewriteModelsDisplay(body []byte, mode string) ([]byte, bool) {
	return rewriteModels(body, mode, nil)
}

// rewriteModels canonicalises a models catalogue:
//
//   - an id that appears more than once keeps only its first row, so a client
//     merging discovery results cannot end up listing one model twice;
//   - display_name is rewritten per mode ("id" or "id-desc");
//   - every configured alias is prepended as its own row, so the alias is
//     discoverable and any client-side check against the catalogue passes.
//
// mode may be "" when only the alias rows are wanted.
func rewriteModels(body []byte, mode string, aliases map[string]string) ([]byte, bool) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	data, ok := doc["data"].([]any)
	if !ok {
		return nil, false
	}

	changed := false
	seen := make(map[string]bool, len(data)+len(aliases))
	kept := make([]any, 0, len(data)+len(aliases))

	for _, item := range data {
		entry, isObj := item.(map[string]any)
		if !isObj {
			kept = append(kept, item)
			continue
		}
		id, _ := entry["id"].(string)
		if id != "" {
			if seen[id] {
				changed = true
				continue
			}
			seen[id] = true
		}
		if mode != "" && id != "" {
			orig, _ := entry["display_name"].(string)
			want := orig
			switch mode {
			case "id":
				want = id
			case "id-desc":
				// Already in the target form: leave it, so a second pass is a no-op.
				switch {
				case orig == "":
					want = id
				case orig == id, strings.HasPrefix(orig, id+" — "):
					want = orig
				default:
					want = id + " — " + orig
				}
			}
			if orig != want {
				entry["display_name"] = want
				changed = true
			}
		}
		kept = append(kept, entry)
	}

	if len(aliases) > 0 {
		names := make([]string, 0, len(aliases))
		for from := range aliases {
			names = append(names, from)
		}
		sort.Strings(names)
		extra := make([]any, 0, len(names))
		for _, from := range names {
			if from == "" || seen[from] {
				continue
			}
			seen[from] = true
			extra = append(extra, aliasEntry(from))
			changed = true
		}
		if len(extra) > 0 {
			kept = append(extra, kept...)
		}
	}

	if !changed {
		return body, true
	}

	doc["data"] = kept
	// first_id/last_id describe the paging window; keep them consistent with
	// the rows we are about to emit rather than leaving them pointing at a
	// row that no longer leads or trails.
	if _, present := doc["first_id"]; present {
		if id := entryID(kept, true); id != "" {
			doc["first_id"] = id
		}
	}
	if _, present := doc["last_id"]; present {
		if id := entryID(kept, false); id != "" {
			doc["last_id"] = id
		}
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// entryID returns the id of the first or last row that has one.
func entryID(rows []any, first bool) string {
	walk := func(i int) string {
		entry, ok := rows[i].(map[string]any)
		if !ok {
			return ""
		}
		id, _ := entry["id"].(string)
		return id
	}
	if first {
		for i := range rows {
			if id := walk(i); id != "" {
				return id
			}
		}
		return ""
	}
	for i := len(rows) - 1; i >= 0; i-- {
		if id := walk(i); id != "" {
			return id
		}
	}
	return ""
}

// rewriteModelsResponse buffers a models response, rewrites it, and writes it
// to the client. It reports whether it handled the response; when it returns
// false the caller must continue with the normal byte-for-byte path, and
// nothing has been written to w.
//
// A body larger than maxModelsBodyBytes, or one that is not the JSON shape we
// understand, is passed through unchanged: this route is a workaround for a
// mislabelled catalogue, not a reason to break an upstream that behaves.
func (s *Server) rewriteModelsResponse(w http.ResponseWriter, resp *http.Response, mode string, aliases map[string]string) bool {
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

	rewritten, ok := rewriteModels(body, mode, aliases)
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
