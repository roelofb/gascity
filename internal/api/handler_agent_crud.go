package api

import (
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// agentCreateRequest is the JSON body for POST /v0/agents.
type agentCreateRequest struct {
	Name     string `json:"name"`
	Dir      string `json:"dir,omitempty"`
	Provider string `json:"provider"`
	Scope    string `json:"scope,omitempty"`
}

// agentUpdateRequest is the JSON body for PUT/PATCH /v0/agent/{name}.
type agentUpdateRequest struct {
	Provider  string `json:"provider,omitempty"`
	Suspended *bool  `json:"suspended,omitempty"`
	Scope     string `json:"scope,omitempty"`
}

func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	var body agentCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "name is required")
		return
	}
	if body.Provider == "" {
		writeError(w, http.StatusBadRequest, "invalid", "provider is required")
		return
	}

	a := config.Agent{
		Name:     body.Name,
		Dir:      body.Dir,
		Provider: body.Provider,
		Scope:    body.Scope,
	}

	if err := sm.CreateAgent(a); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "agent": a.QualifiedName()})
}

func (s *Server) handleAgentUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	// Validate agent exists.
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	var body agentUpdateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	// Build updated agent from existing config + request body.
	updated := agentCfg
	if body.Provider != "" {
		updated.Provider = body.Provider
	}
	if body.Suspended != nil {
		updated.Suspended = *body.Suspended
	}
	if body.Scope != "" {
		updated.Scope = body.Scope
	}

	if err := sm.UpdateAgent(name, updated); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	if err := sm.DeleteAgent(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		if strings.Contains(err.Error(), "pack-derived") {
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "agent": name})
}
