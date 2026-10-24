package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxBackoff   = 30 * time.Second
	watchTimeout = 5 * time.Minute
)

// Object is a raw API object with its identifying metadata decoded.
type Object struct {
	Namespace  string
	Name       string
	Generation int64
	Raw        json.RawMessage
}

// Key is "namespace/name" (or "name" for cluster-scoped objects).
func (o Object) Key() string {
	if o.Namespace == "" {
		return o.Name
	}
	return o.Namespace + "/" + o.Name
}

type meta struct {
	Metadata struct {
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		Generation      int64  `json:"generation"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
}

// Informer keeps an up-to-date local copy of a collection using list+watch
// and calls OnChange (with a snapshot) after the initial list and after
// every change. It relists after watch expiry (410) and backs off on errors,
// keeping the last known state meanwhile.
type Informer struct {
	Client *Client
	// Path is the collection path, e.g. /apis/gateway.networking.k8s.io/v1/httproutes.
	Path string
	// Query adds selectors (labelSelector, fieldSelector).
	Query    url.Values
	OnChange func(map[string]Object)
	// Optional marks APIs that may not be installed (e.g. CRDs): a 404 is
	// treated as an empty collection and retried later.
	Optional bool

	mu     sync.Mutex
	store  map[string]Object
	synced chan struct{}
	once   sync.Once
}

// Synced is closed after the first successful list.
func (i *Informer) Synced() <-chan struct{} {
	i.init()
	return i.synced
}

func (i *Informer) init() {
	i.once.Do(func() { i.synced = make(chan struct{}) })
}

// Snapshot returns a copy of the current objects.
func (i *Informer) Snapshot() map[string]Object {
	i.mu.Lock()
	defer i.mu.Unlock()
	return maps.Clone(i.store)
}

func (i *Informer) Run(ctx context.Context) {
	i.init()
	backoff := time.Second
	for ctx.Err() == nil {
		rv, err := i.list(ctx)
		if err == nil {
			backoff = time.Second
			if err = i.watch(ctx, rv); err == nil {
				continue // watch ended cleanly (timeout or 410): relist now
			}
		}
		if ctx.Err() != nil {
			return
		}
		wait := backoff
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound && i.Optional {
			wait = maxBackoff // API not installed (yet): poll slowly
		} else {
			slog.Warn("kubernetes watch error; keeping last known state", "path", i.Path, "err", err)
			backoff = min(backoff*2, maxBackoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (i *Informer) url(extra url.Values) string {
	q := url.Values{}
	maps.Copy(q, i.Query)
	maps.Copy(q, extra)
	if len(q) == 0 {
		return i.Path
	}
	return i.Path + "?" + q.Encode()
}

func (i *Informer) list(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := i.Client.Do(ctx, http.MethodGet, i.url(nil), "", nil)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound && i.Optional {
			i.replace(map[string]Object{})
		}
		return "", err
	}
	defer resp.Body.Close()
	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", fmt.Errorf("decode list %s: %w", i.Path, err)
	}
	store := map[string]Object{}
	for _, raw := range list.Items {
		o, err := decode(raw)
		if err != nil {
			return "", err
		}
		store[o.Key()] = o
	}
	i.replace(store)
	return list.Metadata.ResourceVersion, nil
}

func (i *Informer) replace(store map[string]Object) {
	i.mu.Lock()
	i.store = store
	snap := maps.Clone(store)
	i.mu.Unlock()
	select {
	case <-i.synced:
	default:
		close(i.synced)
	}
	if i.OnChange != nil {
		i.OnChange(snap)
	}
}

func decode(raw json.RawMessage) (Object, error) {
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return Object{}, fmt.Errorf("decode object: %w", err)
	}
	return Object{Namespace: m.Metadata.Namespace, Name: m.Metadata.Name, Generation: m.Metadata.Generation, Raw: raw}, nil
}

func (i *Informer) watch(ctx context.Context, rv string) error {
	ctx, cancel := context.WithTimeout(ctx, watchTimeout)
	defer cancel()
	resp, err := i.Client.Do(ctx, http.MethodGet, i.url(url.Values{
		"watch": {"1"}, "resourceVersion": {rv}, "allowWatchBookmarks": {"true"},
		"timeoutSeconds": {strconv.Itoa(int(watchTimeout.Seconds()) - 10)},
	}), "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return fmt.Errorf("decode watch event: %w", err)
		}
		switch ev.Type {
		case "ADDED", "MODIFIED", "DELETED":
			o, err := decode(ev.Object)
			if err != nil {
				return err
			}
			i.mu.Lock()
			if ev.Type == "DELETED" {
				delete(i.store, o.Key())
			} else {
				i.store[o.Key()] = o
			}
			snap := maps.Clone(i.store)
			i.mu.Unlock()
			if i.OnChange != nil {
				i.OnChange(snap)
			}
		case "BOOKMARK":
		case "ERROR":
			var st struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(ev.Object, &st)
			if st.Code == http.StatusGone {
				return nil // history expired: relist
			}
			return fmt.Errorf("watch error: %s", strings.TrimSpace(string(ev.Object)))
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
