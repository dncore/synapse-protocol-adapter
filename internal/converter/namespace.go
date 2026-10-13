package converter

import "github.com/dncore/synapse-protocol-adapter/internal/responses"

// ---------------------------------------------------------------------------
// Namespace tool flattening (Codex ↔ chat completions)
// ---------------------------------------------------------------------------
//
// Codex wraps each MCP server's tools — and its own multi-agent and skills
// tools — in {"type":"namespace","name":"mcp__chrome_devtools","tools":[...]}.
// That wrapper exists only on OpenAI's own Responses endpoint; chat completions
// has no equivalent, so convertTools used to drop the whole wrapper and the
// model never saw those tools at all.
//
// The round trip has to land back on the exact shape the client resolves.
// Measured 2026-10-13 against Codex CLI 0.162.1 by serving a real captured
// stream with the function_call item rewritten:
//
//	name="mcp__chrome_devtools__click"                → "unsupported call"
//	name="click"                                      → "unsupported call"
//	name="click" + namespace="mcp__chrome_devtools"   → dispatched to the MCP server
//
// So the flat name below is only what the MODEL sees. The client is handed the
// bare nested name back, with the wrapper restored as a sibling field.

// flatToolName is the name the model sees for a tool inside a namespace
// wrapper. "__" keeps it within the character set OpenAI-compatible function
// names allow; a "." would be rejected outright.
func flatToolName(namespace, name string) string { return namespace + "__" + name }

// namespacedTool is one namespace member, resolved.
type namespacedTool struct {
	namespace string
	name      string
}

// namespaceIndex maps the flat name of every namespaced tool in a request back
// to its wrapper and nested name, so a model-chosen name resolves without
// having to parse the flat string apart again.
func namespaceIndex(tools []responses.Tool) map[string]namespacedTool {
	idx := make(map[string]namespacedTool)
	taken := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Type == "" || t.Type == "function" {
			taken[t.Name] = true
		}
	}
	for _, t := range tools {
		if t.Type != "namespace" {
			continue
		}
		for _, nt := range t.Tools {
			if nt.Type != "" && nt.Type != "function" {
				continue
			}
			flat := flatToolName(t.Name, nt.Name)
			// A plain top-level tool of the same name would shadow this one in
			// the model's view, so that spelling is left alone.
			if taken[flat] {
				continue
			}
			idx[flat] = namespacedTool{namespace: t.Name, name: nt.Name}
		}
	}
	return idx
}

// resolveCallName rewrites a model-chosen tool name into the (name, namespace)
// pair the client dispatches on. A name outside any namespace passes through
// unchanged, with an empty namespace.
func resolveCallName(idx map[string]namespacedTool, flat string) (name, namespace string) {
	if t, ok := idx[flat]; ok {
		return t.name, t.namespace
	}
	return flat, ""
}

// historyCallName is resolveCallName in reverse, for a function_call the client
// replayed in the request history. A client stores the call the way it received
// it — bare nested name plus the wrapper — while the model only ever knew the
// flat name, so the flat spelling has to be put back.
func historyCallName(idx map[string]namespacedTool, item *responses.Item) string {
	if item.Namespace == "" {
		return item.Name
	}
	if _, ok := idx[flatToolName(item.Namespace, item.Name)]; ok {
		return flatToolName(item.Namespace, item.Name)
	}
	return item.Name
}
