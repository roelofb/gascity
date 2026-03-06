package api

import (
	"context"
	"net"
	"net/http"
)

// Server is the GC API HTTP server. It serves /v0/* endpoints and /health.
type Server struct {
	state    State
	mux      *http.ServeMux
	server   *http.Server
	readOnly bool // when true, POST endpoints return 403

	// sessionLogSearchPaths overrides the default search paths for Claude
	// session JSONL files. Nil means use sessionlog.DefaultSearchPaths().
	sessionLogSearchPaths []string
}

// New creates a Server with all routes registered. Does not start listening.
func New(state State) *Server {
	s := &Server{state: state, mux: http.NewServeMux()}
	s.registerRoutes()
	return s
}

// NewReadOnly creates a read-only Server that rejects all mutation requests.
// Use this when the server binds to a non-localhost address.
func NewReadOnly(state State) *Server {
	s := &Server{state: state, mux: http.NewServeMux(), readOnly: true}
	s.registerRoutes()
	return s
}

// ServeHTTP implements http.Handler for testing with httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler().ServeHTTP(w, r)
}

func (s *Server) handler() http.Handler {
	inner := withCSRFCheck(s.mux)
	if s.readOnly {
		inner = withReadOnly(inner)
	}
	return withLogging(withRecovery(withCORS(inner)))
}

// ListenAndServe starts the HTTP listener. Blocks until stopped.
func (s *Server) ListenAndServe(addr string) error {
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.handler(),
	}
	return s.server.ListenAndServe()
}

// Serve accepts connections on lis. Blocks until stopped.
// Use this with a pre-created listener for synchronous bind validation.
func (s *Server) Serve(lis net.Listener) error {
	s.server = &http.Server{
		Handler: s.handler(),
	}
	return s.server.Serve(lis)
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) registerRoutes() {
	// Status + Health
	s.mux.HandleFunc("GET /v0/status", s.handleStatus)
	s.mux.HandleFunc("GET /health", s.handleHealth)

	// City
	s.mux.HandleFunc("PATCH /v0/city", s.handleCityPatch)

	// Agents — read
	s.mux.HandleFunc("GET /v0/agents", s.handleAgentList)
	s.mux.HandleFunc("GET /v0/agent/{name...}", s.handleAgent)
	// Agents — CRUD
	s.mux.HandleFunc("POST /v0/agents", s.handleAgentCreate)
	s.mux.HandleFunc("PUT /v0/agent/{name...}", s.handleAgentUpdate)
	s.mux.HandleFunc("PATCH /v0/agent/{name...}", s.handleAgentUpdate)
	s.mux.HandleFunc("DELETE /v0/agent/{name...}", s.handleAgentDelete)
	// Agents — actions
	s.mux.HandleFunc("POST /v0/agent/{name...}", s.handleAgentAction)

	// Rigs — read
	s.mux.HandleFunc("GET /v0/rigs", s.handleRigList)
	s.mux.HandleFunc("GET /v0/rig/{name}", s.handleRig)
	// Rigs — CRUD
	s.mux.HandleFunc("POST /v0/rigs", s.handleRigCreate)
	s.mux.HandleFunc("PATCH /v0/rig/{name}", s.handleRigUpdate)
	s.mux.HandleFunc("DELETE /v0/rig/{name}", s.handleRigDelete)
	// Rigs — actions
	s.mux.HandleFunc("POST /v0/rig/{name}/{action}", s.handleRigAction)

	// Beads
	s.mux.HandleFunc("GET /v0/beads", s.handleBeadList)
	s.mux.HandleFunc("GET /v0/beads/ready", s.handleBeadReady)
	s.mux.HandleFunc("POST /v0/beads", s.handleBeadCreate)
	s.mux.HandleFunc("GET /v0/bead/{id}", s.handleBeadGet)
	s.mux.HandleFunc("GET /v0/bead/{id}/deps", s.handleBeadDeps)
	s.mux.HandleFunc("POST /v0/bead/{id}/close", s.handleBeadClose)
	s.mux.HandleFunc("POST /v0/bead/{id}/update", s.handleBeadUpdate)

	// Mail
	s.mux.HandleFunc("GET /v0/mail", s.handleMailList)
	s.mux.HandleFunc("POST /v0/mail", s.handleMailSend)
	s.mux.HandleFunc("GET /v0/mail/count", s.handleMailCount)
	s.mux.HandleFunc("GET /v0/mail/thread/{id}", s.handleMailThread)
	s.mux.HandleFunc("GET /v0/mail/{id}", s.handleMailGet)
	s.mux.HandleFunc("POST /v0/mail/{id}/read", s.handleMailRead)
	s.mux.HandleFunc("POST /v0/mail/{id}/mark-unread", s.handleMailMarkUnread)
	s.mux.HandleFunc("POST /v0/mail/{id}/archive", s.handleMailArchive)
	s.mux.HandleFunc("POST /v0/mail/{id}/reply", s.handleMailReply)

	// Convoys
	s.mux.HandleFunc("GET /v0/convoys", s.handleConvoyList)
	s.mux.HandleFunc("POST /v0/convoys", s.handleConvoyCreate)
	s.mux.HandleFunc("GET /v0/convoy/{id}", s.handleConvoyGet)
	s.mux.HandleFunc("POST /v0/convoy/{id}/add", s.handleConvoyAdd)
	s.mux.HandleFunc("POST /v0/convoy/{id}/close", s.handleConvoyClose)

	// Events
	s.mux.HandleFunc("GET /v0/events", s.handleEventList)
	s.mux.HandleFunc("GET /v0/events/stream", s.handleEventStream)

	// Sling (dispatch)
	s.mux.HandleFunc("POST /v0/sling", s.handleSling)
}
