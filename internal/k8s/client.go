// Package k8s is a small Kubernetes API client: authenticated REST calls and
// list+watch informers. It avoids client-go to keep the gateway lean.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Options selects the API server and credentials. Empty fields default to
// in-cluster service account access.
type Options struct {
	APIServer string
	TokenFile string
	CAFile    string
}

type Client struct {
	base      string
	tokenFile string
	http      *http.Client
}

func NewClient(o Options) (*Client, error) {
	c := &Client{base: strings.TrimSuffix(o.APIServer, "/"), tokenFile: o.TokenFile}
	inCluster := o.APIServer == ""
	if inCluster {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("kubernetes: apiServer not set and KUBERNETES_SERVICE_HOST is empty (not running in a pod?)")
		}
		c.base = "https://" + net.JoinHostPort(host, port)
		if c.tokenFile == "" {
			c.tokenFile = serviceAccountDir + "/token"
		}
	}
	caFile := o.CAFile
	if caFile == "" && inCluster {
		caFile = serviceAccountDir + "/ca.crt"
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("kubernetes: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tc.RootCAs = pool
	}
	// No client timeout: watches are long-lived; callers bound requests.
	c.http = &http.Client{Transport: &http.Transport{TLSClientConfig: tc, Proxy: nil}}
	return c, nil
}

// InClusterNamespace returns the pod's namespace, if running in a pod.
func InClusterNamespace() (string, error) {
	b, err := os.ReadFile(serviceAccountDir + "/namespace")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Code int
	URL  string
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kubernetes API %s: %d %s", e.URL, e.Code, strings.TrimSpace(e.Body))
}

// Do sends an authenticated request; non-2xx responses become *StatusError.
func (c *Client) Do(ctx context.Context, method, path, contentType string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.tokenFile != "" {
		// Re-read every time: projected service-account tokens rotate.
		tok, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, URL: path, Body: string(b)}
	}
	return resp, nil
}

// MergePatchStatus applies a JSON merge patch to an object's status subresource.
func (c *Client) MergePatchStatus(ctx context.Context, objectPath string, patch []byte) error {
	resp, err := c.Do(ctx, http.MethodPatch, objectPath+"/status", "application/merge-patch+json", patch)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
