package argus

import (
	"context"
	"errors"
	"log/slog"
)

// SlogOptions configures NewSlogHandler.
type SlogOptions struct {
	// Level is the lowest level sent to Argus Logs. Default slog.LevelInfo.
	Level slog.Leveler
	// Next also receives every record, so local output keeps working:
	//
	//	argus.NewSlogHandler(&argus.SlogOptions{Next: slog.NewTextHandler(os.Stderr, nil)})
	Next slog.Handler
	// CaptureErrors also sends records at slog.LevelError and above as Argus
	// error events. A record with an error-valued attribute is sent as that
	// exception; otherwise as a message.
	CaptureErrors bool
}

// NewSlogHandler returns a slog.Handler that sends records to Argus Logs,
// with the active span's trace id, and leaves a breadcrumb for the next error.
//
//	slog.SetDefault(slog.New(argus.NewSlogHandler(&argus.SlogOptions{
//		Next: slog.NewJSONHandler(os.Stdout, nil),
//	})))
//
// Pass the request's context (slog.InfoContext) so logs join its trace.
func NewSlogHandler(opts *SlogOptions) slog.Handler {
	h := &slogHandler{}
	if opts != nil {
		h.opts = *opts
	}
	if h.opts.Level == nil {
		h.opts.Level = slog.LevelInfo
	}
	h.next = h.opts.Next
	return h
}

type slogHandler struct {
	opts   SlogOptions
	next   slog.Handler
	attrs  []attr
	errs   []error
	prefix string
}

func (h *slogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.opts.Level.Level() {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, level)
}

func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next != nil && h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	if r.Level >= h.opts.Level.Level() {
		h.send(ctx, r)
	}
	return err
}

func (h *slogHandler) send(ctx context.Context, r slog.Record) {
	c := get()
	if c == nil {
		return
	}
	defer c.guard()
	attrs := append([]attr(nil), h.attrs...)
	errs := append([]error(nil), h.errs...)
	r.Attrs(func(a slog.Attr) bool {
		if e := errorValue(a); e != nil {
			errs = append(errs, e)
		}
		attrs = flattenSlog(attrs, h.prefix, a)
		return true
	})
	sev := sevInfo + int(r.Level)
	c.log(ctx, sev, r.Message, append([]attr(nil), attrs...))

	level := LevelInfo
	switch {
	case r.Level >= slog.LevelError:
		level = LevelError
	case r.Level >= slog.LevelWarn:
		level = LevelWarning
	case r.Level < slog.LevelInfo:
		level = LevelDebug
	}
	data := map[string]any{}
	for _, a := range attrs {
		data[a.key] = a.val
	}
	c.addBreadcrumb(ctx, Breadcrumb{Type: "default", Category: "log", Message: truncate(r.Message, 1000), Level: level, Data: data})

	if h.opts.CaptureErrors && r.Level >= slog.LevelError {
		d := Details{Level: level, Extra: map[string]any{"log.message": r.Message}}
		for k, v := range data {
			d.Extra[k] = v
		}
		if len(errs) > 0 {
			err := errors.Join(errs...)
			if len(errs) == 1 {
				err = errs[0]
			}
			c.capture(ctx, &Event{Exception: exceptionChain(err, callerStack())}, d)
		} else {
			c.capture(ctx, &Event{Message: r.Message}, d)
		}
	}
}

func (h *slogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	h2 := *h
	h2.attrs = append([]attr(nil), h.attrs...)
	h2.errs = append([]error(nil), h.errs...)
	for _, a := range as {
		if e := errorValue(a); e != nil {
			h2.errs = append(h2.errs, e)
		}
		h2.attrs = flattenSlog(h2.attrs, h.prefix, a)
	}
	if h.next != nil {
		h2.next = h.next.WithAttrs(as)
	}
	return &h2
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.prefix = h.prefix + name + "."
	if h.next != nil {
		h2.next = h.next.WithGroup(name)
	}
	return &h2
}

func errorValue(a slog.Attr) error {
	v := a.Value.Resolve()
	if v.Kind() != slog.KindAny {
		return nil
	}
	e, _ := v.Any().(error)
	return e
}
