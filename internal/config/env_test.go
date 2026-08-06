package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveEnv_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.yml")

	in := map[string]any{
		"mbus": map[string]any{
			"port": "COM3",
		},
		"station":   "A1",
		"operators": []any{"张三", "李四"},
	}
	if err := SaveEnv(path, in); err != nil {
		t.Fatalf("SaveEnv: %v", err)
	}

	out, err := LoadEnv(path)
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	mbus, ok := out["mbus"].(map[string]any)
	if !ok {
		t.Fatalf("mbus missing or wrong type: %#v", out["mbus"])
	}
	if mbus["port"] != "COM3" {
		t.Errorf("mbus.port = %v, want COM3", mbus["port"])
	}
	if out["station"] != "A1" {
		t.Errorf("station = %v", out["station"])
	}
}

func TestSaveEnv_Overwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vars.yml")
	// Pre-seed with stale data; SaveEnv must truncate it.
	if err := os.WriteFile(path, []byte("legacy: junk\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := SaveEnv(path, map[string]any{"fresh": "yes"}); err != nil {
		t.Fatalf("SaveEnv: %v", err)
	}
	out, err := LoadEnv(path)
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if _, stale := out["legacy"]; stale {
		t.Errorf("legacy key should have been truncated; got: %#v", out)
	}
	if out["fresh"] != "yes" {
		t.Errorf("fresh = %v", out["fresh"])
	}
}

func TestSaveEnv_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	// 父目录 confs/ 尚不存在；SaveEnv 必须自动创建。
	path := filepath.Join(dir, "confs", "env.yml")
	if err := SaveEnv(path, map[string]any{"port": "COM9"}); err != nil {
		t.Fatalf("SaveEnv: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "confs")); err != nil {
		t.Fatalf("parent dir should have been created: %v", err)
	}
	out, err := LoadEnv(path)
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if out["port"] != "COM9" {
		t.Errorf("port = %v, want COM9", out["port"])
	}
}

func TestSaveEnv_BadPath(t *testing.T) {
	// 空路径不可写：SaveEnv 必须返回 error。
	if err := SaveEnv("", map[string]any{}); err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestCleanVars(t *testing.T) {
	in := map[string]any{
		"HeatNote": map[string]any{
			"mac":       "AA:BB:CC:DD:EE:FF",
			"bt_mock":   true,
			"bluetooth": &struct{}{}, // 运行时对象，必须被剔除
		},
		"station": "A1",
	}
	out := CleanVars(in, "bluetooth")
	if _, has := out["station"]; !has {
		t.Fatalf("station should be kept")
	}
	hn, ok := out["HeatNote"].(map[string]any)
	if !ok {
		t.Fatalf("HeatNote should be a map: %#v", out["HeatNote"])
	}
	if _, has := hn["bluetooth"]; has {
		t.Fatalf("bluetooth should be stripped")
	}
	if _, has := hn["bt_mock"]; !has {
		t.Fatalf("bt_mock should be kept")
	}
	// 原 map 不被修改
	if _, has := in["HeatNote"].(map[string]any)["bluetooth"]; !has {
		t.Fatalf("original map must not be modified")
	}
	// 嵌套 map 是深拷贝：改 out 不影响 in
	hn["mac"] = "CHANGED"
	if in["HeatNote"].(map[string]any)["mac"] != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("nested map should be deep-copied")
	}
}
