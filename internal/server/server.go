// Package server is the minimal E2B API: the subset of the public E2B REST
// API the official SDKs use for sandboxes and template builds, backed directly
// by one orchestrator over gRPC. One static API key, no teams.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/traumventures/e2b-minimal/internal/pb/orchestrator"
	tm "github.com/traumventures/e2b-minimal/internal/pb/templatemanager"
	"github.com/traumventures/e2b-minimal/internal/store"
)

type Config struct {
	APIKey string
	// PublicURL is how SDK clients reach this server. Only used to rewrite
	// template file upload URLs; empty derives it from the request.
	PublicURL string
	// Domain is returned as the sandbox domain. Clients that use
	// E2B_SANDBOX_URL never read it; clients that route by host name
	// (<port>-<sandboxID>.<domain>) need wildcard DNS for it.
	Domain            string
	OrchestratorGRPC  string
	OrchestratorProxy string
	OrchestratorHTTP  string
	MaxSandboxHours   int64
	BuildDiskMB       int64
	DefaultFreeDiskMB int64
	DefaultCPUCount   int64
	DefaultMemoryMB   int64
	// HugePages is the page size new template builds use.
	HugePages bool
	// CgroupRoot is the cgroup every sandbox runs under, with the limits for
	// all sandboxes together.
	CgroupRoot string
	// MemoryMinMB is guaranteed to every running sandbox (cgroup memory.min);
	// a create that cannot be guaranteed it is refused.
	MemoryMinMB int64
	// ColdBoot starts new sandboxes by booting the template\'s filesystem
	// instead of restoring its memory snapshot.
	ColdBoot      bool
	BaseImage     string
	BootstrapBase bool
	// Fallbacks for builds whose metadata does not name the versions.
	KernelVersion      string
	FirecrackerVersion string
}

// teamID is the one fixed team every sandbox and build belongs to. The
// orchestrator only uses it as a label and cache scope.
const teamID = "00000000-0000-4000-8000-000000000001"

type Server struct {
	cfg      Config
	store    *store.Store
	sbx      orchestrator.SandboxServiceClient
	tpl      tm.TemplateServiceClient
	conn     *grpc.ClientConn
	sbxProxy *httputil.ReverseProxy
	upProxy  *httputil.ReverseProxy
	mux      *http.ServeMux
}

func New(cfg Config, st *store.Store) (*Server, error) {
	conn, err := grpc.NewClient(cfg.OrchestratorGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:   cfg,
		store: st,
		conn:  conn,
		sbx:   orchestrator.NewSandboxServiceClient(conn),
		tpl:   tm.NewTemplateServiceClient(conn),
		mux:   http.NewServeMux(),
	}

	s.sbxProxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = cfg.OrchestratorProxy
			// The orchestrator proxy only honours the E2b-Sandbox-Id/Port
			// headers for localhost or IP hosts. Host-routed requests keep
			// their <port>-<id>.<domain> host so the sandbox app sees it.
			if r.In.Header.Get("E2b-Sandbox-Id") != "" {
				r.Out.Host = cfg.OrchestratorProxy
			} else {
				r.Out.Host = r.In.Host
			}
			r.SetXForwarded()
		},
		FlushInterval: -1, // envd streams process output
		ErrorLog:      slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	s.upProxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = cfg.OrchestratorHTTP
			r.Out.Host = cfg.OrchestratorHTTP
		},
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	s.routes()

	return s, nil
}

func (s *Server) Close() error { return s.conn.Close() }

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.authorized(r) {
				writeError(w, http.StatusUnauthorized, "Invalid API key")

				return
			}
			h(w, r)
		}
	}

	// Sandboxes. v1 routes stay for older SDKs.
	m.HandleFunc("POST /sandboxes", auth(s.createSandbox))
	m.HandleFunc("POST /v2/sandboxes", auth(s.createSandbox))
	m.HandleFunc("GET /sandboxes", auth(s.listSandboxesV1))
	m.HandleFunc("GET /v2/sandboxes", auth(s.listSandboxes))
	m.HandleFunc("GET /sandboxes/{sandboxID}", auth(s.getSandbox))
	m.HandleFunc("DELETE /sandboxes/{sandboxID}", auth(s.killSandbox))
	m.HandleFunc("POST /sandboxes/{sandboxID}/timeout", auth(s.setTimeout))
	m.HandleFunc("POST /sandboxes/{sandboxID}/refreshes", auth(s.refresh))
	m.HandleFunc("POST /sandboxes/{sandboxID}/pause", auth(s.pauseSandbox))
	m.HandleFunc("POST /sandboxes/{sandboxID}/resume", auth(s.resumeSandbox))
	m.HandleFunc("POST /sandboxes/{sandboxID}/connect", auth(s.connectSandbox))
	m.HandleFunc("POST /v2/sandboxes/{sandboxID}/connect", auth(s.connectSandbox))
	m.HandleFunc("GET /sandboxes/{sandboxID}/metrics", auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, []any{}) }))
	m.HandleFunc("GET /sandboxes/metrics", auth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sandboxes": map[string]any{}})
	}))

	// Templates.
	m.HandleFunc("GET /templates", auth(s.listTemplates))
	m.HandleFunc("POST /v3/templates", auth(s.requestBuild))
	m.HandleFunc("GET /templates/{templateID}/files/{hash}", auth(s.fileUpload))
	m.HandleFunc("POST /v2/templates/{templateID}/builds/{buildID}", auth(s.startBuild))
	m.HandleFunc("GET /templates/{templateID}/builds/{buildID}/status", auth(s.buildStatus))
	m.HandleFunc("GET /templates/aliases/{alias}", auth(s.getAlias))
	m.HandleFunc("DELETE /templates/{templateID}", auth(s.deleteTemplate))

	// Everything else the SDKs know (volumes, secrets, snapshots, fork,
	// network updates, logs, tags) is out of scope.
	m.HandleFunc("/", auth(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented, "e2b-minimal does not implement "+r.Method+" "+r.URL.Path)
	}))
}

// hostRoute matches <port>-<sandboxID>.<domain>.
var hostRoute = regexp.MustCompile(`^\d+-[a-z0-9]+\.`)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("E2b-Sandbox-Id") != "" || hostRoute.MatchString(r.Host):
		// Sandbox traffic. envd checks its own X-Access-Token and the
		// orchestrator proxy checks the traffic token on other ports.
		s.sbxProxy.ServeHTTP(w, r)
	case r.URL.Path == "/upload":
		// Template file uploads; the URL carries an orchestrator HMAC.
		s.upProxy.ServeHTTP(w, r)
	default:
		s.mux.ServeHTTP(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		key, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}

	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) == 1
}

// publicBase is the scheme://host SDK clients use to reach this server.
func (s *Server) publicBase(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	return scheme + "://" + r.Host
}

// rewriteUploadURL points a local orchestrator upload URL at this server.
func (s *Server) rewriteUploadURL(r *http.Request, raw string) string {
	u, err := url.Parse(raw)
	// Only the orchestrator's own upload endpoint is rewritten; a presigned
	// S3 URL already points where the client can upload.
	if err != nil || u.Host != s.cfg.OrchestratorHTTP {
		return raw
	}
	base, err := url.Parse(s.publicBase(r))
	if err != nil {
		return raw
	}
	u.Scheme, u.Host = base.Scheme, base.Host

	return u.String()
}

// WaitForOrchestrator blocks until the orchestrator answers List.
func (s *Server) WaitForOrchestrator(ctx context.Context) error {
	for {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := s.sbx.List(cctx, nil)
		cancel()
		if err == nil {
			return nil
		}
		slog.Info("waiting for orchestrator", "addr", s.cfg.OrchestratorGRPC, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, apiError{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}
