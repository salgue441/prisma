package authn

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"prisma/internal/config"
	"prisma/internal/pipeline"
)

const (
	HeaderSignatureKey       = "X-Signature-Key"
	HeaderSignatureTimestamp = "X-Signature-Timestamp"
	HeaderSignature          = "X-Signature"

	defaultHMACSkew    = 5 * time.Minute
	defaultHMACMaxBody = 10 << 20
)

// Sign computes the v1 signature header value. It is the reference
// implementation for clients; the gateway verifies with the same function.
func Sign(secret []byte, method, requestURI, host string, ts int64, body []byte, signed http.Header, signedHeaders []string) string {
	sum := sha256.Sum256(body)
	var b strings.Builder
	fmt.Fprintf(&b, "v1\n%d\n%s\n%s\n%s\n%x", ts, strings.ToUpper(method), requestURI, strings.ToLower(host), sum)
	for _, h := range signedHeaders {
		fmt.Fprintf(&b, "\n%s:%s", strings.ToLower(h), strings.TrimSpace(signed.Get(h)))
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(b.String()))
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

type hmacKey struct {
	consumer *config.Consumer
	secret   []byte
}

type hmacProvider struct {
	cfg     *config.HMAC
	keys    map[string]hmacKey
	skew    time.Duration
	maxBody int64
	replay  *replayCache
}

func newHMACProvider(cfg *config.HMAC, consumers []config.Consumer, reg *Registry) *hmacProvider {
	p := &hmacProvider{cfg: cfg, keys: map[string]hmacKey{}, skew: cfg.ClockSkew, maxBody: int64(cfg.MaxBodyBytes), replay: reg.replay}
	if p.skew == 0 {
		p.skew = defaultHMACSkew
	}
	if p.maxBody == 0 {
		p.maxBody = defaultHMACMaxBody
	}
	for i := range consumers {
		for id, secret := range consumers[i].HMACKeys {
			p.keys[id] = hmacKey{consumer: &consumers[i], secret: []byte(secret)}
		}
	}
	return p
}

func (p *hmacProvider) authenticate(c *pipeline.Context) (*pipeline.Principal, error) {
	r := c.Request
	sig := r.Header.Get(HeaderSignature)
	if sig == "" {
		return nil, errNoCredentials
	}
	key, ok := p.keys[r.Header.Get(HeaderSignatureKey)]
	if !ok {
		return nil, errors.New("hmac: unknown key id")
	}
	ts, err := strconv.ParseInt(r.Header.Get(HeaderSignatureTimestamp), 10, 64)
	if err != nil {
		return nil, errors.New("hmac: missing or invalid timestamp")
	}
	if d := time.Since(time.Unix(ts, 0)); d > p.skew || d < -p.skew {
		return nil, errors.New("hmac: timestamp outside the accepted window")
	}
	for _, h := range p.cfg.SignedHeaders {
		if r.Header.Get(h) == "" {
			return nil, fmt.Errorf("hmac: signed header %s is missing", h)
		}
	}
	body, err := p.readBody(r)
	if err != nil {
		return nil, err
	}
	want := Sign(key.secret, r.Method, r.URL.RequestURI(), r.Host, ts, body, r.Header, p.cfg.SignedHeaders)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return nil, errors.New("hmac: signature mismatch")
	}
	// Each signature is accepted once per window, per gateway instance.
	if !p.replay.firstUse(sig, time.Unix(ts, 0).Add(p.skew)) {
		return nil, errors.New("hmac: signature replayed")
	}
	cons := key.consumer
	return &pipeline.Principal{ID: cons.Name, Consumer: cons.Name, Method: "hmac", Scopes: slices.Clone(cons.Scopes)}, nil
}

// readBody buffers the body for hashing and restores it for the upstream.
func (p *hmacProvider) readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, p.maxBody+1))
	r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("hmac: read body: %w", err)
	}
	if int64(len(body)) > p.maxBody {
		return nil, errors.New("hmac: body too large to verify")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	return body, nil
}

// replayCache remembers accepted signatures until their timestamp leaves the
// acceptance window, after which the timestamp check rejects them anyway.
type replayCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	next time.Time
}

func newReplayCache() *replayCache { return &replayCache{seen: map[string]time.Time{}} }

func (rc *replayCache) firstUse(sig string, expires time.Time) bool {
	now := time.Now()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if now.After(rc.next) {
		for k, exp := range rc.seen {
			if now.After(exp) {
				delete(rc.seen, k)
			}
		}
		rc.next = now.Add(time.Minute)
	}
	if exp, ok := rc.seen[sig]; ok && now.Before(exp) {
		return false
	}
	rc.seen[sig] = expires
	return true
}
