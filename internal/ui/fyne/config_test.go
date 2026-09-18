package fyneui

import (
	"reflect"
	"testing"

	"blat/internal/config"
	"blat/internal/core"
)

// TestBuildMinimalEnv 验证落盘 env.yml 的最小 schema：
// 始终含 port，firmware 仅在非空时包含。
func TestBuildMinimalEnv(t *testing.T) {
	t.Run("带升级文件", func(t *testing.T) {
		got := buildMinimalEnv("COM9", `C:\fw\a.bin`)
		want := map[string]any{
			"HeatNote": map[string]any{
				"mbus": map[string]any{
					"baudRate": config.DefaultMBUSBaudRate,
					"parity":   config.DefaultMBUSParity,
					"port":     "COM9",
					"firmware": `C:\fw\a.bin`,
				},
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("buildMinimalEnv(COM9, C:\\fw\\a.bin) = %#v, want %#v", got, want)
		}
	})

	t.Run("不带升级文件不写 firmware 键", func(t *testing.T) {
		got := buildMinimalEnv("COM9", "")
		mbus, ok := got["HeatNote"].(map[string]any)["mbus"].(map[string]any)
		if !ok {
			t.Fatalf("HeatNote.mbus 缺失或类型不对: %#v", got)
		}
		if _, exists := mbus["firmware"]; exists {
			t.Errorf("firmware 不应出现在最小 env 中: %#v", mbus)
		}
		if mbus["port"] != "COM9" {
			t.Errorf("port = %v, want COM9", mbus["port"])
		}
		if mbus["baudRate"] != config.DefaultMBUSBaudRate || mbus["parity"] != config.DefaultMBUSParity {
			t.Errorf("baudRate/parity 默认值不对: %#v", mbus)
		}
	})
}

// TestCurrentConfig 验证从 env.Vars 读取当前串口与升级文件：
// 字段存在时返回对应值；缺失、类型不对、env 为 nil 时返回空串不 panic。
func TestCurrentConfig(t *testing.T) {
	t.Run("正常读取", func(t *testing.T) {
		env := &core.Env{Vars: map[string]any{
			"HeatNote": map[string]any{
				"mbus": map[string]any{
					"port":     "COM5",
					"firmware": `D:\upgrade\v2.1.bin`,
				},
			},
		}}
		port, firmware := currentConfig(env)
		if port != "COM5" || firmware != `D:\upgrade\v2.1.bin` {
			t.Errorf("currentConfig = (%q, %q), want (COM5, D:\\upgrade\\v2.1.bin)", port, firmware)
		}
	})

	t.Run("字段缺失返回空串", func(t *testing.T) {
		env := &core.Env{Vars: map[string]any{
			"HeatNote": map[string]any{
				"mbus": map[string]any{},
			},
		}}
		port, firmware := currentConfig(env)
		if port != "" || firmware != "" {
			t.Errorf("currentConfig = (%q, %q), want empty strings", port, firmware)
		}
	})

	t.Run("类型不对不 panic 且返回空串", func(t *testing.T) {
		env := &core.Env{Vars: map[string]any{
			"HeatNote": map[string]any{
				"mbus": map[string]any{
					"port":     123, // 故意给非 string
					"firmware": true,
				},
			},
		}}
		port, firmware := currentConfig(env)
		if port != "" || firmware != "" {
			t.Errorf("currentConfig = (%q, %q), want empty strings", port, firmware)
		}
	})

	t.Run("嵌套层级缺失不 panic", func(t *testing.T) {
		env := &core.Env{Vars: map[string]any{"HeatNote": map[string]any{}}}
		port, firmware := currentConfig(env)
		if port != "" || firmware != "" {
			t.Errorf("currentConfig = (%q, %q), want empty strings", port, firmware)
		}
	})

	t.Run("env 为 nil 不 panic", func(t *testing.T) {
		port, firmware := currentConfig(nil)
		if port != "" || firmware != "" {
			t.Errorf("currentConfig(nil) = (%q, %q), want empty strings", port, firmware)
		}
	})

	t.Run("Vars 未初始化不 panic", func(t *testing.T) {
		port, firmware := currentConfig(&core.Env{})
		if port != "" || firmware != "" {
			t.Errorf("currentConfig = (%q, %q), want empty strings", port, firmware)
		}
	})
}
