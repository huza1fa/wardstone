package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminWebUIIsEmbeddedAndSecurityHardened(t *testing.T) {
	t.Parallel()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "Wardstone Admin") {
		t.Fatal("admin page did not contain expected title")
	}
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %v", response.Header())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}

	request = httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "/v1/admin/overview") {
		t.Fatalf("embedded application asset unavailable: status=%d", response.Code)
	}
}

func TestAdminWebRedirectAndUnknownPath(t *testing.T) {
	t.Parallel()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "/admin/" {
		t.Fatalf("unexpected redirect: status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
	request = httptest.NewRequest(http.MethodGet, "/admin/not-found", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}
