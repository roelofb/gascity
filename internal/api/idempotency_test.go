package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestIdempotency_MissOnFirstRequest(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	w := httptest.NewRecorder()
	handled := c.handleIdempotent(w, "key-1", "hash-abc")
	if handled {
		t.Error("handleIdempotent should return false on cache miss")
	}
}

func TestIdempotency_HitOnReplay(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)

	// Store a response.
	c.storeResponse("key-1", "hash-abc", 201, map[string]string{"id": "bead-42"})

	// Replay with same key and hash.
	w := httptest.NewRecorder()
	handled := c.handleIdempotent(w, "key-1", "hash-abc")
	if !handled {
		t.Fatal("handleIdempotent should return true on cache hit")
	}
	if w.Code != 201 {
		t.Errorf("status = %d, want 201", w.Code)
	}
	body := w.Body.String()
	if !containsStr(body, "bead-42") {
		t.Errorf("body should contain cached response: %s", body)
	}
}

func TestIdempotency_MismatchReturns422(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	c.storeResponse("key-1", "hash-abc", 201, map[string]string{"id": "bead-42"})

	// Replay with same key but different hash.
	w := httptest.NewRecorder()
	handled := c.handleIdempotent(w, "key-1", "hash-xyz")
	if !handled {
		t.Fatal("handleIdempotent should return true on mismatch")
	}
	if w.Code != 422 {
		t.Errorf("status = %d, want 422", w.Code)
	}
	body := w.Body.String()
	if !containsStr(body, "idempotency_mismatch") {
		t.Errorf("body should contain idempotency_mismatch: %s", body)
	}
}

func TestIdempotency_ExpiredEntryMisses(t *testing.T) {
	c := newIdempotencyCache(1 * time.Millisecond)
	c.storeResponse("key-1", "hash-abc", 201, map[string]string{"id": "bead-42"})

	time.Sleep(5 * time.Millisecond)

	w := httptest.NewRecorder()
	handled := c.handleIdempotent(w, "key-1", "hash-abc")
	if handled {
		t.Error("handleIdempotent should return false for expired entry")
	}
}

func TestIdempotency_EmptyKeySkips(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	w := httptest.NewRecorder()
	handled := c.handleIdempotent(w, "", "hash-abc")
	if handled {
		t.Error("handleIdempotent should return false for empty key")
	}
}

func TestIdempotency_StoreResponseNoKey(t *testing.T) {
	c := newIdempotencyCache(30 * time.Minute)
	// Should be a no-op.
	c.storeResponse("", "hash-abc", 201, map[string]string{"id": "bead-42"})
	if len(c.entries) != 0 {
		t.Errorf("cache should be empty after storeResponse with empty key, got %d entries", len(c.entries))
	}
}

func TestHashBody_Deterministic(t *testing.T) {
	body := map[string]string{"title": "test", "rig": "myrig"}
	h1 := hashBody(body)
	h2 := hashBody(body)
	if h1 != h2 {
		t.Errorf("hashBody should be deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hashBody should return 64-char hex string, got %d chars", len(h1))
	}
}

func TestHashBody_DifferentInputs(t *testing.T) {
	h1 := hashBody(map[string]string{"title": "a"})
	h2 := hashBody(map[string]string{"title": "b"})
	if h1 == h2 {
		t.Error("hashBody should produce different hashes for different inputs")
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
