package theory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cgi"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Server serves the vaults over git smart HTTP and the plugin's API.
type Server struct {
	DataDir string
	Store   *Store
	// HookBin is this program, which git runs as the pre-receive hook (GIT-9).
	HookBin string
	// HookEnv is extra environment for git and the hook.
	HookEnv []string
	// Now is the clock. Tests replace it.
	Now func() time.Time
}

type memberKey struct{}

func memberOf(r *http.Request) *Member { return r.Context().Value(memberKey{}).(*Member) }

// Handler routes every request. Each one needs a member's token first (AUTH-8).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/git/", s.serveGit)
	mux.HandleFunc("GET /api/v1/me", s.me)
	mux.HandleFunc("GET /api/v1/vaults/{vault}/lock", s.vault(s.getLock))
	mux.HandleFunc("POST /api/v1/vaults/{vault}/lock", s.vault(s.takeLock))
	mux.HandleFunc("POST /api/v1/vaults/{vault}/lock/{lockId}/renew", s.vault(s.renewLock))
	mux.HandleFunc("POST /api/v1/vaults/{vault}/lock/{lockId}/release", s.vault(s.releaseLock))
	mux.HandleFunc("GET /api/v1/vaults/{vault}/reports", s.vault(s.reports))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "There is no such request in this API.")
	})
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		if strings.HasPrefix(r.URL.Path, "/git/") {
			// Git sends the token as the Basic auth password. The user name is ignored.
			_, token, _ = r.BasicAuth()
		} else if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			token = strings.TrimSpace(t)
		}
		m, err := s.Store.MemberByToken(token)
		if err != nil {
			log.Printf("check token: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if m == nil {
			if strings.HasPrefix(r.URL.Path, "/git/") {
				w.Header().Set("WWW-Authenticate", `Basic realm="theory-server"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), memberKey{}, m)))
	})
}

// serveGit hands the request to git's own http-backend (GIT-3).
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	repo, _, _ := strings.Cut(rest, "/")
	name, ok := strings.CutSuffix(repo, ".git")
	if !ok || !VaultExists(s.DataDir, name) {
		http.NotFound(w, r)
		return
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		log.Printf("find git: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	m := memberOf(r)
	h := &cgi.Handler{
		Path: gitPath,
		Root: "/git",
		Args: []string{"http-backend"},
		Env: append([]string{
			"GIT_PROJECT_ROOT=" + VaultsDir(s.DataDir),
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=" + m.Email,
			PusherEnv + "=" + m.Email,
			"THEORY_SERVER_BIN=" + s.HookBin,
		}, s.HookEnv...),
		InheritEnv: []string{"PATH", "HOME", "TMPDIR"},
		Logger:     log.Default(),
	}
	h.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError answers with a message for the engineer (API-3).
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "The server failed: "+err.Error())
}

type person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	m := memberOf(r)
	writeJSON(w, http.StatusOK, person{m.Name, m.Email})
}

func (s *Server) vault(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("vault")
		if !VaultExists(s.DataDir, name) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("There is no vault named %s on this server.", name))
			return
		}
		h(w, r, name)
	}
}

func timeString(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (s *Server) getLock(w http.ResponseWriter, r *http.Request, vault string) {
	l, err := s.Store.CurrentLock(vault, s.Now())
	if err != nil {
		s.internalError(w, err)
		return
	}
	if l == nil {
		writeJSON(w, http.StatusOK, map[string]any{"holder": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holder": person{l.Name, l.Email}, "since": timeString(l.Since)})
}

func (s *Server) takeLock(w http.ResponseWriter, r *http.Request, vault string) {
	l, held, err := s.Store.TakeLock(vault, memberOf(r), s.Now())
	if err != nil {
		s.internalError(w, err)
		return
	}
	if held != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  fmt.Sprintf("%s has been compiling since %s. Try again when it's done.", held.Name, timeString(held.Since)),
			"holder": person{held.Name, held.Email},
			"since":  timeString(held.Since),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lockId": l.ID, "expiresAt": timeString(l.ExpiresAt)})
}

func (s *Server) renewLock(w http.ResponseWriter, r *http.Request, vault string) {
	expires, err := s.Store.RenewLock(vault, r.PathValue("lockId"), s.Now())
	if errors.Is(err, ErrLockGone) {
		writeError(w, http.StatusGone, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"expiresAt": timeString(expires)})
}

type releaseRequest struct {
	Marker *string         `json:"marker"`
	Report json.RawMessage `json:"report"`
}

func (s *Server) releaseLock(w http.ResponseWriter, r *http.Request, vault string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "The release couldn't be read: "+err.Error())
		return
	}
	var req releaseRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "The release isn't valid JSON: "+err.Error())
			return
		}
	}
	var report []byte
	if len(req.Report) > 0 && string(req.Report) != "null" {
		// Stored unchanged (SHR-1).
		report = []byte(req.Report)
	}
	path := VaultPath(s.DataDir, vault)
	err = s.Store.ReleaseLock(vault, r.PathValue("lockId"), s.Now(), report, func() (func(), error) {
		if req.Marker == nil || *req.Marker == "" {
			return nil, nil
		}
		return MoveMarker(path, *req.Marker)
	})
	var markerErr *MarkerError
	switch {
	case errors.Is(err, ErrLockGone):
		writeError(w, http.StatusGone, err.Error())
	case errors.As(err, &markerErr):
		writeError(w, http.StatusConflict, markerErr.Message+" Your lock is still held.")
	case err != nil:
		s.internalError(w, err)
	default:
		var marker any
		if m := Marker(path); m != "" {
			marker = m
		}
		writeJSON(w, http.StatusOK, map[string]any{"marker": marker})
	}
}

type reportJSON struct {
	ID        int64           `json:"id"`
	Member    person          `json:"member"`
	CreatedAt string          `json:"createdAt"`
	Report    json.RawMessage `json:"report"`
}

func (s *Server) reports(w http.ResponseWriter, r *http.Request, vault string) {
	var before int64
	if b := r.URL.Query().Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q isn't a report id.", b))
			return
		}
		before = n
	}
	list, err := s.Store.Reports(vault, before)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := make([]reportJSON, 0, len(list))
	for _, rep := range list {
		out = append(out, reportJSON{rep.ID, person{rep.Name, rep.Email}, timeString(rep.CreatedAt), rep.JSON})
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": out})
}
