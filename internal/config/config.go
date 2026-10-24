// Package config defines Prisma's declarative configuration model.
package config

import (
	"time"
)

// APIVersion is the only configuration schema version this build understands.
const APIVersion = "prisma/v1"

type Config struct {
	APIVersion string            `yaml:"apiVersion" json:"apiVersion"`
	Global     Global            `yaml:"global" json:"global"`
	Admin      Admin             `yaml:"admin" json:"admin"`
	Redis      *Redis            `yaml:"redis,omitempty" json:"redis,omitempty"`
	Listeners  []Listener        `yaml:"listeners" json:"listeners"`
	Clusters   []Cluster         `yaml:"clusters" json:"clusters"`
	Policies   map[string]Policy `yaml:"policies,omitempty" json:"policies,omitempty"`
	Consumers  []Consumer        `yaml:"consumers,omitempty" json:"consumers,omitempty"`
	Routes     []Route           `yaml:"routes" json:"routes"`
}

type Global struct {
	RequestTimeout time.Duration `yaml:"requestTimeout" json:"requestTimeout"`
	MaxBodyBytes   ByteSize      `yaml:"maxBodyBytes" json:"maxBodyBytes"`
	TrustedProxies []string      `yaml:"trustedProxies,omitempty" json:"trustedProxies,omitempty"`
	ShutdownDelay  time.Duration `yaml:"shutdownDelay" json:"shutdownDelay"`
	DrainTimeout   time.Duration `yaml:"drainTimeout" json:"drainTimeout"`
	AccessLog      AccessLog     `yaml:"accessLog" json:"accessLog"`
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

type Listener struct {
	Name                 string        `yaml:"name" json:"name"`
	Address              string        `yaml:"address" json:"address"`
	Protocols            []string      `yaml:"protocols" json:"protocols"` // http1, http2, h2c
	TLS                  *ListenerTLS  `yaml:"tls,omitempty" json:"tls,omitempty"`
	ReadHeaderTimeout    time.Duration `yaml:"readHeaderTimeout" json:"readHeaderTimeout"`
	IdleTimeout          time.Duration `yaml:"idleTimeout" json:"idleTimeout"`
	MaxHeaderBytes       ByteSize      `yaml:"maxHeaderBytes" json:"maxHeaderBytes"`
	MaxConcurrentStreams int           `yaml:"maxConcurrentStreams" json:"maxConcurrentStreams"`
}

type ListenerTLS struct {
	MinVersion   string        `yaml:"minVersion" json:"minVersion"`
	Certificates []Certificate `yaml:"certificates" json:"certificates"`
	ClientCAFile string        `yaml:"clientCAFile,omitempty" json:"clientCAFile,omitempty"`
	// ClientAuth: none | request | require
	ClientAuth string `yaml:"clientAuth,omitempty" json:"clientAuth,omitempty"`
}

type Certificate struct {
	CertFile string `yaml:"certFile" json:"certFile"`
	KeyFile  string `yaml:"keyFile" json:"keyFile"`
}

type Cluster struct {
	Name                string            `yaml:"name" json:"name"`
	Protocol            string            `yaml:"protocol" json:"protocol"` // http1 | http2 | h2c
	Endpoints           []Endpoint        `yaml:"endpoints" json:"endpoints"`
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
	AuthN     *AuthN      `yaml:"authn,omitempty" json:"authn,omitempty"`
	AuthZ     *AuthZ      `yaml:"authz,omitempty" json:"authz,omitempty"`
	RateLimit []RateLimit `yaml:"rateLimit,omitempty" json:"rateLimit,omitempty"`
	Masking   *Masking    `yaml:"masking,omitempty" json:"masking,omitempty"`
	Headers   *Headers    `yaml:"headers,omitempty" json:"headers,omitempty"`
	GraphQL   *GraphQL    `yaml:"graphql,omitempty" json:"graphql,omitempty"`
	CORS      *CORS       `yaml:"cors,omitempty" json:"cors,omitempty"`
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
	Mode     string  `yaml:"mode,omitempty" json:"mode,omitempty"` // any (default) | all
	Optional bool    `yaml:"optional" json:"optional"`
	JWT      *JWT    `yaml:"jwt,omitempty" json:"jwt,omitempty"`
	APIKey   *APIKey `yaml:"apiKey,omitempty" json:"apiKey,omitempty"`
	MTLS     *MTLS   `yaml:"mtls,omitempty" json:"mtls,omitempty"`
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
	Scopes  []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
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
}

type Route struct {
	Name      string   `yaml:"name" json:"name"`
	Type      string   `yaml:"type,omitempty" json:"type,omitempty"` // http | graphql | grpc
	Listeners []string `yaml:"listeners,omitempty" json:"listeners,omitempty"`
	Priority  int      `yaml:"priority" json:"priority"`
	Match     Match    `yaml:"match" json:"match"`
	Use       []string `yaml:"use,omitempty" json:"use,omitempty"`

	Policy `yaml:",inline" json:",inline"`

	Redirect     *Redirect      `yaml:"redirect,omitempty" json:"redirect,omitempty"`
	Rewrite      *Rewrite       `yaml:"rewrite,omitempty" json:"rewrite,omitempty"`
	Retries      *Retries       `yaml:"retries,omitempty" json:"retries,omitempty"`
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

type Backend struct {
	Cluster string `yaml:"cluster" json:"cluster"`
	Weight  int    `yaml:"weight" json:"weight"`
}
