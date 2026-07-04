package portal

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// templates is parsed once at init; html/template's contextual escaping is
// the XSS defense for every dynamic value (logins, error text).
var templates = template.Must(template.ParseFS(templateFS, "templates/*.tmpl"))

type loginData struct {
	OIDCEnabled bool
}

type dashboardData struct {
	Login    string
	CSRF     string
	HasToken bool
	Created  string
	LastUsed string
}

type tokenData struct {
	Token       string
	Regenerated bool
}

type errorData struct {
	Message string
}

// render executes one page template. Rendering to a buffer first keeps a
// template failure from emitting a half page with a 200 status.
func (h *Handler) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		h.log().Error("template render failed", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
