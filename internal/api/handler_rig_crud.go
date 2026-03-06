package api

import (
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// rigCreateRequest is the JSON body for POST /v0/rigs.
type rigCreateRequest struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Prefix string `json:"prefix,omitempty"`
}

// rigUpdateRequest is the JSON body for PATCH /v0/rig/{name}.
type rigUpdateRequest struct {
	Path      string `json:"path,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Suspended *bool  `json:"suspended,omitempty"`
}

func (s *Server) handleRigCreate(w http.ResponseWriter, r *http.Request) {
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	var body rigCreateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid", "name is required")
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "invalid", "path is required")
		return
	}

	rig := config.Rig{
		Name:   body.Name,
		Path:   body.Path,
		Prefix: body.Prefix,
	}

	if err := sm.CreateRig(rig); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created", "rig": body.Name})
}

func (s *Server) handleRigUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	var body rigUpdateRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	patch := config.Rig{
		Path:   body.Path,
		Prefix: body.Prefix,
	}
	if body.Suspended != nil {
		patch.Suspended = *body.Suspended
	}

	if err := sm.UpdateRig(name, patch); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated", "rig": name})
}

func (s *Server) handleRigDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sm, ok := s.state.(StateMutator)
	if !ok {
		writeError(w, http.StatusNotImplemented, "internal", "mutations not supported")
		return
	}

	if err := sm.DeleteRig(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "rig": name})
}
