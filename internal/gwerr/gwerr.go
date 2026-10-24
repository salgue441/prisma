// Package gwerr is the gateway's single error model, rendered per protocol:
// RFC 9457 problem+json for HTTP, the GraphQL error envelope for GraphQL, and
// a Trailers-Only response for gRPC.
package gwerr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"prisma/internal/protocol"
)

// gRPC status codes used by the gateway.
const (
	grpcInvalidArgument   = 3
	grpcDeadlineExceeded  = 4
	grpcPermissionDenied  = 7
	grpcResourceExhausted = 8
	grpcUnimplemented     = 12
	grpcInternal          = 13
	grpcUnavailable       = 14
	grpcUnauthenticated   = 16
)

type Error struct {
	Code       string
	HTTPStatus int
	GRPCCode   int
	Message    string
	// Reason is logged but never sent to clients.
	Reason string
	Header http.Header
	// Details are sent to clients as the problem document's "errors" member.
	Details []Detail
}

// Detail points at one invalid part of a request.
type Detail struct {
	In      string `json:"in"`              // path, query, header, cookie or body
	Field   string `json:"field,omitempty"` // parameter name or JSON pointer
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Reason != "" {
		return e.Code + ": " + e.Message + " (" + e.Reason + ")"
	}
	return e.Code + ": " + e.Message
}

// WithReason attaches an internal-only explanation for logs.
func (e *Error) WithReason(format string, args ...any) *Error {
	cp := *e
	cp.Reason = fmt.Sprintf(format, args...)
	return &cp
}

func newErr(code string, status, grpc int, msg string) *Error {
	return &Error{Code: code, HTTPStatus: status, GRPCCode: grpc, Message: msg}
}

func Unauthenticated(msg string) *Error {
	e := newErr("UNAUTHENTICATED", http.StatusUnauthorized, grpcUnauthenticated, msg)
	e.Header = http.Header{"Www-Authenticate": {`Bearer realm="prisma"`}}
	return e
}
func Forbidden(msg string) *Error {
	return newErr("FORBIDDEN", http.StatusForbidden, grpcPermissionDenied, msg)
}
func NotFound(msg string) *Error {
	return newErr("NOT_FOUND", http.StatusNotFound, grpcUnimplemented, msg)
}
func BadRequest(code, msg string) *Error {
	return newErr(code, http.StatusBadRequest, grpcInvalidArgument, msg)
}
func TooComplex(msg string) *Error {
	return newErr("QUERY_TOO_COMPLEX", http.StatusBadRequest, grpcInvalidArgument, msg)
}
func MethodNotAllowed(msg string) *Error {
	return newErr("METHOD_NOT_ALLOWED", http.StatusMethodNotAllowed, grpcUnimplemented, msg)
}

// ValidationFailed rejects a request that does not match its API contract.
func ValidationFailed(details []Detail) *Error {
	e := newErr("REQUEST_VALIDATION_FAILED", http.StatusBadRequest, grpcInvalidArgument, "request does not match the API specification")
	e.Details = details
	return e
}

func PayloadTooLarge() *Error {
	return newErr("PAYLOAD_TOO_LARGE", http.StatusRequestEntityTooLarge, grpcResourceExhausted, "request body too large")
}
func RateLimited(retryAfter time.Duration) *Error {
	e := newErr("RATE_LIMITED", http.StatusTooManyRequests, grpcResourceExhausted, "rate limit exceeded")
	secs := int((retryAfter + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	e.Header = http.Header{"Retry-After": {strconv.Itoa(secs)}}
	return e
}
func Unavailable(msg string) *Error {
	return newErr("SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, grpcUnavailable, msg)
}
func Timeout() *Error {
	return newErr("GATEWAY_TIMEOUT", http.StatusGatewayTimeout, grpcDeadlineExceeded, "upstream timed out")
}
func BadGateway(msg string) *Error {
	return newErr("BAD_GATEWAY", http.StatusBadGateway, grpcUnavailable, msg)
}
func Internal() *Error {
	return newErr("INTERNAL", http.StatusInternalServerError, grpcInternal, "internal gateway error")
}

// Respond writes the error in the wire format of the given protocol.
func (e *Error) Respond(w http.ResponseWriter, kind protocol.Kind) {
	h := w.Header()
	for k, vs := range e.Header {
		h[k] = vs
	}
	h.Del("Content-Length")
	switch {
	case kind.IsGRPC():
		ct := "application/grpc"
		if kind == protocol.GRPCWeb {
			ct = "application/grpc-web+proto"
		}
		// Trailers-Only: status in the single HEADERS frame, no body. net/http
		// sends HEADERS with END_STREAM when the handler returns without a body.
		h.Set("Content-Type", ct)
		h.Set("Grpc-Status", strconv.Itoa(e.GRPCCode))
		h.Set("Grpc-Message", grpcEncode(e.Message))
		w.WriteHeader(http.StatusOK)
	case kind == protocol.GraphQL:
		h.Set("Content-Type", "application/graphql-response+json")
		w.WriteHeader(e.HTTPStatus)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []any{map[string]any{
				"message":    e.Message,
				"extensions": map[string]any{"code": e.Code},
			}},
		})
	default:
		h.Set("Content-Type", "application/problem+json")
		w.WriteHeader(e.HTTPStatus)
		doc := map[string]any{
			"type":   "about:blank",
			"title":  http.StatusText(e.HTTPStatus),
			"status": e.HTTPStatus,
			"detail": e.Message,
			"code":   e.Code,
		}
		if len(e.Details) > 0 {
			doc["errors"] = e.Details
		}
		_ = json.NewEncoder(w).Encode(doc)
	}
}

// grpcEncode percent-encodes a grpc-message value per the gRPC HTTP/2 spec.
func grpcEncode(s string) string {
	return url.PathEscape(s)
}
