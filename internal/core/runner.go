package core

import (
	"context"
	"fmt"
)

// Runner executes a Suite's Cases sequentially and stops on the first error.
// It mirrors the Perl BLAT::Core::Runner behavior.
type Runner struct{}

func NewRunner() *Runner { return &Runner{} }

// Run iterates over s.Cases() and returns the first failure wrapped with the
// case name. The Env is shared across every Case and must not be nil.
func (r *Runner) Run(ctx context.Context, s Suite, env *Env) error {
	if s == nil {
		return fmt.Errorf("nil suite")
	}
	if env == nil {
		return fmt.Errorf("nil env")
	}
	for _, c := range s.Cases() {
		if c == nil {
			return fmt.Errorf("suite %s: nil case", s.Name())
		}
		env.Log.Info(">>> " + c.Name())
		if err := c.Run(ctx, env); err != nil {
			return fmt.Errorf("case %s failed: %w", c.Name(), err)
		}
	}
	return nil
}
