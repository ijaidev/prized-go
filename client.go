package prized

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	// DefaultBaseURL is the Prized API origin.
	DefaultBaseURL = "https://api.prized.dev"
	// DefaultEdgeURL is the Prized command edge.
	DefaultEdgeURL = "https://term.prized.dev"
	// DefaultTemplate is used when a create names none.
	DefaultTemplate = "prized-base-v1"
	// DefaultMaxRetries is the number of times a failed request is sent again.
	DefaultMaxRetries = 3
	// DefaultRequestTimeout bounds one HTTP request.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultPollInterval is the pause between state reads.
	DefaultPollInterval = 500 * time.Millisecond
	// DefaultStartTimeout bounds a create and its wait for running.
	DefaultStartTimeout = 180 * time.Second
	// DefaultDeleteTimeout bounds a delete and its optional wait.
	DefaultDeleteTimeout = 120 * time.Second
	// DefaultExecTimeout bounds a command when the call names none.
	DefaultExecTimeout = 60 * time.Second
)

const userAgent = "prized-go/0.1.0"

// Option configures a Client.
type Option func(*clientOptions)

type clientOptions struct {
	apiKey         string
	apiKeySet      bool
	baseURL        string
	baseURLSet     bool
	httpClient     *http.Client
	httpClientSet  bool
	maxRetries     int
	maxRetriesSet  bool
	requestTimeout time.Duration
	timeoutSet     bool
	pollInterval   time.Duration
	pollSet        bool
}

// WithAPIKey sets the key sent with every request. Without this option the
// client reads PRIZED_API_KEY.
func WithAPIKey(key string) Option {
	return func(options *clientOptions) {
		options.apiKey = key
		options.apiKeySet = true
	}
}

// WithBaseURL sets the API origin. It must be https, or plain http on
// localhost, 127.0.0.1 or [::1].
func WithBaseURL(raw string) Option {
	return func(options *clientOptions) {
		options.baseURL = raw
		options.baseURLSet = true
	}
}

// WithHTTPClient sets the HTTP client used for API and edge requests.
func WithHTTPClient(client *http.Client) Option {
	return func(options *clientOptions) {
		options.httpClient = client
		options.httpClientSet = true
	}
}

// WithMaxRetries sets how many times one failed request is sent again.
func WithMaxRetries(retries int) Option {
	return func(options *clientOptions) {
		options.maxRetries = retries
		options.maxRetriesSet = true
	}
}

// WithRequestTimeout bounds one HTTP request, retries excluded.
func WithRequestTimeout(timeout time.Duration) Option {
	return func(options *clientOptions) {
		options.requestTimeout = timeout
		options.timeoutSet = true
	}
}

// WithPollInterval sets the pause between sandbox state reads.
func WithPollInterval(interval time.Duration) Option {
	return func(options *clientOptions) {
		options.pollInterval = interval
		options.pollSet = true
	}
}

// Client holds authentication and transport settings.
type Client struct {
	apiKey         string
	baseURL        string
	baseHost       string
	httpClient     *http.Client
	maxRetries     int
	requestTimeout time.Duration
	pollInterval   time.Duration
	retryBase      time.Duration
	sandboxes      *Sandboxes
}

// NewClient builds a client. It reads the key from options or
// PRIZED_API_KEY and fails before any request when neither is usable.
func NewClient(options ...Option) (*Client, error) {
	config := clientOptions{
		baseURL:        DefaultBaseURL,
		maxRetries:     DefaultMaxRetries,
		requestTimeout: DefaultRequestTimeout,
		pollInterval:   DefaultPollInterval,
	}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	apiKey := config.apiKey
	if !config.apiKeySet {
		apiKey = os.Getenv("PRIZED_API_KEY")
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, newError("missing_api_key", "")
	}
	for i := 0; i < len(apiKey); i++ {
		if apiKey[i] < 33 || apiKey[i] > 126 {
			return nil, newError("invalid_api_key", "")
		}
	}

	baseURL, err := normalizeOrigin("baseUrl", config.baseURL)
	if err != nil {
		return nil, err
	}
	if config.maxRetries < 0 {
		return nil, newError("invalid_argument", "maxRetries must be a whole number >= 0")
	}
	if config.requestTimeout <= 0 {
		return nil, newError("invalid_argument", "requestTimeout must be greater than zero")
	}
	if config.pollInterval <= 0 {
		return nil, newError("invalid_argument", "pollInterval must be greater than zero")
	}

	httpClient := config.httpClient
	if config.httpClientSet && httpClient == nil {
		return nil, newError("invalid_argument", "the HTTP client must not be nil")
	}
	refuseRedirect := func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if httpClient == nil {
		httpClient = &http.Client{CheckRedirect: refuseRedirect}
	} else {
		// The key rides on every request. Keep a caller's client for its
		// transport, but never let it follow a redirect with the key attached.
		clone := *httpClient
		clone.CheckRedirect = refuseRedirect
		httpClient = &clone
	}

	parsed, _ := url.Parse(baseURL)
	client := &Client{
		apiKey:         apiKey,
		baseURL:        baseURL,
		baseHost:       parsed.Hostname(),
		httpClient:     httpClient,
		maxRetries:     config.maxRetries,
		requestTimeout: config.requestTimeout,
		pollInterval:   config.pollInterval,
		retryBase:      500 * time.Millisecond,
	}
	client.sandboxes = &Sandboxes{client: client}
	return client, nil
}

// Sandboxes returns the service that creates sandboxes.
func (c *Client) Sandboxes() *Sandboxes {
	return c.sandboxes
}

func normalizeOrigin(name, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", newError("invalid_argument", name+" is not a URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", newError("invalid_argument", name+" must be an http(s) URL")
	}
	if parsed.Hostname() == "" {
		return "", newError("invalid_argument", name+" must name a host")
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return "", newError("invalid_argument", name+" must use https (plain http only for localhost, 127.0.0.1 or [::1])")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", newError("invalid_argument", name+" must not carry credentials, a query or a fragment")
	}
	path := strings.TrimRight(parsed.Path, "/")
	path = strings.TrimSuffix(path, "/api/v1")
	return parsed.Scheme + "://" + parsed.Host + path, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func (c *Client) apiURL(path string) string {
	return c.baseURL + "/api/v1" + path
}

type requestMode int

const (
	requestRead requestMode = iota
	requestSafe
	requestExec
)

type requestSpec struct {
	method         string
	url            string
	body           []byte
	headers        map[string]string
	mode           requestMode
	sandboxID      string
	attemptTimeout time.Duration // 0: client default; negative: no attempt timeout
}

type httpResponse struct {
	status int
	header http.Header
	body   []byte
}

// neverRetry lists refusals that no wait can change. Because they also prove
// the request did nothing, a 5xx carrying one keeps EffectNotRun.
var neverRetry = map[string]bool{
	"sandbox_quota_exceeded":   true,
	"sandboxes_disabled":       true,
	"sandbox_create_disabled":  true,
	"sandbox_fork_disabled":    true,
	"idempotency_conflict":     true,
	"paid_balance_required":    true,
	"insufficient_balance":     true,
	"preview_unavailable":      true,
	"share_links_need_payment": true,
}

var execUndelivered = map[string]bool{
	"rate_limited":                    true,
	"resolve_failed":                  true,
	"sandbox_credentials_unavailable": true,
	"control_plane_unavailable":       true,
	"sandbox_not_ready":               true,
}

func (c *Client) do(ctx context.Context, spec requestSpec) (*httpResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	acted := EffectNotRun
	started := false
	attemptTimeout := spec.attemptTimeout
	if attemptTimeout == 0 {
		attemptTimeout = c.requestTimeout
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			effect := acted
			if effect == EffectNotRun && started {
				effect = EffectMaybeRan
			}
			return nil, contextError(err, spec.sandboxID, effect)
		}

		attemptCtx := ctx
		cancel := func() {}
		if attemptTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, attemptTimeout)
		}
		started = true

		var reader io.Reader
		if spec.body != nil {
			reader = bytes.NewReader(spec.body)
		}
		request, err := http.NewRequestWithContext(attemptCtx, spec.method, spec.url, reader)
		if err != nil {
			cancel()
			return nil, newError("invalid_argument", "could not build the request")
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
		request.Header.Set("User-Agent", userAgent)
		for name, value := range spec.headers {
			request.Header.Set(name, value)
		}

		response, doErr := c.httpClient.Do(request)
		if doErr != nil {
			cancel()
			if ctxErr := ctx.Err(); ctxErr != nil {
				effect := acted
				if effect == EffectNotRun && started {
					effect = EffectMaybeRan
				}
				return nil, contextError(ctxErr, spec.sandboxID, effect)
			}
			if attemptCtxErr := attemptCtx.Err(); attemptCtxErr != nil && errors.Is(attemptCtxErr, context.DeadlineExceeded) {
				failure := &Error{
					Code:      "timeout",
					Message:   fmt.Sprintf("no answer within %s", attemptTimeout),
					SandboxID: spec.sandboxID,
					Effect:    EffectMaybeRan,
					cause:     doErr,
				}
				if attempt >= c.maxRetries || !c.retryable(spec.mode, spec.method, failure) {
					return nil, mergeEffect(failure, acted)
				}
				acted = mergeEffect(failure, acted).Effect
				if err := c.waitBeforeRetry(ctx, attempt, failure); err != nil {
					return nil, err
				}
				continue
			}

			failure := networkFailure(spec.method, spec.url, spec.sandboxID, doErr)
			if attempt >= c.maxRetries || !c.retryable(spec.mode, spec.method, failure) {
				return nil, mergeEffect(failure, acted)
			}
			acted = mergeEffect(failure, acted).Effect
			if err := c.waitBeforeRetry(ctx, attempt, failure); err != nil {
				return nil, err
			}
			continue
		}

		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, contextError(ctxErr, spec.sandboxID, mergeEffect(&Error{Effect: EffectMaybeRan}, acted).Effect)
			}
			failure := &Error{
				Code:      "network_error",
				Message:   "the answer broke off",
				SandboxID: spec.sandboxID,
				Effect:    EffectMaybeRan,
				cause:     readErr,
			}
			if attempt >= c.maxRetries || !c.retryable(spec.mode, spec.method, failure) {
				return nil, mergeEffect(failure, acted)
			}
			acted = mergeEffect(failure, acted).Effect
			if err := c.waitBeforeRetry(ctx, attempt, failure); err != nil {
				return nil, err
			}
			continue
		}

		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return &httpResponse{status: response.StatusCode, header: response.Header, body: body}, nil
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return nil, &Error{
				Code:      "unexpected_redirect",
				Status:    response.StatusCode,
				SandboxID: spec.sandboxID,
				Effect:    EffectNotRun,
			}
		}

		failure := errorFromResponse(response.StatusCode, body, response.Header, spec.sandboxID)
		if response.StatusCode >= 500 && spec.mode != requestRead {
			if !execUndelivered[failure.Code] && !refusedBeforeCreate[failure.Code] && !neverRetry[failure.Code] {
				failure.Effect = EffectMaybeRan
			}
		}
		if attempt >= c.maxRetries || !c.retryable(spec.mode, spec.method, failure) {
			return nil, mergeEffect(failure, acted)
		}
		acted = mergeEffect(failure, acted).Effect
		if err := c.waitBeforeRetry(ctx, attempt, failure); err != nil {
			return nil, err
		}
	}
}

func mergeEffect(failure *Error, acted Effect) *Error {
	if acted > failure.Effect {
		failure.Effect = acted
	}
	return failure
}

func (c *Client) retryable(mode requestMode, method string, failure *Error) bool {
	if neverRetry[failure.Code] {
		return false
	}
	if failure.Status == 0 {
		if mode == requestExec {
			return failure.notSent
		}
		return true
	}
	if failure.Status == http.StatusTooManyRequests {
		return true
	}
	if mode == requestExec {
		return execUndelivered[failure.Code]
	}
	switch failure.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	if mode == requestRead && method == http.MethodGet && failure.Status >= 500 {
		return true
	}
	return false
}

func (c *Client) waitBeforeRetry(ctx context.Context, attempt int, failure *Error) error {
	wait := c.backoff(attempt)
	if failure.RetryAfter > wait {
		wait = failure.RetryAfter
	}
	if wait > 60*time.Second {
		wait = 60 * time.Second
	}
	if wait <= 0 {
		return nil
	}
	if deadline, ok := ctx.Deadline(); ok && time.Now().Add(wait).After(deadline) {
		return &Error{
			Code:      "timeout",
			Message:   "the time allowed for this call ran out while waiting to retry",
			SandboxID: failure.SandboxID,
			Effect:    failure.Effect,
			cause:     failure,
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return contextError(ctx.Err(), failure.SandboxID, failure.Effect)
	case <-timer.C:
		return nil
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	if c.retryBase <= 0 {
		return 0
	}
	base := c.retryBase
	for i := 0; i < attempt; i++ {
		base *= 2
		if base >= 8*time.Second {
			base = 8 * time.Second
			break
		}
	}
	if base <= 0 {
		return 0
	}
	half := base / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

func networkFailure(method, rawURL, sandboxID string, err error) *Error {
	notSent := neverSent(err)
	path := rawURL
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil {
		path = parsed.Path
	}
	message := fmt.Sprintf("%s %s: the connection failed", method, path)
	effect := EffectMaybeRan
	if notSent {
		message = "could not reach the API"
		effect = EffectNotRun
	}
	return &Error{
		Code:      "network_error",
		Message:   message,
		SandboxID: sandboxID,
		Effect:    effect,
		cause:     err,
		notSent:   notSent,
	}
}

func neverSent(err error) bool {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		err = urlError.Err
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) && operationError.Op == "dial" {
		return true
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}

func newIdempotencyKey() (string, error) {
	var bytes [16]byte
	if _, err := cryptorand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "prized-go-" + hex.EncodeToString(bytes[:]), nil
}

func validIdempotencyKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 33 || key[i] > 126 {
			return false
		}
	}
	return true
}
