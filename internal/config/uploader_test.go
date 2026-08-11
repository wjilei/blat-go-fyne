package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadUploader_OK 验证 LoadUploader 能正确解析合法的 uploader YAML，
// 各字段逐一断言。测试用虚构值，避免把真实凭据写进 .go 文件。
func TestLoadUploader_OK(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uploader.yml")
	content := `oss:
    endpoint: "https://oss-cn-hangzhou.example.com"
    log_bucket: "blat-app-log"
blat:
    base_url: "https://blat.example.com"
    token: "fake-token-value"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write uploader yaml: %v", err)
	}

	cfg, err := LoadUploader(path)
	if err != nil {
		t.Fatalf("LoadUploader() error = %v", err)
	}
	if cfg.OSS.Endpoint != "https://oss-cn-hangzhou.example.com" {
		t.Errorf("OSS.Endpoint = %q, want %q", cfg.OSS.Endpoint, "https://oss-cn-hangzhou.example.com")
	}
	if cfg.OSS.LogBucket != "blat-app-log" {
		t.Errorf("OSS.LogBucket = %q, want %q", cfg.OSS.LogBucket, "blat-app-log")
	}
	if cfg.Blat.BaseURL != "https://blat.example.com" {
		t.Errorf("Blat.BaseURL = %q, want %q", cfg.Blat.BaseURL, "https://blat.example.com")
	}
	if cfg.Blat.Token != "fake-token-value" {
		t.Errorf("Blat.Token = %q, want %q", cfg.Blat.Token, "fake-token-value")
	}
}

// TestLoadUploader_MissingFile 验证加载不存在的文件必须返回 error。
func TestLoadUploader_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no_such_uploader.yml")
	if _, err := LoadUploader(path); err == nil {
		t.Fatal("LoadUploader() error = nil, want error for missing file")
	}
}

// TestLoadUploader_BadYAML 验证写入非法 YAML 时必须返回 error。
func TestLoadUploader_BadYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uploader.yml")
	if err := os.WriteFile(path, []byte("oss: [unclosed"), 0o644); err != nil {
		t.Fatalf("write bad yaml: %v", err)
	}
	if _, err := LoadUploader(path); err == nil {
		t.Fatal("LoadUploader() error = nil, want error for bad yaml")
	}
}