// Package masking redacts sensitive data in JSON (REST and GraphQL) response
// bodies before it leaves the gateway. Unmaskable responses fail closed by
// default so data never leaks on error paths.
package masking

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

const defaultMaxBuffer = 4 << 20

type rule struct {
	segs     []seg
	strategy string
	keepLast int
}

type detector struct {
	re       *regexp.Regexp
	validate func(string) bool
	strategy string
	keepLast int
}

type Filter struct {
	route       string
	rules       []rule
	detectors   []detector
	unlessScope string
	failOpen    bool
	maxBuf      int64
	hashKey     []byte
	metrics     *telemetry.Metrics
}

var processHashKey = func() []byte {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return k
}()

func New(route string, cfg *config.Masking, m *telemetry.Metrics) (*Filter, error) {
	f := &Filter{
		route:       route,
		unlessScope: cfg.UnlessScope,
		failOpen:    cfg.OnFailure == "open",
		maxBuf:      int64(cfg.MaxBufferBytes),
		hashKey:     []byte(cfg.HashKey),
		metrics:     m,
	}
	if f.maxBuf <= 0 {
		f.maxBuf = defaultMaxBuffer
	}
	if len(f.hashKey) == 0 {
		// Hashes stay consistent within this process only; set hashKey to make
		// them joinable across replicas and restarts.
		f.hashKey = processHashKey
	}
	for _, r := range cfg.Rules {
		keep := r.KeepLast
		if keep == 0 {
			keep = 4
		}
		if r.Selector != "" {
			segs, err := parseSelector(r.Selector)
			if err != nil {
				return nil, err
			}
			f.rules = append(f.rules, rule{segs: segs, strategy: r.Strategy, keepLast: keep})
			continue
		}
		d := detector{strategy: r.Strategy, keepLast: keep}
		switch r.Detector {
		case "email":
			d.re = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
		case "ssn":
			d.re = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
		case "credit_card":
			d.re = regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`)
			d.validate = luhn
		default:
			return nil, fmt.Errorf("unknown detector %q", r.Detector)
		}
		f.detectors = append(f.detectors, d)
	}
	return f, nil
}

func (f *Filter) Name() string { return "masking" }

func (f *Filter) bypass(c *pipeline.Context) bool {
	return f.unlessScope != "" && c.Principal.HasScope(f.unlessScope)
}

// OnRequest asks the upstream for an uncompressed body so it can be inspected.
func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	if !f.bypass(c) {
		c.Request.Header.Del("Accept-Encoding")
	}
	return nil
}

func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	if f.bypass(c) || !isJSON(resp.Header.Get("Content-Type")) ||
		resp.Body == nil || resp.Body == http.NoBody || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return f.fail(resp, nil, "response is "+ce+" encoded")
	}
	if resp.ContentLength > f.maxBuf {
		return f.fail(resp, nil, "response exceeds maxBufferBytes")
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBuf+1))
	if err != nil {
		return f.fail(resp, buf, "read body: "+err.Error())
	}
	if int64(len(buf)) > f.maxBuf {
		return f.fail(resp, buf, "response exceeds maxBufferBytes")
	}
	out, err := f.Mask(buf)
	if err != nil {
		return f.fail(resp, buf, err.Error())
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Content-Md5")
	return nil
}

// fail either passes the original bytes through (open) or rejects (closed).
// consumed holds bytes already read from resp.Body.
func (f *Filter) fail(resp *http.Response, consumed []byte, reason string) error {
	if f.failOpen {
		f.metrics.MaskingFailures.WithLabelValues(f.route, "passed").Inc()
		slog.Warn("masking skipped, passing response unmasked", "route", f.route, "reason", reason)
		if consumed != nil {
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(consumed), resp.Body), resp.Body}
		}
		return nil
	}
	f.metrics.MaskingFailures.WithLabelValues(f.route, "blocked").Inc()
	return gwerr.BadGateway("upstream response could not be processed").WithReason("masking: %s", reason)
}

// Mask applies all rules and detectors to a JSON document.
func (f *Filter) Mask(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("invalid json: trailing data")
	}
	for _, r := range f.rules {
		doc = walk(doc, r.segs, f.action(r.strategy, r.keepLast))
	}
	if len(f.detectors) > 0 {
		doc = f.scan(doc)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

func (f *Filter) action(strategy string, keepLast int) action {
	return func(v any) (any, bool) {
		if strategy == "drop" {
			return nil, false
		}
		s, scalar := scalarString(v)
		if !scalar {
			return "***", true
		}
		return f.apply(strategy, keepLast, s), true
	}
}

func (f *Filter) apply(strategy string, keepLast int, s string) string {
	switch strategy {
	case "partial":
		r := []rune(s)
		if len(r) <= keepLast {
			return strings.Repeat("*", len(r))
		}
		return strings.Repeat("*", len(r)-keepLast) + string(r[len(r)-keepLast:])
	case "hash":
		mac := hmac.New(sha256.New, f.hashKey)
		mac.Write([]byte(s))
		return "hmac:" + hex.EncodeToString(mac.Sum(nil)[:16])
	default:
		return "***"
	}
}

// scan applies detectors to every string value in the document.
func (f *Filter) scan(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = f.scan(child)
		}
	case []any:
		for i := range t {
			t[i] = f.scan(t[i])
		}
	case string:
		for _, d := range f.detectors {
			t = d.re.ReplaceAllStringFunc(t, func(m string) string {
				if d.validate != nil && !d.validate(m) {
					return m
				}
				return f.apply(d.strategy, d.keepLast, m)
			})
		}
		return t
	}
	return v
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "", true
	}
	return "", false
}

func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func luhn(s string) bool {
	sum, n, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		r := rune(s[i])
		if !unicode.IsDigit(r) {
			continue
		}
		d := int(r - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum, n, double = sum+d, n+1, !double
	}
	return n >= 13 && sum%10 == 0
}
