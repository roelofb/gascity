// Package configedit provides serialized, atomic mutations of city.toml.
//
// It extracts the load → mutate → validate → write-back pattern used
// throughout the CLI (cmd/gc) into a reusable package that the API layer
// can share. All mutations go through [Editor], which serializes access
// with a mutex and writes atomically via temp file + rename.
package configedit

import (
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Origin describes where an agent or rig is defined in the config.
type Origin int

const (
	// OriginInline means the resource is defined directly in city.toml
	// (or a merged fragment) and can be edited in place.
	OriginInline Origin = iota
	// OriginDerived means the resource comes from pack expansion and
	// must be modified via [[patches.agents]] or [[patches.rigs]].
	OriginDerived
	// OriginNotFound means the resource was not found in any config.
	OriginNotFound
)

// Editor provides serialized, atomic mutations of a city.toml file.
// It is safe for concurrent use from multiple goroutines.
type Editor struct {
	mu       sync.Mutex
	tomlPath string
	fs       fsys.FS
}

// NewEditor creates an Editor for the city.toml at the given path.
func NewEditor(fs fsys.FS, tomlPath string) *Editor {
	return &Editor{
		tomlPath: tomlPath,
		fs:       fs,
	}
}

// Edit loads the raw config (no pack expansion), calls fn to mutate it,
// validates the result, and writes it back atomically. The mutex ensures
// only one mutation runs at a time.
func (e *Editor) Edit(fn func(cfg *config.City) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := config.Load(e.fs, e.tomlPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if err := fn(cfg); err != nil {
		return err
	}

	if err := config.ValidateAgents(cfg.Agents); err != nil {
		return fmt.Errorf("validating agents: %w", err)
	}
	if err := config.ValidateRigs(cfg.Rigs, cfg.Workspace.Name); err != nil {
		return fmt.Errorf("validating rigs: %w", err)
	}

	content, err := cfg.Marshal()
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	return fsys.WriteFileAtomic(e.fs, e.tomlPath, content, 0o644)
}

// EditExpanded loads both raw and expanded configs, calls fn with both,
// then validates and writes back the raw config. Use this when the
// mutation needs provenance detection (e.g., to decide whether to edit
// an inline agent or add a patch for a pack-derived agent).
//
// The fn receives the raw config (which will be written back) and the
// expanded config (read-only, for provenance checks). Only mutations
// to raw are persisted.
func (e *Editor) EditExpanded(fn func(raw, expanded *config.City) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	raw, err := config.Load(e.fs, e.tomlPath)
	if err != nil {
		return fmt.Errorf("loading raw config: %w", err)
	}

	expanded, _, err := config.LoadWithIncludes(e.fs, e.tomlPath)
	if err != nil {
		return fmt.Errorf("loading expanded config: %w", err)
	}

	if err := fn(raw, expanded); err != nil {
		return err
	}

	if err := config.ValidateAgents(raw.Agents); err != nil {
		return fmt.Errorf("validating agents: %w", err)
	}
	if err := config.ValidateRigs(raw.Rigs, raw.Workspace.Name); err != nil {
		return fmt.Errorf("validating rigs: %w", err)
	}

	content, err := raw.Marshal()
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	return fsys.WriteFileAtomic(e.fs, e.tomlPath, content, 0o644)
}

// AgentOrigin determines whether an agent is defined inline in the raw
// config or derived from pack expansion. This is the two-phase detection
// pattern extracted from the CLI's doAgentSuspend/doAgentResume.
func AgentOrigin(raw, expanded *config.City, name string) Origin {
	dir, base := config.ParseQualifiedName(name)
	// Check raw config first.
	for _, a := range raw.Agents {
		if a.Dir == dir && a.Name == base {
			return OriginInline
		}
	}
	// Check expanded config for pack-derived agents.
	for _, a := range expanded.Agents {
		if a.Dir == dir && a.Name == base {
			return OriginDerived
		}
	}
	return OriginNotFound
}

// RigOrigin determines whether a rig is defined inline in the raw config.
// Rigs cannot currently be pack-derived, so this is simpler than agents.
func RigOrigin(raw *config.City, name string) Origin {
	for _, r := range raw.Rigs {
		if r.Name == name {
			return OriginInline
		}
	}
	return OriginNotFound
}

// SetAgentSuspended sets the suspended field on an inline agent.
// Returns an error if the agent is not found in the config.
func SetAgentSuspended(cfg *config.City, name string, suspended bool) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range cfg.Agents {
		if cfg.Agents[i].Dir == dir && cfg.Agents[i].Name == base {
			cfg.Agents[i].Suspended = suspended
			return nil
		}
	}
	return fmt.Errorf("agent %q not found in config", name)
}

// SetRigSuspended sets the suspended field on an inline rig.
// Returns an error if the rig is not found in the config.
func SetRigSuspended(cfg *config.City, name string, suspended bool) error {
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == name {
			cfg.Rigs[i].Suspended = suspended
			return nil
		}
	}
	return fmt.Errorf("rig %q not found in config", name)
}

// AddOrUpdateAgentPatch adds or updates an agent patch in the config's
// [[patches.agents]] section. If a patch for the given agent already
// exists, fn is called on it. Otherwise a new patch is created.
func AddOrUpdateAgentPatch(cfg *config.City, name string, fn func(p *config.AgentPatch)) error {
	dir, base := config.ParseQualifiedName(name)
	for i := range cfg.Patches.Agents {
		if cfg.Patches.Agents[i].Dir == dir && cfg.Patches.Agents[i].Name == base {
			fn(&cfg.Patches.Agents[i])
			return nil
		}
	}
	p := config.AgentPatch{Dir: dir, Name: base}
	fn(&p)
	cfg.Patches.Agents = append(cfg.Patches.Agents, p)
	return nil
}

// AddOrUpdateRigPatch adds or updates a rig patch in the config's
// [[patches.rigs]] section. If a patch for the given rig already exists,
// fn is called on it. Otherwise a new patch is created.
func AddOrUpdateRigPatch(cfg *config.City, name string, fn func(p *config.RigPatch)) error {
	for i := range cfg.Patches.Rigs {
		if cfg.Patches.Rigs[i].Name == name {
			fn(&cfg.Patches.Rigs[i])
			return nil
		}
	}
	p := config.RigPatch{Name: name}
	fn(&p)
	cfg.Patches.Rigs = append(cfg.Patches.Rigs, p)
	return nil
}

// boolPtr returns a pointer to a bool value.
func boolPtr(b bool) *bool { return &b }

// SuspendAgent suspends an agent, using inline edit or patch depending
// on provenance. This is the correct implementation that writes desired
// state to city.toml (not ephemeral session metadata).
func (e *Editor) SuspendAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		switch AgentOrigin(raw, expanded, name) {
		case OriginInline:
			return SetAgentSuspended(raw, name, true)
		case OriginDerived:
			return AddOrUpdateAgentPatch(raw, name, func(p *config.AgentPatch) {
				p.Suspended = boolPtr(true)
			})
		default:
			return fmt.Errorf("agent %q not found", name)
		}
	})
}

// ResumeAgent resumes a suspended agent, using inline edit or patch
// depending on provenance.
func (e *Editor) ResumeAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		switch AgentOrigin(raw, expanded, name) {
		case OriginInline:
			return SetAgentSuspended(raw, name, false)
		case OriginDerived:
			return AddOrUpdateAgentPatch(raw, name, func(p *config.AgentPatch) {
				p.Suspended = boolPtr(false)
			})
		default:
			return fmt.Errorf("agent %q not found", name)
		}
	})
}

// SuspendRig suspends a rig by setting suspended=true in city.toml.
func (e *Editor) SuspendRig(name string) error {
	return e.Edit(func(cfg *config.City) error {
		return SetRigSuspended(cfg, name, true)
	})
}

// ResumeRig resumes a rig by clearing suspended in city.toml.
func (e *Editor) ResumeRig(name string) error {
	return e.Edit(func(cfg *config.City) error {
		return SetRigSuspended(cfg, name, false)
	})
}

// SuspendCity sets workspace.suspended = true.
func (e *Editor) SuspendCity() error {
	return e.Edit(func(cfg *config.City) error {
		cfg.Workspace.Suspended = true
		return nil
	})
}

// ResumeCity sets workspace.suspended = false.
func (e *Editor) ResumeCity() error {
	return e.Edit(func(cfg *config.City) error {
		cfg.Workspace.Suspended = false
		return nil
	})
}

// CreateAgent adds a new agent to the config. Returns an error if an
// agent with the same qualified name already exists.
func (e *Editor) CreateAgent(a config.Agent) error {
	return e.Edit(func(cfg *config.City) error {
		qn := a.QualifiedName()
		for _, existing := range cfg.Agents {
			if existing.QualifiedName() == qn {
				return fmt.Errorf("agent %q already exists", qn)
			}
		}
		cfg.Agents = append(cfg.Agents, a)
		return nil
	})
}

// UpdateAgent replaces an existing inline agent definition.
// Returns an error if the agent is not found.
func (e *Editor) UpdateAgent(name string, a config.Agent) error {
	return e.Edit(func(cfg *config.City) error {
		dir, base := config.ParseQualifiedName(name)
		for i := range cfg.Agents {
			if cfg.Agents[i].Dir == dir && cfg.Agents[i].Name == base {
				// Preserve Dir from the target slot.
				a.Dir = cfg.Agents[i].Dir
				cfg.Agents[i] = a
				return nil
			}
		}
		return fmt.Errorf("agent %q not found", name)
	})
}

// DeleteAgent removes an inline agent from the config.
// Returns an error if the agent is not found.
func (e *Editor) DeleteAgent(name string) error {
	return e.EditExpanded(func(raw, expanded *config.City) error {
		origin := AgentOrigin(raw, expanded, name)
		switch origin {
		case OriginDerived:
			return fmt.Errorf("agent %q is pack-derived; cannot delete (use patches to override)", name)
		case OriginNotFound:
			return fmt.Errorf("agent %q not found", name)
		}
		dir, base := config.ParseQualifiedName(name)
		for i := range raw.Agents {
			if raw.Agents[i].Dir == dir && raw.Agents[i].Name == base {
				raw.Agents = append(raw.Agents[:i], raw.Agents[i+1:]...)
				return nil
			}
		}
		return fmt.Errorf("agent %q not found", name)
	})
}

// CreateRig adds a new rig to the config. Returns an error if a rig with
// the same name already exists.
func (e *Editor) CreateRig(r config.Rig) error {
	return e.Edit(func(cfg *config.City) error {
		for _, existing := range cfg.Rigs {
			if existing.Name == r.Name {
				return fmt.Errorf("rig %q already exists", r.Name)
			}
		}
		cfg.Rigs = append(cfg.Rigs, r)
		return nil
	})
}

// UpdateRig partially updates an existing rig. Only non-zero fields in
// the provided rig are applied. Returns an error if the rig is not found.
func (e *Editor) UpdateRig(name string, patch config.Rig) error {
	return e.Edit(func(cfg *config.City) error {
		for i := range cfg.Rigs {
			if cfg.Rigs[i].Name == name {
				if patch.Path != "" {
					cfg.Rigs[i].Path = patch.Path
				}
				if patch.Prefix != "" {
					cfg.Rigs[i].Prefix = patch.Prefix
				}
				// Suspended is explicitly set (not a zero-value check).
				cfg.Rigs[i].Suspended = patch.Suspended
				return nil
			}
		}
		return fmt.Errorf("rig %q not found", name)
	})
}

// DeleteRig removes a rig and all its scoped agents from the config.
// Returns an error if the rig is not found.
func (e *Editor) DeleteRig(name string) error {
	return e.Edit(func(cfg *config.City) error {
		found := false
		for i := range cfg.Rigs {
			if cfg.Rigs[i].Name == name {
				cfg.Rigs = append(cfg.Rigs[:i], cfg.Rigs[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("rig %q not found", name)
		}
		// Remove rig-scoped agents.
		var kept []config.Agent
		for _, a := range cfg.Agents {
			if a.Dir != name {
				kept = append(kept, a)
			}
		}
		cfg.Agents = kept
		return nil
	})
}
