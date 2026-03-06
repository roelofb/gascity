package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

// agentLogMessage is a single message in the agent log response.
type agentLogMessage struct {
	UUID      string          `json:"uuid"`
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
}

// agentLogsResponse is the response for GET /v0/agent/{name}/logs.
type agentLogsResponse struct {
	Agent      string                     `json:"agent"`
	SessionID  string                     `json:"session_id"`
	Messages   []agentLogMessage          `json:"messages"`
	Pagination *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// handleAgentLogs returns structured session log messages for an agent.
//
// Query params:
//   - tail: number of compaction segments to return (default 1, 0 = all)
//   - before: message UUID cursor for loading older messages
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request, name string) {
	cfg := s.state.Config()
	agentCfg, ok := findAgent(cfg, name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "agent "+name+" not found")
		return
	}

	// Resolve the agent's working directory to find its session file.
	workDir := s.resolveAgentWorkDir(agentCfg)
	if workDir == "" {
		writeError(w, http.StatusNotFound, "not_found", "cannot resolve working directory for "+name)
		return
	}

	searchPaths := s.sessionLogSearchPaths
	if searchPaths == nil {
		searchPaths = sessionlog.DefaultSearchPaths()
	}
	path := sessionlog.FindSessionFile(searchPaths, workDir)
	if path == "" {
		writeError(w, http.StatusNotFound, "not_found", "no session file found for "+name)
		return
	}

	// Parse query params.
	tail := 1
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			tail = n
		}
	}
	before := r.URL.Query().Get("before")

	var sess *sessionlog.Session
	var err error
	if before != "" {
		sess, err = sessionlog.ReadFileOlder(path, tail, before)
	} else {
		sess, err = sessionlog.ReadFile(path, tail)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Convert entries to response messages.
	msgs := make([]agentLogMessage, 0, len(sess.Messages))
	for _, e := range sess.Messages {
		msg := agentLogMessage{
			UUID:    e.UUID,
			Type:    e.Type,
			Message: e.Raw,
		}
		if !e.Timestamp.IsZero() {
			msg.Timestamp = e.Timestamp.Format("2006-01-02T15:04:05Z07:00")
		}
		msgs = append(msgs, msg)
	}

	resp := agentLogsResponse{
		Agent:      name,
		SessionID:  sess.ID,
		Messages:   msgs,
		Pagination: sess.Pagination,
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveAgentWorkDir returns the absolute working directory for an agent.
// For rig-scoped agents, this is the rig's Path. For city-scoped agents,
// this is the city root.
func (s *Server) resolveAgentWorkDir(a config.Agent) string {
	if a.Dir == "" {
		return s.state.CityPath()
	}
	cfg := s.state.Config()
	for _, rig := range cfg.Rigs {
		if rig.Name == a.Dir {
			return rig.Path
		}
	}
	return ""
}
