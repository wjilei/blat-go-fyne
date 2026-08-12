package report

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"blat/internal/logfile"
)

// YAMLReporter buffers CaseReports during a run and flushes a multi-document
// YAML stream on OnPlanStop, mirroring the Perl ConfigRole.pm:yaml_dump
// signature yaml_dump($summary, $env, $reports):
//
//	---
//	summary:           # 段 1：plan-level 统计（8 个 test_* 键 + 可选 start/stop）
//	  test_total_num: 3
//	  test_result: 1
//	  test_failreason: ok
//	  ...
//	---
//	<vars keys>:       # 段 2：Env.Vars 全量透出
//	  HeatNote:
//	    ...
//	---
//	- case_seq: 1     # 段 3：cases 序列（每个 case 一项，含 log/name/title/result/time 等）
//	  name: HelloSuite::SayHello
//	  result: ok
//	- case_seq: 2
//	  ...
//
// Output goes to an io.Writer (default os.Stdout), or, in file mode, to a
// fixed path (NewYAMLPath) or to a timestamped report_<ts>.yml in a directory
// (NewYAMLFile), opened on OnPlanStart.
//
// 用 yaml.Encoder 流式多次 Encode 即可拿到每段前的 `---` 分隔符（已用
// yaml_multi 实验验证），手动构造 DocumentNode 反而增加不必要复杂度。
// caseWindow 记录 OnCaseStart 时 logfile 的文件位置快照，OnCaseStop 用它
// 切片出该 case 运行期间的日志增量（对齐 Perl DisplayRole poll_log_tick
// 的 offset/gen 增量读）。
type caseWindow struct {
	offset int64
	gen    int
}

type YAMLReporter struct {
	w     io.Writer
	dir   string // non-empty => file mode, timestamped per run
	path  string // non-empty => file mode, fixed path, truncated on each run
	vars  map[string]any // 段 2 来源：Env.Vars clone，仅做 password 打码
	f     *os.File
	cases []CaseReport
	// lf 为 case 日志窗口的数据源（WithLogfile 注入）。nil 时不做窗口切片，
	// Log 字段保持空（yaml omitempty 省略），行为与 Phase 1 一致。
	lf          *logfile.FileLogger
	caseWindows map[int]caseWindow
}

// NewYAML returns a reporter that writes to w. If w is nil, output goes to
// os.Stdout. 默认不输出 vars 段；调用 WithVars(nil) 或传 map 启用段 2。
func NewYAML(w io.Writer) *YAMLReporter {
	if w == nil {
		w = os.Stdout
	}
	return &YAMLReporter{w: w}
}

// NewYAMLFile returns a reporter that opens report_<timestamp>.yml in dir
// when OnPlanStart is called. An empty dir means the current directory.
func NewYAMLFile(dir string) *YAMLReporter {
	return &YAMLReporter{dir: dir}
}

// NewYAMLPath returns a reporter that writes to a fixed file path,
// truncating any existing file at OnPlanStart. Use this when callers want a
// stable filename across runs (e.g. the GUI's single rolling report) instead
// of one timestamped file per run.
func NewYAMLPath(path string) *YAMLReporter {
	return &YAMLReporter{path: path}
}

// WithVars 注入段 2 来源（Env.Vars）。YAMLReporter 不直接持有 Env 指针，
// 由 main / Fyne 装配时把 vars clone 传入，避免 case 端对 reporter 持有的
// vars 改动产生副作用。
//
// 传 nil 等价禁用段 2（保留旧行为，向后兼容）。
func (y *YAMLReporter) WithVars(vars map[string]any) *YAMLReporter {
	if vars == nil {
		y.vars = nil
		return y
	}
	y.vars = cloneVarsForReport(vars)
	return y
}

// WithLogfile 注入 case 日志窗口的数据源。注入后每个 case 的 Log 字段为
// OnCaseStart..OnCaseStop 期间 logfile 新增的行（含 case_start ... case_stop
// 的 RUNNER 行），对齐 Perl DisplayRole.pm:153-195 按 case_seq 切片日志。
// 不注入时 Log 字段留空，YAML 输出省略（与 Phase 1 行为一致）。
// caseWindows 由 OnCaseStart lazy 初始化，这里不预建。
func (y *YAMLReporter) WithLogfile(lf *logfile.FileLogger) *YAMLReporter {
	y.lf = lf
	return y
}

func (y *YAMLReporter) OnPlanStart(total int, startTime time.Time) {
	y.cases = y.cases[:0]
	// 清空上一 run 的窗口快照：同一 reporter 复用时（GUI 单例）seq 会从 1
	// 重新编号，旧条目若残留可能被 OnCaseStop 误读。
	y.caseWindows = nil
	switch {
	case y.path != "":
		// 固定路径：os.Create 行为等价 truncate+create —— 每次开始测试
		// 都会把上一次运行留下的日志清空。
		f, err := os.Create(y.path)
		if err != nil {
			y.w = os.Stdout
			return
		}
		y.f = f
		y.w = f
	case y.dir != "":
		name := filepath.Join(y.dir, fmt.Sprintf("report_%s.yml", startTime.Format("20060102_150405")))
		f, err := os.Create(name)
		if err != nil {
			// Fall back to stdout rather than silently losing the report.
			y.w = os.Stdout
			return
		}
		y.f = f
		y.w = f
	}
}

func (y *YAMLReporter) OnCaseStart(seq int, cr CaseReport) {
	if y.lf == nil {
		return
	}
	// 记录窗口起点：TailFrom(0, 0) 返回当前文件 size + gen，作为本次 case
	// 日志切片的 offset（对齐 Perl DisplayRole.pm:309-318 的 1s/16KB 增量读，
	// 只是 Go 侧按 case 粒度切片而非轮询）。
	_, size, gen, _ := y.lf.TailFrom(0, 0)
	if y.caseWindows == nil {
		y.caseWindows = map[int]caseWindow{}
	}
	y.caseWindows[seq] = caseWindow{offset: size, gen: gen}
}

func (y *YAMLReporter) OnCaseStop(seq int, cr CaseReport) {
	if y.lf != nil {
		if w, ok := y.caseWindows[seq]; ok {
			// 切片出 case 窗口内的新增日志（对齐 Perl DisplayRole.pm:153-195
			// 把日志归并到 $report->{log}）；TailFrom 返回的 text 含尾换行，
			// yaml.v3 对以 \n 结尾的多行字符串渲染为 `|` 块字符串。
			text, _, _, _ := y.lf.TailFrom(w.offset, w.gen)
			cr.Log = text
		}
	}
	y.cases = append(y.cases, cr)
}

// OnPlanStop 流式写出三段：summary → vars → cases。
//
// 段顺序严格对齐 Perl ConfigRole yaml_dump($summary, $env, $reports)；vars 段
// 仅在 WithVars 注入且非空时输出。cases 段无论 vars 是否注入都会输出。
// 序列化逻辑复用 RenderYAMLReport（Phase 4 A 抽出的纯函数），这里只负责
// 落盘 writer / 文件句柄关闭。
func (y *YAMLReporter) OnPlanStop(summary Summary) {
	defer y.closeFile()
	bs, err := RenderYAMLReport(summary, y.vars, y.cases)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yaml: render report:", err)
		return
	}
	if _, err := y.w.Write(bs); err != nil {
		fmt.Fprintln(os.Stderr, "yaml: write report:", err)
	}
}

func (y *YAMLReporter) closeFile() {
	if y.f != nil {
		y.f.Close()
		y.f = nil
	}
}

// cloneVarsForReport 深拷贝 vars 并对所有嵌套 map 中 key == "password" 的
// 值替换为 `"******"`。实现对齐 Perl ConfigRole.pm:1028 行为：只遮一层嵌套
// 里的 password 字段（顶层类型为 scalar 的 password 不会被替换——Perl 端
// if ref $env->{$key} eq 'HASH' 也是这个语义）。
//
// 不返回错误是因为密码打码是 best-effort 的安全策略，缺失字段不应阻塞报告
// 输出；异常 value 类型（如 chan/func）在 yaml 序列化时会自然暴露。
func cloneVarsForReport(vars map[string]any) map[string]any {
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		if m, ok := v.(map[string]any); ok {
			out[k] = cloneHashWithPasswordMasked(m)
			continue
		}
		out[k] = v
	}
	return out
}

func cloneHashWithPasswordMasked(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "password" {
			out[k] = "******"
			continue
		}
		out[k] = v
	}
	return out
}
