package logfile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWriteLineFormat 校验单行格式对齐 Perl Log4perl PatternLayout：
//
//	%d{ABSOLUTE} %p %x %c %L - %m%n  →  15:04:05,000 INFO APP - hello
func TestWriteLineFormat(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := l.WriteLine("info", "APP", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := l.WriteLine("warn", "TAP", "not ok 1"); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), string(raw))
	}
	re := regexp.MustCompile(`^\d{2}:\d{2}:\d{2},\d{3} (INFO|WARN) (APP|TAP) - .+$`)
	for _, ln := range lines {
		if !re.MatchString(ln) {
			t.Errorf("line format mismatch: %q", ln)
		}
	}
	if !strings.Contains(lines[0], "APP - hello") {
		t.Errorf("line0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "TAP - not ok 1") {
		t.Errorf("line1 = %q", lines[1])
	}
}

// TestTruncate 校验 Truncate 后文件清空、后续写入从空文件开始。
func TestTruncate(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := l.WriteLine("info", "APP", "before"); err != nil {
		t.Fatal(err)
	}
	if err := l.Truncate(); err != nil {
		t.Fatal(err)
	}
	if got := l.Snapshot(); got != "" {
		t.Fatalf("after Truncate Snapshot = %q, want empty", got)
	}
	if err := l.WriteLine("info", "APP", "after"); err != nil {
		t.Fatal(err)
	}
	if got := l.Snapshot(); !strings.Contains(got, "after") || strings.Contains(got, "before") {
		t.Fatalf("Snapshot = %q", got)
	}
}

// TestTailFrom 校验增量读：offset 之外的新行才返回，旧 offset 读为空。
func TestTailFrom(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if err := l.WriteLine("info", "APP", "line1"); err != nil {
		t.Fatal(err)
	}
	text, size, gen, cleared := l.TailFrom(0, 0)
	if cleared {
		t.Error("first read should not be cleared")
	}
	if !strings.Contains(text, "line1") {
		t.Errorf("first read = %q", text)
	}
	if size <= 0 || gen != 0 {
		t.Errorf("size=%d gen=%d", size, gen)
	}

	// 无新数据：空读
	text, size2, _, cleared := l.TailFrom(size, gen)
	if text != "" {
		t.Errorf("empty read = %q", text)
	}
	if size2 != size {
		t.Errorf("size changed on empty read: %d -> %d", size, size2)
	}
	if cleared {
		t.Error("empty read should not be cleared")
	}

	// 新增一行：只返回新内容
	if err := l.WriteLine("info", "APP", "line2"); err != nil {
		t.Fatal(err)
	}
	text, size3, _, cleared := l.TailFrom(size, gen)
	if !strings.Contains(text, "line2") || strings.Contains(text, "line1") {
		t.Errorf("incremental read = %q", text)
	}
	if size3 <= size {
		t.Errorf("size did not grow: %d -> %d", size, size3)
	}
}

// TestTailFromCleared 校验截断后：gen 变化 → cleared=true 且从头读新内容
// （即使新内容长度超过旧 offset 也能正确从头读，避免错位）。
func TestTailFromCleared(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 写大量内容，让旧 offset 足够大
	for i := 0; i < 50; i++ {
		if err := l.WriteLine("info", "APP", "filler line for offset"); err != nil {
			t.Fatal(err)
		}
	}
	_, size, gen, _ := l.TailFrom(0, 0)
	if size == 0 {
		t.Fatal("size should be > 0")
	}

	// 截断 + 重写，且新内容长度必须超过旧 offset（覆盖最刁钻的竞态）
	if err := l.Truncate(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if err := l.WriteLine("info", "APP", "post-truncate content"); err != nil {
			t.Fatal(err)
		}
	}

	text, newSize, newGen, cleared := l.TailFrom(size, gen)
	if !cleared {
		t.Error("gen changed: expect cleared=true")
	}
	if newGen == gen {
		t.Error("gen should advance after Truncate")
	}
	if !strings.Contains(text, "post-truncate content") {
		t.Errorf("read = %q (truncated at %q)", text[:min(80, len(text))], text)
	}
	if strings.Contains(text, "filler line") {
		t.Error("should not contain pre-truncate content")
	}
	if newSize <= 0 {
		t.Error("new size should be > 0")
	}
	// 若 cleared 处理正确，增量位置应等于新 size，而不是被旧 offset 污染
	if text != "" && int64(len(text)) != newSize {
		t.Errorf("text len %d != newSize %d (cleared 后必须从头完整读)", len(text), newSize)
	}
}

// TestSnapshot 校验全量读包含全部行。
func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(filepath.Join(dir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for i := 0; i < 5; i++ {
		if err := l.WriteLine("debug", "APP", "snap"); err != nil {
			t.Fatal(err)
		}
	}
	got := l.Snapshot()
	if strings.Count(got, "snap") != 5 {
		t.Fatalf("Snapshot = %q, want 5 lines", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("Snapshot should end with newline: %q", got)
	}
}
