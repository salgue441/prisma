package authn

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"prisma/internal/config"
	"prisma/internal/pipeline"
)

const (
	defaultIntrospectionTTL         = time.Minute
	defaultIntrospectionNegativeTTL = 10 * time.Second
	defaultIntrospectionTimeout     = 2 * time.Second
	maxIntrospectionCacheEntries    = 50_000
)

// errUnavailable marks failures to reach the identity provider; they are
// reported as 503 rather than 401 so clients don't discard valid tokens.
var errUnavailable = errors.New("identity provider unavailable")

type introspectionProvider struct {
	cfg    *config.Introspection
	client *http.Client
	cache  *introspectionCache
}

func newIntrospectionProvider(cfg *config.Introspection, reg *Registry) *introspectionProvider {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultIntrospectionTimeout
	}
	return &introspectionProvider{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
		cache:  reg.introspectionCache(cfg.Endpoint + "|" + cfg.ClientID),
	}
}

func (p *introspectionProvider) authenticate(c *pipeline.Context) (*pipeline.Principal, error) {
	token, ok := cutPrefixFold(c.Request.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	if !ok || token == "" {
		return nil, errNoCredentials
	}
	key := sha256.Sum256([]byte(token))
	res, err := p.cache.get(key, func() (*introspectionResult, error) {
		return p.introspect(c.Request.Context(), token)
	}, p.ttl)
	if err != nil {
		return nil, err
	}
	if !res.Active {
		return nil, errors.New("introspection: token is not active")
	}
	if p.cfg.Issuer != "" && res.Iss != p.cfg.Issuer {
		return nil, fmt.Errorf("introspection: issuer %q not allowed", res.Iss)
	}
	if len(p.cfg.Audiences) > 0 && !slices.ContainsFunc(res.Aud, func(a string) bool { return slices.Contains(p.cfg.Audiences, a) }) {
		return nil, errors.New("introspection: audience not allowed")
	}
	id := res.Sub
	if id == "" {
		id = res.ClientID
	}
	claims := map[string]any{"sub": res.Sub, "client_id": res.ClientID, "username": res.Username, "iss": res.Iss}
	return &pipeline.Principal{ID: id, Consumer: id, Method: "introspection", Scopes: strings.Fields(res.Scope), Claims: claims}, nil
}

// ttl never caches a token beyond its own expiry.
func (p *introspectionProvider) ttl(r *introspectionResult) time.Duration {
	if !r.Active {
		if p.cfg.NegativeCacheTTL > 0 {
			return p.cfg.NegativeCacheTTL
		}
		return defaultIntrospectionNegativeTTL
	}
	ttl := p.cfg.CacheTTL
	if ttl == 0 {
		ttl = defaultIntrospectionTTL
	}
	if r.Exp > 0 {
		ttl = min(ttl, time.Until(time.Unix(r.Exp, 0)))
	}
	return ttl
}

type introspectionResult struct {
	Active   bool     `json:"active"`
	Scope    string   `json:"scope"`
	ClientID string   `json:"client_id"`
	Username string   `json:"username"`
	Sub      string   `json:"sub"`
	Iss      string   `json:"iss"`
	Exp      int64    `json:"exp"`
	Aud      audience `json:"aud"`
}

// audience accepts the string or array forms allowed by RFC 7662.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func (p *introspectionProvider) introspect(ctx context.Context, token string) (*introspectionResult, error) {
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	// Detach from the client request: a client disconnecting must not poison
	// the shared (singleflight) lookup for everyone else.
	ctx = context.WithoutCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: introspection returned %d", errUnavailable, resp.StatusCode)
	}
	var res introspectionResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("%w: invalid introspection response: %v", errUnavailable, err)
	}
	return &res, nil
}

// introspectionCache caches results by token digest and collapses concurrent
// lookups for the same token into one request.
type introspectionCache struct {
	mu       sync.Mutex
	entries  map[[32]byte]cacheEntry
	inflight map[[32]byte]*call
}

type cacheEntry struct {
	res     *introspectionResult
	expires time.Time
}

type call struct {
	done chan struct{}
	res  *introspectionResult
	err  error
}

func newIntrospectionCache() *introspectionCache {
	return &introspectionCache{entries: map[[32]byte]cacheEntry{}, inflight: map[[32]byte]*call{}}
}

func (c *introspectionCache) get(key [32]byte, fetch func() (*introspectionResult, error), ttl func(*introspectionResult) time.Duration) (*introspectionResult, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		if time.Now().Before(e.expires) {
			c.mu.Unlock()
			return e.res, nil
		}
		delete(c.entries, key)
	}
	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-cl.done
		return cl.res, cl.err
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[key] = cl
	c.mu.Unlock()

	cl.res, cl.err = fetch()

	c.mu.Lock()
	delete(c.inflight, key)
	// Errors (IdP unavailable) are never cached.
	if cl.err == nil {
		if d := ttl(cl.res); d > 0 {
			if len(c.entries) >= maxIntrospectionCacheEntries {
				c.evictLocked()
			}
			c.entries[key] = cacheEntry{res: cl.res, expires: time.Now().Add(d)}
		}
	}
	c.mu.Unlock()
	close(cl.done)
	return cl.res, cl.err
}

// evictLocked drops expired entries, or an arbitrary tenth when none expired.
func (c *introspectionCache) evictLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	n := len(c.entries) / 10
	for k := range c.entries {
		if n <= 0 || len(c.entries) < maxIntrospectionCacheEntries {
			break
		}
		delete(c.entries, k)
		n--
	}
}
