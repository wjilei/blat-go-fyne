// Package core defines the minimal application framework.
//
// Mirrors the BLAT Perl layout:
//
//	App (AppBase)        -> core.App
//	Suite (SuiteRole)    -> core.Suite
//	Case                 -> core.Case
//	Runner               -> core.Runner
//	Test Environment     -> core.Env
package core

import (
	"context"
	"io"
)

// Logger is a minimal log interface so the framework does not depend on a
// concrete logger library.
//
// Phase 2 A：三个方法都带 category 参数，对齐 Perl LogAnyConf.pm 的
// `%c`（category）字段。cat=="" 表示不带分类（case 端旧用法沿用，调用侧
// 一律写 Info("", ...)）；cat!="" 表示显式分类（如 Runner 的 "RUNNER"）。
// Go 无方法重载，接口升级一次到位，所有实现同步改签名。
type Logger interface {
	Info(category, msg string)
	Warn(category, msg string)
	Error(category, msg string)
}

// UI is the minimal UI hook interface. Concrete implementations may be a
// console UI, a Tk UI, or a stub for tests.
//
// Prompt, WaitContinue, Message and Confirm take a context so the runner
// can cancel a blocking dialog (e.g. when the user presses the toolbar Stop
// button or the window is closed). Implementations MUST honor the context
// and return ctx.Err() when it is done.
//
// Info 与 Logger.Info 同名同签名（Phase 2 A 起带 category 参数），使
// Console/Fyne App 一个方法同时满足 UI 与 Logger 两个接口；Go 不允许同一
// 类型上有两个同名异参方法，UI 接口必须跟随 Logger 一起升级。
type UI interface {
	Info(category, msg string)
	Prompt(ctx context.Context, label, def string) (string, error)
	WaitContinue(ctx context.Context, msg string) error
	// Message 弹出一个只有"确定"按钮的消息框（无取消）。danger 为 true 时
	// 消息文字以错误色（红色）渲染，用于醒目提醒。返回 nil 表示用户已确认。
	Message(ctx context.Context, msg string, danger bool) error
	// Confirm 弹出一个"是/否"选择框，返回用户的选择。
	// true=是（继续），false=否（用例应主动失败）。GUI 实现通常对应
	// dialog.NewConfirm；Console 实现通过 stdin 输入 y/n 解析。
	Confirm(ctx context.Context, msg string, danger bool) (bool, error)
}

// Env is the shared context passed through every Case at run time. It carries
// configuration (Vars), devices, logger, UI hooks and an output writer for
// reporters.
type Env struct {
	Ctx  context.Context
	Log  Logger
	UI   UI
	Vars map[string]any
	// Devs maps a name to a business-level device. Values are any: low
	// level drivers implement Device, while higher-level devices (e.g.
	// bluetooth) hang methods directly off their concrete type and are
	// type-asserted by the Case that owns them.
	Devs map[string]any
	Out  io.Writer
}

// Device is re-declared here to avoid an import cycle. It is implemented in
// package internal/device but is consumed through this interface so that
// test code can supply fakes without importing the device package.
//
// The alias below is satisfied by *device.Device implementations and by any
// other type that implements Open/Close/Command.
type Device interface {
	Open(ctx context.Context) error
	Close() error
	Command(ctx context.Context, cmd string) (string, error)
}

// Configurable is implemented by Cases that want to receive YAML plan
// arguments before Run is called. The runtime package will call Configure
// once per attempt with the args hash parsed from the plan file. Cases
// that do not implement this interface simply ignore the args.
type Configurable interface {
	Configure(args map[string]any) error
}
