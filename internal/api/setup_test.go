package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/readiness"
)

func TestSetupEndpointsRequireOperatorToken(t *testing.T) {
	t.Parallel()
	setup := &fakeSetupService{}
	server := newSetupTestServer(t, setup)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/admin/setup", nil),
		httptest.NewRequest(http.MethodPost, "/v1/admin/setup/connectors/model/test", nil),
	} {
		request.Header.Set("Authorization", "Bearer webhook-secret")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", request.URL.Path, response.Code)
		}
	}
	if setup.statusCalls != 0 || setup.probeCalls != 0 {
		t.Fatal("unauthorized request reached setup service")
	}
}

func TestSetupStatusAndProbe(t *testing.T) {
	t.Parallel()
	setup := &fakeSetupService{status: admin.SetupStatus{State: admin.SetupNotTested, Components: []admin.SetupComponent{{Name: "model", State: admin.SetupNotTested}}}, component: admin.SetupComponent{Name: "model", State: admin.SetupReady}}
	server := newSetupTestServer(t, setup)
	request := operatorRequest(http.MethodGet, "/v1/admin/setup", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status response = %d headers=%v", response.Code, response.Header())
	}
	var status admin.SetupStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil || status.State != admin.SetupNotTested {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	request = operatorRequest(http.MethodPost, "/v1/admin/setup/connectors/model/test", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || setup.name != "model" {
		t.Fatalf("probe status=%d name=%q body=%s", response.Code, setup.name, response.Body.String())
	}
}

func TestSetupProbeErrorsAreStable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"unknown":      {readiness.ErrNotFound, http.StatusNotFound},
		"unconfigured": {readiness.ErrNotConfigured, http.StatusConflict},
		"static":       {readiness.ErrNotProbeable, http.StatusConflict},
		"running":      {readiness.ErrProbeRunning, http.StatusConflict},
		"throttled":    {readiness.ErrProbeThrottled, http.StatusTooManyRequests},
		"internal":     {errors.New("secret upstream response"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newSetupTestServer(t, &fakeSetupService{err: tc.err})
			request := operatorRequest(http.MethodPost, "/v1/admin/setup/connectors/"+name+"/test", nil)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != tc.want || strings.Contains(response.Body.String(), "secret upstream response") {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestDisabledIntakeCannotAuthorizeEmptySecret(t *testing.T) {
	t.Parallel()
	server, err := NewServer(nil, fakeReader{}, "", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/tickets/jira", nil)
	request.Header.Set("Authorization", "Bearer ")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if authorized(request, "") {
		t.Fatal("empty configured secret authorized a request")
	}
}

func newSetupTestServer(t *testing.T, setup SetupService) *Server {
	t.Helper()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret", WithSetup(setup))
	if err != nil {
		t.Fatal(err)
	}
	return server
}

type fakeSetupService struct {
	status      admin.SetupStatus
	component   admin.SetupComponent
	err         error
	name        string
	statusCalls int
	probeCalls  int
}

func (f *fakeSetupService) Status(context.Context) (admin.SetupStatus, error) {
	f.statusCalls++
	return f.status, f.err
}

func (f *fakeSetupService) Probe(_ context.Context, name string) (admin.SetupComponent, error) {
	f.probeCalls++
	f.name = name
	return f.component, f.err
}
