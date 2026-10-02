// Package readiness coordinates explicit, read-only operator connection tests.
// It never stores or returns connector credentials or raw upstream errors.
package readiness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/connectors"
)

var (
	ErrNotFound       = errors.New("setup component not found")
	ErrNotConfigured  = errors.New("setup component is not configured")
	ErrNotProbeable   = errors.New("setup component cannot be probed")
	ErrProbeRunning   = errors.New("setup component probe is already running")
	ErrProbeThrottled = errors.New("setup component probe was run too recently")
)

const probeCooldown = time.Second

type ProbeFunc func(context.Context) error

type Check struct {
	Name         string
	Label        string
	Description  string
	Permission   string
	Required     bool
	Configured   bool
	Probe        ProbeFunc
	Ready        bool
	ReadyCode    string
	ReadyMessage string
}

type Service struct {
	mu       sync.RWMutex
	checks   []Check
	index    map[string]int
	results  map[string]admin.SetupComponent
	running  map[string]bool
	timeout  time.Duration
	features admin.RuntimeFeatures
	logger   *slog.Logger
	now      func() time.Time
}

func New(checks []Check, features admin.RuntimeFeatures, timeout time.Duration, logger *slog.Logger) (*Service, error) {
	if len(checks) == 0 || timeout <= 0 || logger == nil {
		return nil, errors.New("setup checks, a positive timeout, and logger are required")
	}
	service := &Service{
		checks: append([]Check(nil), checks...), index: make(map[string]int, len(checks)),
		results: make(map[string]admin.SetupComponent, len(checks)), running: make(map[string]bool, len(checks)),
		timeout: timeout, features: features, logger: logger, now: func() time.Time { return time.Now().UTC() },
	}
	for i, check := range service.checks {
		if check.Name == "" || check.Label == "" || check.Description == "" || check.Permission == "" {
			return nil, errors.New("setup checks require a name, label, description, and permission summary")
		}
		if _, exists := service.index[check.Name]; exists {
			return nil, fmt.Errorf("duplicate setup check %q", check.Name)
		}
		service.index[check.Name] = i
		service.results[check.Name] = initialResult(check)
	}
	return service, nil
}

func initialResult(check Check) admin.SetupComponent {
	component := admin.SetupComponent{
		Name: check.Name, Label: check.Label, Description: check.Description, Permission: check.Permission,
		Required: check.Required, Configured: check.Configured, Probeable: check.Probe != nil,
		State: admin.SetupNotTested, Code: "not_tested", Message: "Connection has not been tested yet.",
	}
	if !check.Configured {
		component.State, component.Code, component.Message = admin.SetupNotConfigured, "not_configured", "Configuration is not present."
	} else if check.Ready {
		component.State, component.Code = admin.SetupReady, check.ReadyCode
		component.Message = check.ReadyMessage
	}
	return component
}

func (s *Service) Status(context.Context) (admin.SetupStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statusLocked(), nil
}

func (s *Service) Probe(ctx context.Context, name string) (admin.SetupComponent, error) {
	s.mu.Lock()
	position, ok := s.index[name]
	if !ok {
		s.mu.Unlock()
		return admin.SetupComponent{}, ErrNotFound
	}
	check := s.checks[position]
	if !check.Configured {
		s.mu.Unlock()
		return admin.SetupComponent{}, ErrNotConfigured
	}
	if check.Probe == nil {
		s.mu.Unlock()
		return admin.SetupComponent{}, ErrNotProbeable
	}
	if s.running[name] {
		s.mu.Unlock()
		return admin.SetupComponent{}, ErrProbeRunning
	}
	if checkedAt := s.results[name].CheckedAt; checkedAt != nil && s.now().Sub(*checkedAt) < probeCooldown {
		s.mu.Unlock()
		return admin.SetupComponent{}, ErrProbeThrottled
	}
	s.running[name] = true
	s.mu.Unlock()

	started := s.now()
	probeCtx, cancel := context.WithTimeout(ctx, s.timeout)
	err := check.Probe(probeCtx)
	cancel()
	finished := s.now()

	s.mu.Lock()
	delete(s.running, name)
	if ctx.Err() != nil {
		s.mu.Unlock()
		s.logger.Info("setup connection tested", "component", name, "result", "canceled", "duration_ms", max(finished.Sub(started).Milliseconds(), 0))
		return admin.SetupComponent{}, ctx.Err()
	}
	result := s.results[name]
	checkedAt := finished
	result.CheckedAt = &checkedAt
	result.DurationMS = max(finished.Sub(started).Milliseconds(), 0)
	if err == nil {
		result.State, result.Code, result.Message = admin.SetupReady, "ready", "Connection and required read access verified."
	} else {
		result.State, result.Code = admin.SetupFailed, failureCode(err)
		result.Message = failureMessage(result.Code)
	}
	s.results[name] = result
	s.mu.Unlock()
	s.logger.Info("setup connection tested", "component", name, "result", result.Code, "duration_ms", result.DurationMS)
	return result, nil
}

func (s *Service) statusLocked() admin.SetupStatus {
	status := admin.SetupStatus{State: admin.SetupReady, GeneratedAt: s.now(), Features: s.features, Components: make([]admin.SetupComponent, 0, len(s.checks))}
	requiredMissing, failed, notTested := false, false, false
	for _, check := range s.checks {
		result := s.results[check.Name]
		status.Components = append(status.Components, result)
		requiredMissing = requiredMissing || result.Required && result.State == admin.SetupNotConfigured
		failed = failed || result.State == admin.SetupFailed
		notTested = notTested || result.Configured && result.Probeable && result.State == admin.SetupNotTested
	}
	switch {
	case requiredMissing:
		status.State = admin.SetupRequired
	case failed:
		status.State = admin.SetupDegraded
	case notTested:
		status.State = admin.SetupNotTested
	}
	return status
}

func failureCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if failure, ok := connectors.ProbeFailureOf(err); ok {
		return string(failure)
	}
	return "unavailable"
}

func failureMessage(code string) string {
	switch code {
	case "timeout":
		return "The connection test timed out."
	case "unauthorized":
		return "The configured credentials were not accepted."
	case "forbidden":
		return "Credentials were accepted but required read permission is missing."
	case "not_found":
		return "The configured resource or model was not found."
	case "rate_limited":
		return "The provider rate-limited the connection test."
	case "invalid_response":
		return "The provider returned an invalid or oversized response."
	case "redirect_blocked":
		return "The provider attempted a redirect; credentials were not forwarded."
	case "schema_mismatch":
		return "The database schema does not match this Wardstone build."
	default:
		return "The service could not be reached or is unavailable."
	}
}
