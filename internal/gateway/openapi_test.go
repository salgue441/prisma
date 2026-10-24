package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"prisma/internal/gateway"
)

const ordersSpec = `
openapi: 3.0.3
info: {title: Orders, version: "1.0"}
servers: [{url: "https://api.example.com/v1"}]
components:
  securitySchemes:
    key: {type: apiKey, in: header, name: X-API-Key}
security: [{key: []}]
paths:
  /orders/{id}:
    get:
      parameters:
        - {name: id, in: path, required: true, schema: {type: integer, minimum: 1}}
        - {name: expand, in: query, schema: {type: string, enum: [items, customer]}}
      responses:
        "200":
          description: an order
          content:
            application/json:
              schema:
                type: object
                required: [id, total]
                properties:
                  id: {type: integer}
                  total: {type: number}
  /orders:
    post:
      parameters:
        - {name: Idempotency-Key, in: header, required: true, schema: {type: string, minLength: 8}}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              required: [item, qty]
              properties:
                item: {type: string, minLength: 1}
                qty: {type: integer, minimum: 1, maximum: 100}
      responses:
        "201": {description: created}
`

// openapiGateway serves the spec on /v1 in front of an upstream that echoes
// request bodies and answers GET /orders/{id} with the given JSON.
func openapiGateway(t *testing.T, extra, orderJSON string) (*gateway.Gateway, *httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(b)
			return
		}
		_, _ = io.WriteString(w, orderJSON)
	}))
	t.Cleanup(up.Close)
	spec := filepath.Join(t.TempDir(), "orders.yaml")
	if err := os.WriteFile(spec, []byte(ordersSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	gw, srv := newGateway(t, basePrefix+`
clusters:
  - {name: orders, endpoints: [{address: "{{up}}"}]}
routes:
  - name: orders
    match: {pathPrefix: /v1}
    openapi:
      specFile: "{{spec}}"
`+extra+`
    backends: [{cluster: orders}]
`, map[string]string{"up": addr(up), "spec": filepath.ToSlash(spec)})
	return gw, srv, &seen
}

type problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Errors []struct {
		In, Field, Message string
	} `json:"errors"`
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, hdr map[string]string) (int, problem) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p problem
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &p)
	return resp.StatusCode, p
}

func TestOpenAPIRequestValidation(t *testing.T) {
	_, srv, seen := openapiGateway(t, "", `{"id": 42, "total": 9.5}`)
	key := map[string]string{"Idempotency-Key": "abcdefgh-1"}

	if code, _ := call(t, srv, "GET", "/v1/orders/42?expand=items", "", nil); code != 200 {
		t.Fatalf("valid GET: %d", code)
	}
	if code, p := call(t, srv, "POST", "/v1/orders", `{"item":"book","qty":2}`, key); code != 201 {
		t.Fatalf("valid POST: %d %+v", code, p)
	}
	// The upstream receives the body unchanged.
	if last := (*seen)[len(*seen)-1]; last != `POST /v1/orders {"item":"book","qty":2}` {
		t.Fatalf("upstream saw %q", last)
	}

	cases := []struct {
		name, method, path, body string
		hdr                      map[string]string
		status                   int
		code, in, field          string
	}{
		{"path type", "GET", "/v1/orders/abc", "", nil, 400, "REQUEST_VALIDATION_FAILED", "path", "id"},
		{"path minimum", "GET", "/v1/orders/0", "", nil, 400, "REQUEST_VALIDATION_FAILED", "path", "id"},
		{"query enum", "GET", "/v1/orders/1?expand=everything", "", nil, 400, "REQUEST_VALIDATION_FAILED", "query", "expand"},
		{"missing header", "POST", "/v1/orders", `{"item":"book","qty":2}`, nil, 400, "REQUEST_VALIDATION_FAILED", "header", "Idempotency-Key"},
		{"body maximum", "POST", "/v1/orders", `{"item":"book","qty":500}`, key, 400, "REQUEST_VALIDATION_FAILED", "body", "/qty"},
		{"body extra field", "POST", "/v1/orders", `{"item":"book","qty":1,"admin":true}`, key, 400, "REQUEST_VALIDATION_FAILED", "body", ""},
		{"body not json", "POST", "/v1/orders", `item=book`, key, 400, "REQUEST_VALIDATION_FAILED", "body", ""},
		{"unknown method", "DELETE", "/v1/orders/1", "", nil, 405, "METHOD_NOT_ALLOWED", "", ""},
		{"unknown path", "GET", "/v1/customers", "", nil, 404, "NOT_FOUND", "", ""},
	}
	before := len(*seen)
	for _, tc := range cases {
		code, p := call(t, srv, tc.method, tc.path, tc.body, tc.hdr)
		if code != tc.status || p.Code != tc.code {
			t.Errorf("%s: %d %s, want %d %s (%+v)", tc.name, code, p.Code, tc.status, tc.code, p)
			continue
		}
		if tc.in != "" && (len(p.Errors) == 0 || p.Errors[0].In != tc.in || (tc.field != "" && p.Errors[0].Field != tc.field)) {
			t.Errorf("%s: errors = %+v, want in=%s field=%s", tc.name, p.Errors, tc.in, tc.field)
		}
	}
	if len(*seen) != before {
		t.Errorf("invalid requests reached the upstream: %v", (*seen)[before:])
	}

	// Several problems are reported together.
	_, p := call(t, srv, "POST", "/v1/orders", `{"qty":0}`, key)
	if len(p.Errors) < 2 {
		t.Errorf("want every body problem reported, got %+v", p.Errors)
	}
}

func TestOpenAPIReportModeAndUnknownOperations(t *testing.T) {
	gw, srv, seen := openapiGateway(t, "      mode: report\n      unknownOperations: allow\n", `{"id": 1, "total": 1}`)
	for _, path := range []string{"/v1/orders/abc", "/v1/anything"} {
		if code, _ := call(t, srv, "GET", path, "", nil); code != 200 {
			t.Errorf("report mode %s: %d", path, code)
		}
	}
	if len(*seen) != 2 {
		t.Fatalf("upstream calls = %v", *seen)
	}
	m := gw.Metrics().OpenAPI
	if v := testutil.ToFloat64(m.WithLabelValues("orders", "request", "invalid")); v != 1 {
		t.Errorf("invalid count = %v", v)
	}
	if v := testutil.ToFloat64(m.WithLabelValues("orders", "request", "unknown_operation")); v != 1 {
		t.Errorf("unknown count = %v", v)
	}
}

func TestOpenAPIResponseReport(t *testing.T) {
	// total is a string: the upstream breaks its contract.
	gw, srv, _ := openapiGateway(t, "      responses: report\n", `{"id": 7, "total": "lots"}`)
	resp, err := http.Get(srv.URL + "/v1/orders/7")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != `{"id": 7, "total": "lots"}` {
		t.Fatalf("response changed: %d %s", resp.StatusCode, b)
	}
	if v := testutil.ToFloat64(gw.Metrics().OpenAPI.WithLabelValues("orders", "response", "invalid")); v != 1 {
		t.Errorf("response invalid count = %v", v)
	}
}

func TestOpenAPIBasePathOverride(t *testing.T) {
	// The document's server says /v1; clients call /public/v1.
	_, srv, _ := openapiGateway(t, "      basePath: /public/v1\n", `{"id": 1, "total": 1}`)
	if code, p := call(t, srv, "GET", "/v1/orders/1", "", nil); code != 404 {
		t.Errorf("old prefix: %d %+v", code, p)
	}
}
