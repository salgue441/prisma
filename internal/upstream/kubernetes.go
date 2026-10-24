package upstream

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"prisma/internal/config"
)

const (
	saDir            = "/var/run/secrets/kubernetes.io/serviceaccount"
	k8sMaxBackoff    = 30 * time.Second
	k8sWatchTimeout  = 5 * time.Minute
	serviceNameLabel = "kubernetes.io/service-name"
)

// k8sWatcher keeps a cluster's endpoints in sync with the EndpointSlices of
// a Kubernetes Service using list+watch against the API server directly.
type k8sWatcher struct {
	c         *Cluster
	cfg       config.KubernetesDiscovery
	port      int
	base      string
	tokenFile string
	client    *http.Client
	slices    map[string]endpointSlice
}

type endpointSlice struct {
	Metadata struct {
		Name            string `json:"name"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Endpoints []struct {
		Addresses  []string `json:"addresses"`
		Conditions struct {
			Ready       *bool `json:"ready"`
			Terminating *bool `json:"terminating"`
		} `json:"conditions"`
	} `json:"endpoints"`
	Ports []struct {
		Name *string `json:"name"`
		Port *int    `json:"port"`
	} `json:"ports"`
}

func newK8sWatcher(c *Cluster) (*k8sWatcher, error) {
	d := c.Config.Discovery
	kc := *d.Kubernetes
	w := &k8sWatcher{c: c, cfg: kc, port: d.Port, slices: map[string]endpointSlice{}}
	if kc.Namespace == "" {
		b, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("kubernetes discovery: namespace not set and not running in a pod: %w", err)
		}
		w.cfg.Namespace = strings.TrimSpace(string(b))
	}
	w.base = kc.APIServer
	if w.base == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("kubernetes discovery: apiServer not set and KUBERNETES_SERVICE_HOST is empty")
		}
		w.base = "https://" + net.JoinHostPort(host, port)
	}
	w.tokenFile = kc.TokenFile
	if w.tokenFile == "" && kc.APIServer == "" {
		w.tokenFile = saDir + "/token"
	}
	caFile := kc.CAFile
	if caFile == "" && kc.APIServer == "" {
		caFile = saDir + "/ca.crt"
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("kubernetes discovery: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tc.RootCAs = pool
	}
	// No client timeout: watches are long-lived; requests carry their own.
	w.client = &http.Client{Transport: &http.Transport{TLSClientConfig: tc, Proxy: nil}}
	return w, nil
}

// run lists then watches until ctx ends, relisting after errors or when the
// watch expires (410 Gone). The last known endpoints keep serving meanwhile.
func (w *k8sWatcher) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		rv, err := w.list(ctx)
		if err == nil {
			backoff = time.Second
			err = w.watch(ctx, rv)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("kubernetes discovery error; keeping last known endpoints",
				"cluster", w.c.Name, "service", w.cfg.Namespace+"/"+w.cfg.Service, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, k8sMaxBackoff)
		}
	}
}

func (w *k8sWatcher) url(extra url.Values) string {
	q := url.Values{"labelSelector": {serviceNameLabel + "=" + w.cfg.Service}}
	for k, v := range extra {
		q[k] = v
	}
	return fmt.Sprintf("%s/apis/discovery.k8s.io/v1/namespaces/%s/endpointslices?%s",
		strings.TrimSuffix(w.base, "/"), url.PathEscape(w.cfg.Namespace), q.Encode())
}

func (w *k8sWatcher) do(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if w.tokenFile != "" {
		// Re-read every time: projected service-account tokens rotate.
		tok, err := os.ReadFile(w.tokenFile)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("kubernetes API %s: %s", u, resp.Status)
	}
	return resp, nil
}

func (w *k8sWatcher) list(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := w.do(ctx, w.url(nil))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []endpointSlice `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return "", fmt.Errorf("decode endpointslice list: %w", err)
	}
	w.slices = map[string]endpointSlice{}
	for _, s := range list.Items {
		w.slices[s.Metadata.Name] = s
	}
	w.apply()
	return list.Metadata.ResourceVersion, nil
}

func (w *k8sWatcher) watch(ctx context.Context, rv string) error {
	ctx, cancel := context.WithTimeout(ctx, k8sWatchTimeout)
	defer cancel()
	resp, err := w.do(ctx, w.url(url.Values{
		"watch": {"1"}, "resourceVersion": {rv}, "allowWatchBookmarks": {"true"},
		"timeoutSeconds": {strconv.Itoa(int(k8sWatchTimeout.Seconds()) - 10)},
	}))
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
			var s endpointSlice
			if err := json.Unmarshal(ev.Object, &s); err != nil {
				return fmt.Errorf("decode endpointslice: %w", err)
			}
			if ev.Type == "DELETED" {
				delete(w.slices, s.Metadata.Name)
			} else {
				w.slices[s.Metadata.Name] = s
			}
			w.apply()
		case "BOOKMARK":
		case "ERROR":
			var st struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(ev.Object, &st)
			if st.Code == http.StatusGone {
				return nil // history expired: relist without backoff
			}
			return fmt.Errorf("watch error %s", ev.Object)
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil // server closed the watch (timeout): relist and continue
}

// apply converts the slices into endpoints: ready (or unspecified), not
// terminating, on the selected port.
func (w *k8sWatcher) apply() {
	var eps []config.Endpoint
	for _, s := range w.slices {
		port := w.pickPort(s)
		if port == 0 {
			continue
		}
		for _, e := range s.Endpoints {
			if e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			if e.Conditions.Terminating != nil && *e.Conditions.Terminating {
				continue
			}
			for _, a := range e.Addresses {
				eps = append(eps, config.Endpoint{Address: net.JoinHostPort(a, strconv.Itoa(port)), Weight: 1})
			}
		}
	}
	slices.SortFunc(eps, func(a, b config.Endpoint) int { return cmp.Compare(a.Address, b.Address) })
	eps = slices.CompactFunc(eps, func(a, b config.Endpoint) bool { return a.Address == b.Address })
	if !sameEndpoints(w.c.Endpoints(), eps) {
		w.c.setEndpoints(eps)
		slog.Info("endpoints updated", "cluster", w.c.Name, "endpoints", len(eps), "source", "kubernetes")
	}
}

func (w *k8sWatcher) pickPort(s endpointSlice) int {
	for _, p := range s.Ports {
		if p.Port == nil {
			continue
		}
		name := ""
		if p.Name != nil {
			name = *p.Name
		}
		if w.cfg.PortName != "" && name == w.cfg.PortName {
			return *p.Port
		}
		if w.cfg.PortName == "" && len(s.Ports) == 1 {
			return *p.Port
		}
	}
	if w.cfg.PortName == "" {
		return w.port
	}
	return 0
}
