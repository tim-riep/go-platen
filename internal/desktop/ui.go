package desktop

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The settings page is served on the loopback interface under a random
// path, so other users of the computer cannot reach it, and opened in the
// default browser. Changing requests need the X-Platen-Desktop header,
// which a foreign web page cannot send without a CORS preflight, and the
// Host header must name the listener, which defeats DNS rebinding.

//go:embed ui
var uiFiles embed.FS

const uiHeader = "X-Platen-Desktop"

// UI is the settings page of an App.
type UI struct {
	App *App
	URL string // http://127.0.0.1:<port>/<token>/

	host   string
	prefix string
}

// UIInfo is stored in a file so that the tray icon, or a second start of
// the app, can open the page of the running one.
type UIInfo struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

// RunningUI returns the page of a running app from its info file.
func RunningUI(infoPath string) (string, bool) {
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return "", false
	}
	var info UIInfo
	if json.Unmarshal(data, &info) != nil || info.URL == "" {
		return "", false
	}
	c := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, info.URL+"api/state", nil)
	resp, err := c.Do(req)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	return info.URL, resp.StatusCode == http.StatusOK
}

// StartUI listens on a random loopback port and serves the page until ctx
// is canceled. Whoever can read infoPath can open the page.
func StartUI(ctx context.Context, app *App, infoPath string) (*UI, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := make([]byte, 16)
	rand.Read(b)
	u := &UI{App: app, host: ln.Addr().String(), prefix: "/" + hex.EncodeToString(b) + "/"}
	u.URL = "http://" + u.host + u.prefix
	srv := &http.Server{Handler: u.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go func() {
		<-ctx.Done()
		srv.Close()
		os.Remove(infoPath)
	}()
	data, _ := json.Marshal(UIInfo{URL: u.URL, PID: os.Getpid()})
	if err := os.WriteFile(infoPath, data, 0o644); err != nil {
		app.Log.Warn("ui: store address", "err", err)
	}
	return u, nil
}

// Handler serves the page; exported for tests.
func (u *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	p := u.prefix
	mux.HandleFunc("GET "+p+"{$}", u.static("ui/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET "+p+"app.js", u.static("ui/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET "+p+"style.css", u.static("ui/style.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+p+"api/state", func(w http.ResponseWriter, r *http.Request) {
		writeUIJSON(w, http.StatusOK, u.App.State())
	})
	mux.HandleFunc("POST "+p+"api/connect", u.change(func(r *http.Request) error {
		var req struct {
			Address string `json:"address"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
			return err
		}
		return u.App.Connect(r.Context(), strings.TrimSpace(req.Address))
	}))
	mux.HandleFunc("POST "+p+"api/sync", u.change(func(r *http.Request) error { return u.App.Sync(r.Context()) }))
	mux.HandleFunc("POST "+p+"api/disconnect", u.change(func(r *http.Request) error { return u.App.Disconnect(r.Context()) }))
	mux.HandleFunc("POST "+p+"api/retry", u.change(func(r *http.Request) error { u.App.RetryNow(); return nil }))
	mux.HandleFunc("POST "+p+"api/jobs/{id}/discard", u.change(func(r *http.Request) error {
		u.App.Discard(r.PathValue("id"))
		return nil
	}))
	mux.HandleFunc("POST "+p+"api/login", u.change(func(r *http.Request) error {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
			return err
		}
		if u.App.Login == nil {
			return nil
		}
		return u.App.Login.SetEnabled(req.Enabled)
	}))
	return u.guard(mux)
}

func (u *UI) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.host != "" && r.Host != u.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(uiHeader) == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func (u *UI) static(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := uiFiles.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Write(data)
	}
}

// change runs f and answers with the new state, or the error.
func (u *UI) change(f func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f(r); err != nil {
			writeUIJSON(w, http.StatusBadRequest, map[string]string{"error": Describe(err)})
			return
		}
		writeUIJSON(w, http.StatusOK, u.App.State())
	}
}

func writeUIJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
