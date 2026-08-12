package cases

import (
	"context"

	"blat/internal/core"
)

// SayByeCase 演示第二个 Case：询问告别对象并回显。
type SayByeCase struct {
	who string
}

func (c *SayByeCase) Name() string { return "SayBye" }

// Configure reads the "who" string from the YAML args; "guest" is used
// when the plan does not provide it.
func (c *SayByeCase) Configure(args map[string]any) error {
	if v, ok := args["who"].(string); ok && v != "" {
		c.who = v
	}
	return nil
}

func (c *SayByeCase) Run(ctx context.Context, env *core.Env) error {
	env.Log.Info("", "[SayBye] 准备告别 ...")
	def := c.who
	if def == "" {
		def = "guest"
	}
	name, err := env.UI.Prompt(ctx, "请输入告别对象", def)
	if err != nil {
		return err
	}
	env.Log.Info("", "Bye, " + name + "!")
	return env.UI.WaitContinue(ctx, "按回车继续")
}

func init() {
	Register("HelloSuite::SayBye", func() (core.Case, error) {
		return &SayByeCase{}, nil
	})
}
