package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleAgentCreate(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"name":"coder","provider":"claude"}`
	req := newPostRequest("/v0/agents", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Verify agent was added.
	found := false
	for _, a := range fs.cfg.Agents {
		if a.Name == "coder" && a.Provider == "claude" {
			found = true
		}
	}
	if !found {
		t.Error("agent 'coder' not found in config after create")
	}
}

func TestHandleAgentCreate_MissingName(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"provider":"claude"}`
	req := newPostRequest("/v0/agents", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleAgentUpdate(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"provider":"gemini"}`
	req := httptest.NewRequest("PATCH", "/v0/agent/myrig/worker", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify provider was updated.
	for _, a := range fs.cfg.Agents {
		if a.Name == "worker" && a.Dir == "myrig" {
			if a.Provider != "gemini" {
				t.Errorf("provider = %q, want %q", a.Provider, "gemini")
			}
			return
		}
	}
	t.Error("agent 'myrig/worker' not found after update")
}

func TestHandleAgentUpdate_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"provider":"gemini"}`
	req := httptest.NewRequest("PATCH", "/v0/agent/nonexistent", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleAgentDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/agent/myrig/worker", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify agent was removed.
	for _, a := range fs.cfg.Agents {
		if a.Name == "worker" && a.Dir == "myrig" {
			t.Error("agent 'myrig/worker' still exists after delete")
		}
	}
}

func TestHandleAgentDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/agent/nonexistent", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleCityPatch_Suspend(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	body := `{"suspended": true}`
	req := httptest.NewRequest("PATCH", "/v0/city", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !fs.cfg.Workspace.Suspended {
		t.Error("expected workspace to be suspended")
	}
}

func TestHandleCityPatch_Resume(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Workspace.Suspended = true
	srv := New(fs)

	body := `{"suspended": false}`
	req := httptest.NewRequest("PATCH", "/v0/city", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if fs.cfg.Workspace.Suspended {
		t.Error("expected workspace to not be suspended")
	}
}

func TestCSRF_BlocksDeleteWithoutHeader(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := New(fs)

	req := httptest.NewRequest("DELETE", "/v0/agent/myrig/worker", nil)
	// No X-GC-Request header.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	var errResp Error
	json.NewDecoder(w.Body).Decode(&errResp) //nolint:errcheck
	if errResp.Code != "csrf" {
		t.Errorf("error code = %q, want %q", errResp.Code, "csrf")
	}
}

func TestReadOnly_BlocksPatch(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := NewReadOnly(fs)

	body := `{"suspended": true}`
	req := httptest.NewRequest("PATCH", "/v0/city", strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestReadOnly_BlocksDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	srv := NewReadOnly(fs)

	req := httptest.NewRequest("DELETE", "/v0/agent/myrig/worker", nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
