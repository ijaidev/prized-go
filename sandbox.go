package prized

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sandbox states reported by the API.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StatePausing  = "pausing"
	StatePaused   = "paused"
	StateResizing = "resizing"
	StateDeleting = "deleting"
	StateDeleted  = "deleted"
	StateError    = "error"
)

// Sandboxes is the client's sandbox service.
type Sandboxes struct {
	client *Client
}

// CreateSandboxOptions describes a new sandbox.
type CreateSandboxOptions struct {
	// Template is the sandbox template; DefaultTemplate when empty.
	Template string
	// IdempotencyKey makes a create repeatable. The client generates one when
	// empty; a caller-supplied key also disables cleanup of a failed create.
	IdempotencyKey string
	// Timeout bounds the request, its retries and the wait for running.
	Timeout time.Duration
}

// ExecOptions describes one foreground command.
type ExecOptions struct {
	// Timeout bounds the command; DefaultExecTimeout when zero.
	Timeout time.Duration
}

// DeleteOptions describes one delete.
type DeleteOptions struct {
	// Wait also waits until the sandbox is gone.
	Wait bool
	// Timeout bounds the request, its retries and the optional wait.
	Timeout time.Duration
}

// ExecResult is one finished command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	TimedOut bool
}

// Sandbox is a handle to one sandbox.
type Sandbox struct {
	client *Client

	mu          sync.RWMutex
	id          string
	state       string
	template    string
	toolboxBase string
	errorReason string
	closed      bool
}

// ID returns the sandbox id.
func (s *Sandbox) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

// State returns the state read last from the API.
func (s *Sandbox) State() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Template returns the template named by the last answer.
func (s *Sandbox) Template() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.template
}

// Close marks the handle closed. It sends nothing; the sandbox keeps running.
// Later Exec and Delete calls fail locally with ErrHandleClosed.
func (s *Sandbox) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *Sandbox) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// Create makes a sandbox and returns once it is running.
func (s *Sandboxes) Create(ctx context.Context, options CreateSandboxOptions) (*Sandbox, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	template := options.Template
	if template == "" {
		template = DefaultTemplate
	}
	timeout := options.Timeout
	if timeout == 0 {
		timeout = DefaultStartTimeout
	}
	if timeout < 0 {
		return nil, newError("invalid_argument", "timeout must be greater than zero")
	}
	key := options.IdempotencyKey
	generatedKey := false
	if key == "" {
		var err error
		key, err = newIdempotencyKey()
		if err != nil {
			return nil, &Error{Code: "internal_error", Message: "could not generate an idempotency key", cause: err}
		}
		generatedKey = true
	} else if !validIdempotencyKey(key) {
		return nil, newError("invalid_argument", "idempotencyKey must be 1 to 128 printable characters with no spaces")
	}

	body, err := json.Marshal(struct {
		Template string `json:"template"`
	}{Template: template})
	if err != nil {
		return nil, &Error{Code: "internal_error", Message: "could not encode the create request", cause: err}
	}

	deadline := time.Now().Add(timeout)
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	response, err := s.client.do(callCtx, requestSpec{
		method: http.MethodPost,
		url:    s.client.apiURL("/sandboxes"),
		body:   body,
		headers: map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": key,
		},
		mode: requestKeyed,
	})
	if err != nil {
		if generatedKey && mayHaveCreated(err) {
			s.client.reclaimCreate(key, body)
		}
		return nil, err
	}

	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		if generatedKey {
			s.client.reclaimCreate(key, body)
		}
		return nil, parseErr
	}
	sandbox := newSandbox(s.client, document)
	if sandbox.State() == StateRunning {
		return sandbox, nil
	}

	if err := sandbox.waitFor(callCtx, StateRunning); err != nil {
		if generatedKey {
			s.client.deleteBestEffort(sandbox.ID())
		}
		return nil, err
	}
	return sandbox, nil
}

// Exec runs one foreground command and returns its output. A non-zero exit
// code is a result, not an error.
func (s *Sandbox) Exec(ctx context.Context, command string, options *ExecOptions) (*ExecResult, error) {
	if s.isClosed() {
		return nil, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(command) == "" {
		return nil, newError("invalid_argument", "the command is empty")
	}
	timeout := DefaultExecTimeout
	if options != nil {
		if options.Timeout < 0 {
			return nil, newError("invalid_argument", "timeout must be greater than zero")
		}
		if options.Timeout > 0 {
			timeout = options.Timeout
		}
	}

	base, err := s.client.toolboxBaseFor(s.ID(), s.toolboxBaseValue())
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	}{
		Command: command,
		Timeout: timeout.Seconds(),
	})
	if err != nil {
		return nil, &Error{Code: "internal_error", Message: "could not encode the command request", cause: err}
	}

	response, err := s.client.do(ctx, requestSpec{
		method:         http.MethodPost,
		url:            base + "/process/execute",
		body:           body,
		headers:        map[string]string{"Content-Type": "application/json"},
		mode:           requestExec,
		sandboxID:      s.ID(),
		attemptTimeout: timeout + 30*time.Second,
	})
	if err != nil {
		return nil, err
	}

	var document struct {
		Stdout     string   `json:"stdout"`
		Stderr     string   `json:"stderr"`
		ExitCode   *int     `json:"exitCode"`
		DurationMS *float64 `json:"durationMs"`
		TimedOut   bool     `json:"timedOut"`
	}
	if err := json.Unmarshal(response.body, &document); err != nil || document.ExitCode == nil {
		return nil, &Error{
			Code:      "bad_response",
			Status:    response.status,
			Message:   "the exec answer carried no exit code",
			SandboxID: s.ID(),
			Effect:    EffectRan,
			cause:     err,
		}
	}
	duration := time.Duration(0)
	if document.DurationMS != nil {
		duration = time.Duration(*document.DurationMS * float64(time.Millisecond))
	}
	return &ExecResult{
		Stdout:   document.Stdout,
		Stderr:   document.Stderr,
		ExitCode: *document.ExitCode,
		Duration: duration,
		TimedOut: document.TimedOut,
	}, nil
}

// Delete deletes the sandbox. It returns once the API accepts; Wait also
// waits until the sandbox is gone. Deleting a sandbox already gone succeeds.
func (s *Sandbox) Delete(ctx context.Context, options *DeleteOptions) error {
	if s.isClosed() {
		return closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := DefaultDeleteTimeout
	wait := false
	if options != nil {
		wait = options.Wait
		if options.Timeout < 0 {
			return newError("invalid_argument", "timeout must be greater than zero")
		}
		if options.Timeout > 0 {
			timeout = options.Timeout
		}
	}

	callCtx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
	defer cancel()

	response, err := s.client.do(callCtx, requestSpec{
		method:    http.MethodDelete,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(s.ID())),
		mode:      requestRead,
		sandboxID: s.ID(),
	})
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
			s.setState(StateDeleted)
			return nil
		}
		return err
	}
	if len(response.body) > 0 {
		if document, parseErr := parseSandboxDocument(response.status, response.body); parseErr == nil {
			s.applyDocument(document)
		}
	} else {
		s.setState(StateDeleting)
	}
	if !wait {
		return nil
	}
	return s.waitFor(callCtx, StateDeleted)
}

func (s *Sandbox) waitFor(ctx context.Context, want string) error {
	for {
		state, reason := s.stateAndReason()
		switch state {
		case want:
			return nil
		case StateError:
			message := "the sandbox failed"
			if reason != "" {
				message = "the sandbox failed: " + reason
			}
			return &Error{Code: "sandbox_error", Message: message, Reason: reason, SandboxID: s.ID()}
		case StateDeleted:
			if want == StateDeleted {
				return nil
			}
			return &Error{Code: "sandbox_deleted", SandboxID: s.ID()}
		case StateDeleting:
			if want != StateDeleted {
				return &Error{Code: "sandbox_deleted", SandboxID: s.ID()}
			}
		}

		gone, err := s.read(ctx)
		if err != nil {
			return err
		}
		if gone {
			if want == StateDeleted {
				return nil
			}
			return &Error{Code: "sandbox_deleted", SandboxID: s.ID()}
		}
		if s.State() == want {
			return nil
		}

		timer := time.NewTimer(s.client.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return contextError(ctx.Err(), s.ID(), EffectNotRun)
		case <-timer.C:
		}
	}
}

func (s *Sandbox) read(ctx context.Context) (bool, error) {
	response, err := s.client.do(ctx, requestSpec{
		method:    http.MethodGet,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(s.ID())),
		mode:      requestRead,
		sandboxID: s.ID(),
	})
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
			s.setState(StateDeleted)
			return true, nil
		}
		return false, err
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return false, parseErr
	}
	s.applyDocument(document)
	return false, nil
}

func (s *Sandbox) stateAndReason() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state, s.errorReason
}

func (s *Sandbox) toolboxBaseValue() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.toolboxBase
}

func (s *Sandbox) setState(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
}

func (s *Sandbox) applyDocument(document *sandboxDocument) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.id = document.ID
	if document.State == "" {
		s.state = StateStarting
	} else {
		s.state = document.State
	}
	if document.Template != "" {
		s.template = document.Template
	}
	if document.ToolboxBase != "" {
		s.toolboxBase = document.ToolboxBase
	}
	s.errorReason = document.ErrorReason
}

type sandboxDocument struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	Template    string `json:"template"`
	ToolboxBase string `json:"toolboxBase"`
	ErrorReason string `json:"errorReason"`
}

type sandboxEnvelope struct {
	Sandbox *sandboxDocument `json:"sandbox"`
}

func parseSandboxDocument(status int, body []byte) (*sandboxDocument, error) {
	var envelope sandboxEnvelope
	if len(body) == 0 || json.Unmarshal(body, &envelope) != nil || envelope.Sandbox == nil || envelope.Sandbox.ID == "" {
		return nil, &Error{
			Code:    "bad_response",
			Status:  status,
			Message: "the API answered without a sandbox",
			Effect:  EffectRan,
		}
	}
	if envelope.Sandbox.State == "" {
		envelope.Sandbox.State = StateStarting
	}
	return envelope.Sandbox, nil
}

func newSandbox(client *Client, document *sandboxDocument) *Sandbox {
	sandbox := &Sandbox{
		client:      client,
		id:          document.ID,
		state:       document.State,
		template:    document.Template,
		toolboxBase: document.ToolboxBase,
		errorReason: document.ErrorReason,
	}
	if sandbox.state == "" {
		sandbox.state = StateStarting
	}
	return sandbox
}

var refusedBeforeCreate = map[string]bool{
	"sandboxes_disabled":      true,
	"sandbox_create_disabled": true,
	"sandbox_fork_disabled":   true,
	"sandbox_capacity":        true,
}

func mayHaveCreated(err error) bool {
	var failure *Error
	if !errors.As(err, &failure) {
		return true
	}
	if failure.Status == 0 || failure.Status >= 500 {
		return failure.Effect != EffectNotRun
	}
	return failure.Status >= 200 && failure.Status < 300
}

func (c *Client) reclaimCreate(key string, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := c.do(ctx, requestSpec{
		method: http.MethodPost,
		url:    c.apiURL("/sandboxes"),
		body:   body,
		headers: map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": key,
		},
		mode: requestKeyed,
	})
	if err != nil {
		return
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return
	}
	c.deleteBestEffort(document.ID)
}

func (c *Client) deleteBestEffort(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = c.do(ctx, requestSpec{
		method:    http.MethodDelete,
		url:       c.apiURL("/sandboxes/" + url.PathEscape(id)),
		mode:      requestRead,
		sandboxID: id,
	})
}

func (c *Client) toolboxBaseFor(sandboxID, named string) (string, error) {
	path := "/v1/sandbox/" + url.PathEscape(sandboxID)
	if named != "" {
		parsed, err := url.Parse(named)
		if err == nil &&
			(parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) &&
			parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" &&
			strings.TrimRight(parsed.Path, "/") == path &&
			trustedEdge(parsed.Hostname(), c.baseHost) {
			return parsed.Scheme + "://" + parsed.Host + path, nil
		}
	}
	if underDomain(c.baseHost, "prized.dev") {
		return DefaultEdgeURL + path, nil
	}
	return "", &Error{
		Code:      "edge_unknown",
		Message:   "the API named no command endpoint this client trusts; the command was not sent",
		SandboxID: sandboxID,
	}
}

func trustedEdge(edgeHost, apiHost string) bool {
	if edgeHost == "" || apiHost == "" {
		return false
	}
	if isLoopback(apiHost) || isLoopback(edgeHost) {
		return isLoopback(apiHost) && isLoopback(edgeHost)
	}
	return edgeHost == apiHost || (underDomain(apiHost, "prized.dev") && underDomain(edgeHost, "prized.dev"))
}

func underDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}
