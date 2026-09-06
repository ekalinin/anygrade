package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestLangCookieSecureFlag: the locale cookie is written with the same rule as
// every other cookie the site sets (session.go), so a deployment that
// terminates TLS at a proxy does not keep one cookie a browser may send in the
// clear. It is a preference, not a credential - the point is that there is one
// rule, and that the forgeable header still needs the operator's opt-in.
func TestLangCookieSecureFlag(t *testing.T) {
	for _, tc := range []struct {
		name        string
		forwarded   string
		behindProxy bool
		want        bool
	}{
		{name: "plain http"},
		{name: "forged header without the opt-in", forwarded: "https"},
		{name: "trusted proxy", forwarded: "https", behindProxy: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestSite(t)
			h.BehindProxy = tc.behindProxy

			form := url.Values{"lang": {"ru"}}
			req := httptest.NewRequest(http.MethodPost, "/lang", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			rec := httptest.NewRecorder()
			New(h).ServeHTTP(rec, req)

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("POST /lang: status %d, want 303 (body %q)", rec.Code, rec.Body.String())
			}
			cookies := (&http.Response{Header: rec.Header()}).Cookies()
			if len(cookies) == 0 {
				t.Fatal("no language cookie was set")
			}
			if got := cookies[0].Secure; got != tc.want {
				t.Errorf("Secure = %v, want %v (Set-Cookie: %q)",
					got, tc.want, rec.Header().Get("Set-Cookie"))
			}
		})
	}
}
