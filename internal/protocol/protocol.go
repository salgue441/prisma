// Package protocol classifies inbound requests by application protocol.
package protocol

import (
	"net/http"
	"strings"
)

type Kind uint8

const (
	HTTP Kind = iota
	GraphQL
	GRPC
	GRPCWeb
	WebSocket
)

func (k Kind) String() string {
	switch k {
	case GraphQL:
		return "graphql"
	case GRPC:
		return "grpc"
	case GRPCWeb:
		return "grpc-web"
	case WebSocket:
		return "websocket"
	default:
		return "http"
	}
}

// IsGRPC reports whether errors must be signalled with grpc-status.
func (k Kind) IsGRPC() bool { return k == GRPC || k == GRPCWeb }

// IsStreaming reports whether the request is long-lived and must not be bound
// by the default request timeout or body size limit.
func (k Kind) IsStreaming() bool { return k == GRPC || k == GRPCWeb || k == WebSocket }

// Detect classifies r. routeType is the route's declared type and is
// authoritative; content sniffing is the fallback.
func Detect(r *http.Request, routeType string) Kind {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "application/grpc-web"):
		return GRPCWeb
	case strings.HasPrefix(ct, "application/grpc"):
		return GRPC
	case isWebSocket(r):
		return WebSocket
	}
	switch routeType {
	case "graphql":
		return GraphQL
	case "grpc":
		return GRPC
	}
	if strings.HasPrefix(ct, "application/graphql") {
		return GraphQL
	}
	return HTTP
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
