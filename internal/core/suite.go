package core

// Suite is a named group of Cases. The Runner executes Cases in slice order.
type Suite interface {
	Name() string
	Cases() []Case
}
