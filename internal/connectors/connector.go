package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/wardstone-project/wardstone/internal/capabilities"
	"github.com/wardstone-project/wardstone/internal/domain"
)

type Registrar interface {
	Name() domain.ConnectorName
	Capabilities() []capabilities.Definition
}

type EvidenceCollector interface {
	Name() domain.ConnectorName
	Capability() domain.CapabilityName
	Collect(context.Context, domain.Ticket) ([]domain.Evidence, error)
}

// Prober performs a bounded, read-only connectivity and permission check.
type Prober interface {
	Probe(context.Context) error
}

type ProbeFailure string

const (
	ProbeUnauthorized    ProbeFailure = "unauthorized"
	ProbeForbidden       ProbeFailure = "forbidden"
	ProbeNotFound        ProbeFailure = "not_found"
	ProbeRateLimited     ProbeFailure = "rate_limited"
	ProbeUnavailable     ProbeFailure = "unavailable"
	ProbeInvalidResponse ProbeFailure = "invalid_response"
	ProbeRedirectBlocked ProbeFailure = "redirect_blocked"
	ProbeSchemaMismatch  ProbeFailure = "schema_mismatch"
)

// ProbeError carries a stable category without exposing an upstream response,
// endpoint, account, or credential to the operator API.
type ProbeError struct {
	Failure ProbeFailure
}

func (e *ProbeError) Error() string { return string(e.Failure) }

func NewProbeError(failure ProbeFailure) error { return &ProbeError{Failure: failure} }

func ProbeFailureOf(err error) (ProbeFailure, bool) {
	var probeErr *ProbeError
	if errors.As(err, &probeErr) {
		return probeErr.Failure, true
	}
	return "", false
}

func ProbeFailureForStatus(status int) ProbeFailure {
	switch {
	case status == http.StatusUnauthorized:
		return ProbeUnauthorized
	case status == http.StatusForbidden:
		return ProbeForbidden
	case status == http.StatusNotFound:
		return ProbeNotFound
	case status == http.StatusTooManyRequests:
		return ProbeRateLimited
	case status >= http.StatusInternalServerError:
		return ProbeUnavailable
	case status >= http.StatusMultipleChoices && status < http.StatusBadRequest:
		return ProbeRedirectBlocked
	default:
		return ProbeInvalidResponse
	}
}

// ParseBaseURL rejects URL forms that could leak connector credentials or make
// endpoint composition ambiguous. HTTP is accepted only for loopback fixtures.
func ParseBaseURL(label, value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New(label + " base URL must be an absolute origin without credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New(label + " base URL must use HTTPS unless it is a loopback development endpoint")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func NoRedirectClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func DecodeProbeResponse(body io.Reader, target any) error {
	limited := &io.LimitedReader{R: body, N: (64 << 10) + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil || limited.N <= 0 {
		return NewProbeError(ProbeInvalidResponse)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) || limited.N <= 0 {
		return NewProbeError(ProbeInvalidResponse)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
