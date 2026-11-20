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

// SandboxResources is a sandbox's shape. Memory is in MiB, as the API sends
// it; DiskGB is in GiB. A zero field is left out of a create, so the
// template's default applies.
type SandboxResources struct {
	VCPU     int `json:"vcpu,omitempty"`
	MemoryMB int `json:"memoryMb,omitempty"`
	DiskGB   int `json:"diskGb,omitempty"`
}

// MemoryGB returns the memory in GiB.
func (r SandboxResources) MemoryGB() float64 {
	return float64(r.MemoryMB) / 1024
}

// CreateSandboxOptions describes a new sandbox.
type CreateSandboxOptions struct {
	// Template is the sandbox template; DefaultTemplate when empty.
	Template string
	// Resources sets the shape. A nil pointer or a zero field leaves that
	// part at the template's default.
	Resources *SandboxResources
	// EnvVars are set for every command the sandbox runs.
	EnvVars map[string]string
	// Labels are yours to filter on later with List and DeleteByLabels.
	Labels map[string]string
	// AutoPauseMin pauses the sandbox after this many idle minutes; 0 is off.
	AutoPauseMin int
	// AutoDeleteMin deletes the sandbox after this many minutes paused; 0 is off.
	AutoDeleteMin int
	// MaxRuntimeMin is the sandbox's run time in minutes; 0 uses the
	// template's default.
	MaxRuntimeMin int
	// NoWait returns as soon as the API accepts (state starting) instead of
	// waiting for running. The request still stays within Timeout.
	NoWait bool
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

	mu                 sync.RWMutex
	id                 string
	state              string
	template           string
	templateBuild      string
	resources          SandboxResources
	labels             map[string]string
	autoPauseMin       int
	autoDeleteMin      int
	maxRuntimeMin      int
	expiresAt          time.Time
	createdAt          time.Time
	lastActiveAt       time.Time
	errorReason        string
	toolboxBase        string
	previewURLTemplate string
	raw                map[string]any
	closed             bool
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

// TemplateBuild returns the template build named by the last answer.
func (s *Sandbox) TemplateBuild() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.templateBuild
}

// Resources returns the shape named by the last answer.
func (s *Sandbox) Resources() SandboxResources {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resources
}

// Labels returns a copy of the labels named by the last answer.
func (s *Sandbox) Labels() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneLabels(s.labels)
}

// AutoPauseMin returns the idle minutes before the sandbox pauses itself,
// or 0 when that is off.
func (s *Sandbox) AutoPauseMin() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.autoPauseMin
}

// AutoDeleteMin returns the minutes paused before the sandbox is deleted,
// or 0 when that is off.
func (s *Sandbox) AutoDeleteMin() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.autoDeleteMin
}

// MaxRuntimeMin returns the run time in minutes named by the last answer.
func (s *Sandbox) MaxRuntimeMin() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.maxRuntimeMin
}

// ExpiresAt returns when the sandbox's run time ends. It is the zero time
// while a paused sandbox's clock is stopped.
func (s *Sandbox) ExpiresAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiresAt
}

// CreatedAt returns when the sandbox was created, as of the last answer.
func (s *Sandbox) CreatedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.createdAt
}

// LastActiveAt returns the last activity the auto-pause clock counts from,
// as of the last answer.
func (s *Sandbox) LastActiveAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActiveAt
}

// ErrorReason returns the short failure explanation from the last answer,
// or "" when there is none.
func (s *Sandbox) ErrorReason() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.errorReason
}

// Raw returns a shallow copy of the sandbox document as the API last sent it.
// Nested maps and slices are shared with the handle.
func (s *Sandbox) Raw() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.raw == nil {
		return nil
	}
	document := make(map[string]any, len(s.raw))
	for key, value := range s.raw {
		document[key] = value
	}
	return document
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

// createSandboxRequest is the POST /sandboxes body. Fields the caller left
// alone are omitted, so the template's defaults apply.
type createSandboxRequest struct {
	Template      string            `json:"template"`
	Resources     *SandboxResources `json:"resources,omitempty"`
	EnvVars       map[string]string `json:"envVars,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	AutoPauseMin  *int              `json:"autoPauseMin,omitempty"`
	AutoDeleteMin *int              `json:"autoDeleteMin,omitempty"`
	MaxRuntimeMin *int              `json:"maxRuntimeMin,omitempty"`
}

// Create makes a sandbox and returns once it is running, or as soon as the
// API accepts when NoWait is set.
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
	if options.Resources != nil && (options.Resources.VCPU < 0 || options.Resources.MemoryMB < 0 || options.Resources.DiskGB < 0) {
		return nil, newError("invalid_argument", "resources must not be negative")
	}
	if options.AutoPauseMin < 0 || options.AutoDeleteMin < 0 || options.MaxRuntimeMin < 0 {
		return nil, newError("invalid_argument", "auto-pause, auto-delete and max runtime must not be negative")
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

	request := createSandboxRequest{Template: template}
	if options.Resources != nil {
		resources := *options.Resources
		if resources.VCPU != 0 || resources.MemoryMB != 0 || resources.DiskGB != 0 {
			request.Resources = &resources
		}
	}
	if len(options.EnvVars) > 0 {
		request.EnvVars = options.EnvVars
	}
	if len(options.Labels) > 0 {
		request.Labels = options.Labels
	}
	if options.AutoPauseMin > 0 {
		pause := options.AutoPauseMin
		request.AutoPauseMin = &pause
	}
	if options.AutoDeleteMin > 0 {
		deleteAfter := options.AutoDeleteMin
		request.AutoDeleteMin = &deleteAfter
	}
	if options.MaxRuntimeMin > 0 {
		runtime := options.MaxRuntimeMin
		request.MaxRuntimeMin = &runtime
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, &Error{Code: "internal_error", Message: "could not encode the create request", cause: err}
	}

	deadline := time.Now().Add(timeout)
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	endpoint := s.client.apiURL("/sandboxes")
	if !options.NoWait {
		// Ask the API to hold the answer until running; it answers early
		// when the bound passes, and the handle is polled below.
		endpoint += "?wait=running"
	}
	response, err := s.client.do(callCtx, requestSpec{
		method: http.MethodPost,
		url:    endpoint,
		body:   body,
		headers: map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": key,
		},
		mode: requestSafe,
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
	if options.NoWait || sandbox.State() == StateRunning {
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
			return asRan(&Error{Code: "sandbox_error", Message: message, Reason: reason, SandboxID: s.ID()})
		case StateDeleted:
			if want == StateDeleted {
				return nil
			}
			return asRan(&Error{Code: "sandbox_deleted", SandboxID: s.ID()})
		case StateDeleting:
			if want != StateDeleted {
				return asRan(&Error{Code: "sandbox_deleted", SandboxID: s.ID()})
			}
		}

		gone, err := s.read(ctx)
		if err != nil {
			return asRan(err)
		}
		if gone {
			if want == StateDeleted {
				return nil
			}
			return asRan(&Error{Code: "sandbox_deleted", SandboxID: s.ID()})
		}
		if s.State() == want {
			return nil
		}

		timer := time.NewTimer(s.client.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return asRan(contextError(ctx.Err(), s.ID(), EffectNotRun))
		case <-timer.C:
		}
	}
}

// asRan marks a wait's failure as one whose create or delete was already
// accepted by the API, whatever happened to the wait.
func asRan(err error) error {
	var failure *Error
	if errors.As(err, &failure) && failure.Effect < EffectRan {
		failure.Effect = EffectRan
	}
	return err
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
	if document.TemplateBuild != "" {
		s.templateBuild = document.TemplateBuild
	}
	if document.ToolboxBase != "" {
		s.toolboxBase = document.ToolboxBase
	}
	s.resources = document.Resources
	s.labels = cloneLabels(document.Labels)
	s.autoPauseMin = intValue(document.AutoPauseMin)
	s.autoDeleteMin = intValue(document.AutoDeleteMin)
	s.maxRuntimeMin = intValue(document.MaxRuntimeMin)
	s.expiresAt = parseAPITime(document.ExpiresAt)
	s.createdAt = parseAPITime(document.CreatedAt)
	s.lastActiveAt = parseAPITime(document.LastActiveAt)
	s.previewURLTemplate = document.PreviewURLTemplate
	s.errorReason = document.ErrorReason
	s.raw = document.raw
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func parseAPITime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

type sandboxDocument struct {
	ID                 string            `json:"id"`
	State              string            `json:"state"`
	Template           string            `json:"template"`
	TemplateBuild      string            `json:"templateBuild"`
	Resources          SandboxResources  `json:"resources"`
	Labels             map[string]string `json:"labels"`
	AutoPauseMin       *int              `json:"autoPauseMin"`
	AutoDeleteMin      *int              `json:"autoDeleteMin"`
	MaxRuntimeMin      *int              `json:"maxRuntimeMin"`
	ExpiresAt          string            `json:"expiresAt"`
	CreatedAt          string            `json:"createdAt"`
	LastActiveAt       string            `json:"lastActiveAt"`
	ToolboxBase        string            `json:"toolboxBase"`
	PreviewURLTemplate string            `json:"previewUrlTemplate"`
	ErrorReason        string            `json:"errorReason"`

	raw map[string]any
}

type sandboxEnvelope struct {
	Sandbox json.RawMessage `json:"sandbox"`
}

func badSandboxResponse(status int) *Error {
	return &Error{
		Code:    "bad_response",
		Status:  status,
		Message: "the API answered without a sandbox",
		Effect:  EffectRan,
	}
}

func parseSandboxDocument(status int, body []byte) (*sandboxDocument, error) {
	var envelope sandboxEnvelope
	if len(body) == 0 || json.Unmarshal(body, &envelope) != nil {
		return nil, badSandboxResponse(status)
	}
	return parseSandboxObject(status, envelope.Sandbox)
}

func parseSandboxObject(status int, raw json.RawMessage) (*sandboxDocument, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, badSandboxResponse(status)
	}
	var document sandboxDocument
	if err := json.Unmarshal(raw, &document); err != nil || document.ID == "" {
		return nil, badSandboxResponse(status)
	}
	var rawDocument map[string]any
	if err := json.Unmarshal(raw, &rawDocument); err == nil {
		document.raw = rawDocument
	}
	if document.State == "" {
		document.State = StateStarting
	}
	return &document, nil
}

func parseSandboxList(status int, body []byte) ([]*sandboxDocument, error) {
	var envelope struct {
		Sandboxes *[]json.RawMessage `json:"sandboxes"`
	}
	if len(body) == 0 || json.Unmarshal(body, &envelope) != nil || envelope.Sandboxes == nil {
		return nil, &Error{
			Code:    "bad_response",
			Status:  status,
			Message: "the API answered without a sandbox list",
			Effect:  EffectRan,
		}
	}
	documents := make([]*sandboxDocument, 0, len(*envelope.Sandboxes))
	for _, raw := range *envelope.Sandboxes {
		document, err := parseSandboxObject(status, raw)
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
	return documents, nil
}

func newSandbox(client *Client, document *sandboxDocument) *Sandbox {
	sandbox := &Sandbox{client: client}
	sandbox.applyDocument(document)
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
		mode: requestSafe,
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
