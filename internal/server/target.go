package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sync"
	"time"
)

var (
	ErrorInvalidHostPattern = errors.New("invalid host pattern")
	ErrorDraining           = errors.New("target is draining")

	hostRegex = regexp.MustCompile(`^(\w[-_.\w+]+)(:\d+)?$`)
)

type TargetState int

const (
	TargetStateAdding TargetState = iota
	TargetStateDraining
	TargetStateHealthy
)

func (ts TargetState) String() string {
	switch ts {
	case TargetStateAdding:
		return "adding"
	case TargetStateDraining:
		return "draining"
	case TargetStateHealthy:
		return "healthy"
	}
	return ""
}

type inflightRequest struct {
	cancel   context.CancelFunc
	hijacked bool
}

type inflightMap map[*http.Request]*inflightRequest

type TargetOptions struct {
	HealthCheckConfig   HealthCheckConfig `json:"health_check_config"`
	ResponseTimeout     time.Duration     `json:"response_timeout"`
	BufferRequests      bool              `json:"buffer_requests"`
	BufferResponses     bool              `json:"buffer_responses"`
	MaxMemoryBufferSize int64             `json:"max_memory_buffer_size"`
	MaxRequestBodySize  int64             `json:"max_request_body_size"`
	MaxResponseBodySize int64             `json:"max_response_body_size"`
	LogRequestHeaders   []string          `json:"log_request_headers"`
	LogResponseHeaders  []string          `json:"log_response_headers"`
	ForwardHeaders      bool              `json:"forward_headers"`
}

func (to *TargetOptions) canonicalizeLogHeaders() {
	for i, header := range to.LogRequestHeaders {
		to.LogRequestHeaders[i] = http.CanonicalHeaderKey(header)
	}
	for i, header := range to.LogResponseHeaders {
		to.LogResponseHeaders[i] = http.CanonicalHeaderKey(header)
	}
}

type Target struct {
	targetURL    *url.URL
	options      TargetOptions
	proxyHandler http.Handler

	state        TargetState
	inflight     inflightMap
	inflightLock sync.Mutex

	healthcheck   *HealthCheck
	becameHealthy chan (bool)
}

func NewTarget(targetURL string, options TargetOptions) (*Target, error) {
	uri, err := parseTargetURL(targetURL)
	if err != nil {
		return nil, err
	}

	options.canonicalizeLogHeaders()

	target := &Target{
		targetURL: uri,
		options:   options,

		state:    TargetStateAdding,
		inflight: inflightMap{},
	}

	target.proxyHandler = target.createProxyHandler()

	if options.BufferResponses {
		target.proxyHandler = WithResponseBufferMiddleware(options.MaxMemoryBufferSize, options.MaxResponseBodySize, target.proxyHandler)
	}
	if options.BufferRequests {
		target.proxyHandler = WithRequestBufferMiddleware(options.MaxMemoryBufferSize, options.MaxRequestBodySize, target.proxyHandler)
	}

	return target, nil
}

func (t *Target) Target() string {
	return t.targetURL.Host
}

func (t *Target) StartRequest(req *http.Request) (*http.Request, error) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	if t.state == TargetStateDraining {
		return nil, ErrorDraining
	}

	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	inflightRequest := &inflightRequest{cancel: cancel}
	t.inflight[req] = inflightRequest

	return req, nil
}

func (t *Target) SendRequest(w http.ResponseWriter, req *http.Request) {
	LoggingRequestContext(req).Target = t.Target()
	LoggingRequestContext(req).RequestHeaders = t.options.LogRequestHeaders
	LoggingRequestContext(req).ResponseHeaders = t.options.LogResponseHeaders

	inflightRequest := t.getInflightRequest(req)
	defer t.endInflightRequest(req)

	tw := newTargetResponseWriter(w, inflightRequest)
	t.proxyHandler.ServeHTTP(tw, req)
}

func (t *Target) IsHealthCheckRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == t.options.HealthCheckConfig.Path
}

func (t *Target) Drain(timeout time.Duration) {
	originalState := t.updateState(TargetStateDraining)
	if originalState == TargetStateDraining {
		return
	}
	defer t.updateState(originalState)

	deadline := time.After(timeout)
	toCancel := t.pendingRequestsToCancel()

	// Cancel any hijacked requests immediately, as they may be long-running.
	for _, inflight := range toCancel {
		if inflight.hijacked {
			inflight.cancel()
		}
	}

WAIT_FOR_REQUESTS_TO_COMPLETE:
	for req := range toCancel {
		select {
		case <-req.Context().Done():
		case <-deadline:
			break WAIT_FOR_REQUESTS_TO_COMPLETE
		}
	}

	// Cancel any remaining requests.
	for _, inflight := range toCancel {
		inflight.cancel()
	}
}

func (t *Target) BeginHealthChecks() {
	t.becameHealthy = make(chan bool)
	t.healthcheck = NewHealthCheck(t,
		t.targetURL.JoinPath(t.options.HealthCheckConfig.Path),
		t.options.HealthCheckConfig.Interval,
		t.options.HealthCheckConfig.Timeout,
	)
}

func (t *Target) StopHealthChecks() {
	if t.healthcheck != nil {
		t.healthcheck.Close()
		t.healthcheck = nil
	}
}

func (t *Target) WaitUntilHealthy(timeout time.Duration) bool {
	t.BeginHealthChecks()
	defer t.StopHealthChecks()

	select {
	case <-time.After(timeout):
		return false
	case <-t.becameHealthy:
		return true
	}
}

// HealthCheckConsumer

func (t *Target) HealthCheckCompleted(success bool) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	if success && t.state == TargetStateAdding {
		t.state = TargetStateHealthy
		close(t.becameHealthy)
	}

	slog.Info("Target health updated", "target", t.Target(), "success", success, "state", t.state.String())
}

// Private

func (t *Target) createProxyHandler() http.Handler {
	bufferPool := NewBufferPool(ProxyBufferSize)

	return &httputil.ReverseProxy{
		BufferPool:   bufferPool,
		Rewrite:      t.rewrite,
		ErrorHandler: t.handleProxyError,
		Transport: &http.Transport{
			MaxIdleConnsPerHost:   MaxIdleConnsPerHost,
			ResponseHeaderTimeout: t.options.ResponseTimeout,
		},
	}
}

func (t *Target) rewrite(req *httputil.ProxyRequest) {
	t.forwardHeaders(req)

	req.SetURL(t.targetURL)
	req.Out.Host = req.In.Host

	// Ensure query params are preserved exactly, including those we could not
	// parse.
	//
	// By default, httputil.ReverseProxy will drop unparseable query params to
	// guard against parameter smuggling attacks
	// (https://github.com/golang/go/issues/54663).
	//
	// One example of this is the use of semicolons in query params. Given a URL
	// like:
	//
	//   /path?p=a;b
	//
	// Some platforms interpret these params as equivalent to `p=a` and `b=`,
	// while others interpret it as a single query param: `p=a;b`. Because of this
	// confusion, Go's default behaviour is to drop the parameter entirely,
	// effectively turning our URL into just `/path`.
	//
	// However, any changes to the query params could break applications that
	// depend on them, so we should avoid doing this, and strive to be as
	// transparent as possible.
	//
	// In our case, we don't make any decisions based on the query params, so it's
	// safe for us to pass them through verbatim.
	req.Out.URL.RawQuery = req.In.URL.RawQuery
}

func (t *Target) forwardHeaders(req *httputil.ProxyRequest) {
	if t.options.ForwardHeaders {
		req.Out.Header["X-Forwarded-For"] = req.In.Header["X-Forwarded-For"]
	}

	req.SetXForwarded()

	if t.options.ForwardHeaders {
		if req.In.Header.Get("X-Forwarded-Proto") != "" {
			req.Out.Header.Set("X-Forwarded-Proto", req.In.Header.Get("X-Forwarded-Proto"))
		}
		if req.In.Header.Get("X-Forwarded-Host") != "" {
			req.Out.Header.Set("X-Forwarded-Host", req.In.Header.Get("X-Forwarded-Host"))
		}
	}
}

func (t *Target) handleProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if t.isRequestEntityTooLarge(err) {
		SetErrorResponse(w, r, http.StatusRequestEntityTooLarge, nil)
		return
	}

	if t.isGatewayTimeout(err) {
		SetErrorResponse(w, r, http.StatusGatewayTimeout, nil)
		return
	}

	slog.Error("Error while proxying", "target", t.Target(), "path", r.URL.Path, "error", err)
	SetErrorResponse(w, r, http.StatusBadGateway, nil)
}

func (t *Target) isRequestEntityTooLarge(err error) bool {
	var maxBytesError *http.MaxBytesError
	return errors.As(err, &maxBytesError)
}

func (t *Target) isGatewayTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return false
}

func (t *Target) updateState(state TargetState) TargetState {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	originalState := t.state
	t.state = state

	return originalState
}

func (t *Target) getInflightRequest(req *http.Request) *inflightRequest {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	return t.inflight[req]
}

func (t *Target) endInflightRequest(req *http.Request) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	inflightRequest, ok := t.inflight[req]
	if ok {
		inflightRequest.cancel()
		delete(t.inflight, req)
	}
}

func (t *Target) pendingRequestsToCancel() inflightMap {
	// We use a copy of the inflight map to iterate over while draining, so that
	// we don't need to lock it the whole time, which could interfere with the
	// locking that happens when requests end.
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	result := inflightMap{}
	for k, v := range t.inflight {
		result[k] = v
	}

	return result
}

func parseTargetURL(targetURL string) (*url.URL, error) {
	if !hostRegex.MatchString(targetURL) {
		return nil, fmt.Errorf("%s :%w", targetURL, ErrorInvalidHostPattern)
	}

	uri, _ := url.Parse("http://" + targetURL)
	return uri, nil
}

type targetResponseWriter struct {
	http.ResponseWriter
	inflightRequest *inflightRequest
}

func newTargetResponseWriter(w http.ResponseWriter, inflightRequest *inflightRequest) *targetResponseWriter {
	return &targetResponseWriter{w, inflightRequest}
}

func (r *targetResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("ResponseWriter does not implement http.Hijacker")
	}

	r.inflightRequest.hijacked = true
	return hijacker.Hijack()
}
