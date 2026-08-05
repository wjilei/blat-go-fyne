// Package cases holds all HelloSuite Case implementations and registers
// them automatically via init(). main.go just imports this package and
// calls cases.Global() — adding a new case only requires a new file in
// this directory.
package cases

import "blat/internal/runtime"

var global = runtime.NewRegistry()

// Global returns the process-wide registry. It is populated by init()
// in each case file under this package.
func Global() *runtime.Registry { return global }

// Register adds a case factory. Each case file calls this from init().
func Register(name string, f runtime.Factory) {
	global.Register(name, f)
}
