package redirect

import (
	"net/http/httptest"
	"testing"

	"prisma/internal/config"
	"prisma/internal/pipeline"
)

func TestRewritePrefix(t *testing.T) {
	cases := []struct{ strip, replace, in, want string }{
		{"/foo", "/", "/foo/bar", "/bar"},
		{"/foo", "/", "/foo", "/"},
		{"/foo", "/v2", "/foo/bar", "/v2/bar"},
		{"/foo", "/v2/", "/foo/bar", "/v2/bar"},
		{"/foo", "", "/foo/bar", "/bar"},
		{"/api/v1", "", "/api/v1/orders/42", "/orders/42"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "http://x"+tc.in, nil)
		NewRewrite(&config.Rewrite{StripPrefix: tc.strip, ReplacePrefix: tc.replace}).OnRequest(&pipeline.Context{Request: r})
		if r.URL.Path != tc.want {
			t.Errorf("strip %q replace %q on %q = %q, want %q", tc.strip, tc.replace, tc.in, r.URL.Path, tc.want)
		}
	}
}
