// Package authn establishes the caller's identity. Providers are combined with
// "any" (first success wins) or "all" (every provider must succeed) semantics.
package authn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

// errNoCredentials means the provider found nothing to verify in the request.
var errNoCredentials = errors.New("no credentials")

type provider interface {
	authenticate(c *pipeline.Context) (*pipeline.Principal, error)
}

type Filter struct {
	route     string
	all       bool
	optional  bool
	providers []provider
	metrics   *telemetry.Metrics
}

// Consumers indexes API key digests to consumers.
type Consumers struct {
	byKeyDigest map[string]*config.Consumer
}

func NewConsumers(cs []config.Consumer) *Consumers {
	out := &Consumers{byKeyDigest: map[string]*config.Consumer{}}
	for i := range cs {
		c := &cs[i]
		for _, k := range c.APIKeys {
			out.byKeyDigest[keyDigest(k)] = c
		}
	}
	return out
}

func keyDigest(k string) string {
	if d, ok := strings.CutPrefix(k, "sha256:"); ok {
		return strings.ToLower(d)
	}
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

func New(route string, cfg *config.AuthN, consumers *Consumers, reg *Registry, m *telemetry.Metrics) (*Filter, error) {
	f := &Filter{route: route, all: cfg.Mode == "all", optional: cfg.Optional, metrics: m}
	if cfg.MTLS != nil {
		f.providers = append(f.providers, &mtlsProvider{allowed: cfg.MTLS.AllowedSubjects})
	}
	if cfg.APIKey != nil {
		h := cfg.APIKey.Header
		if h == "" && cfg.APIKey.Query == "" {
			h = "X-API-Key"
		}
		f.providers = append(f.providers, &apiKeyProvider{header: h, query: cfg.APIKey.Query, consumers: consumers})
	}
	if cfg.JWT != nil {
		p, err := newJWTProvider(cfg.JWT, reg)
		if err != nil {
			return nil, err
		}
		f.providers = append(f.providers, p)
	}
	return f, nil
}

func (f *Filter) Name() string { return "authn" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	var principal *pipeline.Principal
	var firstErr error
	for _, p := range f.providers {
		pr, err := p.authenticate(c)
		switch {
		case err == nil:
			if principal == nil {
				principal = pr
			} else {
				principal.Scopes = append(principal.Scopes, pr.Scopes...)
			}
			if !f.all {
				c.Principal = principal
				return nil
			}
		case f.all:
			return f.reject(err)
		case !errors.Is(err, errNoCredentials) && firstErr == nil:
			firstErr = err
		}
	}
	if principal != nil {
		c.Principal = principal
		return nil
	}
	if firstErr != nil {
		return f.reject(firstErr)
	}
	if f.optional {
		return nil
	}
	return f.reject(errNoCredentials)
}

func (f *Filter) reject(err error) pipeline.Responder {
	reason := "invalid_credentials"
	if errors.Is(err, errNoCredentials) {
		reason = "missing_credentials"
	}
	f.metrics.AuthFailures.WithLabelValues(f.route, reason).Inc()
	msg := "authentication required"
	if reason == "invalid_credentials" {
		msg = "invalid credentials"
	}
	return gwerr.Unauthenticated(msg).WithReason("%v", err)
}

type apiKeyProvider struct {
	header, query string
	consumers     *Consumers
}

func (p *apiKeyProvider) authenticate(c *pipeline.Context) (*pipeline.Principal, error) {
	key := ""
	if p.header != "" {
		key = c.Request.Header.Get(p.header)
	}
	if key == "" && p.query != "" {
		key = c.Request.URL.Query().Get(p.query)
	}
	if key == "" {
		return nil, errNoCredentials
	}
	cons, ok := p.consumers.byKeyDigest[keyDigest(key)]
	if !ok {
		return nil, errors.New("unknown api key")
	}
	if p.header != "" {
		// Never forward the credential upstream.
		c.Request.Header.Del(p.header)
	}
	return &pipeline.Principal{ID: cons.Name, Consumer: cons.Name, Method: "apikey", Scopes: slices.Clone(cons.Scopes)}, nil
}

type mtlsProvider struct{ allowed []string }

func (p *mtlsProvider) authenticate(c *pipeline.Context) (*pipeline.Principal, error) {
	cs := c.Request.TLS
	if cs == nil || len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 {
		return nil, errNoCredentials
	}
	cert := cs.PeerCertificates[0]
	ids := []string{cert.Subject.CommonName}
	for _, u := range cert.URIs {
		ids = append(ids, u.String())
	}
	ids = append(ids, cert.DNSNames...)
	if len(p.allowed) > 0 && !slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(p.allowed, id) }) {
		return nil, fmt.Errorf("client certificate subject %q not allowed", ids[0])
	}
	id := ids[0]
	if id == "" && len(ids) > 1 {
		id = ids[1]
	}
	return &pipeline.Principal{ID: id, Consumer: id, Method: "mtls"}, nil
}

type jwtProvider struct {
	cfg     *config.JWT
	header  string
	parser  *jwt.Parser
	keyfunc jwt.Keyfunc
}

func newJWTProvider(cfg *config.JWT, reg *Registry) (*jwtProvider, error) {
	algs := cfg.Algorithms
	if len(algs) == 0 {
		if cfg.Secret != "" {
			algs = []string{"HS256"}
		} else {
			algs = []string{"RS256", "ES256", "EdDSA"}
		}
	}
	opts := []jwt.ParserOption{jwt.WithValidMethods(algs), jwt.WithExpirationRequired(), jwt.WithLeeway(cfg.ClockSkew)}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	p := &jwtProvider{cfg: cfg, header: cfg.Header, parser: jwt.NewParser(opts...)}
	if p.header == "" {
		p.header = "Authorization"
	}
	if cfg.Secret != "" {
		secret := []byte(cfg.Secret)
		p.keyfunc = func(*jwt.Token) (any, error) { return secret, nil }
	} else {
		keys := reg.get(cfg.JWKSURI)
		p.keyfunc = func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			return keys.key(kid)
		}
	}
	return p, nil
}

func (p *jwtProvider) authenticate(c *pipeline.Context) (*pipeline.Principal, error) {
	raw := c.Request.Header.Get(p.header)
	if p.header == "Authorization" {
		var ok bool
		if raw, ok = cutPrefixFold(raw, "Bearer "); !ok {
			return nil, errNoCredentials
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errNoCredentials
	}
	claims := jwt.MapClaims{}
	if _, err := p.parser.ParseWithClaims(raw, claims, p.keyfunc); err != nil {
		return nil, fmt.Errorf("jwt: %w", err)
	}
	if len(p.cfg.Audiences) > 0 {
		aud, _ := claims.GetAudience()
		if !slices.ContainsFunc(aud, func(a string) bool { return slices.Contains(p.cfg.Audiences, a) }) {
			return nil, errors.New("jwt: audience not allowed")
		}
	}
	sub, _ := claims.GetSubject()
	pr := &pipeline.Principal{ID: sub, Consumer: sub, Method: "jwt", Claims: claims, Scopes: scopes(claims, p.cfg.ScopeClaim)}
	if cc := p.cfg.ConsumerClaim; cc != "" {
		if v, ok := claims[cc].(string); ok {
			pr.Consumer = v
		}
	}
	return pr, nil
}

// scopes reads space-delimited "scope" (RFC 8693) or array "scp"/"scopes" claims.
func scopes(claims jwt.MapClaims, claim string) []string {
	names := []string{"scope", "scp", "scopes"}
	if claim != "" {
		names = []string{claim}
	}
	for _, n := range names {
		switch v := claims[n].(type) {
		case string:
			return strings.Fields(v)
		case []any:
			out := make([]string, 0, len(v))
			for _, s := range v {
				if str, ok := s.(string); ok {
					out = append(out, str)
				}
			}
			return out
		}
	}
	return nil
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}
