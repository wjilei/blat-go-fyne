package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// envFileName 是用户持久化环境配置的文件名，位于 ~/.blat/ 下，与
// uploader.uuidFileName 同目录。env.yml 只保存用户可见/可编辑的配置
// （当前仅 HeatNote.mbus），不应安装到 $INSTDIR\confs\（无写权限）。
const envFileName = "env.yml"

// MBUS 串口默认参数：保存 env.yml 时若 mbus 子树缺失这些键，用以下值补齐。
// 2400 baud / even parity 对应温控阀 MBUS 标准设置（与 Perl 端默认值对齐）。
const (
	DefaultMBUSBaudRate = 2400
	DefaultMBUSParity   = "even"
)

// DefaultEnvPath 返回用户家目录下 ~/.blat/env.yml 的绝对路径。不可用时
// 退化为当前目录下的 envFileName（与 uploader.defaultUUIDPath 行为一致）。
// 不强制保证目录存在——调用方需自行 MkdirAll。
func DefaultEnvPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return envFileName
	}
	return filepath.Join(home, ".blat", envFileName)
}

// CleanVars 递归深拷贝 vars 并剔除 skip 中的键（如蓝牙运行时对象），
// 使 SaveEnv 落盘时不尝试序列化不可序列化的对象。skip 中的键名在任意
// 深度的 map 中都会被删除。返回新 map，不改原 map。
func CleanVars(vars map[string]any, skip ...string) map[string]any {
	skipSet := make(map[string]bool, len(skip))
	for _, k := range skip {
		skipSet[k] = true
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if skipSet[k] {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			out[k] = CleanVars(m, skip...)
			continue
		}
		out[k] = v
	}
	return out
}

// SaveEnv writes vars back to a YAML file at path, overwriting any
// previous contents. It is the write counterpart of LoadEnv and is
// used by the GUI to persist user-edited configuration (e.g. the
// selected MBUS port) to confs/env.yml. The parent directory is created
// automatically if it does not exist yet.
//
// The YAML is emitted via yaml.Marshal, which sorts map keys
// alphabetically — fine for our flat/nested vars bags.
func SaveEnv(path string, vars map[string]any) error {
	data, err := yaml.Marshal(vars)
	if err != nil {
		return fmt.Errorf("marshal env: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir env dir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write env %s: %w", path, err)
	}
	return nil
}
