package argus

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Middleware traces incoming requests and reports their panics.
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("GET /orders/{id}", getOrder)
//	http.ListenAndServe(":8080", argus.Middleware(mux))
//
// For each request it:
//   - starts a server span, continuing the caller's W3C traceparent; the span
//     is named "GET /orders/{id}" and carries http.route when the handler is
//     a Go 1.23+ ServeMux pattern (set the "http.route" attribute yourself
//     with other routers);
//   - gives the request its own scope, so tags, the user and breadcrumbs set
//     with the request's context stay on that request's errors;
//   - reports a panic with the request attached, then panics again so
//     net/http handles it as it would have without Argus.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := get()
		if c == nil {
			next.ServeHTTP(w, r)
			return
		}
		r2, span := c.startRequest(r)
		if r2 == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			v := recover()
			c.endRequest(r2, span, rec, v)
			if v != nil {
				panic(v)
			}
		}()
		next.ServeHTTP(rec, r2)
	})
}

func (c *client) startRequest(r *http.Request) (out *http.Request, span *Span) {
	defer func() {
		if v := recover(); v != nil {
			c.debugf("middleware: internal error (%v); request not traced", v)
			out, span = nil, nil
		}
	}()
	ctx := WithScope(r.Context())
	if tp := r.Header.Get("traceparent"); tp != "" {
		ctx = ContinueTrace(ctx, tp)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	full := scheme + "://" + r.Host + r.URL.EscapedPath()
	ctx, span = c.startSpan(ctx, r.Method+" "+r.URL.Path, SpanKindServer, []attr{
		{"http.request.method", r.Method},
		{"url.path", r.URL.Path},
		{"url.scheme", scheme},
		{"url.full", full},
		{"server.address", r.Host},
	})
	headers := map[string]string{}
	for k, v := range r.Header {
		if len(v) == 0 || strings.EqualFold(k, "Cookie") {
			continue
		}
		headers[k] = v[0]
	}
	setRequest(ctx, &Request{Method: r.Method, URL: full, Headers: headers})
	out = r.WithContext(ctx)
	span.req = out
	if c.opts.SendDefaultPii {
		if ip := clientIP(r); ip != "" {
			SetUser(ctx, User{IPAddress: ip})
		}
	}
	return out, span
}

func (c *client) endRequest(r *http.Request, span *Span, rec *statusRecorder, panicked any) {
	defer c.guard()
	method := r.Method
	route := ""
	if v, ok := span.attribute("http.route"); ok {
		route, _ = v.(string)
	} else if route = routePattern(r); route != "" {
		span.SetAttribute("http.route", route)
	}
	if route != "" {
		span.SetName(method + " " + route)
	}
	status := rec.status
	if panicked != nil {
		if err, ok := panicked.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			span.End()
			return
		}
		if !rec.wrote {
			status = http.StatusInternalServerError
		}
		span.setErrorMessage(fmt.Sprintf("panic: %v", panicked))
		c.capturePanic(r.Context(), panicked, LevelError)
	}
	if status == 0 {
		status = http.StatusOK
	}
	span.SetAttribute("http.response.status_code", status)
	if status >= 500 {
		span.setErrorMessage(fmt.Sprintf("HTTP %d", status))
	}
	span.End()
}

// routePattern turns a ServeMux pattern ("GET example.com/orders/{id}") into
// an http.route ("/orders/{id}").
func routePattern(r *http.Request) string {
	p := requestPattern(r)
	if p == "" {
		return ""
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		p = strings.TrimSpace(p[i+1:])
	}
	if !strings.HasPrefix(p, "/") {
		if i := strings.IndexByte(p, '/'); i >= 0 {
			p = p[i:]
		} else {
			return ""
		}
	}
	return p
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// statusRecorder notes the status code. It keeps Flush and Hijack working,
// and Unwrap lets http.ResponseController reach the original writer.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		if !s.wrote {
			s.status, s.wrote = http.StatusOK, true
		}
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		s.wrote = true
		if s.status == 0 {
			s.status = http.StatusSwitchingProtocols
		}
		return h.Hijack()
	}
	return nil, nil, errors.New("argus: the response writer does not support hijacking")
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// RoundTripper wraps an http.RoundTripper (nil means http.DefaultTransport)
// so outgoing requests made inside a span become client spans, carry a W3C
// traceparent header, and leave a breadcrumb:
//
//	client := &http.Client{Transport: argus.RoundTripper(nil)}
//	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.paystack.co/…", nil)
//
// The request's context must carry the span (use NewRequestWithContext).
func RoundTripper(base http.RoundTripper) http.RoundTripper {
	return &roundTripper{base: base}
}

type roundTripper struct{ base http.RoundTripper }

func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	c := get()
	ctx := req.Context()
	if c == nil || isInternal(ctx) {
		return base.RoundTrip(req)
	}
	var span *Span
	out := req
	url := stripQuery(req.URL.String())
	func() {
		defer c.guard()
		if SpanFromContext(ctx) == nil {
			return
		}
		_, span = c.startSpan(ctx, req.Method+" "+url, SpanKindClient, []attr{
			{"http.request.method", req.Method},
			{"url.full", url},
			{"server.address", req.URL.Hostname()},
		})
		if req.Header.Get("traceparent") == "" {
			out = req.Clone(ctx)
			out.Header.Set("traceparent", span.Traceparent())
		}
	}()
	res, err := base.RoundTrip(out)
	func() {
		defer c.guard()
		data := map[string]any{"method": req.Method, "url": url}
		crumb := Breadcrumb{Type: "http", Category: "http"}
		if err != nil {
			span.SetError(err)
			crumb.Level = LevelError
		} else {
			data["status_code"] = res.StatusCode
			span.SetAttribute("http.response.status_code", res.StatusCode)
			if res.StatusCode >= 500 {
				span.setErrorMessage(fmt.Sprintf("HTTP %d", res.StatusCode))
			}
		}
		crumb.Data = data
		c.addBreadcrumb(ctx, crumb)
		span.End()
	}()
	return res, err
}
