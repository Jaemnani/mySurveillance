package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

var liveFileRe = regexp.MustCompile(`^(index\.m3u8|seg\d+\.ts)$`)

const cookieName = "ms_token"

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	// Requiring a JSON content type blocks cross-site form posts (CSRF).
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "요청 형식이 올바르지 않습니다")
		return false
	}
	return true
}

func requestToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

func (a *App) authed(r *http.Request) bool {
	tok := requestToken(r)
	if tok == "" {
		return false
	}
	h := hashToken(tok)
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, t := range a.cfg.Tokens {
		if t.Hash == h && time.Since(t.Created) < tokenTTL {
			return true
		}
	}
	return false
}

func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			writeErr(w, http.StatusUnauthorized, "로그인이 필요합니다")
			return
		}
		next(w, r)
	}
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// routes defines the HTTP API. /api/v1 is the stable contract the mobile
// viewer builds on: login -> status/cameras -> live HLS -> recordings list ->
// range-enabled MP4 playback/download.
func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(sub))

	mux.HandleFunc("POST /api/v1/login", a.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", a.auth(a.handleLogout))
	mux.HandleFunc("POST /api/v1/password", a.auth(a.handlePassword))
	mux.HandleFunc("GET /api/v1/status", a.auth(a.handleStatus))
	mux.HandleFunc("GET /api/v1/config", a.auth(a.handleGetConfig))
	mux.HandleFunc("PUT /api/v1/config", a.auth(a.handlePutConfig))
	mux.HandleFunc("GET /api/v1/devices", a.auth(func(w http.ResponseWriter, r *http.Request) {
		if a.ffmpeg == "" {
			writeErr(w, http.StatusServiceUnavailable, "ffmpeg를 찾을 수 없습니다")
			return
		}
		writeJSON(w, 200, listDevices(a.ffmpeg))
	}))
	mux.HandleFunc("GET /api/v1/discover", a.auth(a.handleDiscover))
	mux.HandleFunc("POST /api/v1/discover/onvif", a.auth(a.handleONVIFStream))
	mux.HandleFunc("GET /api/v1/recordings", a.auth(a.handleRecordings))
	mux.HandleFunc("GET /rec/{cam}/{file}", a.auth(a.handleRecFile))
	mux.HandleFunc("GET /live/{cam}/{file}", a.auth(a.handleLiveFile))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if a.limiter.blocked() {
		writeErr(w, http.StatusTooManyRequests, "로그인 시도가 너무 많습니다. 1분 뒤 다시 시도하세요")
		return
	}
	if !checkPassword(a.Config().PasswordHash, req.Password) {
		a.limiter.fail()
		time.Sleep(time.Second)
		writeErr(w, http.StatusUnauthorized, "비밀번호가 틀렸습니다")
		return
	}
	tok := randomHex(32)
	err := a.Update(func(c *Config) error {
		live := c.Tokens[:0]
		for _, t := range c.Tokens {
			if time.Since(t.Created) < tokenTTL {
				live = append(live, t)
			}
		}
		if len(live) >= maxTokens {
			live = live[len(live)-maxTokens+1:]
		}
		c.Tokens = append(live, Token{Hash: hashToken(tok), Created: time.Now()})
		return nil
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: int(tokenTTL / time.Second)})
	// The token is also returned for native apps, which send it as "Authorization: Bearer".
	writeJSON(w, 200, map[string]any{"token": tok, "expiresInSec": int(tokenTTL / time.Second)})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	h := hashToken(requestToken(r))
	a.Update(func(c *Config) error {
		kept := c.Tokens[:0]
		for _, t := range c.Tokens {
			if t.Hash != h {
				kept = append(kept, t)
			}
		}
		c.Tokens = kept
		return nil
	})
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.New) < 8 {
		writeErr(w, 400, "새 비밀번호는 8자 이상이어야 합니다")
		return
	}
	if a.limiter.blocked() || !checkPassword(a.Config().PasswordHash, req.Current) {
		a.limiter.fail()
		writeErr(w, http.StatusForbidden, "현재 비밀번호가 틀렸습니다")
		return
	}
	keep := hashToken(requestToken(r))
	err := a.Update(func(c *Config) error {
		c.PasswordHash = hashPassword(req.New)
		// sign out every other device
		kept := c.Tokens[:0]
		for _, t := range c.Tokens {
			if t.Hash == keep {
				kept = append(kept, t)
			}
		}
		c.Tokens = kept
		return nil
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.removeInitialPassword()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	states := a.mgr.Status()
	type camOut struct {
		ID      string     `json:"id"`
		Name    string     `json:"name"`
		Type    string     `json:"type"`
		Enabled bool       `json:"enabled"`
		Audio   bool       `json:"audio"`
		State   string     `json:"state"`
		Error   string     `json:"error,omitempty"`
		Since   *time.Time `json:"since,omitempty"`
		LiveURL string     `json:"liveUrl"`
	}
	cams := []camOut{}
	for _, c := range cfg.Cameras {
		o := camOut{ID: c.ID, Name: c.Name, Type: c.Type, Enabled: c.Enabled, Audio: c.Audio,
			State: "disabled", LiveURL: "/live/" + c.ID + "/index.m3u8"}
		if st, ok := states[c.ID]; ok {
			since := st.Since
			o.State, o.Error, o.Since = st.State, st.Error, &since
		} else if c.Enabled && a.ffmpeg == "" {
			o.State, o.Error = "retrying", "ffmpeg를 찾을 수 없습니다"
		}
		cams = append(cams, o)
	}
	writeJSON(w, 200, map[string]any{
		"version":    version,
		"serverTime": time.Now(),
		"ffmpeg":     a.ffmpeg != "",
		"cameras":    cams,
		"storage":    a.store.Status(),
	})
}

type settings struct {
	StorageDir     string   `json:"storageDir"`
	MaxStorageGB   float64  `json:"maxStorageGB"`
	MinFreeGB      float64  `json:"minFreeGB"`
	SegmentMinutes int      `json:"segmentMinutes"`
	Cameras        []Camera `json:"cameras"`
}

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	c := a.Config()
	writeJSON(w, 200, settings{c.StorageDir, c.MaxStorageGB, c.MinFreeGB, c.SegmentMinutes, c.Cameras})
}

func (a *App) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var s settings
	if !readJSON(w, r, &s) {
		return
	}
	var verr error
	err := a.Update(func(c *Config) error {
		c.StorageDir, c.MaxStorageGB, c.MinFreeGB, c.SegmentMinutes = s.StorageDir, s.MaxStorageGB, s.MinFreeGB, s.SegmentMinutes
		c.Cameras = s.Cameras
		if c.Cameras == nil {
			c.Cameras = []Camera{}
		}
		verr = validateSettings(c)
		return verr
	})
	if verr != nil {
		writeErr(w, 400, verr.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.handleGetConfig(w, r)
}

func (a *App) handleRecordings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cam := q.Get("camera")
	if !camIDRe.MatchString(cam) {
		writeErr(w, 400, "camera 값이 필요합니다")
		return
	}
	parse := func(key string) (time.Time, error) {
		v := q.Get(key)
		if v == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339, v)
	}
	from, err1 := parse("from")
	to, err2 := parse("to")
	if err := errors.Join(err1, err2); err != nil {
		writeErr(w, 400, "from/to 는 RFC3339 시각이어야 합니다")
		return
	}
	out := []Recording{}
	for _, rec := range listRecordings(a.Config().StorageDir, cam) {
		if (!from.IsZero() && rec.End.Before(from)) || (!to.IsZero() && !rec.Start.Before(to)) {
			continue
		}
		out = append(out, rec)
	}
	writeJSON(w, 200, map[string]any{"camera": cam, "recordings": out})
}

func (a *App) handleRecFile(w http.ResponseWriter, r *http.Request) {
	cam, file := r.PathValue("cam"), r.PathValue("file")
	if !camIDRe.MatchString(cam) || !recFileRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+cam+"_"+file+`"`)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	// ServeFile supports Range requests: seeking and resumable downloads.
	http.ServeFile(w, r, filepath.Join(a.Config().StorageDir, recSubdir, cam, file))
}

func (a *App) handleLiveFile(w http.ResponseWriter, r *http.Request) {
	cam, file := r.PathValue("cam"), r.PathValue("file")
	if !camIDRe.MatchString(cam) || !liveFileRe.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasSuffix(file, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	}
	http.ServeFile(w, r, filepath.Join(a.Config().StorageDir, liveSubdir, cam, file))
}
