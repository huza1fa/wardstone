package api

import (
	"crypto/sha256"
	"fmt"
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
	if response.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("unversioned asset must revalidate after a console deployment")
	}
}

func TestAdminHTMLPinsItsEmbeddedAssets(t *testing.T) {
	t.Parallel()
	server, err := NewServer(fakeService{}, fakeReader{}, "webhook-secret", "operator-secret")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	for _, name := range []string{"styles.css", "app.js"} {
		content, err := webAssets.ReadFile("web/" + name)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		path := fmt.Sprintf("assets/%s?v=%x", name, digest[:8])
		if !strings.Contains(page.Body.String(), path) {
			t.Fatalf("HTML does not pin embedded %s", name)
		}
		asset := httptest.NewRecorder()
		handler.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/admin/"+path, nil))
		if asset.Code != http.StatusOK || asset.Body.String() != string(content) {
			t.Fatalf("fingerprinted %s unavailable", name)
		}
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
