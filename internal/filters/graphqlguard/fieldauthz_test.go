package graphqlguard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
)

const testSchema = `
type Query { me: User, users(first: Int): [User!]!, node(id: ID!): Node }
type Mutation { deleteUser(id: ID!): Boolean }
interface Node { id: ID! }
type User implements Node { id: ID!, name: String, email: String, ssn: String, billing: Billing }
type Billing { card: String, plan: String }
`

func newGuard(t *testing.T, rules ...config.FieldAuthz) *Filter {
	t.Helper()
	f, err := New(&config.GraphQL{Schema: testSchema, FieldAuthz: rules}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func post(f *Filter, scopes []string, body string) (*pipeline.Context, pipeline.Responder) {
	r := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	c := &pipeline.Context{Request: r}
	if scopes != nil {
		c.Principal = &pipeline.Principal{ID: "u", Scopes: scopes}
	}
	return c, f.OnRequest(c)
}

func forwarded(t *testing.T, c *pipeline.Context) string {
	t.Helper()
	b, _ := io.ReadAll(c.Request.Body)
	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatalf("forwarded body %q: %v", b, err)
	}
	return req.Query
}

func TestFieldAuthzReject(t *testing.T) {
	f := newGuard(t,
		config.FieldAuthz{Coordinate: "User.email", Scopes: []string{"pii:read"}},
		config.FieldAuthz{Coordinate: "Mutation.deleteUser", Scopes: []string{"admin"}},
	)
	for _, q := range []string{
		`{ me { name email } }`,
		`{ me { contact: email } }`,                                 // alias
		`{ users(first: 2) { ...U } } fragment U on User { email }`, // fragment
		`{ node(id: "1") { ... on User { email } } }`,               // inline fragment via interface
		`mutation { deleteUser(id: "1") }`,
	} {
		body, _ := json.Marshal(map[string]string{"query": q})
		_, res := post(f, []string{"profile"}, string(body))
		if e, ok := res.(*gwerr.Error); !ok || e.HTTPStatus != 403 {
			t.Errorf("%s: want 403, got %v", q, res)
		}
	}
	body, _ := json.Marshal(map[string]string{"query": `{ me { name email } }`})
	if _, res := post(f, []string{"pii:read"}, string(body)); res != nil {
		t.Errorf("principal with scope was denied: %v", res)
	}
}

func TestFieldAuthzStrip(t *testing.T) {
	f := newGuard(t,
		config.FieldAuthz{Coordinate: "User.ssn", Scopes: []string{"pii:read"}, OnDeny: "strip"},
		config.FieldAuthz{Coordinate: "Billing.*", Scopes: []string{"billing"}, OnDeny: "strip"},
	)
	body, _ := json.Marshal(map[string]any{
		"query":     `query Q($n: Int) { users(first: $n) { name tax: ssn billing { card plan } } }`,
		"variables": map[string]any{"n": 3},
	})
	c, res := post(f, []string{}, string(body))
	if res != nil {
		t.Fatalf("strip must not reject: %v", res)
	}
	q := forwarded(t, c)
	if strings.Contains(q, "ssn") || strings.Contains(q, "card") || strings.Contains(q, "plan") {
		t.Errorf("denied fields still forwarded: %s", q)
	}
	if !strings.Contains(q, "name") || !strings.Contains(q, "__typename") {
		t.Errorf("allowed fields must stay and emptied selections stay valid: %s", q)
	}
	if c.Request.ContentLength != int64(len(mustRead(t, c))) {
		t.Error("Content-Length does not match the rewritten body")
	}

	// GET requests are rewritten in the URL.
	r := httptest.NewRequest(http.MethodGet, "/graphql?query="+url.QueryEscape(`{ me { ssn name } }`), nil)
	gc := &pipeline.Context{Request: r}
	if res := f.OnRequest(gc); res != nil {
		t.Fatal(res)
	}
	if q := gc.Request.URL.Query().Get("query"); strings.Contains(q, "ssn") {
		t.Errorf("GET query not rewritten: %s", q)
	}
}

func mustRead(t *testing.T, c *pipeline.Context) []byte {
	b, err := c.Request.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(b)
	return out
}

func TestSchemaValidation(t *testing.T) {
	f := newGuard(t)
	_, res := post(f, nil, `{"query":"{ me { password } }"}`)
	if e, ok := res.(*gwerr.Error); !ok || e.Code != "GRAPHQL_VALIDATION_FAILED" {
		t.Errorf("unknown field must fail validation, got %v", res)
	}
}

func TestBadRulesFailAtLoad(t *testing.T) {
	for _, coord := range []string{"Ghost.x", "User.password"} {
		if _, err := New(&config.GraphQL{Schema: testSchema, FieldAuthz: []config.FieldAuthz{{Coordinate: coord, Scopes: []string{"a"}}}}, 1<<20); err == nil {
			t.Errorf("rule %s should fail against the schema", coord)
		}
	}
}
