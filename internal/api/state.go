// Package api implements the GC HTTP API server.
//
// The server embeds in the controller process and serves typed JSON
// endpoints over REST, replacing subprocess-based data access. It
// activates via [api] port = N in city.toml (progressive activation).
package api

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

// State provides read access to controller-managed state.
// The controller implements this with RWMutex-protected hot-reload.
type State interface {
	// Config returns the current city config snapshot.
	Config() *config.City

	// SessionProvider returns the current session provider.
	SessionProvider() session.Provider

	// BeadStore returns the bead store for a rig (by name).
	// Returns nil if the rig doesn't exist.
	BeadStore(rig string) beads.Store

	// BeadStores returns all rig names and their stores.
	BeadStores() map[string]beads.Store

	// MailProvider returns the mail provider for a rig.
	// Returns nil if the rig doesn't exist.
	MailProvider(rig string) mail.Provider

	// MailProviders returns all rig names and their mail providers.
	MailProviders() map[string]mail.Provider

	// EventProvider returns the event provider, or nil if events are disabled.
	EventProvider() events.Provider

	// CityName returns the city name.
	CityName() string

	// CityPath returns the city root directory.
	CityPath() string

	// Version returns the GC binary version string.
	Version() string

	// StartedAt returns when the controller was started.
	StartedAt() time.Time

	// IsQuarantined reports whether an agent (by session name) is
	// currently quarantined due to crash-loop detection.
	IsQuarantined(sessionName string) bool
}

// StateMutator extends State with write operations for mutation endpoints.
type StateMutator interface {
	State

	// --- Desired-state mutations (write to city.toml) ---

	// SuspendAgent marks an agent as suspended in the config.
	SuspendAgent(name string) error

	// ResumeAgent marks an agent as no longer suspended.
	ResumeAgent(name string) error

	// SuspendRig suspends a rig in the config.
	SuspendRig(name string) error

	// ResumeRig resumes a rig in the config.
	ResumeRig(name string) error

	// SuspendCity sets workspace.suspended = true.
	SuspendCity() error

	// ResumeCity sets workspace.suspended = false.
	ResumeCity() error

	// CreateAgent adds a new agent to city.toml.
	CreateAgent(a config.Agent) error

	// UpdateAgent replaces an existing agent definition in city.toml.
	UpdateAgent(name string, a config.Agent) error

	// DeleteAgent removes an agent from city.toml.
	DeleteAgent(name string) error

	// CreateRig adds a new rig to city.toml.
	CreateRig(r config.Rig) error

	// UpdateRig partially updates a rig in city.toml.
	UpdateRig(name string, r config.Rig) error

	// DeleteRig removes a rig from city.toml.
	DeleteRig(name string) error

	// --- Runtime actions (ephemeral session operations) ---

	// KillAgent force-kills an agent's session (reconciler restarts it).
	KillAgent(name string) error

	// DrainAgent signals an agent to wind down gracefully.
	DrainAgent(name string) error

	// UndrainAgent cancels a drain signal.
	UndrainAgent(name string) error

	// NudgeAgent sends a message to a running agent session.
	NudgeAgent(name, message string) error
}
