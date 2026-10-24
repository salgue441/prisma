// Package gatewayapi translates Kubernetes Gateway API resources (Gateway,
// HTTPRoute, GRPCRoute, ReferenceGrant) and Prisma's PrismaPolicy into
// gateway configuration, and computes the status conditions to report.
package gatewayapi

import "time"

const (
	Group       = "gateway.networking.k8s.io"
	PrismaGroup = "prisma.dev"

	KindGateway   = "Gateway"
	KindHTTPRoute = "HTTPRoute"
	KindGRPCRoute = "GRPCRoute"
	KindTLSRoute  = "TLSRoute"
	KindTCPRoute  = "TCPRoute"
	KindService   = "Service"
	KindSecret    = "Secret"
)

// The structs below decode only the fields the translator uses.

type objectMeta struct {
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	Generation        int64     `json:"generation"`
	CreationTimestamp time.Time `json:"creationTimestamp"`
}

type gatewayClass struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ControllerName string `json:"controllerName"`
	} `json:"spec"`
}

type gateway struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		GatewayClassName string     `json:"gatewayClassName"`
		Listeners        []listener `json:"listeners"`
	} `json:"spec"`
}

type listener struct {
	Name     string  `json:"name"`
	Hostname *string `json:"hostname"`
	Port     int     `json:"port"`
	Protocol string  `json:"protocol"`
	TLS      *struct {
		Mode            *string     `json:"mode"`
		CertificateRefs []objectRef `json:"certificateRefs"`
	} `json:"tls"`
	AllowedRoutes *struct {
		Namespaces *struct {
			From *string `json:"from"`
		} `json:"namespaces"`
		Kinds []struct {
			Group *string `json:"group"`
			Kind  string  `json:"kind"`
		} `json:"kinds"`
	} `json:"allowedRoutes"`
}

type objectRef struct {
	Group     *string `json:"group"`
	Kind      *string `json:"kind"`
	Name      string  `json:"name"`
	Namespace *string `json:"namespace"`
}

// parentRef omits unset fields: the API server rejects explicit nulls when
// it is written back in route status.
type parentRef struct {
	Group       *string `json:"group,omitempty"`
	Kind        *string `json:"kind,omitempty"`
	Namespace   *string `json:"namespace,omitempty"`
	Name        string  `json:"name"`
	SectionName *string `json:"sectionName,omitempty"`
	Port        *int    `json:"port,omitempty"`
}

type backendRef struct {
	Group     *string `json:"group"`
	Kind      *string `json:"kind"`
	Name      string  `json:"name"`
	Namespace *string `json:"namespace"`
	Port      *int    `json:"port"`
	Weight    *int    `json:"weight"`
}

type headerModifier struct {
	Set    []nameValue `json:"set"`
	Add    []nameValue `json:"add"`
	Remove []string    `json:"remove"`
}

type nameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type pathModifier struct {
	Type               string  `json:"type"`
	ReplaceFullPath    *string `json:"replaceFullPath"`
	ReplacePrefixMatch *string `json:"replacePrefixMatch"`
}

type routeFilter struct {
	Type                   string          `json:"type"`
	RequestHeaderModifier  *headerModifier `json:"requestHeaderModifier"`
	ResponseHeaderModifier *headerModifier `json:"responseHeaderModifier"`
	RequestRedirect        *struct {
		Scheme     *string       `json:"scheme"`
		Hostname   *string       `json:"hostname"`
		Path       *pathModifier `json:"path"`
		Port       *int          `json:"port"`
		StatusCode *int          `json:"statusCode"`
	} `json:"requestRedirect"`
	URLRewrite *struct {
		Hostname *string       `json:"hostname"`
		Path     *pathModifier `json:"path"`
	} `json:"urlRewrite"`
	RequestMirror *struct {
		BackendRef backendRef `json:"backendRef"`
		Percent    *int       `json:"percent"`
	} `json:"requestMirror"`
}

type typedMatch struct {
	Type  *string `json:"type"`
	Name  string  `json:"name"`
	Value string  `json:"value"`
}

type httpRoute struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []parentRef `json:"parentRefs"`
		Hostnames  []string    `json:"hostnames"`
		Rules      []struct {
			Matches []struct {
				Path *struct {
					Type  *string `json:"type"`
					Value *string `json:"value"`
				} `json:"path"`
				Headers     []typedMatch `json:"headers"`
				QueryParams []typedMatch `json:"queryParams"`
				Method      *string      `json:"method"`
			} `json:"matches"`
			Filters     []routeFilter `json:"filters"`
			BackendRefs []backendRef  `json:"backendRefs"`
			Timeouts    *struct {
				Request *string `json:"request"`
			} `json:"timeouts"`
		} `json:"rules"`
	} `json:"spec"`
}

type grpcRoute struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []parentRef `json:"parentRefs"`
		Hostnames  []string    `json:"hostnames"`
		Rules      []struct {
			Matches []struct {
				Method *struct {
					Type    *string `json:"type"`
					Service *string `json:"service"`
					Method  *string `json:"method"`
				} `json:"method"`
				Headers []typedMatch `json:"headers"`
			} `json:"matches"`
			Filters     []routeFilter `json:"filters"`
			BackendRefs []backendRef  `json:"backendRefs"`
		} `json:"rules"`
	} `json:"spec"`
}

type referenceGrant struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		From []struct {
			Group     string `json:"group"`
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
		} `json:"from"`
		To []struct {
			Group string  `json:"group"`
			Kind  string  `json:"kind"`
			Name  *string `json:"name"`
		} `json:"to"`
	} `json:"spec"`
}

type service struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Ports []struct {
			Name        string  `json:"name"`
			Port        int     `json:"port"`
			TargetPort  any     `json:"targetPort"`
			AppProtocol *string `json:"appProtocol"`
		} `json:"ports"`
	} `json:"spec"`
}

type secret struct {
	Metadata objectMeta        `json:"metadata"`
	Type     string            `json:"type"`
	Data     map[string][]byte `json:"data"` // base64 in JSON, decoded by encoding/json
}

type targetRef struct {
	Group       string  `json:"group" yaml:"group"`
	Kind        string  `json:"kind" yaml:"kind"`
	Name        string  `json:"name" yaml:"name"`
	SectionName *string `json:"sectionName,omitempty" yaml:"sectionName,omitempty"`
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// tlsRoute (v1alpha2) routes TLS connections by SNI; tcpRoute (v1alpha2)
// forwards TCP connections. Both only have backendRefs in their rules.
type tlsRoute struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []parentRef `json:"parentRefs"`
		Hostnames  []string    `json:"hostnames"`
		Rules      []l4Rule    `json:"rules"`
	} `json:"spec"`
}

type tcpRoute struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []parentRef `json:"parentRefs"`
		Rules      []l4Rule    `json:"rules"`
	} `json:"spec"`
}

type l4Rule struct {
	BackendRefs []backendRef `json:"backendRefs"`
}
