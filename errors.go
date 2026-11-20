package prized

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Effect says whether an operation reached the service before it failed.
type Effect int

const (
	// EffectNotRun means the request provably never ran.
	EffectNotRun Effect = iota
	// EffectMaybeRan means the request may have run: its answer was lost.
	EffectMaybeRan
	// EffectRan means the request ran, whatever happened to its answer.
	EffectRan
)

// String returns the effect as a word.
func (e Effect) String() string {
	switch e {
	case EffectMaybeRan:
		return "maybe_ran"
	case EffectRan:
		return "ran"
	default:
		return "not_run"
	}
}

// Error is every failure the client returns.
//
// Status is the HTTP status, or 0 when no answer came back. Code is the API's
// machine-readable error, or one of the client's own codes such as
// missing_api_key, network_error, timeout, canceled, bad_response, or
// handle_closed. Body is the decoded error document when the answer carried
// one. Effect classifies whether the operation ran before the failure.
type Error struct {
	Code       string
	Status     int
	Message    string
	Reason     string
	SandboxID  string
	Body       map[string]any
	RetryAfter time.Duration
	Effect     Effect

	cause   error
	notSent bool
}

// Error returns a sentence with the status, code, reason and message.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	head := e.Code
	if e.Status != 0 {
		head = fmt.Sprintf("%d %s", e.Status, e.Code)
	}
	if e.Reason != "" {
		head += " (" + e.Reason + ")"
	}
	message := e.Message
	if message == "" {
		message = defaultMessage(e.Code)
	}
	return "prized: " + head + ": " + message
}

// Unwrap exposes the underlying transport or context error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// ErrHandleClosed is returned by a Sandbox method called after Close.
var ErrHandleClosed = errors.New("prized: sandbox handle is closed")

func newError(code, message string) *Error {
	if message == "" {
		message = defaultMessage(code)
	}
	return &Error{Code: code, Message: message, Effect: EffectNotRun}
}

func closedError() *Error {
	return &Error{
		Code:    "handle_closed",
		Message: "the sandbox handle is closed; no request was sent",
		Effect:  EffectNotRun,
		cause:   ErrHandleClosed,
	}
}

func contextError(ctxErr error, sandboxID string, effect Effect) *Error {
	code := "canceled"
	message := "the operation was canceled"
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		code = "timeout"
		message = "the operation ran out of time"
	}
	return &Error{
		Code:      code,
		Message:   message,
		SandboxID: sandboxID,
		Effect:    effect,
		cause:     ctxErr,
	}
}

var defaultMessages = map[string]string{
	"missing_api_key":        "no API key: pass WithAPIKey or set PRIZED_API_KEY",
	"invalid_api_key":        "the API key has characters no key has",
	"invalid_argument":       "an argument was not valid",
	"unauthorized":           "the API key was refused",
	"token_expired":          "the API key has expired",
	"key_access_denied":      "the API key lacks the Sandboxes permission it needs",
	"not_found":              "not found, or not visible to this API key",
	"rate_limited":           "too many requests; slow down and retry",
	"sandbox_quota_exceeded": "the workspace is at a sandbox quota",
	"paid_balance_required":  "sandboxes need paid credit right now",
	"insufficient_balance":   "the workspace balance is empty",
	"sandbox_error":          "the sandbox failed",
	"sandbox_deleted":        "the sandbox was deleted",
	"command_timeout":        "the command ran past its timeout and was killed",
	"file_too_large":         "the file is over the 100 MiB transfer limit",
	"not_directory":          "the path is not a directory",
	"directory_not_empty":    "the directory has files in it; remove it recursively",
	"unsupported_file_type":  "only a regular file can be written",
	"permission_denied":      "the sandbox user may not do that",
	"command_failed":         "the command failed",
	"network_error":          "could not reach the Prized API",
	"timeout":                "the operation timed out",
	"canceled":               "the operation was canceled",
	"bad_response":           "the API answered with something the client could not read",
	"http_error":             "the API answered with an error the client does not recognise",
	"edge_unknown":           "the API named no command endpoint this client trusts",
	"handle_closed":          "the sandbox handle is closed",
	"unexpected_redirect":    "the API answered with a redirect; the client does not follow redirects with your key",
}

func defaultMessage(code string) string {
	if message, ok := defaultMessages[code]; ok {
		return message
	}
	return strings.ReplaceAll(code, "_", " ")
}

// errorFromResponse turns an API error document into an Error. It accepts the
// flat {"error":"code",...} and nested {"error":{"code":...}} envelopes.
func errorFromResponse(status int, body []byte, header http.Header, sandboxID string) *Error {
	var document map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &document)
	}

	var code, message string
	if document != nil {
		switch envelope := document["error"].(type) {
		case string:
			code = envelope
		case map[string]any:
			code, _ = envelope["code"].(string)
			message, _ = envelope["message"].(string)
		}
		if message == "" {
			message, _ = document["message"].(string)
		}
	}

	if code == "" {
		snippet := strings.Join(strings.Fields(string(body)), " ")
		if len(snippet) > 160 {
			snippet = snippet[:160]
		}
		if snippet != "" {
			message = fmt.Sprintf("HTTP %d: %s", status, snippet)
		} else {
			message = fmt.Sprintf("HTTP %d", status)
		}
		code = "http_error"
	} else if message == "" {
		message = defaultMessage(code)
	}

	reason := ""
	if document != nil {
		for _, key := range []string{"reason", "field", "detail"} {
			if value, ok := document[key].(string); ok && value != "" {
				reason = value
				break
			}
		}
	}

	return &Error{
		Code:       code,
		Status:     status,
		Message:    message,
		Reason:     reason,
		SandboxID:  sandboxID,
		Body:       document,
		RetryAfter: parseRetryAfter(header.Get("Retry-After"), time.Now()),
		Effect:     EffectNotRun,
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds >= 0 {
		return seconds
	}
	if at, err := http.ParseTime(value); err == nil {
		wait := at.Sub(now)
		if wait > 0 {
			return wait
		}
		return 0
	}
	return 0
}
