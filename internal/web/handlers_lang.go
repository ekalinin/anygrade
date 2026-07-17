package web

import (
	"net/http"
	"net/url"
	"time"

	"github.com/ekalinin/anygrade/internal/i18n"
)

// langCookie holds the per-browser UI language override. It works on the
// anonymous pages (login/register/invite) where there is no user row to attach
// a preference to, so no DB column is needed.
const langCookie = "ag_lang"

// lang resolves the request's UI locale: a valid ag_lang cookie wins, then the
// course default (course.yaml `language:`), then the built-in default.
func (h *Handler) lang(r *http.Request) string {
	if c, err := r.Cookie(langCookie); err == nil && i18n.Supported(c.Value) {
		return c.Value
	}
	if l := h.Course.Get().Resolved.Course.Language; i18n.Supported(l) {
		return l
	}
	return i18n.Default
}

// setLang stores the chosen locale in a cookie and returns to the referring
// page. Public route: it must work before login.
func (h *Handler) setLang(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	lang := r.FormValue("lang")
	if !i18n.Supported(lang) {
		http.Error(w, "unsupported language", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     langCookie,
		Value:    lang,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	http.Redirect(w, r, returnPath(r), http.StatusSeeOther)
}

// returnPath is the same-host Referer path to redirect back to, or "/" when it
// is missing or points elsewhere (never an open redirect).
func returnPath(r *http.Request) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host != r.Host {
		return "/"
	}
	target := u.RequestURI()
	if target == "" || target[0] != '/' || (len(target) > 1 && target[1] == '/') {
		return "/"
	}
	return target
}
