package argus

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// scope holds the user, tags, contexts and breadcrumbs that go on events.
// The process has one global scope; WithScope layers a child on a context
// (Middleware does it per request), so one request's tags never leak into
// another's.
type scope struct {
	parent *scope // nil: the global scope sits underneath

	mu          sync.Mutex
	user        *User
	tags        map[string]string
	contexts    map[string]any
	extra       map[string]any
	request     *Request
	breadcrumbs []Breadcrumb
}

func newScope(parent *scope) *scope { return &scope{parent: parent} }

var globalScope atomic.Pointer[scope]

func init() { globalScope.Store(newScope(nil)) }

type scopeKey struct{}

func scopeFrom(ctx context.Context) *scope {
	if ctx != nil {
		if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
			return s
		}
	}
	return globalScope.Load()
}

// WithScope returns a context carrying a new scope layered on the current
// one. Tags, the user and breadcrumbs set through the returned context stay
// in it. Middleware calls it for every request; call it yourself for jobs or
// goroutines that should be kept apart.
func WithScope(ctx context.Context) context.Context {
	ctx = orBackground(ctx)
	var parent *scope
	if s, ok := ctx.Value(scopeKey{}).(*scope); ok {
		parent = s
	}
	return context.WithValue(ctx, scopeKey{}, newScope(parent))
}

// SetUser sets the user on the context's scope (or the global one).
func SetUser(ctx context.Context, user User) {
	s := scopeFrom(ctx)
	s.mu.Lock()
	u := user
	s.user = &u
	s.mu.Unlock()
}

// SetTag sets a tag on the context's scope (or the global one).
func SetTag(ctx context.Context, key, value string) {
	s := scopeFrom(ctx)
	s.mu.Lock()
	if s.tags == nil {
		s.tags = map[string]string{}
	}
	s.tags[key] = value
	s.mu.Unlock()
}

// SetTags sets several tags at once.
func SetTags(ctx context.Context, tags map[string]string) {
	for k, v := range tags {
		SetTag(ctx, k, v)
	}
}

// SetContext attaches a named block of data, such as "order", to events.
// A nil value removes it.
func SetContext(ctx context.Context, name string, value map[string]any) {
	s := scopeFrom(ctx)
	s.mu.Lock()
	if value == nil {
		delete(s.contexts, name)
	} else {
		if s.contexts == nil {
			s.contexts = map[string]any{}
		}
		s.contexts[name] = value
	}
	s.mu.Unlock()
}

// SetExtra attaches one extra value to events.
func SetExtra(ctx context.Context, key string, value any) {
	s := scopeFrom(ctx)
	s.mu.Lock()
	if s.extra == nil {
		s.extra = map[string]any{}
	}
	s.extra[key] = value
	s.mu.Unlock()
}

func setRequest(ctx context.Context, r *Request) {
	s := scopeFrom(ctx)
	s.mu.Lock()
	s.request = r
	s.mu.Unlock()
}

// AddBreadcrumb records something that happened, for the next error from this
// scope. A zero Timestamp means now.
func AddBreadcrumb(ctx context.Context, crumb Breadcrumb) {
	c := get()
	if c == nil {
		return
	}
	defer c.guard()
	c.addBreadcrumb(ctx, crumb)
}

func (c *client) addBreadcrumb(ctx context.Context, crumb Breadcrumb) {
	if crumb.Timestamp.IsZero() {
		crumb.Timestamp = time.Now()
	}
	b := &crumb
	if c.opts.BeforeBreadcrumb != nil {
		b = c.callBeforeBreadcrumb(b)
		if b == nil {
			return
		}
	}
	if u, ok := b.Data["url"].(string); ok {
		data := make(map[string]any, len(b.Data))
		for k, v := range b.Data {
			data[k] = v
		}
		data["url"] = stripQuery(u)
		b.Data = data
	}
	s := scopeFrom(ctx)
	s.mu.Lock()
	s.breadcrumbs = append(s.breadcrumbs, *b)
	if over := len(s.breadcrumbs) - c.opts.MaxBreadcrumbs; over > 0 {
		s.breadcrumbs = append(s.breadcrumbs[:0:0], s.breadcrumbs[over:]...)
	}
	s.mu.Unlock()
}

func (c *client) callBeforeBreadcrumb(b *Breadcrumb) (out *Breadcrumb) {
	defer func() {
		if r := recover(); r != nil {
			c.debugf("BeforeBreadcrumb panicked (%v); keeping the breadcrumb", r)
			out = b
		}
	}()
	return c.opts.BeforeBreadcrumb(b)
}

// applyScopes fills an event from the scope chain: global first, the
// innermost scope last so it wins.
func applyScopes(ctx context.Context, e *Event, max int) {
	var chain []*scope
	for s := scopeFrom(ctx); s != nil; s = s.parent {
		chain = append(chain, s)
	}
	if g := globalScope.Load(); len(chain) == 0 || chain[len(chain)-1] != g {
		chain = append(chain, g)
	}
	var crumbs []Breadcrumb
	for i := len(chain) - 1; i >= 0; i-- {
		s := chain[i]
		s.mu.Lock()
		if s.user != nil {
			u := *s.user
			e.User = &u
		}
		for k, v := range s.tags {
			if e.Tags == nil {
				e.Tags = map[string]string{}
			}
			e.Tags[k] = v
		}
		for k, v := range s.contexts {
			if e.Contexts == nil {
				e.Contexts = map[string]any{}
			}
			e.Contexts[k] = v
		}
		for k, v := range s.extra {
			if e.Extra == nil {
				e.Extra = map[string]any{}
			}
			e.Extra[k] = v
		}
		if s.request != nil {
			r := *s.request
			e.Request = &r
		}
		crumbs = append(crumbs, s.breadcrumbs...)
		s.mu.Unlock()
	}
	sort.SliceStable(crumbs, func(i, j int) bool { return crumbs[i].Timestamp.Before(crumbs[j].Timestamp) })
	if len(crumbs) > max {
		crumbs = crumbs[len(crumbs)-max:]
	}
	e.Breadcrumbs = crumbs
}
