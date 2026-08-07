// Package logfile 提供对齐 Perl BLAT（BLAT::Core::LogAnyConf + DisplayRole）
// 的文件日志：全部日志追加写入 test.log，界面从文件增量读取刷新；
// 每次开始测试时 Truncate 清空重写。
//
// 行格式对齐 Perl Log4perl PatternLayout：
//
//	%d{ABSOLUTE} %p %x %c %L - %m%n
//
// Go 侧无 NDC(%x) 与行号(%L)，用 ABSOLUTE 时间 + 大写 level + category
// 替代；`-` 分隔符照搬。时间戳用 Log4perl ABSOLUTE（HH:mm:ss,SSS）。
package logfile

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// FileLogger 是并发安全的文件日志：写端（任意 goroutine）与增量读端
// （Fyne 主线程）通过内部互斥锁串行化。gen 世代号在 Truncate 时递增，
// 供 TailFrom 检测"文件被截断重写"以从头读取，避免错位。
type FileLogger struct {
	mu   sync.Mutex
	f    *os.File
	size int64 // 已写字节数（含换行）
	gen  int   // 每次 Truncate 递增
}

// Open 打开（必要时创建）日志文件。路径相对当前工作目录（对齐 Perl
// run_dir/test.log）。
//
// 不用 O_APPEND：Windows 上 O_APPEND 会把句柄权限从 GENERIC_WRITE 降为
// FILE_APPEND_DATA，导致 Truncate（SetEndOfFile）报 Access is denied。
// 改为 O_RDWR + 每次写入前 Seek 到文件末尾（WriteLine 在 mu 锁内执行，
// 写位置串行化，效果等价于原子追加）。
func Open(path string) (*FileLogger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	l := &FileLogger{f: f}
	// 打开时文件可能已有内容（上次运行遗留），把 size 初始化为实际长度，
	// 保证增量读从文件末尾开始，不把旧日志灌进 UI。
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return l, nil
}

// Close 关闭底层文件。
func (l *FileLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// WriteLine 追加一行日志。format 内部使用，与调用方解耦：
// 15:04:05,000 LEVEL CATEGORY - msg\n
func (l *FileLogger) WriteLine(level, category, s string) error {
	ts := time.Now().Format("15:04:05,000")
	line := fmt.Sprintf("%s %s %s - %s\n", ts, strings.ToUpper(level), category, s)
	l.mu.Lock()
	defer l.mu.Unlock()
	// 无 O_APPEND：每次写前把文件位置移到末尾，等价于追加写。
	if _, err := l.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	n, err := l.f.WriteString(line)
	l.size += int64(n)
	return err
}

// Truncate 清空文件（对应 Perl DisplayRole test_start 的 `open $logfh, "+>"`
// 清空重写）。gen 递增，使所有在途的 TailFrom 增量读下次从头开始。
func (l *FileLogger) Truncate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gen++
	l.size = 0
	return l.f.Truncate(0)
}

// TailFrom 从 offset 增量读出新内容。
//
//   - gen 为调用方上次记录的世代号；offset 为上次读到的字节位置。
//   - 若 gen 与当前不一致，或文件长度小于 offset（外部截断），说明文件
//     已被清空重写，从头读并置 cleared=true。
//   - 返回 text（新内容，UTF-8）、size（文件当前长度）、newGen（当前世代）、
//     cleared（是否发生了清空重写，调用方应重置自身 offset/gen 并清 UI）。
//
// 该方法是幂等的：多个调用方（或同一调用方重复调用）得到一致结果。
func (l *FileLogger) TailFrom(offset int64, gen int) (text string, size int64, newGen int, cleared bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if gen != l.gen || l.size < offset {
		offset = 0
		cleared = true
	}
	if offset < l.size {
		buf := make([]byte, l.size-offset)
		if n, err := l.f.ReadAt(buf, offset); err == nil {
			text = string(buf[:n])
		}
	}
	return text, l.size, l.gen, cleared
}

// Snapshot 返回文件当前全部内容（供 hook_stop 上报）。空文件返回 ""。
func (l *FileLogger) Snapshot() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	buf := make([]byte, l.size)
	if n, err := l.f.ReadAt(buf, 0); err == nil {
		return string(buf[:n])
	}
	return ""
}
