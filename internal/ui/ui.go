// Package ui provides a minimal console implementation of core.UI suitable
// for the smallest possible application. Real factories should plug in a Tk
// or web UI by satisfying the same interface in another file.
package ui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"blat/internal/config"
	"blat/internal/logfile"
)

// Console is a blocking, line-based UI used as the default reference
// implementation. It satisfies both core.UI and core.Logger.
//
// 日志链路对齐 Perl BLAT：全部日志写 test.log（run_dir/test.log），
// SnapshotLog 从文件读全量供上报；同时保留 stdout 打印（无头模式的可视
// 输出）与内存环形缓冲（NewConsoleWith 测试构造时不建文件，回退用）。
type Console struct {
	r    *bufio.Reader
	w    io.Writer
	log  io.Writer
	file *logfile.FileLogger // 日志文件；NewConsoleWith 不建文件时为 nil

	mu  sync.Mutex
	buf []string // 日志环形缓冲，文件不可用时兜底供上报
	cap int
}

// NewConsole returns a Console that reads from stdin and writes prompts to
// stdout. The log writer is also stdout by default; pass NewConsoleWith to
// redirect it. 日志文件 test.log 在打开时清空重写（Console 模式每次进程
// 启动视为一次运行；GUI 模式由 App.startRun 每次点击"开始测试"清空）。
// 落盘路径由 config.DefaultTestLogPath 决定：release → ~/.blat/test.log，
// dev（go run）→ ./test.log。
func NewConsole() *Console {
	c := &Console{
		r:   bufio.NewReader(os.Stdin),
		w:   os.Stdout,
		log: os.Stdout,
		cap: 1000,
	}
	if f, err := logfile.Open(config.DefaultTestLogPath()); err == nil {
		_ = f.Truncate()
		c.file = f
	} else {
		fmt.Fprintln(os.Stderr, "open test.log:", err)
	}
	return c
}

// NewConsoleWith allows redirecting the log channel separately from the
// prompt channel, which is useful in tests. 不建日志文件：写日志、但
// SnapshotLog 回退到内存环形缓冲。
func NewConsoleWith(in io.Reader, promptOut, logOut io.Writer) *Console {
	return &Console{
		r:   bufio.NewReader(in),
		w:   promptOut,
		log: logOut,
		cap: 1000,
	}
}

// Info/Warn/Error 实现 core.Logger（Phase 2 A：category 透传，空串表示
// 无分类）。category 最终由 logfile 写进文件行（对齐 LogAnyConf 的 %c）。
func (c *Console) Info(category, s string)  { c.logLine("info", category, s) }
func (c *Console) Warn(category, s string)  { c.logLine("warn", category, s) }
func (c *Console) Error(category, s string) { c.logLine("error", category, s) }

func (c *Console) logLine(level, category, s string) {
	line := "[" + strings.ToUpper(level) + "] " + s
	fmt.Fprintln(c.log, line)
	if c.file != nil {
		// 文件行格式与 GUI 一致（15:04:05,000 LEVEL CATEGORY - msg）。
		_ = c.file.WriteLine(level, category, s)
	}
	c.mu.Lock()
	c.buf = append(c.buf, line)
	if len(c.buf) > c.cap {
		c.buf = c.buf[len(c.buf)-c.cap:]
	}
	c.mu.Unlock()
}

// Logfile 返回 Console 持有的 *logfile.FileLogger（NewConsole 创建、test.log）；
// NewConsoleWith 等测试构造里为 nil。供装配阶段把同一个 logfile 注入到
// report.YAMLReporter 的 WithLogfile，让 Console 模式的 report.yml 也能
// 拿到每条 case 的窗口切片日志（GUI 模式由 fyneui.App 直接持有 logfile，
// Console 模式此前未对外暴露）。
func (c *Console) Logfile() *logfile.FileLogger {
	return c.file
}

// SnapshotLog 返回当前全部日志行（按时间顺序，每行以换行结尾）。
// 供上报逻辑（hook_stop 上传 OSS/存库）在计划结束后取完整日志使用。
// 文件日志方案下直接读 test.log 全量；文件不可用时回退内存环形缓冲。
func (c *Console) SnapshotLog() string {
	if c.file != nil {
		return c.file.Snapshot()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.buf, "\n") + "\n"
}

// Prompt blocks until the user enters a line or ctx is cancelled.
func (c *Console) Prompt(ctx context.Context, label, def string) (string, error) {
	fmt.Fprintf(c.w, "%s [%s]: ", label, def)
	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := c.r.ReadString('\n')
		if err != nil {
			ch <- result{err: err}
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = def
		}
		ch <- result{val: line}
	}()
	select {
	case r := <-ch:
		return r.val, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// WaitContinue blocks until the user presses Enter or ctx is cancelled.
func (c *Console) WaitContinue(ctx context.Context, msg string) error {
	fmt.Fprintln(c.w, msg, "(press Enter to continue)")
	ch := make(chan error, 1)
	go func() {
		_, err := c.r.ReadString('\n')
		ch <- err
	}()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Message 打印消息并等待回车（Console 无颜色能力；danger 时用 Error 级别
// 打印体现红色语义）。
func (c *Console) Message(ctx context.Context, msg string, danger bool) error {
	if danger {
		c.Error("", msg)
	} else {
		c.Info("", msg)
	}
	return c.WaitContinue(ctx, msg)
}

// Confirm 在 stdin 上读取一行 y/n 解析为 bool。空行视作"否"。
// 同时支持中文"是/否"。ctx 取消时返回 ctx.Err()。
func (c *Console) Confirm(ctx context.Context, msg string, danger bool) (bool, error) {
	fmt.Fprintf(c.w, "%s [y/N]: ", msg)
	ch := make(chan struct {
		val bool
		err error
	}, 1)
	go func() {
		line, err := c.r.ReadString('\n')
		if err != nil {
			ch <- struct {
				val bool
				err error
			}{err: err}
			return
		}
		ch <- struct {
			val bool
			err error
		}{val: _parseYesNo(strings.TrimSpace(line))}
	}()
	select {
	case r := <-ch:
		return r.val, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// _parseYesNo 解析 y/n/yes/no（大小写不敏感），空字符串返回 false。
// 中文"是/否"也接受：含"否"字直接判负，避免"否是"这种输入误判。
func _parseYesNo(s string) bool {
	low := strings.ToLower(s)
	if low == "" {
		return false
	}
	if strings.Contains(s, "否") {
		return false
	}
	if strings.Contains(s, "是") {
		return true
	}
	if low == "y" || low == "yes" {
		return true
	}
	return false
}
