package transcode

import (
	"reflect"
	"testing"
)

func TestTemplateMatch(t *testing.T) {
	cases := []struct {
		tmpl, path string
		want       map[string]string // nil: no match
	}{
		{"/v1/orders/{id}", "/v1/orders/42", map[string]string{"id": "42"}},
		{"/v1/orders/{id}", "/v1/orders/42/items", nil},
		{"/v1/orders/{id}", "/v1/orders/", nil},
		{"/v1/orders", "/v1/orders", map[string]string{}},
		{"/v1/{name=shelves/*/books/*}", "/v1/shelves/1/books/2", map[string]string{"name": "shelves/1/books/2"}},
		{"/v1/{name=shelves/*/books/*}", "/v1/shelves/1/books", nil},
		{"/v1/{book.shelf}/books/{book.id}", "/v1/s1/books/b%2F2", map[string]string{"book.shelf": "s1", "book.id": "b/2"}},
		{"/v1/files/{path=**}", "/v1/files/a/b/c.txt", map[string]string{"path": "a/b/c.txt"}},
		{"/v1/files/{path=**}", "/v1/files", map[string]string{}},
		{"/v1/orders/{id}:cancel", "/v1/orders/7:cancel", map[string]string{"id": "7"}},
		{"/v1/orders/{id}:cancel", "/v1/orders/7", nil},
		{"/v1/*/items", "/v1/x/items", map[string]string{}},
	}
	for _, tc := range cases {
		tp, err := parseTemplate(tc.tmpl)
		if err != nil {
			t.Fatalf("%s: %v", tc.tmpl, err)
		}
		got, ok := tp.match(tc.path)
		if tc.want == nil {
			if ok {
				t.Errorf("%s matched %s: %v", tc.tmpl, tc.path, got)
			}
			continue
		}
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s on %s = %v %v, want %v", tc.tmpl, tc.path, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"v1/x", "/v1/{id", "/v1/{=x}", "/v1/**/x", "/v1/{a={b}}", "/v1//x"} {
		if _, err := parseTemplate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
