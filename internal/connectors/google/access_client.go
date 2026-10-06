package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wardstone-project/wardstone/internal/connectors"
)

type Member struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Type  string `json:"type"`
	Role  string `json:"role"`
}

// AccessClient is separate from Client so existing collectors and fakes remain compatible.
type AccessClient interface {
	ReadAccessUser(context.Context, string) (User, error)
	GetGroup(context.Context, string) (Group, error)
	GetDirectMember(context.Context, string, string) (Member, error)
}

func (c *HTTPClient) ReadAccessUser(ctx context.Context, email string) (User, error) {
	if !validAccessEmail(email) {
		return User{}, connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	var result struct {
		ID           string `json:"id"`
		PrimaryEmail string `json:"primaryEmail"`
		Suspended    *bool  `json:"suspended"`
	}
	if err := c.accessGet(ctx, "/users/"+url.PathEscape(email), &result); err != nil {
		return User{}, err
	}
	if !strings.EqualFold(result.PrimaryEmail, email) || !validAccessEmail(result.PrimaryEmail) || !validProviderID(result.ID) || result.Suspended == nil {
		return User{}, connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	return User{ID: result.ID, PrimaryEmail: strings.ToLower(result.PrimaryEmail), Suspended: *result.Suspended}, nil
}

func (c *HTTPClient) GetGroup(ctx context.Context, email string) (Group, error) {
	if !validAccessEmail(email) {
		return Group{}, connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	var result Group
	if err := c.accessGet(ctx, "/groups/"+url.PathEscape(email), &result); err != nil {
		return Group{}, err
	}
	if !validGroup(result, email) {
		return Group{}, connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	result.Email = strings.ToLower(result.Email)
	return result, nil
}

func (c *HTTPClient) GetDirectMember(ctx context.Context, groupKey, userKey string) (Member, error) {
	if !validDirectoryKey(groupKey) || !validDirectoryKey(userKey) {
		return Member{}, connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	var result Member
	if err := c.accessGet(ctx, "/groups/"+url.PathEscape(groupKey)+"/members/"+url.PathEscape(userKey), &result); err != nil {
		return Member{}, err
	}
	if !validMemberShape(result) || !(strings.EqualFold(result.Email, userKey) || result.ID == userKey) {
		return Member{}, connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	return result, nil
}

func validProviderID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !asciiAlnum(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
func validDirectoryKey(key string) bool { return validAccessEmail(key) || validProviderID(key) }
func validGroup(group Group, email string) bool {
	return validProviderID(group.ID) && validAccessEmail(group.Email) && strings.EqualFold(group.Email, email)
}
func validMemberShape(member Member) bool {
	return validProviderID(member.ID) && validAccessEmail(member.Email) && member.Type == "USER" && (member.Role == "MEMBER" || member.Role == "MANAGER" || member.Role == "OWNER")
}

// Single-object reads: one attempt, 64 KiB maximum, fixed deadline, no redirects,
// no response bodies, endpoint strings, credentials, or transport errors in failures.
func (c *HTTPClient) accessGet(ctx context.Context, path string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return connectors.NewProbeError(connectors.ProbeUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return connectors.NewProbeError(connectors.ProbeFailureForStatus(response.StatusCode))
	}
	if err := decodeAccessResponse(response.Body, target); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	return nil
}

// Reject duplicate top-level identity fields rather than allowing last-value wins.
// Unknown Directory metadata is permitted, but only one bounded JSON object is accepted.
func decodeAccessResponse(body io.Reader, target any) error {
	return decodeBoundedAccessObject(body, target, 64<<10)
}

func decodeBoundedAccessObject(body io.Reader, target any, maxBytes int) error {
	contents, err := io.ReadAll(io.LimitReader(body, int64(maxBytes)+1))
	if err != nil || len(contents) > maxBytes || !utf8.Valid(contents) {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	keys := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return connectors.NewProbeError(connectors.ProbeInvalidResponse)
		}
		key = strings.ToLower(key)
		if _, duplicate := keys[key]; duplicate {
			return connectors.NewProbeError(connectors.ProbeInvalidResponse)
		}
		keys[key] = struct{}{}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return connectors.NewProbeError(connectors.ProbeInvalidResponse)
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	if err := json.Unmarshal(contents, target); err != nil {
		return connectors.NewProbeError(connectors.ProbeInvalidResponse)
	}
	return nil
}

func accessFailure(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if failure, ok := connectors.ProbeFailureOf(err); ok {
		return string(failure)
	}
	return "unavailable"
}
