package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
)

const maxResponseBody = 2 << 20

// API is the operator-facing surface used by the terminal application.
// Keeping it small makes the TUI independent from Wardstone's persistence
// implementation and straightforward to exercise in tests.
type API interface {
	Snapshot(context.Context) Snapshot
	DecideApproval(context.Context, string, string, string) error
}

type Overview = admin.Overview
type Investigation = admin.InvestigationSummary
type Approval = admin.ApprovalSummary

type Snapshot struct {
	Overview       Overview
	Investigations []Investigation
	Approvals      []Approval
	LoadedAt       time.Time
	Problems       []string
}

// Client talks only to the authenticated operator API. It never reaches into
// the database, which keeps authorization and approval invariants server-side.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

func NewClient(rawURL, token string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Wardstone API URL must be an absolute http or https URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("Wardstone API URL must use http or https")
	}
	if token == "" {
		return nil, errors.New("WARDSTONE_OPERATOR_TOKEN is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: parsed, token: token, httpClient: httpClient}, nil
}

func (c *Client) Snapshot(ctx context.Context) Snapshot {
	result := Snapshot{LoadedAt: time.Now()}
	type fetchResult struct {
		name string
		err  error
	}
	results := make(chan fetchResult, 3)
	var mu sync.Mutex

	go func() {
		var overview Overview
		err := c.get(ctx, "/v1/admin/overview", &overview)
		if err == nil {
			mu.Lock()
			result.Overview = overview
			mu.Unlock()
		}
		results <- fetchResult{name: "overview", err: err}
	}()
	go func() {
		var response investigationResponse
		err := c.get(ctx, "/v1/admin/investigations?limit=100", &response)
		if err == nil {
			mu.Lock()
			result.Investigations = response.Items()
			mu.Unlock()
		}
		results <- fetchResult{name: "investigations", err: err}
	}()
	go func() {
		var response approvalResponse
		err := c.get(ctx, "/v1/admin/approvals?status=PENDING&limit=100", &response)
		if err == nil {
			mu.Lock()
			result.Approvals = response.Items()
			mu.Unlock()
		}
		results <- fetchResult{name: "approvals", err: err}
	}()

	for range 3 {
		outcome := <-results
		if outcome.err != nil {
			result.Problems = append(result.Problems, fmt.Sprintf("%s: %v", outcome.name, outcome.err))
		}
	}
	return result
}

func (c *Client) DecideApproval(ctx context.Context, id, decision, actor string) error {
	payload := struct {
		Decision string `json:"decision"`
		Actor    string `json:"actor"`
	}{Decision: decision, Actor: actor}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, "/v1/admin/approvals/"+url.PathEscape(id)+"/decision", bytes.NewReader(body), nil)
}

func (c *Client) get(ctx context.Context, path string, target any) error {
	return c.request(ctx, http.MethodGet, path, nil, target)
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, target any) error {
	reference, err := url.Parse(path)
	if err != nil {
		return err
	}
	endpoint := c.baseURL.ResolveReference(reference)
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return friendlyNetworkError(err)
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maxResponseBody+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(encoded) > maxResponseBody {
		return errors.New("response exceeded 2 MiB")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var apiError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(encoded, &apiError)
		if apiError.Error != "" {
			return fmt.Errorf("%s (%s)", apiError.Error, response.Status)
		}
		return fmt.Errorf("API returned %s", response.Status)
	}
	if target == nil || len(bytes.TrimSpace(encoded)) == 0 {
		return nil
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func friendlyNetworkError(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return fmt.Errorf("cannot reach Wardstone: %v", urlError.Err)
	}
	return fmt.Errorf("cannot reach Wardstone: %w", err)
}

// The collection responses accept either a bare JSON array or the object
// wrappers used by the admin API. This also keeps the client tolerant while
// the early API evolves.
type investigationResponse struct {
	Investigations []Investigation `json:"investigations"`
	Data           []Investigation `json:"data"`
}

func (r *investigationResponse) UnmarshalJSON(data []byte) error {
	var bare []Investigation
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &bare); err != nil {
			return err
		}
		r.Investigations = bare
		return nil
	}
	type alias investigationResponse
	return json.Unmarshal(data, (*alias)(r))
}

func (r investigationResponse) Items() []Investigation {
	if r.Investigations != nil {
		return r.Investigations
	}
	return r.Data
}

type approvalResponse struct {
	Approvals []Approval `json:"approvals"`
	Data      []Approval `json:"data"`
}

func (r *approvalResponse) UnmarshalJSON(data []byte) error {
	var bare []Approval
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &bare); err != nil {
			return err
		}
		r.Approvals = bare
		return nil
	}
	type alias approvalResponse
	return json.Unmarshal(data, (*alias)(r))
}

func (r approvalResponse) Items() []Approval {
	if r.Approvals != nil {
		return r.Approvals
	}
	return r.Data
}
