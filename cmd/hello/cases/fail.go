package cases

import (
	"context"
	"errors"

	"blat/internal/core"
)

// FailCase 总是返回 error，用于验证失败路径与 TAP "not ok" / "Bail out!"
// 输出。
type FailCase struct {
	reason string
}

func (c *FailCase) Name() string { return "FailCase" }

func (c *FailCase) Configure(args map[string]any) error {
	if v, ok := args["reason"].(string); ok && v != "" {
		c.reason = v
	}
	if c.reason == "" {
		c.reason = "intentional failure"
	}
	return nil
}

func (c *FailCase) Run(ctx context.Context, env *core.Env) error {
	env.Log.Info("[FailCase] running, will fail: " + c.reason)
	return errors.New(c.reason)
}

// func init() {
// 	Register("HelloSuite::FailCase", func() (core.Case, error) {
// 		return &FailCase{}, nil
// 	})
// }
