package uploader

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// uuidFileName 是 UUID 持久化文件名。
const uuidFileName = "uuid.txt"

// defaultUUIDPath 返回用户家目录下的 ~/.blat/uuid.txt；跨平台统一用同一
// 路径，与 Perl 端解耦。
func defaultUUIDPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return uuidFileName
	}
	return filepath.Join(home, ".blat", uuidFileName)
}

// GetOrCreatePCUUID 读默认路径的 uuid.txt 取得本机 UUID；文件不存在或
// 内容为空则随机生成 32 位十六进制并写回。对应 BLAT app.pl:869-891
// _get_pc_uuid（跳过 wmic 段，直接用文件优先 + 随机兜底）。
func GetOrCreatePCUUID() (string, error) {
	return GetOrCreatePCUUIDAt(defaultUUIDPath())
}

// GetOrCreatePCUUIDAt 与 GetOrCreatePCUUID 等价，但允许显式指定文件路径，
// 主要用于测试。文件不存在或内容仅空白时会生成新值并写回（必要时建父目录）。
func GetOrCreatePCUUIDAt(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			return v, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read uuid file %s: %w", path, err)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate random uuid: %w", err)
	}
	v := hex.EncodeToString(raw)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
		return "", fmt.Errorf("write uuid file %s: %w", path, err)
	}
	return v, nil
}
