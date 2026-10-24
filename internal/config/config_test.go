package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExampleConfigIsValid(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "configs", "prisma.yaml")
	t.Setenv("PRISMA_JWT_SECRET", "dev-secret-dev-secret-dev-secret!")
	t.Setenv("PRISMA_DEMO_API_KEY", "demo-key")
	if _, _, err := LoadFile(path); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestDefaultsAndSecrets(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KEY", "from-env")
	cfg, _, err := Parse([]byte(`
apiVersion: prisma/v1
listeners: [{name: l, address: ":8080"}]
clusters: [{name: c, endpoints: [{address: "a:1"}]}]
consumers: [{name: x, apiKeys: ["${env:KEY}", "${file:` + filepath.ToSlash(secretFile) + `}"]}]
routes:
  - name: r
    match: {pathPrefix: /}
    headers: {request: {set: {X-Who: "${consumer}"}}}
    maxBodyBytes: 2MiB
    backends: [{cluster: c}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Consumers[0].APIKeys; got[0] != "from-env" || got[1] != "from-file" {
		t.Errorf("secret expansion: %v", got)
	}
	if v := cfg.Routes[0].Headers.Request.Set["X-Who"]; v != "${consumer}" {
		t.Errorf("request template was expanded at load time: %q", v)
	}
	if cfg.Routes[0].MaxBodyBytes != 2<<20 {
		t.Errorf("maxBodyBytes = %d", cfg.Routes[0].MaxBodyBytes)
	}
	if cfg.Global.RequestTimeout != 30*time.Second || cfg.Clusters[0].LoadBalancer.Policy != "round_robin" {
		t.Errorf("defaults not applied: %+v", cfg.Global)
	}
	if strings.Contains(cfg.Redacted().Consumers[0].APIKeys[0], "from-env") {
		t.Error("Redacted leaked an API key")
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	_, _, err := Parse([]byte(`
apiVersion: prisma/v1
listeners: [{name: l, address: "nope", protocols: [http2]}]
clusters: [{name: c, protocol: h2c, endpoints: [{address: "a"}], loadBalancer: {policy: magic}}]
routes:
  - name: r
    match: {path: /a, pathPrefix: /b}
    use: [missing]
    authn: {jwt: {algorithms: [none]}}
    rateLimit: [{key: [bogus], limit: 0, period: 1s, backend: redis}]
    masking: {rules: [{selector: "a.b", strategy: shred}]}
    backends: [{cluster: ghost}]
  - name: loop-a
    match: {path: /x}
    redirect: {path: /y}
  - name: loop-b
    match: {path: /y}
    redirect: {path: /x}
`))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{
		"invalid address", "http2 requires tls", "must be host:port", "unknown load balancer",
		"only one of path", "unknown policy", "exactly one of jwksUri or secret", "algorithm none",
		"limit and period must be positive", "unknown key descriptor", "requires a top-level redis",
		"selector must start with $", "unknown strategy", "unknown cluster", "redirect loop",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing error %q", want)
		}
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	_, _, err := Parse([]byte("apiVersion: prisma/v1\nlistners: []\n"))
	if err == nil || !strings.Contains(err.Error(), "listners") {
		t.Fatalf("typo should be rejected, got %v", err)
	}
}

func TestConsulTokenRedacted(t *testing.T) {
	cfg, _, err := Parse([]byte(`
apiVersion: prisma/v1
listeners: [{name: public, address: ":8080"}]
clusters:
  - name: orders
    discovery: {type: consul, consul: {service: orders, token: s3cret}}
routes: [{name: r, match: {pathPrefix: /}, backends: [{cluster: orders}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Redacted().Clusters[0].Discovery.Consul.Token; got != "<redacted>" {
		t.Errorf("token = %q", got)
	}
	if cfg.Clusters[0].Discovery.Consul.Token != "s3cret" {
		t.Error("redaction modified the live config")
	}
	if _, _, err := Parse([]byte(`
apiVersion: prisma/v1
listeners: [{name: public, address: ":8080"}]
clusters: [{name: orders, discovery: {type: consul}}]
routes: [{name: r, match: {pathPrefix: /}, backends: [{cluster: orders}]}]
`)); err == nil || !strings.Contains(err.Error(), "consul.service") {
		t.Errorf("missing service: %v", err)
	}
}
