package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/wardstone-project/wardstone/internal/delivery"
	"github.com/wardstone-project/wardstone/internal/domain"
)

// VerifyRequesterMessage reads one operator-selected comment from the original
// issue. Identity, creation time, and the full ADF body must match; a 404 is not
// evidence that no comment was created. No remote writes or searches occur.
func (c *Client) VerifyRequesterMessage(ctx context.Context, job delivery.Job, remoteID string) error {
	if !delivery.ValidRemoteID(remoteID) || job.Ticket.Source != Name || job.Ticket.ExternalID == "" || job.Message.Direction != domain.MessageOutbound || job.Delivery.ReviewedAt == nil || job.Message.Body == "" {
		return delivery.ErrCommentMismatch
	}
	var identity struct {
		AccountID string `json:"accountId"`
	}
	if err := c.readRecoveryJSON(ctx, "/rest/api/3/myself", "/rest/api/3/myself", &identity); err != nil || identity.AccountID == "" {
		return delivery.ErrVerificationUnavailable
	}
	suffix := "/rest/api/3/issue/" + job.Ticket.ExternalID + "/comment/" + remoteID
	escaped := "/rest/api/3/issue/" + url.PathEscape(job.Ticket.ExternalID) + "/comment/" + url.PathEscape(remoteID)
	var comment struct {
		Properties []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"properties"`
		ID     string `json:"id"`
		Author struct {
			AccountID string `json:"accountId"`
		} `json:"author"`
		UpdateAuthor struct {
			AccountID string `json:"accountId"`
		} `json:"updateAuthor"`
		Created string `json:"created"`
		Body    any    `json:"body"`
	}
	if err := c.readRecoveryJSON(ctx, suffix, escaped, &comment); err != nil {
		return err
	}
	created, err := time.Parse("2006-01-02T15:04:05.999999999-0700", comment.Created)
	if err != nil {
		created, err = time.Parse(time.RFC3339Nano, comment.Created)
	}
	// Use milliseconds because Jira's timestamp precision is milliseconds.
	if err != nil || created.Before(job.Delivery.CreatedAt.Truncate(time.Millisecond)) || created.After(time.Now().Add(time.Minute)) || comment.ID != remoteID || comment.Author.AccountID != identity.AccountID ||
		(comment.UpdateAuthor.AccountID != "" && comment.UpdateAuthor.AccountID != identity.AccountID) {
		return delivery.ErrCommentMismatch
	}
	expected := map[string]any{"type": "doc", "version": float64(1), "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": job.Message.Body}}}}}
	if !reflect.DeepEqual(expected, comment.Body) {
		return delivery.ErrCommentMismatch
	}
	matchedMarker := false
	for _, property := range comment.Properties {
		if property.Key != "wardstone.message" {
			continue
		}
		var marker struct {
			MessageID  string `json:"message_id"`
			BodySHA256 string `json:"body_sha256"`
		}
		if err := json.Unmarshal(property.Value, &marker); err != nil {
			return delivery.ErrCommentMismatch
		}
		if matchedMarker || marker.MessageID != string(job.Message.ID) || marker.BodySHA256 != delivery.DraftDigest(job.Message.Body) {
			return delivery.ErrCommentMismatch
		}
		matchedMarker = true
	}
	if job.Delivery.CorrelationRequired && !matchedMarker {
		return delivery.ErrCommentMismatch
	}
	return nil
}

func (c *Client) readRecoveryJSON(ctx context.Context, path, rawPath string, target any) error {
	endpoint := *c.baseURL
	endpoint.Path = c.baseURL.Path + path
	endpoint.RawPath = c.baseURL.EscapedPath() + rawPath
	if strings.HasPrefix(path, "/rest/api/3/issue/") {
		endpoint.RawQuery = "expand=properties"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return delivery.ErrVerificationUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.email, c.apiToken)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return delivery.ErrVerificationUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return delivery.ErrCommentNotFound
	}
	if response.StatusCode != http.StatusOK {
		return delivery.ErrVerificationUnavailable
	}
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(body) > limit {
		return delivery.ErrVerificationUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return delivery.ErrVerificationUnavailable
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return delivery.ErrVerificationUnavailable
	}
	return nil
}
