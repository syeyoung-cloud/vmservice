package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/syeyoung-cloud/vmservice/internal/auth"
	"github.com/syeyoung-cloud/vmservice/internal/limit"
	"github.com/syeyoung-cloud/vmservice/internal/web"
)

// testAPI is the API with sign-in against a fake provider (discovery only) and no machines: these
// tests cover the guards in front of the handlers.
func testAPI(t *testing.T) http.Handler {
	t.Helper()
	var issuer string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token",
			"jwks_uri": issuer + "/certs", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(idp.Close)
	issuer = idp.URL
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, make([]byte, 32), 0o600)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := auth.New(context.Background(), auth.Config{Issuer: issuer, ClientID: "vmservice", Origin: "https://vm.example.org",
		GroupsClaim: "groups", UsernameClaim: "preferred_username", KeyFile: key, Lifetime: time.Hour}, log)
	if err != nil {
		t.Fatal(err)
	}
	// The whole server as main wires it: the UI, sign-in and the API on one mux (registering
	// conflicting patterns panics here).
	h, err := web.Handler(fstest.MapFS{"404.html": {Data: []byte("missing")}}, web.Config{}, "https://vm.example.org",
		a.Routes, New(a, nil, nil, "https://vm.example.org", &limit.PerOwner{Max: 8}, log).Routes)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestGuards(t *testing.T) {
	h := testAPI(t)
	for _, c := range []struct {
		method, path string
		headers      map[string]string
		want         int
	}{
		{"GET", "/api/session", nil, http.StatusUnauthorized},
		{"GET", "/api/vms", nil, http.StatusUnauthorized},
		{"GET", "/api/vms/events", nil, http.StatusUnauthorized},
		{"GET", "/api/vms/vm-a/serial", nil, http.StatusUnauthorized},
		{"POST", "/api/vms", nil, http.StatusForbidden}, // no X-Requested-With
		{"POST", "/api/vms", map[string]string{"X-Requested-With": "XMLHttpRequest", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"POST", "/api/vms/vm-a/start", map[string]string{"X-Requested-With": "XMLHttpRequest", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"DELETE", "/api/vms/vm-a", map[string]string{"X-Requested-With": "XMLHttpRequest", "Origin": "https://vm.example.org"}, http.StatusUnauthorized},
		{"POST", "/auth/logout", nil, http.StatusForbidden},
		{"GET", "/api/nothing", nil, http.StatusNotFound},
		{"POST", "/api/nothing", nil, http.StatusNotFound},
		{"POST", "/somewhere", nil, http.StatusMethodNotAllowed},
		{"GET", "/healthz", nil, http.StatusOK},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s %v = %d, want %d", c.method, c.path, c.headers, w.Code, c.want)
		}
	}
}

func TestLoginRedirectsToProviderWithPKCE(t *testing.T) {
	h := testAPI(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/auth/login?next=/machine/?id=vm-a", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d", w.Code)
	}
	loc := w.Header().Get("Location")
	r, _ := http.NewRequest("GET", loc, nil)
	q := r.URL.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" ||
		q.Get("redirect_uri") != "https://vm.example.org/auth/callback" {
		t.Errorf("authorization URL = %s", loc)
	}
	if len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].HttpOnly || !w.Result().Cookies()[0].Secure {
		t.Errorf("login cookie = %+v", w.Result().Cookies())
	}
}
