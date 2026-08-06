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
)

// Console is a blocking, line-based UI used as the default reference
// implementation. It satisfies both core.UI and core.Logger.
type Console struct {
	r   *bufio.Reader
	w   io.Writer
	log io.Writer

	mu   sync.Mutex
	buf  []string // 日志环形缓冲，供上报逻辑（hook_stop）取完整日志
	cap  int
}

// NewConsole returns a Console that reads from stdin and writes prompts to
// stdout. The log writer is also stdout by default; pass NewConsoleWith to
// redirect it.
func NewConsole() *Console {
	return &Console{
		r:   bufio.NewReader(os.Stdin),
		w:   os.Stdout,
		log: os.Stdout,
		cap: 1000,
	}
}

// NewConsoleWith allows redirecting the log channel separately from the
// prompt channel, which is useful in tests.
func NewConsoleWith(in io.Reader, promptOut, logOut io.Writer) *Console {
	return &Console{
		r:   bufio.NewReader(in),
		w:   promptOut,
		log: logOut,
		cap: 1000,
	}
}

func (c *Console) Info(s string)  { c.logLine("[INFO]", s) }
func (c *Console) Warn(s string)  { c.logLine("[WARN]", s) }
func (c *Console) Error(s string) { c.logLine("[ERROR]", s) }

func (c *Console) logLine(prefix, s string) {
	line := prefix + " " + s
	fmt.Fprintln(c.log, line)
	c.mu.Lock()
	c.buf = append(c.buf, line)
	if len(c.buf) > c.cap {
		c.buf = c.buf[len(c.buf)-c.cap:]
	}
	c.mu.Unlock()
}

// SnapshotLog 返回已缓冲的日志行（按时间顺序，每行以换行结尾）。
// 供上报逻辑（hook_stop 上传 OSS/存库）在计划结束后取完整日志使用。
func (c *Console) SnapshotLog() string {
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
