package librarian

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func FuzzPathValidation(f *testing.F) {
	for _, seed := range []string{
		"/v1/attempts/aaaaaaaa", "/v1/archives/arm-reference/adopt", "/v1/library", "/healthz/startup",
		"/v1/attempts/ABC", "/v1/attempts/a%2Fb", "/v1/archives/../x/adopt", "/v1/archives/%2e%2e/x", "/v1//library", "/", "",
	} {
		f.Add(seed)
	}
	h := newTestHandler(f)
	f.Fuzz(func(t *testing.T, p string) {
		if validPath(p) {
			for seg := range strings.SplitSeq(p, "/") {
				plain, err := url.PathUnescape(seg)
				if err != nil || plain == ".." || plain == "." || strings.Contains(plain, "/") {
					t.Fatalf("validPath accepted %q with segment %q", p, seg)
				}
			}
		}
		r, err := http.NewRequest(http.MethodGet, "http://librarian"+p, http.NoBody)
		if err != nil {
			return
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	})
}
