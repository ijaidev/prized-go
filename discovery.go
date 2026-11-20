package prized

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// maxSelectorLabels is the most labels one API selector may name.
	maxSelectorLabels = 32
	// maxBulkDeleteCalls bounds the requests one DeleteByLabels call makes.
	maxBulkDeleteCalls = 100
)

// Template describes one sandbox template and the shape it allows.
type Template struct {
	ID                   string
	Build                string
	Defaults             SandboxResources
	Min                  SandboxResources
	Max                  SandboxResources
	Durability           string
	DefaultMaxRuntimeMin int
}

// ListSandboxesOptions filters List.
type ListSandboxesOptions struct {
	// Labels keeps sandboxes carrying every given label.
	Labels map[string]string
	// States keeps sandboxes in any of these states; empty keeps them all.
	States []string
}

// Connect attaches to an existing sandbox by id, in whatever state it is in.
// It reads the sandbox once and does not wait for any state.
func (s *Sandboxes) Connect(ctx context.Context, id string) (*Sandbox, error) {
	if id == "" {
		return nil, newError("invalid_argument", "sandbox id must not be empty")
	}
	ctx, cancel := callContext(ctx)
	defer cancel()

	response, err := s.client.do(ctx, requestSpec{
		method:    http.MethodGet,
		url:       s.client.apiURL("/sandboxes/" + url.PathEscape(id)),
		mode:      requestRead,
		sandboxID: id,
	})
	if err != nil {
		return nil, err
	}
	document, parseErr := parseSandboxDocument(response.status, response.body)
	if parseErr != nil {
		return nil, parseErr
	}
	return newSandbox(s.client, document), nil
}

// List returns the workspace's sandboxes that are not deleted, newest first,
// filtered by every given label and by any of the given states.
func (s *Sandboxes) List(ctx context.Context, options ListSandboxesOptions) ([]*Sandbox, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()

	query, _ := labelSelector(options.Labels, false)
	response, err := s.client.do(ctx, requestSpec{
		method: http.MethodGet,
		url:    s.client.apiURL("/sandboxes") + query,
		mode:   requestRead,
	})
	if err != nil {
		return nil, err
	}
	documents, parseErr := parseSandboxList(response.status, response.body)
	if parseErr != nil {
		return nil, parseErr
	}

	states := make(map[string]bool, len(options.States))
	for _, state := range options.States {
		states[state] = true
	}
	found := make([]*Sandbox, 0, len(documents))
	for _, document := range documents {
		// The API leaves deleted sandboxes out; filter one that slips
		// through and re-check the filters from the document itself.
		if document.State == StateDeleted {
			continue
		}
		if len(states) > 0 && !states[document.State] {
			continue
		}
		if !labelsMatch(document.Labels, options.Labels) {
			continue
		}
		found = append(found, newSandbox(s.client, document))
	}
	return found, nil
}

// Templates lists the templates a sandbox can be created from, with each
// one's default, minimum and maximum shape.
func (s *Sandboxes) Templates(ctx context.Context) ([]Template, error) {
	ctx, cancel := callContext(ctx)
	defer cancel()

	response, err := s.client.do(ctx, requestSpec{
		method: http.MethodGet,
		url:    s.client.apiURL("/sandbox-templates"),
		mode:   requestRead,
	})
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Templates *[]json.RawMessage `json:"templates"`
	}
	if len(response.body) == 0 || json.Unmarshal(response.body, &envelope) != nil || envelope.Templates == nil {
		return nil, &Error{
			Code:    "bad_response",
			Status:  response.status,
			Message: "the API answered without a template list",
			Effect:  EffectRan,
		}
	}

	templates := make([]Template, 0, len(*envelope.Templates))
	for _, raw := range *envelope.Templates {
		var entry struct {
			ID        string `json:"id"`
			Build     string `json:"build"`
			Resources struct {
				Defaults SandboxResources `json:"defaults"`
				Min      SandboxResources `json:"min"`
				Max      SandboxResources `json:"max"`
			} `json:"resources"`
			Durability           string `json:"durability"`
			DefaultMaxRuntimeMin int    `json:"defaultMaxRuntimeMin"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" {
			continue
		}
		durability := entry.Durability
		if durability == "" {
			durability = "ephemeral"
		}
		templates = append(templates, Template{
			ID:                   entry.ID,
			Build:                entry.Build,
			Defaults:             entry.Resources.Defaults,
			Min:                  entry.Resources.Min,
			Max:                  entry.Resources.Max,
			Durability:           durability,
			DefaultMaxRuntimeMin: entry.DefaultMaxRuntimeMin,
		})
	}
	return templates, nil
}

// DeleteByLabels deletes every sandbox carrying all of labels and returns
// their ids. It requires at least one label. The API deletes up to 250
// sandboxes a request, so the delete is repeated until no matches are left;
// each request selects the current matches. A delete whose answer was lost is
// not sent again: list the matches before deleting again.
func (s *Sandboxes) DeleteByLabels(ctx context.Context, labels map[string]string) ([]string, error) {
	if len(labels) == 0 {
		return nil, newError("invalid_argument", "delete by labels needs at least one label")
	}
	query, err := labelSelector(labels, true)
	if err != nil {
		return nil, err
	}
	ctx, cancel := callContext(ctx)
	defer cancel()

	deleted := make([]string, 0)
	seen := make(map[string]bool)
	for call := 1; call <= maxBulkDeleteCalls; call++ {
		response, err := s.client.do(ctx, requestSpec{
			method: http.MethodDelete,
			url:    s.client.apiURL("/sandboxes") + query,
			mode:   requestBulkDelete,
		})
		if err != nil {
			return nil, err
		}
		ids, more, parseErr := parseDeletedPage(response.status, response.body)
		if parseErr != nil {
			return nil, parseErr
		}
		fresh := 0
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				deleted = append(deleted, id)
				fresh++
			}
		}
		if !more {
			return deleted, nil
		}
		if fresh == 0 {
			return nil, deleteIncompleteError(len(deleted), call)
		}
	}
	return nil, deleteIncompleteError(len(deleted), maxBulkDeleteCalls)
}

func parseDeletedPage(status int, body []byte) ([]string, bool, error) {
	var document struct {
		IDs  *[]string `json:"ids"`
		More bool      `json:"more"`
	}
	if len(body) == 0 || json.Unmarshal(body, &document) != nil || document.IDs == nil {
		return nil, false, &Error{
			Code:    "bad_response",
			Status:  status,
			Message: "the API answered without the deleted sandboxes' ids",
			Effect:  EffectRan,
		}
	}
	return *document.IDs, document.More, nil
}

func deleteIncompleteError(deleted, calls int) *Error {
	return &Error{
		Code: "delete_incomplete",
		Message: fmt.Sprintf(
			"deleted %d sandboxes in %d requests and some are left; call again to delete the rest",
			deleted, calls,
		),
		Effect: EffectRan,
	}
}

func callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultCallTimeout)
}

// labelSelector builds "?label=key=value&..." for the API, in key order.
// A list leaves a pair the API cannot select on to its own client-side
// filter; a delete refuses it.
func labelSelector(labels map[string]string, strict bool) (string, error) {
	if len(labels) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	values := url.Values{}
	selected := 0
	for _, key := range keys {
		value := labels[key]
		if !selectableLabel(key, value) {
			if strict {
				return "", newError("invalid_argument", fmt.Sprintf(
					"cannot select on label %q: a key is 1 to 64 characters without '=', and a value is up to 256",
					key,
				))
			}
			continue
		}
		values.Add("label", key+"="+value)
		selected++
	}
	if selected > maxSelectorLabels {
		if strict {
			return "", newError("invalid_argument", fmt.Sprintf("at most %d labels can be selected on", maxSelectorLabels))
		}
		return "", nil
	}
	if selected == 0 {
		return "", nil
	}
	return "?" + values.Encode(), nil
}

func selectableLabel(key, value string) bool {
	if key == "" || strings.Contains(key, "=") || strings.ContainsRune(key+value, '\x00') {
		return false
	}
	if utf16Length(key) > 64 || utf16Length(value) > 256 {
		return false
	}
	return utf8.ValidString(key) && utf8.ValidString(value)
}

func utf16Length(text string) int {
	units := 0
	for _, char := range text {
		if char > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

func labelsMatch(have, want map[string]string) bool {
	for key, value := range want {
		got, ok := have[key]
		if !ok || got != value {
			return false
		}
	}
	return true
}
