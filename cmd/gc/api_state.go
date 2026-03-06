package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

// controllerState implements api.State and api.StateMutator.
// Protected by an RWMutex for hot-reload: readers take RLock,
// the controller loop takes Lock when updating cfg/sp/stores.
type controllerState struct {
	mu         sync.RWMutex
	cfg        *config.City
	sp         session.Provider
	beadStores map[string]beads.Store
	mailProvs  map[string]mail.Provider
	eventProv  events.Provider
	editor     *configedit.Editor
	cityName   string
	cityPath   string
	version    string
	startedAt  time.Time
	ct         crashTracker // nil if crash tracking disabled
}

// newControllerState creates a controllerState with per-rig stores.
func newControllerState(
	cfg *config.City,
	sp session.Provider,
	ep events.Provider,
	cityName, cityPath string,
) *controllerState {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cs := &controllerState{
		cfg:       cfg,
		sp:        sp,
		eventProv: ep,
		editor:    configedit.NewEditor(fsys.OSFS{}, tomlPath),
		cityName:  cityName,
		cityPath:  cityPath,
		version:   version,
		startedAt: time.Now(),
	}
	cs.beadStores, cs.mailProvs = cs.buildStores(cfg)
	return cs
}

// buildStores creates bead stores and mail providers for each rig in cfg.
// Pure function of cfg — does not read or write cs fields (safe to call unlocked).
func (cs *controllerState) buildStores(cfg *config.City) (map[string]beads.Store, map[string]mail.Provider) {
	provider := beadsProviderFor(cfg)
	stores := make(map[string]beads.Store, len(cfg.Rigs))
	provs := make(map[string]mail.Provider, len(cfg.Rigs))

	// For the "file" provider, all rigs share the same city-level beads.json
	// and a single mail provider to ensure identity-based dedup works correctly.
	var sharedFileStore beads.Store
	var sharedMailProv mail.Provider
	if provider == "file" {
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cs.cityPath, ".gc", "beads.json"))
		if err == nil {
			sharedFileStore = store
			sharedMailProv = beadmail.New(store)
		} else {
			// Fall back to bd provider rather than opening duplicate per-rig file stores.
			fmt.Fprintf(os.Stderr, "api: failed to open shared file store: %v (falling back to bd provider)\n", err)
			provider = "bd"
		}
	}

	for _, rig := range cfg.Rigs {
		if sharedFileStore != nil {
			stores[rig.Name] = sharedFileStore
			provs[rig.Name] = sharedMailProv
		} else {
			store := cs.openRigStore(provider, rig.Path)
			stores[rig.Name] = store
			provs[rig.Name] = beadmail.New(store)
		}
	}
	return stores, provs
}

// beadsProviderFor returns the bead store provider name from the given config.
// Pure function — does not read controllerState fields.
func beadsProviderFor(cfg *config.City) string {
	if v := os.Getenv("GC_BEADS"); v != "" {
		return v
	}
	if cfg.Beads.Provider != "" {
		return cfg.Beads.Provider
	}
	return "bd"
}

// openRigStore creates a bead store for a rig path using the given provider.
func (cs *controllerState) openRigStore(provider, rigPath string) beads.Store {
	if strings.HasPrefix(provider, "exec:") {
		s := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		return s
	}
	switch provider {
	case "file":
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cs.cityPath, ".gc", "beads.json"))
		if err != nil {
			return beads.NewBdStore(rigPath, beads.ExecCommandRunner())
		}
		return store
	default: // "bd" or unrecognized
		return beads.NewBdStore(rigPath, beads.ExecCommandRunner())
	}
}

// update replaces the config, session provider, and reopens stores.
// Stores are built outside the lock to avoid blocking readers during I/O.
func (cs *controllerState) update(cfg *config.City, sp session.Provider) {
	// Build new stores outside the lock (may do file I/O / subprocess spawns).
	stores, provs := cs.buildStores(cfg)

	// Swap under short critical section.
	cs.mu.Lock()
	cs.cfg = cfg
	cs.sp = sp
	cs.beadStores = stores
	cs.mailProvs = provs
	cs.mu.Unlock()
}

// --- api.State implementation ---

// Config returns the current city config snapshot.
func (cs *controllerState) Config() *config.City {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

// SessionProvider returns the current session provider.
func (cs *controllerState) SessionProvider() session.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.sp
}

// BeadStore returns the bead store for a rig (by name).
func (cs *controllerState) BeadStore(rig string) beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.beadStores[rig]
}

// BeadStores returns all rig names and their stores.
func (cs *controllerState) BeadStores() map[string]beads.Store {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	// Return a copy to avoid races.
	m := make(map[string]beads.Store, len(cs.beadStores))
	for k, v := range cs.beadStores {
		m[k] = v
	}
	return m
}

// MailProvider returns the mail provider for a rig.
func (cs *controllerState) MailProvider(rig string) mail.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.mailProvs[rig]
}

// MailProviders returns all rig names and their mail providers.
func (cs *controllerState) MailProviders() map[string]mail.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	m := make(map[string]mail.Provider, len(cs.mailProvs))
	for k, v := range cs.mailProvs {
		m[k] = v
	}
	return m
}

// EventProvider returns the event provider.
func (cs *controllerState) EventProvider() events.Provider {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.eventProv
}

// CityName returns the city name.
func (cs *controllerState) CityName() string {
	return cs.cityName
}

// CityPath returns the city root directory.
func (cs *controllerState) CityPath() string {
	return cs.cityPath
}

// Version returns the GC binary version string.
func (cs *controllerState) Version() string {
	return cs.version
}

// StartedAt returns when the controller was started.
func (cs *controllerState) StartedAt() time.Time {
	return cs.startedAt
}

// IsQuarantined reports whether an agent is quarantined by the crash tracker.
func (cs *controllerState) IsQuarantined(sessionName string) bool {
	cs.mu.RLock()
	ct := cs.ct
	cs.mu.RUnlock()
	if ct == nil {
		return false
	}
	return ct.isQuarantined(sessionName, time.Now())
}

// --- api.StateMutator implementation ---

// spAndSession captures the session provider and computes the session name
// in a single critical section to avoid TOCTOU with config reloads.
func (cs *controllerState) spAndSession(name string) (session.Provider, string) {
	cs.mu.RLock()
	sp := cs.sp
	tmpl := cs.cfg.Workspace.SessionTemplate
	cs.mu.RUnlock()
	return sp, agent.SessionNameFor(cs.cityName, name, tmpl)
}

// SuspendAgent writes suspended=true to city.toml (durable desired state).
// Uses configedit.Editor for provenance-aware edit (inline vs patch).
func (cs *controllerState) SuspendAgent(name string) error {
	return cs.editor.SuspendAgent(name)
}

// ResumeAgent clears suspended in city.toml (durable desired state).
func (cs *controllerState) ResumeAgent(name string) error {
	return cs.editor.ResumeAgent(name)
}

// KillAgent force-kills the agent session.
func (cs *controllerState) KillAgent(name string) error {
	sp, sessionName := cs.spAndSession(name)
	return sp.Stop(sessionName)
}

// DrainAgent signals graceful wind-down.
func (cs *controllerState) DrainAgent(name string) error {
	sp, sessionName := cs.spAndSession(name)
	return sp.SetMeta(sessionName, "drain", "true")
}

// UndrainAgent cancels a drain signal.
func (cs *controllerState) UndrainAgent(name string) error {
	sp, sessionName := cs.spAndSession(name)
	return sp.RemoveMeta(sessionName, "drain")
}

// NudgeAgent sends a message to a running agent session.
func (cs *controllerState) NudgeAgent(name, message string) error {
	sp, sessionName := cs.spAndSession(name)
	if !sp.IsRunning(sessionName) {
		return fmt.Errorf("agent %q not running", name)
	}
	return sp.Nudge(sessionName, message)
}

// SuspendRig writes suspended=true on the rig in city.toml.
func (cs *controllerState) SuspendRig(name string) error {
	return cs.editor.SuspendRig(name)
}

// ResumeRig clears suspended on the rig in city.toml.
func (cs *controllerState) ResumeRig(name string) error {
	return cs.editor.ResumeRig(name)
}

// SuspendCity sets workspace.suspended = true.
func (cs *controllerState) SuspendCity() error {
	return cs.editor.SuspendCity()
}

// ResumeCity sets workspace.suspended = false.
func (cs *controllerState) ResumeCity() error {
	return cs.editor.ResumeCity()
}

// CreateAgent adds a new agent to city.toml.
func (cs *controllerState) CreateAgent(a config.Agent) error {
	return cs.editor.CreateAgent(a)
}

// UpdateAgent replaces an existing agent definition in city.toml.
func (cs *controllerState) UpdateAgent(name string, a config.Agent) error {
	return cs.editor.UpdateAgent(name, a)
}

// DeleteAgent removes an agent from city.toml.
func (cs *controllerState) DeleteAgent(name string) error {
	return cs.editor.DeleteAgent(name)
}

// CreateRig adds a new rig to city.toml.
func (cs *controllerState) CreateRig(r config.Rig) error {
	return cs.editor.CreateRig(r)
}

// UpdateRig partially updates a rig in city.toml.
func (cs *controllerState) UpdateRig(name string, r config.Rig) error {
	return cs.editor.UpdateRig(name, r)
}

// DeleteRig removes a rig from city.toml.
func (cs *controllerState) DeleteRig(name string) error {
	return cs.editor.DeleteRig(name)
}
