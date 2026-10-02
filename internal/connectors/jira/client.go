package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/wardstone-project/wardstone/internal/connectors"
	"github.com/wardstone-project/wardstone/internal/domain"
)

const maxCommentBody = 16 << 10

type Client struct {
	baseURL    *url.URL
	email      string
	apiToken   string
	httpClient *http.Client
}

func NewClient(baseURL, email, apiToken string, httpClient *http.Client) (*Client, error) {
	if email == "" || apiToken == "" || httpClient == nil {
		return nil, errors.New("Jira base URL, email, API token, and HTTP client are required")
	}
	parsed, err := connectors.ParseBaseURL("Jira", baseURL)
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: parsed, email: email, apiToken: apiToken, httpClient: connectors.NoRedirectClient(httpClient)}, nil
}

func (c *Client) Probe(ctx context.Context) error {
	endpoint := *c.baseURL
	endpoint.Path = c.baseURL.Path + "/rest/api/3/myself"
	endpoint.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	request.Header.Set("Accept", "application/json")
	request.SetBasicAuth(c.email, c.apiToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return connectors.NewProbeError(connectors.ProbeUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return connectors.NewProbeError(connectors.ProbeFailureForStatus(response.StatusCode))
	}
	var identity struct {
		AccountID string `json:"accountId"`
	}
	if err := connectors.DecodeProbeResponse(response.Body, &identity); err != nil || identity.AccountID == "" {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	return nil
}

func (c *Client) SendRequesterMessage(ctx context.Context, ticket domain.Ticket, message domain.CaseMessage) (string, error) {
	if ticket.Source != Name || ticket.ExternalID == "" || message.Direction != domain.MessageOutbound || len(message.Body) == 0 || len(message.Body) > maxCommentBody {
		return "", sendError{err: errors.New("invalid Jira requester message"), retryable: false}
	}
	payload, err := json.Marshal(map[string]any{"body": map[string]any{
		"type": "doc", "version": 1,
		"content": []map[string]any{{"type": "paragraph", "content": []map[string]string{{"type": "text", "text": message.Body}}}},
	}})
	if err != nil {
		return "", sendError{err: fmt.Errorf("encode Jira comment: %w", err), retryable: false}
	}
	endpoint := *c.baseURL
	endpoint.Path = c.baseURL.Path + "/rest/api/3/issue/" + ticket.ExternalID + "/comment"
	endpoint.RawPath = c.baseURL.EscapedPath() + "/rest/api/3/issue/" + url.PathEscape(ticket.ExternalID) + "/comment"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return "", sendError{err: fmt.Errorf("build Jira comment request: %w", err), retryable: false}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.SetBasicAuth(c.email, c.apiToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", sendError{err: fmt.Errorf("send Jira comment: %w", err), retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", sendError{err: fmt.Errorf("Jira comment API returned %s", response.Status), retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError}
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", sendError{err: fmt.Errorf("decode Jira comment response: %w", err), retryable: true}
	}
	if result.ID == "" {
		return "", sendError{err: errors.New("Jira comment response has no ID"), retryable: true}
	}
	return result.ID, nil
}

type sendError struct {
	err       error
	retryable bool
}

func (e sendError) Error() string   { return e.err.Error() }
func (e sendError) Unwrap() error   { return e.err }
func (e sendError) Retryable() bool { return e.retryable }
