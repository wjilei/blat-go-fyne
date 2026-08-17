package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultEnvPath(t *testing.T) {
	// HOME / USERPROFILE 指向临时目录，断言路径形如 <home>/.blat/env.yml。
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	got := DefaultEnvPath()
	want := filepath.Join(dir, ".blat", "env.yml")
	if got != want {
		t.Fatalf("DefaultEnvPath() = %q, want %q", got, want)
	}
}

func TestDefaultEnvPath_FallbackOnEmptyHome(t *testing.T) {
	// HOME / USERPROFILE 为空时退化为裸文件名 env.yml。
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if got := DefaultEnvPath(); got != "env.yml" {
		t.Fatalf("DefaultEnvPath() with empty home = %q, want %q", got, "env.yml")
	}
}

func TestDefaultBlatDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	got := DefaultBlatDir()
	want := filepath.Join(dir, ".blat")
	if got != want {
		t.Fatalf("DefaultBlatDir() = %q, want %q", got, want)
	}
}

func TestDefaultBlatDir_FallbackOnEmptyHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	if got := DefaultBlatDir(); got != "." {
		t.Fatalf("DefaultBlatDir() with empty home = %q, want %q", got, ".")
	}
}

func TestDefaultTestLogPath_Release(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	// 强制走 release 分支——测试运行时 os.Executable() 路径不可控，
	// 显式覆盖避免依赖 exe 路径检测。
	t.Setenv("BLAT_DEV_FILES", "release")
	got := DefaultTestLogPath()
	want := filepath.Join(dir, ".blat", "test.log")
	if got != want {
		t.Fatalf("DefaultTestLogPath() = %q, want %q", got, want)
	}
}

func TestDefaultTestLogPath_Dev(t *testing.T) {
	t.Setenv("BLAT_DEV_FILES", "1")
	if got := DefaultTestLogPath(); got != "test.log" {
		t.Fatalf("dev mode TestLogPath = %q, want %q", got, "test.log")
	}
}

func TestDefaultPanelLogPath_Dev(t *testing.T) {
	t.Setenv("BLAT_DEV_FILES", "1")
	for i := 1; i <= 3; i++ {
		got := DefaultPanelLogPath(i)
		want := fmt.Sprintf("test_P%d.log", i)
		if got != want {
			t.Errorf("DefaultPanelLogPath(%d) = %q, want %q", i, got, want)
		}
		// 与 DefaultTestLogPath 同目录（dev → cwd）
		if filepath.Dir(got) != filepath.Dir(DefaultTestLogPath()) {
			t.Errorf("DefaultPanelLogPath(%d) dir = %q, want %q", i, filepath.Dir(got), filepath.Dir(DefaultTestLogPath()))
		}
	}
}

func TestDefaultPanelLogPath_Release(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	// 强制走 release 分支，避免依赖 exe 路径检测（与 TestDefaultTestLogPath_Release 一致）。
	t.Setenv("BLAT_DEV_FILES", "release")
	for i := 1; i <= 3; i++ {
		got := DefaultPanelLogPath(i)
		want := filepath.Join(dir, ".blat", fmt.Sprintf("test_P%d.log", i))
		if got != want {
			t.Errorf("DefaultPanelLogPath(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestDefaultPanelReportPath_Dev(t *testing.T) {
	t.Setenv("BLAT_DEV_FILES", "1")
	for i := 1; i <= 3; i++ {
		got := DefaultPanelReportPath(i)
		want := fmt.Sprintf("report_P%d.yml", i)
		if got != want {
			t.Errorf("DefaultPanelReportPath(%d) = %q, want %q", i, got, want)
		}
		// 与 DefaultReportPath 同目录（dev → cwd）
		if filepath.Dir(got) != filepath.Dir(DefaultReportPath()) {
			t.Errorf("DefaultPanelReportPath(%d) dir = %q, want %q", i, filepath.Dir(got), filepath.Dir(DefaultReportPath()))
		}
	}
}

func TestDefaultPanelReportPath_Release(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	// 强制走 release 分支，避免依赖 exe 路径检测（与 TestDefaultPanelLogPath_Release 一致）。
	t.Setenv("BLAT_DEV_FILES", "release")
	for i := 1; i <= 3; i++ {
		got := DefaultPanelReportPath(i)
		want := filepath.Join(dir, ".blat", fmt.Sprintf("report_P%d.yml", i))
		if got != want {
			t.Errorf("DefaultPanelReportPath(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestDefaultReportPath_Release(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("BLAT_DEV_FILES", "release")
	got := DefaultReportPath()
	want := filepath.Join(dir, ".blat", "report.yml")
	if got != want {
		t.Fatalf("DefaultReportPath() = %q, want %q", got, want)
	}
}

func TestDefaultReportPath_Dev(t *testing.T) {
	t.Setenv("BLAT_DEV_FILES", "1")
	if got := DefaultReportPath(); got != "report.yml" {
		t.Fatalf("dev mode ReportPath = %q, want %q", got, "report.yml")
	}
}

func TestDefaultReportDir_Dev(t *testing.T) {
	t.Setenv("BLAT_DEV_FILES", "1")
	if got := DefaultReportDir(); got != "." {
		t.Fatalf("dev mode ReportDir = %q, want %q", got, ".")
	}
}

func TestDefaultReportDir_Release(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("BLAT_DEV_FILES", "release")
	want := filepath.Join(dir, ".blat")
	if got := DefaultReportDir(); got != want {
		t.Fatalf("release mode ReportDir = %q, want %q", got, want)
	}
}

func TestIsInstalledExe_BLAT_DEV_FILES_Override(t *testing.T) {
	// 强制覆盖 > exe 路径自动检测。子测试用 BLAT_DEV_FILES=1 / release
	// 各验一遍；不依赖 ProgramFiles 环境。
	for _, c := range []struct {
		val  string
		want bool
	}{
		{"1", false},     // dev
		{"true", false},  // dev
		{"yes", false},   // dev
		{"0", true},      // 未识别值兜底走 exe 自动检测（go test 不在 ProgramFiles → false）
		{"release", true},
		{"prod", true},
	} {
		t.Run("BLAT_DEV_FILES="+c.val, func(t *testing.T) {
			t.Setenv("BLAT_DEV_FILES", c.val)
			got := isInstalledExe()
			if c.val == "0" {
				// "0" 是未识别值，走 exe 路径自动检测：
				// 测试可执行文件在 TEMP 下，不在 ProgramFiles → false (dev)。
				if got != false {
					t.Fatalf("isInstalledExe(BLAT_DEV_FILES=0) = %v, want false (test exe 在 TEMP 下)", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("isInstalledExe(BLAT_DEV_FILES=%q) = %v, want %v", c.val, got, c.want)
			}
		})
	}
}

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
