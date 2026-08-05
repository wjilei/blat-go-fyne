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
type Logger interface {
	Info(string)
	Warn(string)
	Error(string)
}

// UI is the minimal UI hook interface. Concrete implementations may be a
// console UI, a Tk UI, or a stub for tests.
//
// Prompt and WaitContinue take a context so the runner can cancel a
// blocking dialog (e.g. when the user presses the toolbar Stop button or
// the window is closed). Implementations MUST honor the context and
// return ctx.Err() when it is done.
type UI interface {
	Info(string)
	Prompt(ctx context.Context, label, def string) (string, error)
	WaitContinue(ctx context.Context, msg string) error
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
