package report

import (
	"bytes"
	"sort"

	"gopkg.in/yaml.v3"
)

// RenderYAMLReport 把 summary、env vars、cases 序列化成 Perl yaml_dump 风格的
// 三段式 YAML 字节流：summary → env(vars) → cases。供 uploader 在
// OnPlanStop 时压缩上传 OSS 使用（对齐 Perl DisplayRole.pm:281-286 的
// $self->save_report_file({format => 'yml', tostr => \$log_str})），不依赖
// YAMLReporter 实例，避免 logfile/落盘文件副作用。
//
// 输出与 YAMLReporter.OnPlanStop 完全同构：用 yaml.Encoder 流式多次 Encode
// 让库自动 emit `---` 分隔符；vars 段经 cloneVarsForReport 做 password 打码
// （ConfigRole.pm:1028 语义）；cases 段经 mergeExtraIntoCase 把 Extra 键按
// case_seq/name/title/result/time/[desc/][args]/log/[error] 顺序合并进 case
// 顶层 map（Bug 2：替代被弃用的 `,inline` 平铺，后者把键挤到 log/error 之
// 后）。vars 为空时省略段 2，cases 为空时省略段 3（与 OnPlanStop 行为一致）。
//
// yaml.v3 的 Encoder 只在第 2 个及以后文档前输出 `---`，首文档没有——为对齐
// Perl yaml_dump 的三段 `---`（每段前都有分隔符，Perl ConfigRole.pm:919-940
// 的 yaml_dump 输出），这里手动补一个前缀 `---\n`。
func RenderYAMLReport(summary Summary, vars map[string]any, cases []CaseReport) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.WriteString("---\n"); err != nil {
		return nil, err
	}
	enc := yaml.NewEncoder(&buf)
	defer enc.Close()

	// 段 1：summary map（用 map 包装让顶层键为 `summary:`，与 Perl 端
	// yaml_dump($summary, $env, $reports) 输出一致）。Summary 实现
	// MarshalYAML，TotalTime 输出为 %.2f 字符串。
	if err := enc.Encode(map[string]any{"summary": summary}); err != nil {
		return nil, err
	}
	// 段 2：env vars（仅非空时输出）。
	if len(vars) > 0 {
		if err := enc.Encode(cloneVarsForReport(vars)); err != nil {
			return nil, err
		}
	}
	// 段 3：cases 序列（仅非空时输出）。逐 case 手工合并成保序 MappingNode，
	// 不直接 Encode([]CaseReport)——yaml.v3 对 map[string]any 按键字典序
	// 排序，无法表达 Perl 的目标字段顺序。
	if len(cases) > 0 {
		seqNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, cr := range cases {
			seqNode.Content = append(seqNode.Content, mergeExtraIntoCase(cr))
		}
		if err := enc.Encode(seqNode); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// mergeExtraIntoCase 把单个 CaseReport 手工合并为保序 YAML mapping 节点，
// 字段顺序对齐 Perl DisplayRole 的目标输出：
//
//	case_seq → name → title → result → time → [desc → 其余 Extra 键] → log → [error]
//
// Extra 的键按字节序排序（desc 是 ASCII 前缀，天然排最前），其中 desc ==
// title 时剔除（DisplayRole.pm:69-88 app_reports 删冗余规则，与
// runtime.cleanCaseArgs 语义一致——这里兜底，因为调用方可直接构造 CaseReport
// 而不经过 cleanCaseArgs）。
func mergeExtraIntoCase(cr CaseReport) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(k string, v any) {
		vn := &yaml.Node{}
		_ = vn.Encode(v)
		n.Content = append(n.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
			vn,
		)
	}
	add("case_seq", cr.Seq)
	add("name", cr.Name)
	if cr.Title != "" {
		add("title", cr.Title)
	}
	add("result", cr.Result)
	if cr.Time != 0 {
		add("time", cr.Time)
	}
	for _, k := range sortedExtraKeys(cr.Extra) {
		if k == "desc" {
			if d, ok := cr.Extra[k].(string); ok && d == cr.Title {
				continue // desc == title：删冗余
			}
		}
		add(k, cr.Extra[k])
	}
	if cr.Log != "" {
		add("log", cr.Log)
	}
	if cr.Error != "" {
		add("error", cr.Error)
	}
	return n
}

// sortedExtraKeys 返回 Extra 的键，按字节序排序（yaml.v3 对 inline map 键
// 的输出原本就无序；排序让 report 字节流确定，测试可稳定断言）。
func sortedExtraKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
