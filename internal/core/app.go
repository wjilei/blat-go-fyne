package core

import (
	"context"
	"fmt"
)

// App is the top level entry point. An App bundles one or more Suites and
// is what main() actually constructs.
type App interface {
	Name() string
	Suites() []Suite
	HookStop()
}

// Run is the convenience helper used by main. It walks the App's Suites in
// order and delegates to a Runner.
func Run(ctx context.Context, app App, env *Env) error {
	if app == nil {
		return fmt.Errorf("nil app")
	}
	if env == nil {
		return fmt.Errorf("nil env")
	}
	r := NewRunner()
	for _, s := range app.Suites() {
		env.Log.Info("", "== suite: " + s.Name())
		if err := r.Run(ctx, s, env); err != nil {
			return err
		}
	}
	return nil
}
