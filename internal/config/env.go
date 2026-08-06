package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

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
