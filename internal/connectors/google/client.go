package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wardstone-project/wardstone/internal/domain"
)

type User struct {
	ID           string `json:"id"`
	PrimaryEmail string `json:"primaryEmail"`
	FullName     string `json:"full_name"`
	Suspended    bool   `json:"suspended"`
}

type Group struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

type Client interface {
	GetUser(context.Context, string) (User, error)
	ListGroups(context.Context, string) ([]Group, error)
}

type HTTPClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewHTTPClient(baseURL, token string, httpClient *http.Client) (*HTTPClient, error) {
	if baseURL == "" || token == "" || httpClient == nil {
		return nil, errors.New("Google base URL, access token, and HTTP client are required")
	}
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, httpClient: httpClient}, nil
}

func (c *HTTPClient) GetUser(ctx context.Context, email string) (User, error) {
	var response struct {
		ID           string `json:"id"`
		PrimaryEmail string `json:"primaryEmail"`
		Suspended    bool   `json:"suspended"`
		Name         struct {
			FullName string `json:"fullName"`
		} `json:"name"`
	}
	if err := c.get(ctx, "/users/"+url.PathEscape(email), nil, &response); err != nil {
		return User{}, err
	}
	return User{ID: response.ID, PrimaryEmail: response.PrimaryEmail, FullName: response.Name.FullName, Suspended: response.Suspended}, nil
}

func (c *HTTPClient) ListGroups(ctx context.Context, email string) ([]Group, error) {
	const maxPages = 5
	groups := make([]Group, 0, 32)
	pageToken := ""
	for page := 0; page < maxPages; page++ {
		var response struct {
			Groups        []Group `json:"groups"`
			NextPageToken string  `json:"nextPageToken"`
		}
		query := url.Values{"userKey": []string{email}, "maxResults": []string{"200"}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		if err := c.get(ctx, "/groups", query, &response); err != nil {
			return nil, err
		}
		groups = append(groups, response.Groups...)
		if response.NextPageToken == "" {
			return groups, nil
		}
		pageToken = response.NextPageToken
	}
	return nil, errors.New("Google groups response exceeded 1000-item collection limit")
}

func (c *HTTPClient) get(ctx context.Context, path string, query url.Values, target any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+c.token)
		response, err := c.httpClient.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			lastErr = fmt.Errorf("Google API returned %s", response.Status)
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			return fmt.Errorf("Google API returned %s", response.Status)
		}
		err = json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(target)
		_ = response.Body.Close()
		if err != nil {
			return fmt.Errorf("decode Google API response: %w", err)
		}
		return nil
	}
	return lastErr
}

type UserCollector struct{ Client Client }

func (UserCollector) Name() domain.ConnectorName        { return Name }
func (UserCollector) Capability() domain.CapabilityName { return UsersGet }

func (c UserCollector) Collect(ctx context.Context, ticket domain.Ticket) ([]domain.Evidence, error) {
	if ticket.ReporterEmail == "" {
		return nil, errors.New("ticket has no reporter email")
	}
	user, err := c.Client.GetUser(ctx, ticket.ReporterEmail)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(user)
	if err != nil {
		return nil, err
	}
	return []domain.Evidence{{Source: Name, Kind: "google.user", Summary: "Google Workspace user record for " + ticket.ReporterEmail, Data: data}}, nil
}

type GroupCollector struct{ Client Client }

func (GroupCollector) Name() domain.ConnectorName        { return Name }
func (GroupCollector) Capability() domain.CapabilityName { return UsersListGroups }

func (c GroupCollector) Collect(ctx context.Context, ticket domain.Ticket) ([]domain.Evidence, error) {
	if ticket.ReporterEmail == "" {
		return nil, errors.New("ticket has no reporter email")
	}
	groups, err := c.Client.ListGroups(ctx, ticket.ReporterEmail)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(groups)
	if err != nil {
		return nil, err
	}
	return []domain.Evidence{{Source: Name, Kind: "google.groups", Summary: fmt.Sprintf("%d Google Workspace groups for %s", len(groups), ticket.ReporterEmail), Data: data}}, nil
}
