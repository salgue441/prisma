package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtAuthz(t *testing.T) {
	var seen *http.Request
	authz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		if r.Header.Get("Authorization") == "Bearer admin" {
			w.Header().Set("X-User-Id", "u-42")
			w.Header().Set("X-Secret", "must-not-leak")
			return
		}
		w.Header().Set("X-Deny-Reason", "not-admin")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"admins only"}`)
	}))
	defer authz.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "user="+r.Header.Get("X-User-Id")+" secret="+r.Header.Get("X-Secret"))
	}))
	defer up.Close()

	yml := basePrefix + `
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: closed
    match: {pathPrefix: /admin}
    extAuthz:
      url: "{{authz}}/check"
      allowedHeaders: [X-Tenant]
      upstreamHeaders: [X-User-Id]
      clientHeaders: [X-Deny-Reason]
    backends: [{cluster: up}]
  - name: open
    match: {pathPrefix: /open}
    extAuthz: {url: "http://127.0.0.1:1", failOpen: true, timeout: 200ms}
    backends: [{cluster: up}]
  - name: down
    match: {pathPrefix: /down}
    extAuthz: {url: "http://127.0.0.1:1", timeout: 200ms}
    backends: [{cluster: up}]
`
	_, gw := newGateway(t, yml, map[string]string{"up": addr(up), "authz": authz.URL})

	call := func(path string, hdr map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, gw.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := call("/admin/users?page=2", map[string]string{"Authorization": "Bearer admin", "X-Tenant": "acme", "X-Other": "x"})
	if resp.StatusCode != 200 || body != "user=u-42 secret=" {
		t.Fatalf("allow: %d %q (only allow-listed upstream headers may pass)", resp.StatusCode, body)
	}
	if seen.URL.Path != "/check/admin/users" || seen.URL.RawQuery != "page=2" || seen.Header.Get("X-Tenant") != "acme" ||
		seen.Header.Get("X-Other") != "" || seen.Header.Get("X-Forwarded-Method") != "GET" {
		t.Errorf("check request = %s %v", seen.URL, seen.Header)
	}

	resp, body = call("/admin/users", map[string]string{"Authorization": "Bearer user"})
	if resp.StatusCode != 403 || !strings.Contains(body, "admins only") || resp.Header.Get("X-Deny-Reason") != "not-admin" {
		t.Errorf("deny: %d %q %v", resp.StatusCode, body, resp.Header)
	}

	if resp, _ = call("/down", nil); resp.StatusCode != 503 {
		t.Errorf("fail-closed: status %d, want 503", resp.StatusCode)
	}
	if resp, _ = call("/open", nil); resp.StatusCode != 200 {
		t.Errorf("fail-open: status %d, want 200", resp.StatusCode)
	}
}
