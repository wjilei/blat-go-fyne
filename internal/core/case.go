package core

import "context"

// Case is a single test step. It receives the shared Env and reports an
// error if the step fails. A nil return value means the case passed.
type Case interface {
	Name() string
	Run(ctx context.Context, env *Env) error
}
