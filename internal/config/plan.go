// Package config loads test plans and environment files for blat-go.
//
// The YAML schema mirrors the Perl BLAT plan/env layout but is kept as a
// strict, evolvable subset:
//
//	plan.yml 顶层数组，每项为一个用例描述：
//	  - name:    HelloSuite::SayHello   # 注册表 key，<Suite>::<Method> 风格
//	    title:   招呼                   # UI 展示名（可选）
//	    desc:    简单问候               # 描述（可选）
//	    case_seq: 1                     # 显示序号（可选）
//	    counts: 1                       # 重复执行次数（可选，默认 1）
//	    parallel: 0                     # 是否并行（占位字段，可选）
//	    <其它键>: <任意值>              # 自定义参数，平铺进 Args
//
//	env.yml 任意 hash，作为 Env.Vars 的初值。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// planModeRe 匹配计划文件名中的测试模式：形如 PSAV_<mode>.yml / PTVB1_<mode>.yml
// / PFW_<mode>.yml（允许 plan_ 等前缀，大小写不敏感）。捕获组 1 是
// PSAV_/PTVB1_/PFW_ 后到 .yml 前的内容。
var planModeRe = regexp.MustCompile(`(?i)(?:PSAV|PTVB1|PFW)_(.+)\.ya?ml$`)

// TestModeFromPlanPath 从计划文件名解析测试模式：匹配 PSAV_(XXX).yml /
// PFW_(XXX).yml，返回括号里的 XXX 作为 test_mode；不匹配时返回空串。
// 例如 confs/plan_PSAV_ut_check_state.yml → "ut_check_state"。
func TestModeFromPlanPath(path string) string {
	if path == "" {
		return ""
	}
	m := planModeRe.FindStringSubmatch(filepath.Base(path))
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// IsPanelPlan 判断计划路径是否为 PTVB1 面板模式计划：文件名含 PTVB1
// （大小写不敏感）。例如 confs/plan_PTVB1_normal_ut_checkmotor.yml → true。
func IsPanelPlan(path string) bool {
	if path == "" {
		return false
	}
	return strings.Contains(strings.ToLower(filepath.Base(path)), "ptvb1")
}

// reserved fields are decoded into the struct; everything else lands in
// Args. Keep this list in sync with the doc-comment above.
type CaseItem struct {
	Name     string         `yaml:"name"`
	Title    string         `yaml:"title"`
	Desc     string         `yaml:"desc"`
	CaseSeq  int            `yaml:"case_seq"`
	Counts   int            `yaml:"counts"`
	Parallel int            `yaml:"parallel"`
	Args     map[string]any `yaml:",inline"`
}

// Plan is an ordered list of CaseItems.
type Plan struct {
	Cases []CaseItem
}

// LoadPlan reads a YAML plan file. The top-level YAML must be a sequence;
// each element becomes one CaseItem.
func LoadPlan(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan %s: %w", path, err)
	}
	// Decode as generic node first so we can detect a non-sequence top
	// level and report a clear error. yaml.Unmarshal wraps everything in
	// a DocumentNode; the actual root sits in doc.Content[0].
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse plan %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return &Plan{}, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("plan %s: top level must be a sequence, got kind=%d", path, root.Kind)
	}
	items := make([]CaseItem, len(root.Content))
	for i, n := range root.Content {
		if err := n.Decode(&items[i]); err != nil {
			return nil, fmt.Errorf("plan %s: case[%d]: %w", path, i, err)
		}
		if items[i].Name == "" {
			return nil, fmt.Errorf("plan %s: case[%d] missing required field 'name'", path, i)
		}
		if items[i].Counts < 1 {
			items[i].Counts = 1
		}
	}
	return &Plan{Cases: items}, nil
}

// LoadEnv reads a YAML env file into a flat map[string]any. Nested maps
// are preserved.
func LoadEnv(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env %s: %w", path, err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse env %s: %w", path, err)
	}
	return out, nil
}
