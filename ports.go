package prized

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PreviewKeyHeader is the header a request to a sandbox port's URL carries the
// API key in.
const PreviewKeyHeader = "Prized-Key"

// PreviewToken is a short-lived URL token for one sandbox port. URL is the
// preview URL with the token in its prized_token query parameter, for
// programs; BrowserURL is the same on prized.link, for a browser tab or
// frame. BrowserURL is empty where the deployment has no browser domain.
type PreviewToken struct {
	URL        string
	BrowserURL string
	Token      string
	ExpiresAt  time.Time
}

// PreviewTokenOptions describes one preview token. ExpiresIn is a whole number
// of seconds: 60 to 86400, 3600 (an hour) when zero.
type PreviewTokenOptions struct {
	ExpiresIn int
}

// PortPreview is a sandbox port's preview link. URL is for programs with the
// API key; BrowserURL is the same port on prized.link, for a browser tab or
// frame (empty where the deployment has no browser domain). PublicExpiresAt
// is nil while the port is private.
type PortPreview struct {
	SandboxID       string
	Port            int
	URL             string
	BrowserURL      string
	Public          bool
	PublicExpiresAt *time.Time
}

// SetPortPublicOptions describes one public/private change. ExpiresIn is a
// whole number of seconds, 300 to 2592000, 604800 (7 days) when zero, and
// applies only when making a port public.
type SetPortPublicOptions struct {
	ExpiresIn int
}

// PreviewURL returns the https URL a port of the sandbox answers at, for a
// request that carries a workspace API key in the Prized-Key header
// (PreviewHeaders). Nothing is created: the URL is the sandbox's own, and
// every port but 22 and 81 has one.
//
// A request to a paused sandbox resumes it and waits up to 10 seconds, as
// Exec does; send Prized-Auto-Resume: false to get a 503 instead.
// PreviewURL fails with preview_unavailable where the deployment has sandbox
// ingress off.
func (s *Sandbox) PreviewURL(port int) (string, error) {
	if err := s.checkPreviewPort(port); err != nil {
		return "", err
	}
	s.mu.RLock()
	template := s.previewURLTemplate
	s.mu.RUnlock()
	if !strings.Contains(template, "{port}") {
		return "", &Error{
			Code:      "preview_unavailable",
			Message:   "sandbox ports at a URL are not turned on for this deployment",
			SandboxID: s.ID(),
		}
	}
	return strings.Replace(template, "{port}", strconv.Itoa(port), 1), nil
}

// PreviewHeaders returns the headers a request to PreviewURL sends: this
// client's API key in Prized-Key. The header never reaches the sandbox's app.
//
// Send these headers only to PreviewURL, and never follow a redirect with
// them: an HTTP client resends custom headers to whatever host a redirect
// names, which could hand that host your key. The API keeps a redirect on
// the preview URL's own origin and answers one that points elsewhere as
// Prized-Location, which no client follows. To follow a redirect, resolve
// its Location against PreviewURL and send PreviewHeaders again only when the
// result has the preview URL's scheme and host. This client refuses redirects
// on its own transport, including one supplied with WithHTTPClient.
func (s *Sandbox) PreviewHeaders() map[string]string {
	return map[string]string{PreviewKeyHeader: s.client.apiKey}
}

// PreviewToken mints a short-lived token for port, for a client that cannot
// send Prized-Key in a header: a person in a browser tab, a WebSocket opened
// from a browser page, or a program you hand this one port to and not your
// API key. The app never sees the token.
//
// The token opens the port until ExpiresAt, or until this client's key is
// revoked or expires, the sandbox is deleted, or the port is made public or
// private again. It never wakes a paused sandbox. Minting one needs a paid
// workspace (share_links_need_payment otherwise).
func (s *Sandbox) PreviewToken(ctx context.Context, port int, options *PreviewTokenOptions) (*PreviewToken, error) {
	if s.isClosed() {
		return nil, closedError()
	}
	if err := s.checkPreviewPort(port); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	expiresIn := 0
	if options != nil {
		expiresIn = options.ExpiresIn
		if expiresIn < 0 {
			return nil, s.invalidArgument("expiresIn must be a whole number of seconds > 0")
		}
	}

	body := []byte("{}")
	if expiresIn > 0 {
		encoded, err := json.Marshal(struct {
			ExpiresIn int `json:"expiresIn"`
		}{ExpiresIn: expiresIn})
		if err != nil {
			return nil, &Error{Code: "internal_error", Message: "could not encode the preview token request", SandboxID: s.ID(), cause: err}
		}
		body = encoded
	}

	id := s.ID()
	callCtx, cancel := context.WithDeadline(ctx, time.Now().Add(2*s.client.requestTimeout))
	defer cancel()
	response, err := s.client.do(callCtx, requestSpec{
		method:    http.MethodPost,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(id) + "/ports/" + strconv.Itoa(port) + "/token"),
		body:      body,
		headers:   map[string]string{"Content-Type": "application/json"},
		mode:      requestSafe,
		sandboxID: id,
	})
	if err != nil {
		return nil, err
	}

	var document struct {
		URL        string `json:"url"`
		BrowserURL string `json:"browserUrl"`
		Token      string `json:"token"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if err := json.Unmarshal(response.body, &document); err != nil || document.URL == "" || document.Token == "" || document.ExpiresAt == "" {
		return nil, badPreviewResponse(response.status, id, "the API answered a preview token without its url, token or expiry")
	}
	expiresAt, valid := parsePreviewTime(document.ExpiresAt)
	if !valid {
		return nil, badPreviewResponse(response.status, id, "the API answered a preview token without its url, token or expiry")
	}
	return &PreviewToken{
		URL:        document.URL,
		BrowserURL: document.BrowserURL,
		Token:      document.Token,
		ExpiresAt:  *expiresAt,
	}, nil
}

// SetPortPublic makes port public for anyone with its URL, with no credential,
// for the requested expiry (7 days by default), or private again with false.
// A port already public keeps its expiry: make it private, then public again,
// to change it.
//
// Public needs the workspace owner's key and a paid workspace, and counts
// toward the workspace's 10 public ports. It never wakes a paused sandbox.
// Making a port public or private again ends the tokens made for it.
func (s *Sandbox) SetPortPublic(ctx context.Context, port int, public bool, options *SetPortPublicOptions) (*PortPreview, error) {
	if s.isClosed() {
		return nil, closedError()
	}
	if err := s.checkPreviewPort(port); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	expiresIn := 0
	if options != nil {
		expiresIn = options.ExpiresIn
		if expiresIn < 0 {
			return nil, s.invalidArgument("expiresIn must be a whole number of seconds > 0")
		}
	}
	if !public && expiresIn > 0 {
		return nil, s.invalidArgument("expiresIn applies only when making a port public")
	}

	var body []byte
	switch {
	case !public:
		body = []byte(`{"public":false}`)
	case expiresIn > 0:
		encoded, err := json.Marshal(struct {
			Public    bool `json:"public"`
			ExpiresIn int  `json:"expiresIn"`
		}{Public: true, ExpiresIn: expiresIn})
		if err != nil {
			return nil, &Error{Code: "internal_error", Message: "could not encode the port request", SandboxID: s.ID(), cause: err}
		}
		body = encoded
	default:
		body = []byte(`{"public":true}`)
	}

	id := s.ID()
	callCtx, cancel := context.WithDeadline(ctx, time.Now().Add(2*s.client.requestTimeout))
	defer cancel()
	response, err := s.client.do(callCtx, requestSpec{
		method:    http.MethodPut,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(id) + "/ports/" + strconv.Itoa(port)),
		body:      body,
		headers:   map[string]string{"Content-Type": "application/json"},
		mode:      requestSafe,
		sandboxID: id,
	})
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Preview *struct {
			SandboxID       string `json:"sandboxId"`
			Port            *int   `json:"port"`
			URL             string `json:"url"`
			BrowserURL      string `json:"browserUrl"`
			Public          *bool  `json:"public"`
			PublicExpiresAt string `json:"publicExpiresAt"`
		} `json:"preview"`
	}
	if err := json.Unmarshal(response.body, &envelope); err != nil || envelope.Preview == nil ||
		envelope.Preview.URL == "" || envelope.Preview.Public == nil {
		return nil, badPreviewResponse(response.status, id, "the API answered a port without its url or public setting")
	}

	preview := &PortPreview{
		SandboxID:  id,
		Port:       port,
		URL:        envelope.Preview.URL,
		BrowserURL: envelope.Preview.BrowserURL,
		Public:     *envelope.Preview.Public,
	}
	if envelope.Preview.SandboxID != "" {
		preview.SandboxID = envelope.Preview.SandboxID
	}
	if envelope.Preview.Port != nil {
		preview.Port = *envelope.Preview.Port
	}
	if expiresAt, valid := parsePreviewTime(envelope.Preview.PublicExpiresAt); valid {
		preview.PublicExpiresAt = expiresAt
	}
	return preview, nil
}

func (s *Sandbox) checkPreviewPort(port int) error {
	if port < 1 || port > 65535 || port == 22 || port == 81 {
		return &Error{
			Code:      "bad_request",
			Message:   fmt.Sprintf("port must be a whole number from 1 to 65535 other than 22 and 81, not %d", port),
			SandboxID: s.ID(),
		}
	}
	return nil
}

func (s *Sandbox) invalidArgument(message string) *Error {
	return &Error{Code: "invalid_argument", Message: message, SandboxID: s.ID()}
}

func badPreviewResponse(status int, sandboxID, message string) *Error {
	return &Error{
		Code:      "bad_response",
		Status:    status,
		Message:   message,
		SandboxID: sandboxID,
		Effect:    EffectRan,
	}
}

func parsePreviewTime(value string) (*time.Time, bool) {
	if value == "" {
		return nil, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, false
	}
	return &parsed, true
}
