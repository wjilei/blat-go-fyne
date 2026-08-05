package cases

import (
	"context"

	"blat/internal/core"
)

// SayHelloCase 是最简演示 Case：问候并询问姓名。
type SayHelloCase struct {
	who string
}

func (c *SayHelloCase) Name() string { return "SayHello" }

// Configure reads the "who" string from the YAML args.
func (c *SayHelloCase) Configure(args map[string]any) error {
	if v, ok := args["who"].(string); ok && v != "" {
		c.who = v
	}
	return nil
}

func (c *SayHelloCase) Run(ctx context.Context, env *core.Env) error {
	env.UI.Info("Hello, " + c.who)
	name, err := env.UI.Prompt(ctx, "Your name", "guest")
	if err != nil {
		return err
	}
	env.UI.Info("Welcome, " + name)
	return env.UI.WaitContinue(ctx, "done")
}

func init() {
	Register("HelloSuite::SayHello", func() (core.Case, error) {
		return &SayHelloCase{}, nil
	})
}
