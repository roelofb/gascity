package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/chatsession"
	"github.com/gastownhall/gascity/internal/session"
)

// autoSuspendChatSessions scans active chat sessions and suspends any that
// have been detached (no human attached) longer than idleTimeout.
// Called on each controller reconciliation tick when [chat_sessions] idle_timeout is set.
func autoSuspendChatSessions(cityPath string, sp session.Provider, idleTimeout time.Duration, stdout, stderr io.Writer) {
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return // best-effort — no store available
	}

	mgr := chatsession.NewManager(store, sp)

	sessions, err := mgr.List("active", "")
	if err != nil {
		return // best-effort
	}

	now := time.Now()
	for _, s := range sessions {
		// Skip sessions with attached terminals — human is active.
		if s.Attached {
			continue
		}
		// Check last activity time.
		if s.LastActive.IsZero() {
			continue // no activity data — skip
		}
		if now.Sub(s.LastActive) < idleTimeout {
			continue // not idle long enough
		}

		if err := mgr.Suspend(s.ID); err != nil {
			fmt.Fprintf(stderr, "gc start: auto-suspend session %s: %v\n", s.ID, err) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stdout, "Session %s auto-suspended (idle %s).\n", s.ID, formatDuration(now.Sub(s.LastActive))) //nolint:errcheck // best-effort stdout
		}
	}
}
