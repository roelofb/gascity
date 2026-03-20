// Package tmuxtest provides helpers for integration tests that need real tmux.
//
// Guard manages tmux session lifecycle for tests: it generates unique city
// names with a "gctest-" prefix, tracks created sessions, and guarantees
// cleanup even on test failures. Three layers prevent orphan sessions:
//
//  1. Pre-sweep (TestMain): kill all gc-gctest-* sessions from prior crashes.
//  2. Per-test (t.Cleanup): kill sessions created by this guard.
//  3. Post-sweep (TestMain defer): final sweep after all tests complete.
//
// All operations use an isolated tmux socket ("gc-test" by default) so tests
// never interfere with the user's running tmux server.
package tmuxtest

import (
	"crypto/rand"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// DefaultSocketName is the tmux socket used by test infrastructure.
// Using a dedicated socket isolates tests from the user's tmux server.
const DefaultSocketName = "gc-test"

// RequireTmux skips the test if tmux is not installed.
func RequireTmux(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// Guard manages tmux session lifecycle for a single test. It generates a
// unique city name with the "gctest-" prefix and guarantees cleanup of all
// sessions matching that city via t.Cleanup.
type Guard struct {
	t          testing.TB
	cityName   string // "gctest-<nibble>-<nibble>-..."
	socketName string // tmux socket for isolation
}

// NewGuard creates a guard with a unique city name. Registers t.Cleanup
// to kill all sessions created under this guard's city name.
func NewGuard(t testing.TB) *Guard {
	return NewGuardWithSocket(t, DefaultSocketName)
}

// NewGuardWithSocket creates a guard using the specified tmux socket.
func NewGuardWithSocket(t testing.TB, socketName string) *Guard {
	t.Helper()
	RequireTmux(t)

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("tmuxtest: generating random city name: %v", err)
	}
	hex := fmt.Sprintf("%x", b)
	parts := make([]string, 0, len(hex)+1)
	parts = append(parts, "gctest")
	for _, r := range hex {
		parts = append(parts, string(r))
	}
	cityName := strings.Join(parts, "-")

	g := &Guard{t: t, cityName: cityName, socketName: socketName}
	t.Cleanup(func() {
		g.killGuardSessions()
	})
	return g
}

// CityName returns the unique city name (e.g., "gctest-a-1-b-2-c-3-d-4").
func (g *Guard) CityName() string {
	return g.cityName
}

// SocketName returns the tmux socket name used by this guard.
func (g *Guard) SocketName() string {
	return g.socketName
}

// SessionName returns the expected tmux session name for an agent.
// Mirrors cmd/gc/main.go:sessionName() — format is "gc-<cityName>-<agentName>".
func (g *Guard) SessionName(agentName string) string {
	return "gc-" + g.cityName + "-" + agentName
}

// HasSession checks if a specific tmux session exists.
func (g *Guard) HasSession(name string) bool {
	g.t.Helper()
	args := tmuxArgs(g.socketName, "has-session", "-t", name)
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		// tmux has-session exits 1 when session doesn't exist
		// and also when no server is running. Both mean "not found".
		_ = out
		return false
	}
	return true
}

// killGuardSessions kills all tmux sessions matching this guard's city
// name pattern: "gc-gctest-XXXX-*".
func (g *Guard) killGuardSessions() {
	g.t.Helper()
	prefix := "gc-" + g.cityName + "-"
	sessions := listSessionsWithPrefix(g.socketName, prefix)
	for _, s := range sessions {
		args := tmuxArgs(g.socketName, "kill-session", "-t", s)
		_ = exec.Command("tmux", args...).Run()
	}
}

// KillAllTestSessions kills all tmux sessions matching "gc-gctest-*".
// Call from TestMain before and after test runs to clean up orphans.
func KillAllTestSessions(t testing.TB) {
	KillAllTestSessionsOnSocket(t, DefaultSocketName)
}

// KillAllTestSessionsOnSocket kills orphaned test sessions on the given socket.
func KillAllTestSessionsOnSocket(t testing.TB, socketName string) {
	t.Helper()
	sessions := listSessionsWithPrefix(socketName, "gc-gctest-")
	for _, s := range sessions {
		args := tmuxArgs(socketName, "kill-session", "-t", s)
		_ = exec.Command("tmux", args...).Run()
	}
	if len(sessions) > 0 {
		t.Logf("tmuxtest: cleaned up %d orphaned test session(s)", len(sessions))
	}
}

// tmuxArgs prepends -L socketName to the given tmux arguments when socketName
// is non-empty.
func tmuxArgs(socketName string, args ...string) []string {
	if socketName == "" {
		return args
	}
	return append([]string{"-L", socketName}, args...)
}

// listSessionsWithPrefix returns all tmux session names starting with prefix.
func listSessionsWithPrefix(socketName, prefix string) []string {
	args := tmuxArgs(socketName, "list-sessions", "-F", "#{session_name}")
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		// No tmux server running means no sessions to clean.
		return nil
	}
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && strings.HasPrefix(line, prefix) {
			matches = append(matches, line)
		}
	}
	return matches
}
