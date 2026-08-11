package uploader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetOrCreatePCUUIDAt_Existing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uuid.txt")
	const existing = "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(path, []byte("  "+existing+"  \n"), 0o600); err != nil {
		t.Fatalf("setup write file: %v", err)
	}

	got, err := GetOrCreatePCUUIDAt(path)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != existing {
		t.Errorf("got %q, want existing trimmed %q", got, existing)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), existing) {
		t.Errorf("file should be untouched, got %q", string(data))
	}
}

func TestGetOrCreatePCUUIDAt_Missing(t *testing.T) {
	dir := t.TempDir()
	// 嵌套目录不存在，验证 MkdirAll 路径
	path := filepath.Join(dir, "nested", "deep", "uuid.txt")

	got, err := GetOrCreatePCUUIDAt(path)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 32 {
		t.Errorf("got %q (len %d), want 32 hex chars", got, len(got))
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("got %q contains non-hex char %q", got, c)
			break
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should be created: %v", err)
	}
	if strings.TrimSpace(string(data)) != got {
		t.Errorf("file content %q != returned %q", string(data), got)
	}
}

func TestGetOrCreatePCUUIDAt_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uuid.txt")
	if err := os.WriteFile(path, []byte("   \n\t"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got, err := GetOrCreatePCUUIDAt(path)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 32 {
		t.Errorf("got %q (len %d), want fresh 32 hex chars", got, len(got))
	}
	data, _ := os.ReadFile(path)
	if strings.TrimSpace(string(data)) != got {
		t.Errorf("file should now hold fresh uuid, got %q", string(data))
	}
}

func TestGetOrCreatePCUUIDAt_ReadError(t *testing.T) {
	// 路径指向一个目录，os.ReadFile 会返回非 IsNotExist 错误，验证错误分支
	dir := t.TempDir()
	path := filepath.Join(dir, "uuid.txt")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := GetOrCreatePCUUIDAt(path); err == nil {
		t.Fatal("want error when path is a directory, got nil")
	}
}
