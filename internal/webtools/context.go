package webtools

import (
	"context"
	"time"
)

// Context is the cancellation scope one adapter call runs under. It is an alias
// so a caller can pass a request's own context without importing anything else
// from this package.
type Context = context.Context

func contextBackground() Context { return context.Background() }

func contextWithTimeout(parent Context, timeout time.Duration) (Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

func contextWithCancel(parent Context) (Context, context.CancelFunc) {
	return context.WithCancel(parent)
}
