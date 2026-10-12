package config

import (
	"os"
	"testing"
)

// 确保 example.yaml(含新增的 model_health 段)能解析并落到默认值/显式值上。
func TestExampleConfigParsesModelHealth(t *testing.T) {
	raw, err := os.ReadFile("example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/example.yaml"
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("example.yaml 解析失败: %v", err)
	}
	if cfg.ModelHealth.FailureThreshold != 2 {
		t.Fatalf("failure_threshold = %d", cfg.ModelHealth.FailureThreshold)
	}
	if len(cfg.ModelHealth.Fallbacks) != 0 {
		t.Fatalf("example 里不应预设 fallback: %v", cfg.ModelHealth.Fallbacks)
	}
}
