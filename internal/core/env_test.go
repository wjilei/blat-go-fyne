package core

import (
	"sync"
	"testing"
)

// categoryRecorder 记录 Logger 每次调用收到的 category，用于断言 Phase 2 A
// 扩展后的 Logger 契约：三个方法都接受 (category, msg)，且 category 原样
// 透传给实现（对齐 Perl LogAnyConf.pm 的 category=RUNNER 语义）。
type categoryRecorder struct {
	mu     sync.Mutex
	last   string
	lasts  []string
	msgs   []string
}

func (r *categoryRecorder) record(category, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = category
	r.lasts = append(r.lasts, category)
	r.msgs = append(r.msgs, msg)
}

func (r *categoryRecorder) Info(category, msg string)  { r.record(category, msg) }
func (r *categoryRecorder) Warn(category, msg string)  { r.record(category, msg) }
func (r *categoryRecorder) Error(category, msg string) { r.record(category, msg) }

// TestLogger_InterfaceExpansion 锁定新 Logger 契约：Info/Warn/Error 都带
// category 参数，且调用时传入的 category 必须被实现方完整收到。cat==""
// 表示不带分类（case 端旧用法），cat!="" 表示显式分类（RUNNER 等）。
func TestLogger_InterfaceExpansion(t *testing.T) {
	var l Logger = &categoryRecorder{}
	rec := &categoryRecorder{}
	l = rec

	l.Info("RUNNER", "case_start HelloSuite::SayHello")
	if rec.last != "RUNNER" {
		t.Errorf("Info category = %q, want RUNNER", rec.last)
	}

	l.Warn("CONFIG", "port missing")
	if rec.last != "CONFIG" {
		t.Errorf("Warn category = %q, want CONFIG", rec.last)
	}

	l.Error("UPLOAD", "oss failed")
	if rec.last != "UPLOAD" {
		t.Errorf("Error category = %q, want UPLOAD", rec.last)
	}

	// 空 category 也应透传（case 端当前用法走 ""）。
	l.Info("", "plain case log")
	if rec.last != "" {
		t.Errorf("Info empty category = %q, want \"\"", rec.last)
	}
	if len(rec.lasts) != 4 || len(rec.msgs) != 4 {
		t.Errorf("记录条数 = %d/%d, want 4/4", len(rec.lasts), len(rec.msgs))
	}
}
