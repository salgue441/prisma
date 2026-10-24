package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	jwksRefreshInterval = 10 * time.Minute
	// jwksMinRefetch rate-limits refetches triggered by unknown key IDs so a
	// flood of forged tokens cannot hammer the identity provider.
	jwksMinRefetch = 30 * time.Second
)

// Registry shares JWKS caches across config snapshots so a reload does not
// force every key set to be refetched.
type Registry struct {
	mu     sync.Mutex
	caches map[string]*jwks
	client *http.Client
}

func NewRegistry() *Registry {
	return &Registry{caches: map[string]*jwks{}, client: &http.Client{Timeout: 5 * time.Second}}
}

func (r *Registry) get(uri string) *jwks {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.caches[uri]; ok {
		return c
	}
	c := &jwks{uri: uri, client: r.client}
	r.caches[uri] = c
	go c.refreshLoop()
	return c
}

type jwks struct {
	uri    string
	client *http.Client

	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
	fetchMu   sync.Mutex
}

func (j *jwks) refreshLoop() {
	if err := j.fetch(); err != nil {
		slog.Warn("jwks fetch failed", "uri", j.uri, "err", err)
	}
	t := time.NewTicker(jwksRefreshInterval)
	defer t.Stop()
	for range t.C {
		if err := j.fetch(); err != nil {
			slog.Warn("jwks refresh failed", "uri", j.uri, "err", err)
		}
	}
}

// key returns the key for kid, refetching once if it is unknown (key rotation).
func (j *jwks) key(kid string) (crypto.PublicKey, error) {
	j.mu.RLock()
	k, ok := j.lookup(kid)
	fetchedAt := j.fetchedAt
	j.mu.RUnlock()
	if ok {
		return k, nil
	}
	if time.Since(fetchedAt) > jwksMinRefetch {
		if err := j.fetch(); err != nil {
			return nil, err
		}
		j.mu.RLock()
		k, ok = j.lookup(kid)
		j.mu.RUnlock()
		if ok {
			return k, nil
		}
	}
	return nil, fmt.Errorf("unknown key id %q", kid)
}

// lookup must be called with j.mu held. An empty kid is accepted only when the
// set has exactly one key.
func (j *jwks) lookup(kid string) (crypto.PublicKey, bool) {
	if kid == "" && len(j.keys) == 1 {
		for _, k := range j.keys {
			return k, true
		}
	}
	k, ok := j.keys[kid]
	return k, ok
}

func (j *jwks) fetch() error {
	j.fetchMu.Lock()
	defer j.fetchMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.uri, nil)
	if err != nil {
		return err
	}
	resp, err := j.client.Do(req)
	if err != nil {
		j.markAttempt()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		j.markAttempt()
		return fmt.Errorf("jwks: unexpected status %d", resp.StatusCode)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		j.markAttempt()
		return fmt.Errorf("jwks: decode: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pk, err := k.publicKey()
		if err != nil {
			slog.Warn("jwks: skipping key", "kid", k.Kid, "err", err)
			continue
		}
		keys[k.Kid] = pk
	}
	j.mu.Lock()
	j.keys, j.fetchedAt = keys, time.Now()
	j.mu.Unlock()
	return nil
}

func (j *jwks) markAttempt() {
	j.mu.Lock()
	j.fetchedAt = time.Now()
	j.mu.Unlock()
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	dec := base64.RawURLEncoding.DecodeString
	switch k.Kty {
	case "RSA":
		n, err := dec(k.N)
		if err != nil {
			return nil, err
		}
		e, err := dec(k.E)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := dec(k.X)
		if err != nil {
			return nil, err
		}
		y, err := dec(k.Y)
		if err != nil {
			return nil, err
		}
		pk := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(pk.X, pk.Y) {
			return nil, errors.New("point not on curve")
		}
		return pk, nil
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := dec(k.X)
		if err != nil {
			return nil, err
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, errors.New("invalid ed25519 key size")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}
