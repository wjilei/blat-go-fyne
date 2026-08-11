package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// envFileName / testLogFileName / reportFileName 是用户运行时产物的文件名，
// 都位于 ~/.blat/ 下（与 uploader.uuidFileName 同目录）。这些文件：
//   - env.yml：用户持久化配置（仅 HeatNote.mbus，避免写 $INSTDIR\confs\ 无写权限）
//   - test.log：测试运行日志（每次 startRun 由 GUI 截断，上报时全量读取）
//   - report.yml：测试报告（GUI 模式固定文件名，Console 模式带时间戳）
// 安装包不应该包含它们——全部由程序运行时落到用户家目录。
const (
	envFileName     = "env.yml"
	testLogFileName = "test.log"
	reportFileName  = "report.yml"
	blatDirName     = ".blat"
)

// MBUS 串口默认参数：保存 env.yml 时若 mbus 子树缺失这些键，用以下值补齐。
// 2400 baud / even parity 对应温控阀 MBUS 标准设置（与 Perl 端默认值对齐）。
const (
	DefaultMBUSBaudRate = 2400
	DefaultMBUSParity   = "even"
)

// DefaultBlatDir 返回用户家目录下 ~/.blat/ 的绝对路径。所有用户可写的运行时
// 产物（env.yml / test.log / report.yml）都落这里。HOME 不可用时退化为
// 当前目录（与 uploader.defaultUUIDPath 同行为）。
func DefaultBlatDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "."
	}
	return filepath.Join(home, blatDirName)
}

// DefaultEnvPath 返回 ~/.blat/env.yml 的绝对路径。安装时 main 须自行 MkdirAll。
func DefaultEnvPath() string {
	return filepath.Join(DefaultBlatDir(), envFileName)
}

// DefaultTestLogPath 返回测试日志路径。release（NSIS 安装包）落到 ~/.blat/；
// dev（go run / go build 直接运行）落到当前目录，便于 `ls` 直接查看产物。
// 也可由 BLAT_DEV_FILES 环境变量强制切换。
func DefaultTestLogPath() string {
	if isInstalledExe() {
		return filepath.Join(DefaultBlatDir(), testLogFileName)
	}
	return testLogFileName
}

// DefaultReportPath 返回 YAML 报告文件路径（GUI 模式固定文件名）。
// release → ~/.blat/report.yml；dev → ./report.yml。
func DefaultReportPath() string {
	if isInstalledExe() {
		return filepath.Join(DefaultBlatDir(), reportFileName)
	}
	return reportFileName
}

// DefaultReportDir 返回 Console 模式 YAML 报告所在目录（每次跑生成
// 带时间戳的 report_<ts>.yml）。release → ~/.blat/；dev → "."。
func DefaultReportDir() string {
	if isInstalledExe() {
		return DefaultBlatDir()
	}
	return "."
}

// isInstalledExe 判断当前进程是 dev（go run / go build 直接在源码目录跑）还是
// release（通过 NSIS 安装包运行）。依据：exe 路径是否在 %ProgramFiles% 或
// %ProgramFiles(x86)% 子树下。
//
// 强制覆盖（env BLAT_DEV_FILES）：
//   - 1 / true / yes → dev
//   - release / prod → release
//
// exe 路径读取失败时按 release 兜底（避免误判把用户配置写到 cwd）。
func isInstalledExe() bool {
	switch strings.ToLower(os.Getenv("BLAT_DEV_FILES")) {
	case "1", "true", "yes":
		return false
	case "release", "prod":
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return true
	}
	exeLower := strings.ToLower(filepath.Clean(exe))
	for _, envKey := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		prefix := strings.ToLower(os.Getenv(envKey))
		if prefix == "" {
			continue
		}
		sep := "\\"
		if strings.Contains(prefix, "/") {
			sep = "/"
		}
		if strings.HasPrefix(exeLower, prefix+sep) {
			return true
		}
	}
	return false
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
