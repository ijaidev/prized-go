package prized

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PauseOptions describes one pause.
type PauseOptions struct {
	// NoWait returns once the API accepts the pause instead of waiting until
	// the sandbox is paused.
	NoWait bool
	// Timeout bounds the request, its retries and the wait;
	// DefaultPauseTimeout when zero.
	Timeout time.Duration
}

// ResumeOptions describes one resume.
type ResumeOptions struct {
	// NoWait returns once the API accepts the resume instead of waiting until
	// the sandbox is running.
	NoWait bool
	// Timeout bounds the request, its retries and the wait;
	// DefaultStartTimeout when zero.
	Timeout time.Duration
}

// ForkOptions describes one fork.
type ForkOptions struct {
	// Count is how many copies to make; 1 when zero.
	Count int
	// Labels replaces the parent's labels on every copy. Nil inherits the
	// parent's labels.
	Labels map[string]string
	// IdempotencyKey makes a fork repeatable. The client generates one when
	// empty; a caller-supplied key also disables cleanup of a failed fork.
	IdempotencyKey string
	// NoWait returns once the API accepts the fork, with copies that may still
	// be starting.
	NoWait bool
	// Timeout bounds the request, its retries and the wait;
	// DefaultStartTimeout when zero.
	Timeout time.Duration
}

// ExtendOptions describes one run-time extension.
type ExtendOptions struct {
	// By is the time added to the sandbox's deletion time. It must be a whole
	// number of minutes.
	By time.Duration
	// IdempotencyKey makes a retry add the time once; generated when empty.
	IdempotencyKey string
}

// TimeoutOptions describes one exact run-time change. Pass exactly one of In
// or At.
type TimeoutOptions struct {
	// In sets the deletion time to now plus this duration. It must be a whole
	// number of minutes.
	In time.Duration
	// At sets the deletion time exactly. It must be in the future.
	At time.Time
	// IdempotencyKey makes a retry set the same time; sent only when non-empty.
	IdempotencyKey string
}

// RunTime is a sandbox's new deletion time after Extend or SetTimeout, and
// whether the API capped the change at the latest time it allows.
type RunTime struct {
	ExpiresAt time.Time
	Capped    bool
}

// Refresh reads the sandbox again, updating its state and the other fields
// read from the API. It does not count as activity.
func (s *Sandbox) Refresh(ctx context.Context) error {
	if s.isClosed() {
		return closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	response, err := s.client.do(ctx, requestSpec{
		method:    http.MethodGet,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(s.ID())),
		mode:      requestRead,
		sandboxID: s.ID(),
	})
	if err != nil {
		return err
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return parseErr
	}
	s.applyDocument(document)
	return nil
}

// KeepAlive counts as activity now: the sandbox's auto-pause clock starts
// over. Commands and file transfers already count as activity; this is for
// idle stretches.
func (s *Sandbox) KeepAlive(ctx context.Context) error {
	if s.isClosed() {
		return closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := s.client.do(ctx, requestSpec{
		method:    http.MethodPost,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(s.ID()) + "/refresh"),
		mode:      requestKeyed,
		sandboxID: s.ID(),
	})
	return err
}

// Pause pauses the sandbox: memory and disk are kept, compute stops. It
// returns once the sandbox is paused, or once the API accepts the pause when
// PauseOptions.NoWait is set.
func (s *Sandbox) Pause(ctx context.Context, options *PauseOptions) error {
	if s.isClosed() {
		return closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	noWait := false
	timeout := DefaultPauseTimeout
	if options != nil {
		noWait = options.NoWait
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
		method:    http.MethodPost,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(s.ID()) + "/pause"),
		mode:      requestKeyed,
		sandboxID: s.ID(),
	})
	if err != nil {
		return err
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return parseErr
	}
	s.applyDocument(document)
	if noWait {
		return nil
	}
	return s.waitFor(callCtx, StatePaused)
}

// Resume resumes a paused sandbox with every process where it left off. It
// returns once the sandbox is running, or once the API accepts the resume
// when ResumeOptions.NoWait is set.
func (s *Sandbox) Resume(ctx context.Context, options *ResumeOptions) error {
	if s.isClosed() {
		return closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	noWait := false
	timeout := DefaultStartTimeout
	if options != nil {
		noWait = options.NoWait
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
		method:    http.MethodPost,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(s.ID()) + "/resume"),
		mode:      requestKeyed,
		sandboxID: s.ID(),
	})
	if err != nil {
		return err
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return parseErr
	}
	s.applyDocument(document)
	if noWait {
		return nil
	}
	return s.waitFor(callCtx, StateRunning)
}

// Fork copies a running or paused sandbox into Count new sandboxes, each with
// its memory and processes as they were. It returns once every copy is
// running; if a copy fails or the wait runs out, every copy is deleted (under
// the client's own idempotency key) and the error is returned. With
// ForkOptions.NoWait it returns as soon as the API accepts.
func (s *Sandbox) Fork(ctx context.Context, options *ForkOptions) ([]*Sandbox, error) {
	if s.isClosed() {
		return nil, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	config := ForkOptions{Count: 1}
	if options != nil {
		config = *options
	}
	if config.Count == 0 {
		config.Count = 1
	}
	if config.Count < 1 || config.Count > 256 {
		return nil, newError("invalid_argument", "count must be between 1 and 256")
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultStartTimeout
	}
	if timeout < 0 {
		return nil, newError("invalid_argument", "timeout must be greater than zero")
	}

	key := config.IdempotencyKey
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

	payload := map[string]any{"count": config.Count}
	if config.Labels != nil {
		payload["labels"] = config.Labels
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &Error{Code: "internal_error", Message: "could not encode the fork request", cause: err}
	}

	callCtx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
	defer cancel()
	response, err := s.client.forkRequest(callCtx, s.ID(), body, key)
	if err != nil {
		if generatedKey && mayHaveCreated(err) {
			s.client.reclaimFork(s.ID(), body, key)
		}
		return nil, err
	}
	documents, parseErr := parseForkDocuments(response.status, response.body)
	if parseErr != nil {
		if generatedKey {
			s.client.reclaimFork(s.ID(), body, key)
		}
		return nil, parseErr
	}
	copies := make([]*Sandbox, 0, len(documents))
	for _, document := range documents {
		copies = append(copies, newSandbox(s.client, document))
	}
	if config.NoWait || len(copies) == 0 {
		return copies, nil
	}
	if err := s.settleFork(callCtx, copies); err != nil {
		if generatedKey {
			s.client.deleteCopiesBestEffort(sandboxIDs(copies))
		}
		return nil, err
	}
	return copies, nil
}

// Extend adds By to the sandbox's deletion time. It never shortens, and the
// API caps it at the latest time it allows.
func (s *Sandbox) Extend(ctx context.Context, options ExtendOptions) (RunTime, error) {
	if s.isClosed() {
		return RunTime{}, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	minutes, err := wholeMinutes("By", options.By)
	if err != nil {
		return RunTime{}, err
	}
	key := options.IdempotencyKey
	if key == "" {
		key, err = newIdempotencyKey()
		if err != nil {
			return RunTime{}, &Error{Code: "internal_error", Message: "could not generate an idempotency key", cause: err}
		}
	} else if !validIdempotencyKey(key) {
		return RunTime{}, newError("invalid_argument", "idempotencyKey must be 1 to 128 printable characters with no spaces")
	}

	body, err := json.Marshal(struct {
		ByMin int64 `json:"byMin"`
	}{ByMin: minutes})
	if err != nil {
		return RunTime{}, &Error{Code: "internal_error", Message: "could not encode the extend request", cause: err}
	}
	response, err := s.client.lifecycleRequest(ctx, s.ID(), "extend", body, key)
	if err != nil {
		return RunTime{}, err
	}
	return s.applyRunTime(response)
}

// SetTimeout sets the sandbox's deletion time exactly, earlier or later, to
// now plus In or to At. The API refuses a time past the latest it allows.
func (s *Sandbox) SetTimeout(ctx context.Context, options TimeoutOptions) (RunTime, error) {
	if s.isClosed() {
		return RunTime{}, closedError()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hasIn := options.In != 0
	hasAt := !options.At.IsZero()
	if hasIn == hasAt {
		return RunTime{}, newError("invalid_argument", "pass exactly one of In or At")
	}

	body := make(map[string]any, 1)
	if hasIn {
		minutes, err := wholeMinutes("In", options.In)
		if err != nil {
			return RunTime{}, err
		}
		body["inMin"] = minutes
	} else {
		if !options.At.After(time.Now()) {
			return RunTime{}, newError("invalid_argument", "At must be in the future")
		}
		body["expiresAt"] = options.At.UTC()
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return RunTime{}, &Error{Code: "internal_error", Message: "could not encode the timeout request", cause: err}
	}
	if options.IdempotencyKey != "" && !validIdempotencyKey(options.IdempotencyKey) {
		return RunTime{}, newError("invalid_argument", "idempotencyKey must be 1 to 128 printable characters with no spaces")
	}
	response, err := s.client.lifecycleRequest(ctx, s.ID(), "timeout", encoded, options.IdempotencyKey)
	if err != nil {
		return RunTime{}, err
	}
	return s.applyRunTime(response)
}

// Fits reports how many more copies the parent's machine had room for when a
// fork was refused with the fork_capacity code.
func (e *Error) Fits() (int, bool) {
	if e == nil || e.Body == nil {
		return 0, false
	}
	switch value := e.Body["fits"].(type) {
	case float64:
		if value < 0 || value != math.Trunc(value) || value > math.MaxInt32 {
			return 0, false
		}
		return int(value), true
	case int:
		if value < 0 {
			return 0, false
		}
		return value, true
	case json.Number:
		fits, err := value.Int64()
		if err != nil || fits < 0 || fits > math.MaxInt32 {
			return 0, false
		}
		return int(fits), true
	default:
		return 0, false
	}
}

func (s *Sandbox) applyRunTime(response *httpResponse) (RunTime, error) {
	var envelope struct {
		Capped bool `json:"capped"`
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return RunTime{}, parseErr
	}
	if document.ExpiresAt == nil {
		return RunTime{}, &Error{
			Code:      "bad_response",
			Status:    response.status,
			Message:   "the API answered without the sandbox's new expiry",
			SandboxID: s.ID(),
			Effect:    EffectRan,
		}
	}
	if len(response.body) > 0 {
		_ = json.Unmarshal(response.body, &envelope)
	}
	s.applyDocument(document)
	return RunTime{ExpiresAt: *document.ExpiresAt, Capped: envelope.Capped}, nil
}

func wholeMinutes(name string, value time.Duration) (int64, error) {
	if value <= 0 {
		return 0, newError("invalid_argument", name+" must be greater than zero")
	}
	if value%time.Minute != 0 {
		return 0, newError("invalid_argument", name+" must be a whole number of minutes")
	}
	return int64(value / time.Minute), nil
}

func (c *Client) forkRequest(ctx context.Context, parentID string, body []byte, key string) (*httpResponse, error) {
	return c.do(ctx, requestSpec{
		method: http.MethodPost,
		url:    c.apiURL("/sandboxes/" + url.PathEscape(parentID) + "/fork"),
		body:   body,
		headers: map[string]string{
			"Content-Type":    "application/json",
			"Idempotency-Key": key,
		},
		mode:      requestKeyed,
		sandboxID: parentID,
	})
}

func (c *Client) lifecycleRequest(ctx context.Context, sandboxID, verb string, body []byte, key string) (*httpResponse, error) {
	headers := map[string]string{"Content-Type": "application/json"}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return c.do(ctx, requestSpec{
		method:    http.MethodPost,
		url:       c.apiURL("/sandboxes/" + url.PathEscape(sandboxID) + "/" + verb),
		body:      body,
		headers:   headers,
		mode:      requestKeyed,
		sandboxID: sandboxID,
	})
}

// settleFork reads the copies until every one is running or has stopped
// heading there.
func (s *Sandbox) settleFork(ctx context.Context, copies []*Sandbox) error {
	for {
		var failed []*Sandbox
		pending := false
		for _, copy := range copies {
			state, _ := copy.stateAndReason()
			if state == StateRunning {
				continue
			}
			if state == StateError || state == StateDeleting || state == StateDeleted {
				failed = append(failed, copy)
				continue
			}
			gone, err := copy.read(ctx)
			if err != nil {
				return asRan(err)
			}
			if gone {
				failed = append(failed, copy)
				continue
			}
			state, _ = copy.stateAndReason()
			switch state {
			case StateRunning:
			case StateError, StateDeleting, StateDeleted:
				failed = append(failed, copy)
			default:
				pending = true
			}
		}
		if len(failed) > 0 {
			return forkFailure(failed)
		}
		if !pending {
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

// forkFailure picks the copy that failed for a reason of its own, preferring
// that reason over the API's "fork undone" cleanup note.
func forkFailure(copies []*Sandbox) error {
	cause := copies[0]
	rank := func(copy *Sandbox) int {
		state, reason := copy.stateAndReason()
		if reason != "" && !strings.HasPrefix(reason, "fork undone") {
			return 3
		}
		if state == StateError {
			return 2
		}
		if reason != "" {
			return 1
		}
		return 0
	}
	for _, copy := range copies[1:] {
		if rank(copy) > rank(cause) {
			cause = copy
		}
	}
	state, reason := cause.stateAndReason()
	if reason != "" {
		return asRan(&Error{
			Code:      "sandbox_error",
			Message:   "the sandbox failed: " + reason,
			Reason:    reason,
			SandboxID: cause.ID(),
		})
	}
	if state == StateDeleting || state == StateDeleted {
		return asRan(&Error{Code: "sandbox_deleted", SandboxID: cause.ID()})
	}
	return asRan(&Error{Code: "sandbox_error", SandboxID: cause.ID()})
}

func parseForkDocuments(status int, body []byte) ([]*sandboxDocument, error) {
	var envelope struct {
		Sandboxes []*sandboxDocument `json:"sandboxes"`
	}
	if len(body) == 0 || json.Unmarshal(body, &envelope) != nil || envelope.Sandboxes == nil {
		return nil, &Error{
			Code:    "bad_response",
			Status:  status,
			Message: "the API answered without the fork's copies",
			Effect:  EffectRan,
		}
	}
	for _, document := range envelope.Sandboxes {
		if document == nil || document.ID == "" {
			return nil, &Error{
				Code:    "bad_response",
				Status:  status,
				Message: "the fork answer carried a copy without an id",
				Effect:  EffectRan,
			}
		}
		if document.State == "" {
			document.State = StateStarting
		}
	}
	return envelope.Sandboxes, nil
}

func (c *Client) reclaimFork(parentID string, body []byte, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := c.forkRequest(ctx, parentID, body, key)
	if err != nil {
		return
	}
	documents, parseErr := parseForkDocuments(response.status, response.body)
	if parseErr != nil {
		return
	}
	ids := make([]string, 0, len(documents))
	for _, document := range documents {
		ids = append(ids, document.ID)
	}
	c.deleteCopiesBestEffort(ids)
}

func (c *Client) deleteCopiesBestEffort(ids []string) {
	if len(ids) == 0 {
		return
	}
	// One bound for every copy, with room for each to get its attempt.
	budget := 10*time.Second + time.Duration(len(ids))*500*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for _, id := range ids {
		_, _ = c.do(ctx, requestSpec{
			method:    http.MethodDelete,
			url:       c.apiURL("/sandboxes/" + url.PathEscape(id)),
			mode:      requestRead,
			sandboxID: id,
		})
	}
}

func sandboxIDs(copies []*Sandbox) []string {
	ids := make([]string, 0, len(copies))
	for _, copy := range copies {
		ids = append(ids, copy.ID())
	}
	return ids
}
