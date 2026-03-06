package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// writeSessionJSONL creates a JSONL session file at the slug path for
// the given workDir, returning the file path.
func writeSessionJSONL(t *testing.T, searchBase, workDir string, lines ...string) {
	t.Helper()
	slug := strings.ReplaceAll(workDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	dir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newServerWithSearchPaths(state State, searchBase string) *Server {
	s := New(state)
	s.sessionLogSearchPaths = []string{searchBase}
	return s
}

func TestAgentLogsBasic(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, rigDir,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"hello\"}","timestamp":"2025-01-01T00:00:00Z"}`,
		`{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"world\"}","timestamp":"2025-01-01T00:00:01Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/logs?tail=0", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp agentLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Agent != "myrig/worker" {
		t.Errorf("Agent = %q, want %q", resp.Agent, "myrig/worker")
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(resp.Messages))
	}
	if resp.Messages[0].Type != "user" {
		t.Errorf("Messages[0].Type = %q, want %q", resp.Messages[0].Type, "user")
	}
	if resp.Messages[1].Type != "assistant" {
		t.Errorf("Messages[1].Type = %q, want %q", resp.Messages[1].Type, "assistant")
	}
}

func TestAgentLogsNotFound(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/v0/agent/nonexistent/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAgentLogsNoSessionFile(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}

	searchBase := t.TempDir() // empty — no session files
	srv := newServerWithSearchPaths(state, searchBase)

	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/logs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestAgentLogsPagination(t *testing.T) {
	state := newFakeState(t)
	rigDir := t.TempDir()
	state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigDir}}
	searchBase := t.TempDir()

	// Two compact boundaries so tail=1 actually truncates.
	var lines []string
	lines = append(lines, `{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"first\"}","timestamp":"2025-01-01T00:00:00Z"}`)
	lines = append(lines, `{"uuid":"2","parentUuid":"1","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"first reply\"}","timestamp":"2025-01-01T00:00:01Z"}`)
	lines = append(lines, `{"uuid":"3","parentUuid":"2","type":"system","subtype":"compact_boundary","message":"{\"role\":\"system\",\"content\":\"compacted 1\"}","timestamp":"2025-01-01T00:00:02Z"}`)
	lines = append(lines, `{"uuid":"4","parentUuid":"3","type":"user","message":"{\"role\":\"user\",\"content\":\"second\"}","timestamp":"2025-01-01T00:00:03Z"}`)
	lines = append(lines, `{"uuid":"5","parentUuid":"4","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"second reply\"}","timestamp":"2025-01-01T00:00:04Z"}`)
	lines = append(lines, `{"uuid":"6","parentUuid":"5","type":"system","subtype":"compact_boundary","message":"{\"role\":\"system\",\"content\":\"compacted 2\"}","timestamp":"2025-01-01T00:00:05Z"}`)
	lines = append(lines, `{"uuid":"7","parentUuid":"6","type":"user","message":"{\"role\":\"user\",\"content\":\"third\"}","timestamp":"2025-01-01T00:00:06Z"}`)
	lines = append(lines, `{"uuid":"8","parentUuid":"7","type":"assistant","message":"{\"role\":\"assistant\",\"content\":\"third reply\"}","timestamp":"2025-01-01T00:00:07Z"}`)

	writeSessionJSONL(t, searchBase, rigDir, lines...)

	srv := newServerWithSearchPaths(state, searchBase)

	// tail=1 should return messages from the last compact boundary onward.
	req := httptest.NewRequest("GET", "/v0/agent/myrig/worker/logs?tail=1", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp agentLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Should get the last compact boundary + messages after it (boundary + user + assistant = 3).
	if len(resp.Messages) != 3 {
		t.Fatalf("got %d messages, want 3 (boundary + 2 after)", len(resp.Messages))
	}

	if resp.Pagination == nil {
		t.Fatal("pagination is nil, expected non-nil")
	}
	if !resp.Pagination.HasOlderMessages {
		t.Error("expected HasOlderMessages=true")
	}
}

func TestAgentLogsCityScoped(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{Name: "mayor"})

	searchBase := t.TempDir()
	writeSessionJSONL(t, searchBase, state.cityPath,
		`{"uuid":"1","parentUuid":"","type":"user","message":"{\"role\":\"user\",\"content\":\"plan\"}","timestamp":"2025-01-01T00:00:00Z"}`,
	)

	srv := newServerWithSearchPaths(state, searchBase)
	req := httptest.NewRequest("GET", "/v0/agent/mayor/logs?tail=0", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp agentLogsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Agent != "mayor" {
		t.Errorf("Agent = %q, want %q", resp.Agent, "mayor")
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(resp.Messages))
	}
}
