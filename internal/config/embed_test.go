package config

import (
	"strings"
	"testing"
)

// TestExampleConfigEmbedded guards the annotated template `synapse init`
// ships: every section and the env-override hint must survive edits to
// example.yaml.
func TestExampleConfigEmbedded(t *testing.T) {
	for _, want := range []string{
		"server:", "upstream:", "limits:", "timeouts:", "shutdown:", "log:",
		"base_url:", "PROXY_UPSTREAM_BASE_URL",
	} {
		if !strings.Contains(ExampleConfig, want) {
			t.Errorf("embedded example config missing %q", want)
		}
	}
}
