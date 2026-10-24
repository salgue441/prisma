package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// secretRef matches ${env:NAME} and ${file:/path}. Other ${...} forms are left
// untouched because header templates use them at request time.
var secretRef = regexp.MustCompile(`\$\{(env|file):([^}]+)\}`)

// LoadFile reads, expands, parses, defaults and validates a config file.
func LoadFile(path string) (*Config, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return Parse(raw)
}

// Parse returns the config and a short content hash used as its version.
func Parse(raw []byte) (*Config, string, error) {
	expanded, err := expand(raw)
	if err != nil {
		return nil, "", err
	}
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, "", fmt.Errorf("parse config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(expanded)
	return &cfg, hex.EncodeToString(sum[:6]), nil
}

func expand(raw []byte) ([]byte, error) {
	var errs []error
	out := secretRef.ReplaceAllFunc(raw, func(m []byte) []byte {
		parts := secretRef.FindSubmatch(m)
		kind, ref := string(parts[1]), strings.TrimSpace(string(parts[2]))
		switch kind {
		case "env":
			v, ok := os.LookupEnv(ref)
			if !ok {
				errs = append(errs, fmt.Errorf("environment variable %q is not set", ref))
			}
			return []byte(v)
		default:
			b, err := os.ReadFile(ref)
			if err != nil {
				errs = append(errs, fmt.Errorf("read secret file: %w", err))
			}
			return bytes.TrimSpace(b)
		}
	})
	return out, errors.Join(errs...)
}

// ApplyDefaults fills zero values with production-sane defaults.
func (c *Config) ApplyDefaults() {
	if c.APIVersion == "" {
		c.APIVersion = APIVersion
	}
	g := &c.Global
	if g.RequestTimeout == 0 {
		g.RequestTimeout = 30 * time.Second
	}
	if g.MaxBodyBytes == 0 {
		g.MaxBodyBytes = 10 << 20
	}
	if g.DrainTimeout == 0 {
		g.DrainTimeout = 30 * time.Second
	}
	if c.Admin.Address == "" {
		c.Admin.Address = "127.0.0.1:9901"
	}
	if c.Redis != nil {
		if c.Redis.Timeout == 0 {
			c.Redis.Timeout = 50 * time.Millisecond
		}
		if c.Redis.Prefix == "" {
			c.Redis.Prefix = "prisma:"
		}
	}
	for i := range c.Listeners {
		l := &c.Listeners[i]
		if len(l.Protocols) == 0 {
			if l.TLS != nil {
				l.Protocols = []string{"http1", "http2"}
			} else {
				l.Protocols = []string{"http1", "h2c"}
			}
		}
		if l.ReadHeaderTimeout == 0 {
			l.ReadHeaderTimeout = 10 * time.Second
		}
		if l.IdleTimeout == 0 {
			l.IdleTimeout = 120 * time.Second
		}
		if l.MaxHeaderBytes == 0 {
			l.MaxHeaderBytes = 1 << 20
		}
		if l.MaxConcurrentStreams == 0 {
			l.MaxConcurrentStreams = 250
		}
		if l.TLS != nil && l.TLS.MinVersion == "" {
			l.TLS.MinVersion = "1.2"
		}
	}
	for i := range c.Clusters {
		cl := &c.Clusters[i]
		if cl.Protocol == "" {
			cl.Protocol = "http1"
		}
		if cl.ConnectTimeout == 0 {
			cl.ConnectTimeout = 5 * time.Second
		}
		if cl.MaxIdleConnsPerHost == 0 {
			cl.MaxIdleConnsPerHost = 64
		}
		if cl.LoadBalancer.Policy == "" {
			if cl.Protocol == "http1" {
				cl.LoadBalancer.Policy = "round_robin"
			} else {
				cl.LoadBalancer.Policy = "p2c_ewma"
			}
		}
		for j := range cl.Endpoints {
			if cl.Endpoints[j].Weight == 0 {
				cl.Endpoints[j].Weight = 1
			}
		}
		if hc := cl.HealthCheck; hc != nil {
			if hc.Type == "" {
				hc.Type = "http"
			}
			if hc.Type == "http" && hc.Path == "" {
				hc.Path = "/"
			}
			if hc.Interval == 0 {
				hc.Interval = 10 * time.Second
			}
			if hc.Timeout == 0 {
				hc.Timeout = 2 * time.Second
			}
			if hc.HealthyThreshold == 0 {
				hc.HealthyThreshold = 2
			}
			if hc.UnhealthyThreshold == 0 {
				hc.UnhealthyThreshold = 3
			}
		}
		if od := cl.OutlierDetection; od != nil {
			if od.ConsecutiveFailures == 0 {
				od.ConsecutiveFailures = 5
			}
			if od.BaseEjection == 0 {
				od.BaseEjection = 30 * time.Second
			}
			if od.MaxEjectionPercent == 0 {
				od.MaxEjectionPercent = 50
			}
		}
		if cb := cl.CircuitBreaker; cb != nil {
			if cb.ErrorRatio == 0 {
				cb.ErrorRatio = 0.5
			}
			if cb.Window == 0 {
				cb.Window = 10 * time.Second
			}
			if cb.MinRequests == 0 {
				cb.MinRequests = 20
			}
			if cb.OpenDuration == 0 {
				cb.OpenDuration = 30 * time.Second
			}
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Redirect != nil && r.Redirect.Status == 0 {
			r.Redirect.Status = 301
		}
		for j := range r.Backends {
			if r.Backends[j].Weight == 0 {
				r.Backends[j].Weight = 1
			}
		}
		if rt := r.Retries; rt != nil {
			if len(rt.On) == 0 {
				rt.On = []string{"connect-failure", "503"}
			}
			if rt.Budget == 0 {
				rt.Budget = 0.2
			}
			if rt.Backoff == 0 {
				rt.Backoff = 25 * time.Millisecond
			}
		}
	}
}

// Redacted returns a deep copy with secrets blanked, safe to expose on the Admin API.
func (c *Config) Redacted() *Config {
	raw, _ := yaml.Marshal(c)
	var cp Config
	_ = yaml.Unmarshal(raw, &cp)
	const r = "<redacted>"
	if cp.Admin.Token != "" {
		cp.Admin.Token = r
	}
	if cp.Redis != nil && cp.Redis.Password != "" {
		cp.Redis.Password = r
	}
	for i := range cp.Consumers {
		for j := range cp.Consumers[i].APIKeys {
			cp.Consumers[i].APIKeys[j] = r
		}
	}
	redactPolicy := func(p *Policy) {
		if p.AuthN != nil && p.AuthN.JWT != nil && p.AuthN.JWT.Secret != "" {
			p.AuthN.JWT.Secret = r
		}
		if p.Masking != nil && p.Masking.HashKey != "" {
			p.Masking.HashKey = r
		}
	}
	for k, p := range cp.Policies {
		redactPolicy(&p)
		cp.Policies[k] = p
	}
	for i := range cp.Routes {
		redactPolicy(&cp.Routes[i].Policy)
	}
	return &cp
}
