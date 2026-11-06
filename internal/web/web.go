// Package web serves knot's browser UI over HTTP.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HimanshuSardana/knot/internal/cluster"
	"github.com/HimanshuSardana/knot/internal/node"
	"github.com/HimanshuSardana/knot/internal/ops"
)

//go:embed static/*
var static embed.FS

// Server is the HTTP frontend.
type Server struct {
	cl    *cluster.Cluster
	token string

	mu   sync.Mutex
	jobs map[string]*job
}

// job tracks a long transfer so the UI can poll for progress.
type job struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Done    bool   `json:"done"`
	Err     string `json:"err,omitempty"`
	Copied  int64  `json:"copied"`
	Total   int64  `json:"total"`
	Started time.Time
}

// New builds a server. An empty token generates one.
func New(cl *cluster.Cluster, token string) *Server {
	if token == "" {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		token = hex.EncodeToString(buf)
	}
	return &Server{cl: cl, token: token, jobs: map[string]*job{}}
}

// Token returns the shared secret required by the UI.
func (s *Server) Token() string { return s.token }

// Handler returns the routed, authenticated handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/app.css", s.asset("static/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/app.js", s.asset("static/app.js", "application/javascript; charset=utf-8"))
	mux.HandleFunc("/api/nodes", s.auth(s.handleNodes))
	mux.HandleFunc("/api/list", s.auth(s.handleList))
	mux.HandleFunc("/api/stat", s.auth(s.handleStat))
	mux.HandleFunc("/api/download", s.auth(s.handleDownload))
	mux.HandleFunc("/api/upload", s.auth(s.handleUpload))
	mux.HandleFunc("/api/mkdir", s.auth(s.handleMkdir))
	mux.HandleFunc("/api/delete", s.auth(s.handleDelete))
	mux.HandleFunc("/api/rename", s.auth(s.handleRename))
	mux.HandleFunc("/api/transfer", s.auth(s.handleTransfer))
	mux.HandleFunc("/api/auth", s.auth(s.handleAuth))
	mux.HandleFunc("/api/jobs/", s.auth(s.handleJob))
	mux.HandleFunc("/api/login", s.handleLogin)

	return logRequests(mux)
}

// auth requires the token via cookie, header, or query string.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := ""
		if c, err := r.Cookie("knot_token"); err == nil {
			got = c.Value
		}
		if got == "" {
			got = r.Header.Get("X-Knot-Token")
		}
		if got == "" {
			got = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next(w, r)
	}
}

// handleLogin sets the token cookie after checking it.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.token)) != 1 {
		writeErr(w, http.StatusUnauthorized, errors.New("wrong token"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "knot_token",
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := static.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "missing UI", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// asset serves an embedded static file with a content type.
func (s *Server) asset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := static.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"nodes": s.cl.Info()})
}

// handleAuth stores a device password in memory and verifies it immediately.
// A password that does not work is discarded so the UI can ask again.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node     string `json:"node"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	n, err := s.cl.Node(body.Node)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Verify the password itself. A device that accepts our key would otherwise
	// make any password look valid, and a wrong one would be cached.
	if checker, ok := n.(interface {
		CheckPassword(context.Context, string) error
	}); ok {
		if err := checker.CheckPassword(ctx, body.Password); err != nil {
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
	} else {
		home := n.Home()
		if home == "" {
			home = "/"
		}
		if _, err := n.List(ctx, home); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}

	s.cl.Passwords.Set(body.Node, body.Password)

	home := n.Home()
	if home == "" {
		home = "/"
	}
	// Touch the node so the home directory is discovered if it was not set.
	if _, err := n.List(ctx, home); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	writeJSON(w, map[string]any{"ok": true, "home": n.Home()})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("node")
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}

	n, err := s.cl.Node(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	entries, err := n.List(ctx, dir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	parent := ""
	if dir != "/" {
		parent = node.Parent(dir)
	}
	writeJSON(w, map[string]any{
		"node":    name,
		"path":    dir,
		"parent":  parent,
		"home":    n.Home(),
		"entries": entries,
	})
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	n, err := s.cl.Node(r.URL.Query().Get("node"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	entry, err := n.Stat(ctx, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, entry)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("node")
	p := r.URL.Query().Get("path")

	n, err := s.cl.Node(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	entry, err := n.Stat(ctx, p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if entry.IsDir {
		writeErr(w, http.StatusBadRequest, errors.New("cannot download a directory"))
		return
	}

	f, err := n.Open(ctx, p)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", node.Base(p)))
	if entry.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	}
	_, _ = io.Copy(w, f)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	name := r.FormValue("node")
	dir := r.FormValue("path")
	n, err := s.cl.Node(name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	files := r.MultipartForm.File["file"]
	saved := 0
	for _, fh := range files {
		src, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		dst, err := n.Create(ctx, node.Join(dir, fh.Filename))
		if err != nil {
			src.Close()
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		_, cerr := io.Copy(dst, src)
		src.Close()
		if err := dst.Close(); err != nil && cerr == nil {
			cerr = err
		}
		if cerr != nil {
			writeErr(w, http.StatusBadGateway, cerr)
			return
		}
		saved++
	}
	writeJSON(w, map[string]any{"saved": saved})
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct{ Node, Path, Name string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.cl.Node(body.Node)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := n.Mkdir(ctx, node.Join(body.Path, body.Name)); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node  string   `json:"node"`
		Path  string   `json:"path"`
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.cl.Node(body.Node)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	for _, name := range body.Names {
		if err := n.Remove(ctx, node.Join(body.Path, name)); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct{ Node, Path, From, To string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.cl.Node(body.Node)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := n.Rename(ctx, node.Join(body.Path, body.From), node.Join(body.Path, body.To)); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleTransfer starts a copy or move as a background job and returns its ID,
// so the UI can show progress instead of hanging on a large file.
func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SrcNode string   `json:"srcNode"`
		SrcPath string   `json:"srcPath"`
		DstNode string   `json:"dstNode"`
		DstPath string   `json:"dstPath"`
		Names   []string `json:"names"`
		Move    bool     `json:"move"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Names) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("nothing selected"))
		return
	}

	src, err := s.cl.Node(body.SrcNode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	dst, err := s.cl.Node(body.DstNode)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	verb := "copy"
	if body.Move {
		verb = "move"
	}
	j := &job{
		ID:      newID(),
		Label:   fmt.Sprintf("%s %d item(s) %s → %s", verb, len(body.Names), body.SrcNode, body.DstNode),
		Started: time.Now(),
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()

		var base int64
		for _, name := range body.Names {
			from := node.Join(body.SrcPath, name)
			to := node.Join(body.DstPath, name)

			var itemTotal int64
			if t, terr := ops.Total(ctx, src, from); terr == nil {
				itemTotal = t
				s.addTotal(j, t)
			}

			// Each item reports its own cumulative count, so offset by what
			// earlier items already contributed.
			start := base
			report := func(copied, _ int64) {
				s.mu.Lock()
				j.Copied = start + copied
				s.mu.Unlock()
			}

			var err error
			if body.Move {
				err = ops.Move(ctx, src, from, dst, to, report)
			} else {
				err = ops.Copy(ctx, src, from, dst, to, report)
			}
			if err != nil {
				s.finishJob(j, err)
				return
			}

			base = start + itemTotal
			s.mu.Lock()
			j.Copied = base
			s.mu.Unlock()
		}
		s.finishJob(j, nil)
	}()

	writeJSON(w, map[string]string{"id": j.ID})
}

func (s *Server) addTotal(j *job, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Total += n
}

func (s *Server) finishJob(j *job, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Done = true
	if err != nil {
		j.Err = err.Error()
	}
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	s.mu.Lock()
	j, ok := s.jobs[id]
	var snapshot job
	if ok {
		snapshot = *j
	}
	s.mu.Unlock()

	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("unknown job"))
		return
	}
	writeJSON(w, snapshot)
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// Listen resolves the bind address and warns when it is not tailnet-only.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if os.Getenv("KNOT_DEBUG") != "" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

// Serve runs the HTTP server until the context is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := Listen(addr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("knot web listening on http://%s", ln.Addr())
	log.Printf("token: %s", s.token)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

var _ = os.Getenv
