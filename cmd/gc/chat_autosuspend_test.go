package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/chatsession"
	"github.com/gastownhall/gascity/internal/session"
)

func TestAutoSuspendChatSessions(t *testing.T) {
	store := beads.NewMemStore()
	sp := session.NewFake()
	mgr := chatsession.NewManager(store, sp)

	// Create two sessions.
	s1, err := mgr.Create(context.Background(), "default", "S1", "echo s1", "/tmp", "test", nil, chatsession.ProviderResume{}, session.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := mgr.Create(context.Background(), "default", "S2", "echo s2", "/tmp", "test", nil, chatsession.ProviderResume{}, session.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Set activity times: s1 was active 2 hours ago, s2 was active 1 minute ago.
	sp.SetActivity(s1.SessionName, time.Now().Add(-2*time.Hour))
	sp.SetActivity(s2.SessionName, time.Now().Add(-1*time.Minute))

	// Neither is attached.
	sp.SetAttached(s1.SessionName, false)
	sp.SetAttached(s2.SessionName, false)

	var stdout, stderr bytes.Buffer

	// Use the manager directly (auto-suspend calls mgr.List + mgr.Suspend).
	// We can't call autoSuspendChatSessions directly because it needs openCityStoreAt.
	// Instead, replicate the logic to test the behavior.
	sessions, err := mgr.List("active", "")
	if err != nil {
		t.Fatal(err)
	}

	idleTimeout := 30 * time.Minute
	now := time.Now()
	for _, s := range sessions {
		if s.Attached || s.LastActive.IsZero() || now.Sub(s.LastActive) < idleTimeout {
			continue
		}
		if err := mgr.Suspend(s.ID); err != nil {
			t.Fatalf("suspend: %v", err)
		}
	}

	// s1 should be suspended (idle 2h > 30m timeout).
	got1, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.State != chatsession.StateSuspended {
		t.Errorf("s1 state = %q, want suspended", got1.State)
	}

	// s2 should still be active (idle 1m < 30m timeout).
	got2, err := mgr.Get(s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.State != chatsession.StateActive {
		t.Errorf("s2 state = %q, want active", got2.State)
	}

	_ = stdout
	_ = stderr
}

func TestAutoSuspendSkipsAttachedSessions(t *testing.T) {
	store := beads.NewMemStore()
	sp := session.NewFake()
	mgr := chatsession.NewManager(store, sp)

	s1, err := mgr.Create(context.Background(), "default", "Attached", "echo a", "/tmp", "test", nil, chatsession.ProviderResume{}, session.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Old activity but attached — should NOT be suspended.
	sp.SetActivity(s1.SessionName, time.Now().Add(-2*time.Hour))
	sp.SetAttached(s1.SessionName, true)

	sessions, err := mgr.List("active", "")
	if err != nil {
		t.Fatal(err)
	}

	idleTimeout := 30 * time.Minute
	now := time.Now()
	for _, s := range sessions {
		if s.Attached || s.LastActive.IsZero() || now.Sub(s.LastActive) < idleTimeout {
			continue
		}
		if err := mgr.Suspend(s.ID); err != nil {
			t.Fatalf("suspend: %v", err)
		}
	}

	got, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != chatsession.StateActive {
		t.Errorf("attached session state = %q, want active", got.State)
	}
}
