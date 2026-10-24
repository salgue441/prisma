package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"prisma/internal/testutil"
)

func transcodeGateway(t *testing.T, extra string) *httptest.Server {
	t.Helper()
	_, srv := newGateway(t, basePrefix+`
clusters:
  - {name: shop, protocol: h2c, endpoints: [{address: "{{up}}"}]}
routes:
  - name: shop
    match: {pathPrefix: /}
    grpcTranscode:
      descriptorSet: "{{set}}"
`+extra+`
    backends: [{cluster: shop}]
`, map[string]string{"up": testutil.ServeShop(t), "set": filepath.ToSlash(testutil.WriteShopDescriptorSet(t))})
	return srv
}

func doJSON(t *testing.T, srv *httptest.Server, method, path, body string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b)), resp.Header
}

func TestGRPCTranscodingUnary(t *testing.T) {
	srv := transcodeGateway(t, "      autoMapping: true\n")
	cases := []struct {
		name, method, path, body string
		status                   int
		want                     string
	}{
		{"path variable", "GET", "/v1/orders/42", "", 200,
			`{"id":"42","item":"book","qty":1,"status":"SHIPPED","customer":{"name":"Ana","email":"ana@example.com"}}`},
		{"query parameter", "GET", "/v1/orders/42?includeItems=true", "", 200,
			`{"id":"42","item":"book","qty":2,"status":"SHIPPED","customer":{"name":"Ana","email":"ana@example.com"}}`},
		{"proto-name query parameter", "GET", "/v1/orders/42?include_items=true", "", 200,
			`{"id":"42","item":"book","qty":2,"status":"SHIPPED","customer":{"name":"Ana","email":"ana@example.com"}}`},
		{"body field", "POST", "/v1/shops/main/orders", `{"item":"pen","qty":3,"tags":["a","b"]}`, 200,
			`{"id":"new-main","item":"pen","qty":3,"tags":["a","b"]}`},
		{"nested path variable", "PATCH", "/v1/orders/7", `{"item":"lamp","status":"PENDING"}`, 200,
			`{"id":"7","item":"lamp","status":"PENDING"}`},
		{"additional binding, whole body", "PUT", "/v1/orders/8", `{"order":{"item":"desk"}}`, 200,
			`{"id":"8","item":"desk"}`},
		{"custom verb and response_body", "POST", "/v1/orders/9:cancel", "", 200,
			`{"name":"Ana","email":"ana@example.com"}`},
		{"auto mapping", "POST", "/shop.v1.Orders/Ping", `{"id":"p","includeItems":true}`, 200,
			`{"id":"p","includeItems":true}`},
	}
	for _, tc := range cases {
		code, body, hdr := doJSON(t, srv, tc.method, tc.path, tc.body)
		if code != tc.status || !jsonEqual(body, tc.want) {
			t.Errorf("%s: %d %s\nwant %d %s", tc.name, code, body, tc.status, tc.want)
		}
		if ct := hdr.Get("Content-Type"); code == 200 && ct != "application/json" {
			t.Errorf("%s: content-type %q", tc.name, ct)
		}
		if hdr.Get("Grpc-Status") != "" {
			t.Errorf("%s: gRPC headers leaked: %v", tc.name, hdr)
		}
	}

	// gRPC errors become HTTP statuses with a problem document.
	code, body, _ := doJSON(t, srv, "GET", "/v1/orders/404", "")
	var p map[string]any
	_ = json.Unmarshal([]byte(body), &p)
	if code != 404 || p["code"] != "NOT_FOUND" || p["detail"] != "order 404 not found" || p["grpcCode"] != float64(5) {
		t.Errorf("NOT_FOUND: %d %s", code, body)
	}

	// Requests the mapping cannot satisfy never reach the backend.
	for _, bad := range []struct{ method, path, body string }{
		{"GET", "/v1/unknown", ""},
		{"DELETE", "/v1/orders/1", ""},
		{"POST", "/v1/shops/main/orders", `{"nope":1}`},
		{"GET", "/v1/orders/1?includeItems=maybe", ""},
		{"GET", "/v1/orders/1?bogus=1", ""},
		{"POST", "/v1/shops/main/orders", `not json`},
	} {
		if code, body, _ := doJSON(t, srv, bad.method, bad.path, bad.body); code != 404 && code != 400 {
			t.Errorf("%s %s %s: %d %s", bad.method, bad.path, bad.body, code, body)
		}
	}
}

func TestGRPCTranscodingServerStreaming(t *testing.T) {
	srv := transcodeGateway(t, "")
	code, body, _ := doJSON(t, srv, "GET", "/v1/shops/main/orders?pageSize=3&status=PENDING", "")
	var orders []map[string]any
	if err := json.Unmarshal([]byte(body), &orders); err != nil || code != 200 || len(orders) != 3 {
		t.Fatalf("stream: %d %s (%v)", code, body, err)
	}
	if orders[2]["id"] != "main-2" || orders[0]["status"] != "PENDING" {
		t.Errorf("stream contents: %s", body)
	}
	// A failure mid-stream arrives as a final error element.
	code, body, _ = doJSON(t, srv, "GET", "/v1/shops/broken/orders", "")
	var items []map[string]any
	if err := json.Unmarshal([]byte(body), &items); err != nil || code != 200 || len(items) != 2 {
		t.Fatalf("broken stream: %d %s (%v)", code, body, err)
	}
	if e, _ := items[1]["error"].(map[string]any); e["code"] != "UNAVAILABLE" || e["message"] != "backend down" {
		t.Errorf("stream error element: %s", body)
	}
	// gRPC trailers are consumed, not passed on to JSON clients.
	resp, err := http.Get(srv.URL + "/v1/shops/main/orders")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if len(resp.Trailer) > 0 || resp.Header.Get("Trailer") != "" {
		t.Errorf("trailers leaked: %v %v", resp.Trailer, resp.Header)
	}
}

func TestGRPCTranscodingWithMaskingAndOptions(t *testing.T) {
	srv := transcodeGateway(t, `      useProtoNames: true
      emitUnpopulated: true
      discardUnknown: true
    masking:
      rules: [{selector: "$..email", strategy: redact}]
`)
	code, body, _ := doJSON(t, srv, "GET", "/v1/orders/1?bogus=ignored", "")
	var o map[string]any
	_ = json.Unmarshal([]byte(body), &o)
	c, _ := o["customer"].(map[string]any)
	if code != 200 || c["email"] != "***" {
		t.Errorf("masking after transcoding: %d %s", code, body)
	}
	if _, ok := o["tags"]; !ok {
		t.Errorf("emitUnpopulated: %s", body)
	}
	code, body, _ = doJSON(t, srv, "POST", "/v1/shops/x/orders", `{"item":"a","unknown_field":1}`)
	if code != 200 || !strings.Contains(body, `"id":"new-x"`) {
		t.Errorf("discardUnknown / useProtoNames: %d %s", code, body)
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
