package argus

import (
	"context"
)

// Recover reports a panic, flushes, and panics again with the same value, so
// the program fails exactly as it would have without Argus. Defer it first
// thing in main and at the top of every goroutine you start:
//
//	func main() {
//		argus.Init(argus.Options{})
//		defer argus.Recover()
//		…
//	}
//
// It must be deferred directly (defer argus.Recover()), not called from
// another deferred function, because that is the only place recover works.
func Recover() {
	if v := recover(); v != nil {
		recoverAndRepanic(context.Background(), v)
	}
}

// RecoverWithContext is Recover with the context's scope and span on the event.
//
//	go func() {
//		defer argus.RecoverWithContext(ctx)
//		work(ctx)
//	}()
func RecoverWithContext(ctx context.Context) {
	if v := recover(); v != nil {
		recoverAndRepanic(ctx, v)
	}
}

func recoverAndRepanic(ctx context.Context, v any) {
	if c := get(); c != nil {
		func() {
			defer c.guard()
			c.capturePanic(ctx, v, LevelFatal)
			c.flush(defaultFlushTimeout)
		}()
	}
	panic(v)
}

// CapturePanic reports a value you recovered yourself and returns its event
// id. Use it when the program should carry on after the panic:
//
//	defer func() {
//		if v := recover(); v != nil {
//			argus.CapturePanic(ctx, v)
//		}
//	}()
func CapturePanic(ctx context.Context, v any) string {
	c := get()
	if c == nil || v == nil {
		return ""
	}
	defer c.guard()
	return c.capturePanic(ctx, v, LevelError)
}
