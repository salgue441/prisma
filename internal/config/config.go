// Package config defines Prisma's declarative configuration model.
package config

import (
	"time"
)

// APIVersion is the only configuration schema version this build understands.
const APIVersion = "prisma/v1"

type Config struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Global     Global   `yaml:"global" json:"global"`
	Admin      Admin    `yaml:"admin" json:"admin"`
	Redis      *Redis   `yaml:"redis,omitempty" json:"redis,omitempty"`
	Tracing    *Tracing `yaml:"tracing,omitempty" json:"tracing,omitempty"`
	// GatewayAPI turns on the Kubernetes Gateway API controller: Gateways,
	// HTTPRoutes, GRPCRoutes and PrismaPolicies are translated into listeners,
	// routes and policies merged with this file.
	GatewayAPI *GatewayAPI       `yaml:"gatewayAPI,omitempty" json:"gatewayAPI,omitempty"`
	Listeners  []Listener        `yaml:"listeners" json:"listeners"`
	Clusters   []Cluster         `yaml:"clusters" json:"clusters"`
	Policies   map[string]Policy `yaml:"policies,omitempty" json:"policies,omitempty"`
	Consumers  []Consumer        `yaml:"consumers,omitempty" json:"consumers,omitempty"`
	Plugins    []Plugin          `yaml:"plugins,omitempty" json:"plugins,omitempty"`
	Routes     []Route           `yaml:"routes" json:"routes"`
}

type Global struct {
	RequestTimeout time.Duration `yaml:"requestTimeout" json:"requestTimeout"`
	MaxBodyBytes   ByteSize      `yaml:"maxBodyBytes" json:"maxBodyBytes"`
	TrustedProxies []string      `yaml:"trustedProxies,omitempty" json:"trustedProxies,omitempty"`
	ShutdownDelay  time.Duration `yaml:"shutdownDelay" json:"shutdownDelay"`
	DrainTimeout   time.Duration `yaml:"drainTimeout" json:"drainTimeout"`
	// CacheMaxBytes bounds the shared response cache (default 64MiB).
	CacheMaxBytes ByteSize  `yaml:"cacheMaxBytes" json:"cacheMaxBytes"`
	AccessLog     AccessLog `yaml:"accessLog" json:"accessLog"`
	// Use lists policies applied to every route before the route's own policies.
	Use []string `yaml:"use,omitempty" json:"use,omitempty"`
}

type AccessLog struct {
	Disabled bool `yaml:"disabled" json:"disabled"`
}

type Admin struct {
	Address string `yaml:"address" json:"address"`
	Token   string `yaml:"token,omitempty" json:"token,omitempty"`
	Pprof   bool   `yaml:"pprof" json:"pprof"`
}

type Redis struct {
	Address  string        `yaml:"address" json:"address"`
	Password string        `yaml:"password,omitempty" json:"password,omitempty"`
	DB       int           `yaml:"db" json:"db"`
	Timeout  time.Duration `yaml:"timeout" json:"timeout"`
	Prefix   string        `yaml:"prefix" json:"prefix"`
}

// Plugin is a Proxy-Wasm module (ABI 0.2.x), e.g. built with the official Go,
// Rust or C++ SDKs. Routes and policies enable it by name.
type Plugin struct {
	Name string `yaml:"name" json:"name"`
	File string `yaml:"file" json:"file"`
	// Config is passed to the plugin as its plugin configuration: a string
	// as-is, any other YAML value encoded as JSON.
	Config   any `yaml:"config,omitempty" json:"config,omitempty"`
	VMConfig any `yaml:"vmConfig,omitempty" json:"vmConfig,omitempty"`
	// Instances is the number of VMs (default: GOMAXPROCS, at most 8).
	Instances int `yaml:"instances" json:"instances"`
	// Bodies buffers request and response bodies (up to MaxBodyBytes) and
	// runs the body callbacks; otherwise bodies stream past the plugin.
	Bodies       bool     `yaml:"bodies" json:"bodies"`
	MaxBodyBytes ByteSize `yaml:"maxBodyBytes" json:"maxBodyBytes"`
	// FailOpen continues requests when the plugin traps or fails.
	FailOpen bool `yaml:"failOpen" json:"failOpen"`
}

// Tracing exports OpenTelemetry spans over OTLP/gRPC. Changes need a restart.
type Tracing struct {
	Endpoint    string            `yaml:"endpoint" json:"endpoint"` // host:port of an OTLP/gRPC collector
	Insecure    bool              `yaml:"insecure" json:"insecure"`
	SampleRatio float64           `yaml:"sampleRatio" json:"sampleRatio"`
	ServiceName string            `yaml:"serviceName" json:"serviceName"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

type Listener struct {
	Name    string `yaml:"name" json:"name"`
	Address string `yaml:"address" json:"address"`
	// Type: http (default) | tcp | tls-passthrough. tcp proxies raw TCP
	// connections to tcp routes, terminating TLS first when tls is set;
	// tls-passthrough routes TLS connections by SNI without decrypting them.
	Type                 string         `yaml:"type,omitempty" json:"type,omitempty"`
	Protocols            []string       `yaml:"protocols" json:"protocols"` // http1, http2, h2c
	TLS                  *ListenerTLS   `yaml:"tls,omitempty" json:"tls,omitempty"`
	ReadHeaderTimeout    time.Duration  `yaml:"readHeaderTimeout" json:"readHeaderTimeout"`
	IdleTimeout          time.Duration  `yaml:"idleTimeout" json:"idleTimeout"`
	MaxHeaderBytes       ByteSize       `yaml:"maxHeaderBytes" json:"maxHeaderBytes"`
	MaxConcurrentStreams int            `yaml:"maxConcurrentStreams" json:"maxConcurrentStreams"`
	ProxyProtocol        *ProxyProtocol `yaml:"proxyProtocol,omitempty" json:"proxyProtocol,omitempty"`
}

// ProxyProtocol accepts HAProxy PROXY v1/v2 headers from load balancers.
type ProxyProtocol struct {
	// TrustedCIDRs may send headers (empty: every peer). Other peers are
	// served as plain connections and cannot spoof their address.
	TrustedCIDRs []string `yaml:"trustedCIDRs,omitempty" json:"trustedCIDRs,omitempty"`
	// Optional accepts trusted connections without a header.
	Optional      bool          `yaml:"optional" json:"optional"`
	HeaderTimeout time.Duration `yaml:"headerTimeout" json:"headerTimeout"`
}

type ListenerTLS struct {
	MinVersion   string        `yaml:"minVersion" json:"minVersion"`
	Certificates []Certificate `yaml:"certificates" json:"certificates"`
	ClientCAFile string        `yaml:"clientCAFile,omitempty" json:"clientCAFile,omitempty"`
	// ClientAuth: none | request | require
	ClientAuth string `yaml:"clientAuth,omitempty" json:"clientAuth,omitempty"`
}

type Certificate struct {
	CertFile string `yaml:"certFile,omitempty" json:"certFile,omitempty"`
	KeyFile  string `yaml:"keyFile,omitempty" json:"keyFile,omitempty"`
	// CertPEM and KeyPEM carry the pair inline (used for Kubernetes Secrets).
	CertPEM string `yaml:"certPEM,omitempty" json:"certPEM,omitempty"`
	KeyPEM  string `yaml:"keyPEM,omitempty" json:"keyPEM,omitempty"`
}

type GatewayAPI struct {
	// ControllerName selects the GatewayClasses this gateway implements.
	ControllerName string `yaml:"controllerName" json:"controllerName"`
	APIServer      string `yaml:"apiServer,omitempty" json:"apiServer,omitempty"`
	TokenFile      string `yaml:"tokenFile,omitempty" json:"tokenFile,omitempty"`
	CAFile         string `yaml:"caFile,omitempty" json:"caFile,omitempty"`
}

type DirectResponse struct {
	Status      int    `yaml:"status" json:"status"`
	Body        string `yaml:"body,omitempty" json:"body,omitempty"`
	ContentType string `yaml:"contentType,omitempty" json:"contentType,omitempty"`
}

type Cluster struct {
	Name                string            `yaml:"name" json:"name"`
	Protocol            string            `yaml:"protocol" json:"protocol"` // http1 | http2 | h2c
	Endpoints           []Endpoint        `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
	Discovery           *Discovery        `yaml:"discovery,omitempty" json:"discovery,omitempty"`
	TLS                 *UpstreamTLS      `yaml:"tls,omitempty" json:"tls,omitempty"`
	PreserveHost        bool              `yaml:"preserveHost" json:"preserveHost"`
	ConnectTimeout      time.Duration     `yaml:"connectTimeout" json:"connectTimeout"`
	MaxIdleConnsPerHost int               `yaml:"maxIdleConnsPerHost" json:"maxIdleConnsPerHost"`
	LoadBalancer        LoadBalancer      `yaml:"loadBalancer" json:"loadBalancer"`
	HealthCheck         *HealthCheck      `yaml:"healthCheck,omitempty" json:"healthCheck,omitempty"`
	OutlierDetection    *OutlierDetection `yaml:"outlierDetection,omitempty" json:"outlierDetection,omitempty"`
	CircuitBreaker      *CircuitBreaker   `yaml:"circuitBreaker,omitempty" json:"circuitBreaker,omitempty"`
}

type Endpoint struct {
	Address string `yaml:"address" json:"address"`
	Weight  int    `yaml:"weight" json:"weight"`
}

// Discovery resolves endpoints from DNS and refreshes them periodically.
// "dns" uses A/AAAA records with a fixed port (e.g. Kubernetes headless
// services); "srv" uses SRV records, which carry ports and weights.
type Discovery struct {
	Type     string        `yaml:"type" json:"type"` // dns | srv | kubernetes
	Host     string        `yaml:"host,omitempty" json:"host,omitempty"`
	Port     int           `yaml:"port,omitempty" json:"port,omitempty"`
	Interval time.Duration `yaml:"interval" json:"interval"`
	// Kubernetes watches the EndpointSlices of a Service (type kubernetes).
	Kubernetes *KubernetesDiscovery `yaml:"kubernetes,omitempty" json:"kubernetes,omitempty"`
}

// KubernetesDiscovery defaults to in-cluster access with the pod's service
// account; the gateway needs get/list/watch on discovery.k8s.io/endpointslices.
type KubernetesDiscovery struct {
	Service   string `yaml:"service" json:"service"`
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// PortName selects a named Service port; otherwise the only port, or Port.
	PortName  string `yaml:"portName,omitempty" json:"portName,omitempty"`
	APIServer string `yaml:"apiServer,omitempty" json:"apiServer,omitempty"`
	TokenFile string `yaml:"tokenFile,omitempty" json:"tokenFile,omitempty"`
	CAFile    string `yaml:"caFile,omitempty" json:"caFile,omitempty"`
}

type UpstreamTLS struct {
	ServerName         string `yaml:"serverName,omitempty" json:"serverName,omitempty"`
	CAFile             string `yaml:"caFile,omitempty" json:"caFile,omitempty"`
	CertFile           string `yaml:"certFile,omitempty" json:"certFile,omitempty"`
	KeyFile            string `yaml:"keyFile,omitempty" json:"keyFile,omitempty"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify" json:"insecureSkipVerify"`
}

type LoadBalancer struct {
	// Policy: round_robin | weighted_round_robin | least_conn | p2c_ewma | ring_hash
	Policy string `yaml:"policy" json:"policy"`
	// HashOn (ring_hash): ip | header:<name> | cookie:<name>
	HashOn string `yaml:"hashOn,omitempty" json:"hashOn,omitempty"`
}

type HealthCheck struct {
	Type               string        `yaml:"type" json:"type"` // http | grpc | tcp
	Path               string        `yaml:"path,omitempty" json:"path,omitempty"`
	Service            string        `yaml:"service,omitempty" json:"service,omitempty"`
	Interval           time.Duration `yaml:"interval" json:"interval"`
	Timeout            time.Duration `yaml:"timeout" json:"timeout"`
	HealthyThreshold   int           `yaml:"healthyThreshold" json:"healthyThreshold"`
	UnhealthyThreshold int           `yaml:"unhealthyThreshold" json:"unhealthyThreshold"`
}

type OutlierDetection struct {
	ConsecutiveFailures int           `yaml:"consecutiveFailures" json:"consecutiveFailures"`
	BaseEjection        time.Duration `yaml:"baseEjection" json:"baseEjection"`
	MaxEjectionPercent  int           `yaml:"maxEjectionPercent" json:"maxEjectionPercent"`
}

type CircuitBreaker struct {
	ErrorRatio    float64       `yaml:"errorRatio" json:"errorRatio"`
	Window        time.Duration `yaml:"window" json:"window"`
	MinRequests   int           `yaml:"minRequests" json:"minRequests"`
	OpenDuration  time.Duration `yaml:"openDuration" json:"openDuration"`
	MaxConcurrent int           `yaml:"maxConcurrent" json:"maxConcurrent"`
}

// Policy is a reusable bundle of route-level filters. Routes embed the same
// fields inline; route values override policy values.
type Policy struct {
	AuthN       *AuthN       `yaml:"authn,omitempty" json:"authn,omitempty"`
	AuthZ       *AuthZ       `yaml:"authz,omitempty" json:"authz,omitempty"`
	RateLimit   []RateLimit  `yaml:"rateLimit,omitempty" json:"rateLimit,omitempty"`
	Masking     *Masking     `yaml:"masking,omitempty" json:"masking,omitempty"`
	Headers     *Headers     `yaml:"headers,omitempty" json:"headers,omitempty"`
	GraphQL     *GraphQL     `yaml:"graphql,omitempty" json:"graphql,omitempty"`
	ExtAuthz    *ExtAuthz    `yaml:"extAuthz,omitempty" json:"extAuthz,omitempty"`
	CORS        *CORS        `yaml:"cors,omitempty" json:"cors,omitempty"`
	Compression *Compression `yaml:"compression,omitempty" json:"compression,omitempty"`
	Cache       *Cache       `yaml:"cache,omitempty" json:"cache,omitempty"`
	OpenAPI     *OpenAPI     `yaml:"openapi,omitempty" json:"openapi,omitempty"`
	// Plugins run in order; they accumulate across global, policy and route.
	Plugins []string `yaml:"plugins,omitempty" json:"plugins,omitempty"`
}

// OpenAPI validates requests (path, query, headers, cookies and body)
// against an OpenAPI 3 document before they reach the upstream.
type OpenAPI struct {
	// Spec is an inline document (YAML or JSON); SpecFile a path to one.
	Spec     string `yaml:"spec,omitempty" json:"spec,omitempty"`
	SpecFile string `yaml:"specFile,omitempty" json:"specFile,omitempty"`
	// BasePath is the part of request paths before the document's paths,
	// e.g. /api/v1 when clients call /api/v1/orders for the path /orders.
	// Default: the path of the document's first server URL.
	BasePath string `yaml:"basePath,omitempty" json:"basePath,omitempty"`
	// Mode: enforce (default) rejects invalid requests with 400; report
	// only logs and counts them.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// UnknownOperations: reject (default) answers 404/405 for paths and
	// methods the document does not define; allow forwards them unchecked.
	UnknownOperations string `yaml:"unknownOperations,omitempty" json:"unknownOperations,omitempty"`
	// Responses: off (default) | report. report validates upstream JSON
	// responses and logs mismatches; responses are never blocked.
	Responses    string   `yaml:"responses,omitempty" json:"responses,omitempty"`
	MaxBodyBytes ByteSize `yaml:"maxBodyBytes" json:"maxBodyBytes"`
}

// Cache stores GET responses in the gateway's shared in-memory cache,
// honoring Cache-Control (s-maxage, max-age, no-store, private) and Vary.
type Cache struct {
	// DefaultTTL applies when the upstream sends no freshness information.
	// Zero means such responses are not cached.
	DefaultTTL    time.Duration `yaml:"defaultTTL" json:"defaultTTL"`
	MaxEntryBytes ByteSize      `yaml:"maxEntryBytes" json:"maxEntryBytes"`
	// Shared lets different consumers share entries. Leave false whenever
	// responses may differ per caller (masking unlessScope, per-user data).
	Shared bool `yaml:"shared" json:"shared"`
	// KeyHeaders adds request headers to the cache key.
	KeyHeaders []string `yaml:"keyHeaders,omitempty" json:"keyHeaders,omitempty"`
}

type Compression struct {
	// Algorithms in server preference order: zstd, gzip. Default: [zstd, gzip].
	Algorithms []string `yaml:"algorithms,omitempty" json:"algorithms,omitempty"`
	MinBytes   ByteSize `yaml:"minBytes" json:"minBytes"`
	// Types are compressible media types; a trailing "/*" matches a family.
	Types []string `yaml:"types,omitempty" json:"types,omitempty"`
}

// ExtAuthz delegates the allow/deny decision to an external HTTP service,
// compatible with Envoy's HTTP ext_authz contract: the gateway sends the
// request method, path and selected headers (no body) to url+path; a 2xx
// response allows the request, anything else denies it.
type ExtAuthz struct {
	URL     string        `yaml:"url" json:"url"`
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	// FailOpen allows requests when the authorization service is unreachable.
	FailOpen bool `yaml:"failOpen" json:"failOpen"`
	// AllowedHeaders are copied from the client request to the check request.
	// Authorization, Cookie and X-Request-Id are always included.
	AllowedHeaders []string `yaml:"allowedHeaders,omitempty" json:"allowedHeaders,omitempty"`
	// UpstreamHeaders are copied from an allow response onto the upstream request.
	UpstreamHeaders []string `yaml:"upstreamHeaders,omitempty" json:"upstreamHeaders,omitempty"`
	// ClientHeaders are copied from a deny response onto the client response.
	ClientHeaders []string `yaml:"clientHeaders,omitempty" json:"clientHeaders,omitempty"`
}

type CORS struct {
	// AllowOrigins entries are exact origins, "*", or wildcard subdomains
	// such as "https://*.example.com".
	AllowOrigins     []string      `yaml:"allowOrigins" json:"allowOrigins"`
	AllowMethods     []string      `yaml:"allowMethods,omitempty" json:"allowMethods,omitempty"`
	AllowHeaders     []string      `yaml:"allowHeaders,omitempty" json:"allowHeaders,omitempty"`
	ExposeHeaders    []string      `yaml:"exposeHeaders,omitempty" json:"exposeHeaders,omitempty"`
	AllowCredentials bool          `yaml:"allowCredentials" json:"allowCredentials"`
	MaxAge           time.Duration `yaml:"maxAge" json:"maxAge"`
}

type AuthN struct {
	Mode          string         `yaml:"mode,omitempty" json:"mode,omitempty"` // any (default) | all
	Optional      bool           `yaml:"optional" json:"optional"`
	JWT           *JWT           `yaml:"jwt,omitempty" json:"jwt,omitempty"`
	Introspection *Introspection `yaml:"introspection,omitempty" json:"introspection,omitempty"`
	APIKey        *APIKey        `yaml:"apiKey,omitempty" json:"apiKey,omitempty"`
	MTLS          *MTLS          `yaml:"mtls,omitempty" json:"mtls,omitempty"`
	HMAC          *HMAC          `yaml:"hmac,omitempty" json:"hmac,omitempty"`
}

// HMAC verifies signed requests. Clients send X-Signature-Key,
// X-Signature-Timestamp (unix seconds) and X-Signature: v1=<hex>, where the
// HMAC-SHA256 covers "v1\n<ts>\n<METHOD>\n<path?query>\n<host>\n<sha256(body)>"
// followed by "\n<name>:<value>" for each signed header.
type HMAC struct {
	// ClockSkew is the accepted timestamp drift (default 5m).
	ClockSkew time.Duration `yaml:"clockSkew" json:"clockSkew"`
	// SignedHeaders must be present and are covered by the signature.
	SignedHeaders []string `yaml:"signedHeaders,omitempty" json:"signedHeaders,omitempty"`
	MaxBodyBytes  ByteSize `yaml:"maxBodyBytes" json:"maxBodyBytes"`
}

// Introspection validates opaque bearer tokens with an RFC 7662 endpoint.
type Introspection struct {
	Endpoint     string        `yaml:"endpoint" json:"endpoint"`
	ClientID     string        `yaml:"clientId" json:"clientId"`
	ClientSecret string        `yaml:"clientSecret,omitempty" json:"clientSecret,omitempty"`
	Audiences    []string      `yaml:"audiences,omitempty" json:"audiences,omitempty"`
	Issuer       string        `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	CacheTTL     time.Duration `yaml:"cacheTTL" json:"cacheTTL"`
	// NegativeCacheTTL caches inactive tokens so replayed bad tokens don't
	// hammer the identity provider.
	NegativeCacheTTL time.Duration `yaml:"negativeCacheTTL" json:"negativeCacheTTL"`
	Timeout          time.Duration `yaml:"timeout" json:"timeout"`
}

type JWT struct {
	Issuer        string        `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	Audiences     []string      `yaml:"audiences,omitempty" json:"audiences,omitempty"`
	JWKSURI       string        `yaml:"jwksUri,omitempty" json:"jwksUri,omitempty"`
	Secret        string        `yaml:"secret,omitempty" json:"secret,omitempty"`
	Algorithms    []string      `yaml:"algorithms,omitempty" json:"algorithms,omitempty"`
	ClockSkew     time.Duration `yaml:"clockSkew" json:"clockSkew"`
	Header        string        `yaml:"header,omitempty" json:"header,omitempty"`
	ScopeClaim    string        `yaml:"scopeClaim,omitempty" json:"scopeClaim,omitempty"`
	ConsumerClaim string        `yaml:"consumerClaim,omitempty" json:"consumerClaim,omitempty"`
}

type APIKey struct {
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	Query  string `yaml:"query,omitempty" json:"query,omitempty"`
}

type MTLS struct {
	AllowedSubjects []string `yaml:"allowedSubjects,omitempty" json:"allowedSubjects,omitempty"`
}

type Consumer struct {
	Name string `yaml:"name" json:"name"`
	// APIKeys are raw keys or "sha256:<hex>" digests.
	APIKeys []string `yaml:"apiKeys,omitempty" json:"apiKeys,omitempty"`
	// HMACKeys maps signing key IDs to shared secrets.
	HMACKeys map[string]string `yaml:"hmacKeys,omitempty" json:"hmacKeys,omitempty"`
	Scopes   []string          `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

type AuthZ struct {
	Scopes []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	// Methods maps an HTTP method or full gRPC method ("/pkg.Svc/Method") to extra required scopes.
	Methods   map[string][]string `yaml:"methods,omitempty" json:"methods,omitempty"`
	Consumers []string            `yaml:"consumers,omitempty" json:"consumers,omitempty"`
	AllowIPs  []string            `yaml:"allowIPs,omitempty" json:"allowIPs,omitempty"`
	DenyIPs   []string            `yaml:"denyIPs,omitempty" json:"denyIPs,omitempty"`
}

type RateLimit struct {
	Name   string        `yaml:"name,omitempty" json:"name,omitempty"`
	Key    []string      `yaml:"key" json:"key"`
	Limit  int           `yaml:"limit" json:"limit"`
	Period time.Duration `yaml:"period" json:"period"`
	Burst  int           `yaml:"burst" json:"burst"`
	// Backend: local (default) | redis
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	// OnBackendFailure: local (default) | allow | deny
	OnBackendFailure string `yaml:"onBackendFailure,omitempty" json:"onBackendFailure,omitempty"`
	// Cost: "" (1 per request) | graphql (computed query cost)
	Cost string `yaml:"cost,omitempty" json:"cost,omitempty"`
}

type Masking struct {
	Rules          []MaskRule `yaml:"rules" json:"rules"`
	UnlessScope    string     `yaml:"unlessScope,omitempty" json:"unlessScope,omitempty"`
	OnFailure      string     `yaml:"onFailure,omitempty" json:"onFailure,omitempty"` // closed (default) | open
	MaxBufferBytes ByteSize   `yaml:"maxBufferBytes" json:"maxBufferBytes"`
	HashKey        string     `yaml:"hashKey,omitempty" json:"hashKey,omitempty"`
	// DescriptorSet is a FileDescriptorSet (protoc --include_imports
	// --descriptor_set_out=...) used to mask gRPC response messages.
	DescriptorSet string `yaml:"descriptorSet,omitempty" json:"descriptorSet,omitempty"`
}

type MaskRule struct {
	Selector string `yaml:"selector,omitempty" json:"selector,omitempty"` // $.a.b, $..ssn, $.items[*].card
	Detector string `yaml:"detector,omitempty" json:"detector,omitempty"` // email | credit_card | ssn
	Strategy string `yaml:"strategy" json:"strategy"`                     // redact | partial | hash | drop
	KeepLast int    `yaml:"keepLast,omitempty" json:"keepLast,omitempty"`
}

type Headers struct {
	Request  HeaderOps `yaml:"request" json:"request"`
	Response HeaderOps `yaml:"response" json:"response"`
}

type HeaderOps struct {
	Set    map[string]string `yaml:"set,omitempty" json:"set,omitempty"`
	Add    map[string]string `yaml:"add,omitempty" json:"add,omitempty"`
	Remove []string          `yaml:"remove,omitempty" json:"remove,omitempty"`
}

type GraphQL struct {
	MaxDepth           int  `yaml:"maxDepth" json:"maxDepth"`
	MaxAliases         int  `yaml:"maxAliases" json:"maxAliases"`
	MaxRootFields      int  `yaml:"maxRootFields" json:"maxRootFields"`
	MaxCost            int  `yaml:"maxCost" json:"maxCost"`
	MaxBatch           int  `yaml:"maxBatch" json:"maxBatch"`
	BlockIntrospection bool `yaml:"blockIntrospection" json:"blockIntrospection"`
	// Schema (inline SDL) or SchemaFile enables validation against the
	// schema and field-level authorization.
	Schema     string       `yaml:"schema,omitempty" json:"schema,omitempty"`
	SchemaFile string       `yaml:"schemaFile,omitempty" json:"schemaFile,omitempty"`
	FieldAuthz []FieldAuthz `yaml:"fieldAuthz,omitempty" json:"fieldAuthz,omitempty"`
}

// FieldAuthz guards a schema coordinate ("User.email", or "User.*" for every
// field of a type).
type FieldAuthz struct {
	Coordinate string   `yaml:"coordinate" json:"coordinate"`
	Scopes     []string `yaml:"scopes" json:"scopes"`
	// OnDeny: reject (default) fails the operation; strip removes the field
	// from the query and forwards the rest.
	OnDeny string `yaml:"onDeny,omitempty" json:"onDeny,omitempty"`
}

type Route struct {
	Name string `yaml:"name" json:"name"`
	// Type: http (default) | graphql | grpc | tcp. tcp routes attach to tcp
	// and tls-passthrough listeners; they match on SNI (match.hosts) and
	// support backends, authz IP lists and timeout (the idle timeout).
	Type      string   `yaml:"type,omitempty" json:"type,omitempty"`
	Listeners []string `yaml:"listeners,omitempty" json:"listeners,omitempty"`
	Priority  int      `yaml:"priority" json:"priority"`
	Match     Match    `yaml:"match" json:"match"`
	Use       []string `yaml:"use,omitempty" json:"use,omitempty"`

	Policy `yaml:",inline" json:",inline"`

	Redirect *Redirect `yaml:"redirect,omitempty" json:"redirect,omitempty"`
	// DirectResponse answers at the gateway with a fixed status and body.
	DirectResponse *DirectResponse `yaml:"directResponse,omitempty" json:"directResponse,omitempty"`
	Rewrite        *Rewrite        `yaml:"rewrite,omitempty" json:"rewrite,omitempty"`
	Retries        *Retries        `yaml:"retries,omitempty" json:"retries,omitempty"`
	// GRPCWeb translates gRPC-Web requests into native gRPC for the upstream.
	GRPCWeb      bool           `yaml:"grpcWeb" json:"grpcWeb"`
	Mirror       *Mirror        `yaml:"mirror,omitempty" json:"mirror,omitempty"`
	Timeout      *time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxBodyBytes ByteSize       `yaml:"maxBodyBytes" json:"maxBodyBytes"`
	Backends     []Backend      `yaml:"backends,omitempty" json:"backends,omitempty"`
}

type Match struct {
	Hosts       []string          `yaml:"hosts,omitempty" json:"hosts,omitempty"`
	Path        string            `yaml:"path,omitempty" json:"path,omitempty"`
	PathPrefix  string            `yaml:"pathPrefix,omitempty" json:"pathPrefix,omitempty"`
	PathRegex   string            `yaml:"pathRegex,omitempty" json:"pathRegex,omitempty"`
	Methods     []string          `yaml:"methods,omitempty" json:"methods,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	GRPCService string            `yaml:"grpcService,omitempty" json:"grpcService,omitempty"`
	GRPCMethod  string            `yaml:"grpcMethod,omitempty" json:"grpcMethod,omitempty"`
	// QueryParams match exact query parameter values.
	QueryParams map[string]string `yaml:"queryParams,omitempty" json:"queryParams,omitempty"`
}

type Redirect struct {
	Scheme            string `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	Host              string `yaml:"host,omitempty" json:"host,omitempty"`
	Path              string `yaml:"path,omitempty" json:"path,omitempty"`
	PathPrefixReplace string `yaml:"pathPrefixReplace,omitempty" json:"pathPrefixReplace,omitempty"`
	StripQuery        bool   `yaml:"stripQuery" json:"stripQuery"`
	Status            int    `yaml:"status" json:"status"`
}

type Rewrite struct {
	StripPrefix   string `yaml:"stripPrefix,omitempty" json:"stripPrefix,omitempty"`
	ReplacePrefix string `yaml:"replacePrefix,omitempty" json:"replacePrefix,omitempty"`
	PathRegex     string `yaml:"pathRegex,omitempty" json:"pathRegex,omitempty"`
	Replacement   string `yaml:"replacement,omitempty" json:"replacement,omitempty"`
	Host          string `yaml:"host,omitempty" json:"host,omitempty"`
}

type Retries struct {
	Attempts int `yaml:"attempts" json:"attempts"`
	// On: connect-failure | 5xx | 502 | 503 | 504 | unavailable (gRPC status 14)
	On                 []string      `yaml:"on" json:"on"`
	Budget             float64       `yaml:"budget" json:"budget"`
	Backoff            time.Duration `yaml:"backoff" json:"backoff"`
	AllowNonIdempotent bool          `yaml:"allowNonIdempotent" json:"allowNonIdempotent"`
}

// Mirror copies a sample of a route's traffic to a shadow cluster. Shadow
// responses are discarded; the Host header gets a "-shadow" suffix.
type Mirror struct {
	Cluster      string        `yaml:"cluster" json:"cluster"`
	Percent      float64       `yaml:"percent" json:"percent"`
	MaxBodyBytes ByteSize      `yaml:"maxBodyBytes" json:"maxBodyBytes"`
	Timeout      time.Duration `yaml:"timeout" json:"timeout"`
	// MaxConcurrent caps in-flight shadow requests; extra ones are dropped.
	MaxConcurrent int `yaml:"maxConcurrent" json:"maxConcurrent"`
}

type Backend struct {
	Cluster string `yaml:"cluster" json:"cluster"`
	Weight  int    `yaml:"weight" json:"weight"`
}

// Listener types.
const (
	ListenerHTTP           = "http"
	ListenerTCP            = "tcp"
	ListenerTLSPassthrough = "tls-passthrough"
)

// IsStream reports whether the listener proxies connections (tcp,
// tls-passthrough) rather than HTTP requests.
func (l *Listener) IsStream() bool {
	return l.Type == ListenerTCP || l.Type == ListenerTLSPassthrough
}

// RouteTCP is the route type for connection-level (L4) routes.
const RouteTCP = "tcp"
