package uploader

// Phase 4 B Red：HookStopReporter 的上传主体切换为 report.yml 的三段式 YAML
// （对齐 Perl DisplayRole.pm:281-286 的 $log_str = $self->save_report_file(
// {format => 'yml', tostr => \$log_str})）。OnPlanStop 阶段不再取 logSrc()
// 的纯文本日志，而是用 report.RenderYAMLReport(summary, env.Vars, cases)
// 序列化三段 YAML 后压缩上传；reporter["log"] 字段在成功上传后是 OSS 路径。
//
// 本测试用 skipOSS=true（--debug）走不触网分支，从 env.Log 捕获的打印内容
// 断言三段式 YAML（summary + env vars 打码 + 3 个 cases）确实进入了上报路径。
// 当前实现（OnCaseStart/OnCaseStop 空实现、skipOSS 只打印 buildReporter
// payload）不会输出这些，测试先红。

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ulikunitz/xz/lzma"

	"blat/internal/core"
	"blat/internal/report"
)

// lzmaDecompress 是 test-only 解压 helper：验证 compressLog 的输出可还原。
func lzmaDecompress(data []byte) ([]byte, error) {
	r, err := lzma.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// recordLogger 记录所有 Info/Error 消息，供断言 debug 分支打印的上报内容。
type recordLogger struct {
	mu    sync.Mutex
	infos []string
	errs  []string
}

func (l *recordLogger) Info(category, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}
func (l *recordLogger) Warn(category, msg string) {}
func (l *recordLogger) Error(category, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, msg)
}

func (l *recordLogger) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(append(append([]string{}, l.infos...), l.errs...), "\n")
}

// TestHookStopReporter_RendersYAMLReport 验证 skipOSS（debug）分支打印的
// 上报内容包含三段式 YAML：summary（test_* 键）+ env vars（password 打码）+
// 3 个 cases（含失败项的 error/result），且 password 原值不泄漏。
func TestHookStopReporter_RendersYAMLReport(t *testing.T) {
	vars := map[string]any{
		"HeatNote": map[string]any{"serial": "SN001", "lot": "L1"},
		"OAuth":    map[string]any{"password": "secret-xyz"},
	}
	logger := &recordLogger{}
	env := &core.Env{Vars: vars, Log: logger}

	h := NewHookStop(env, nil, true) // skipOSS=true：不触网，走 debug 打印分支
	h.OnPlanStart(3, time.Now())

	cases := []report.CaseReport{
		{Seq: 1, Name: "A::one", Result: report.CaseOK, Time: 0.5},
		{Seq: 2, Name: "B::two", Result: report.CaseOK, Time: 0.6},
		{Seq: 3, Name: "C::three", Result: report.CaseFail, Time: 1.1, Error: "boom"},
	}
	for _, cr := range cases {
		h.OnCaseStart(cr.Seq, cr)
		h.OnCaseStop(cr.Seq, cr)
	}

	h.OnPlanStop(report.Summary{
		TotalNum:  3,
		PlanedNum: 3,
		OKNum:     2,
		FailNum:   1,
		Result:    0,
		Reason:    "C::three",
		TotalTime: 9.5,
	})

	out := logger.all()

	// 三段式 YAML 单独打印（debug 分支把 YAML 段与 buildReporter JSON payload
	// 分两条打出；第一条 Info 是 SaveTestData 摘要日志，JSON 的 log 字段也
	// 内含转义后的 YAML 全文，故三段断言只针对 YAML 打印段）。
	yamlOut := ""
	for _, msg := range logger.infos {
		if strings.HasPrefix(msg, "debug 模式，不保存测试记录，三段式 YAML 如下:") {
			yamlOut = msg
			break
		}
	}
	if yamlOut == "" {
		t.Fatalf("debug 分支未打印三段式 YAML，收到的 Info: %#v", logger.infos)
	}

	// 三段式 YAML：summary 段含 test_* 键。
	for _, want := range []string{
		"summary:",
		"test_total_num: 3",
		"test_fail_num: 1",
		"test_failreason: C::three",
	} {
		if !strings.Contains(yamlOut, want) {
			t.Errorf("三段式 YAML 缺 summary 段 %q in:\n%s", want, yamlOut)
		}
	}

	// env vars 段：HeatNote 透出 + 嵌套 password 打码，原值不泄漏。
	if !strings.Contains(yamlOut, "HeatNote:") || !strings.Contains(yamlOut, "lot: L1") {
		t.Errorf("三段式 YAML 缺 env vars 段（HeatNote）in:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "password: '******'") && !strings.Contains(yamlOut, "password: \"******\"") {
		t.Errorf("嵌套 password 应被打码为 ******，未发现。\n输出:\n%s", yamlOut)
	}
	if strings.Contains(out, "secret-xyz") {
		t.Errorf("password 原值泄露在输出中:\n%s", out)
	}

	// cases 段：3 个 case_seq，失败项含 error + result: fail。
	if got := strings.Count(yamlOut, "case_seq:"); got != 3 {
		t.Errorf("case_seq 数量 = %d, want 3\n输出:\n%s", got, yamlOut)
	}
	if !strings.Contains(yamlOut, "error: boom") {
		t.Errorf("case3 应含 error: boom in:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "result: fail") {
		t.Errorf("case3 应含 result: fail in:\n%s", yamlOut)
	}
	if !strings.Contains(yamlOut, "result: ok") {
		t.Errorf("case1/2 应含 result: ok in:\n%s", yamlOut)
	}

	// buildReporter 的 JSON payload 也应被打印（debug 分支保留）。
	if !strings.Contains(out, `"test_result": 0`) {
		t.Errorf("debug 分支应打印 buildReporter JSON payload（test_result: 0）in:\n%s", out)
	}
}

// TestCompressLog_ProducesLzmaAndPath 验证 compressLog 压缩后可解压还原，
// ossPath 符合 v2/<date>/<workstation>/log_<time>.lzma 模板（任务 B Refactor
// 抽取的 helper，纯逻辑不触网）。
func TestCompressLog_ProducesLzmaAndPath(t *testing.T) {
	content := []byte("summary:\n  test_total_num: 1\n---\n- case_seq: 1\n")

	compressed, ossPath, err := compressLog(content, "WS-01")
	if err != nil {
		t.Fatalf("compressLog() error = %v", err)
	}
	if len(compressed) == 0 {
		t.Fatal("compressLog() 返回空压缩字节")
	}
	// 路径模板：v2/<date>/<ws>/log_<time>.lzma
	if !strings.HasPrefix(ossPath, "v2/") || !strings.HasSuffix(ossPath, ".lzma") {
		t.Errorf("ossPath = %q, want 前缀 v2/ 后缀 .lzma", ossPath)
	}
	if !strings.Contains(ossPath, "/WS-01/") {
		t.Errorf("ossPath = %q, want 含工作站段 WS-01", ossPath)
	}

	// LZMA 可解压还原原始字节流。
	back, err := lzmaDecompress(compressed)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	if string(back) != string(content) {
		t.Errorf("解压内容不还原:\ngot  %q\nwant %q", back, content)
	}
}
