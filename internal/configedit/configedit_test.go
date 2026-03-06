package configedit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

// minimalCity returns a minimal valid city.toml with one agent.
func minimalCity() string {
	return `[workspace]
name = "test-city"

[[agents]]
name = "mayor"
provider = "claude"
`
}

// cityWithRig returns a city.toml with one agent and one rig.
func cityWithRig() string {
	return `[workspace]
name = "test-city"

[[agents]]
name = "mayor"
provider = "claude"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
`
}

func writeTOML(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTOML(t *testing.T, path string) *config.City {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	return cfg
}

func TestEdit_SetsAgentSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetAgentSuspended(cfg, "mayor", true)
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			if !a.Suspended {
				t.Error("expected mayor to be suspended")
			}
			return
		}
	}
	t.Error("mayor not found after edit")
}

func TestEdit_ValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		// Add an agent with an invalid name to trigger validation failure.
		cfg.Agents = append(cfg.Agents, config.Agent{Name: ""})
		return nil
	})
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
}

func TestSetAgentSuspended_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := configedit.SetAgentSuspended(cfg, "nonexistent", true); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSetRigSuspended(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.Edit(func(cfg *config.City) error {
		return configedit.SetRigSuspended(cfg, "my-rig", true)
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	cfg := readTOML(t, path)
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			if !r.Suspended {
				t.Error("expected my-rig to be suspended")
			}
			return
		}
	}
	t.Error("my-rig not found after edit")
}

func TestSetRigSuspended_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	cfg, err := config.Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := configedit.SetRigSuspended(cfg, "nonexistent", true); err == nil {
		t.Error("expected error for nonexistent rig")
	}
}

func TestAgentOrigin_Inline(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	origin := configedit.AgentOrigin(cfg, cfg, "mayor")
	if origin != configedit.OriginInline {
		t.Errorf("got %v, want OriginInline", origin)
	}
}

func TestAgentOrigin_Derived(t *testing.T) {
	raw := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	expanded := &config.City{
		Agents: []config.Agent{
			{Name: "mayor"},
			{Name: "polecat", Dir: "my-rig"},
		},
	}
	origin := configedit.AgentOrigin(raw, expanded, "my-rig/polecat")
	if origin != configedit.OriginDerived {
		t.Errorf("got %v, want OriginDerived", origin)
	}
}

func TestAgentOrigin_NotFound(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	origin := configedit.AgentOrigin(cfg, cfg, "nonexistent")
	if origin != configedit.OriginNotFound {
		t.Errorf("got %v, want OriginNotFound", origin)
	}
}

func TestRigOrigin(t *testing.T) {
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "my-rig"}},
	}
	if configedit.RigOrigin(cfg, "my-rig") != configedit.OriginInline {
		t.Error("expected OriginInline for existing rig")
	}
	if configedit.RigOrigin(cfg, "nope") != configedit.OriginNotFound {
		t.Error("expected OriginNotFound for missing rig")
	}
}

func TestAddOrUpdateAgentPatch_New(t *testing.T) {
	cfg := &config.City{}
	err := configedit.AddOrUpdateAgentPatch(cfg, "my-rig/polecat", func(p *config.AgentPatch) {
		suspended := true
		p.Suspended = &suspended
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(cfg.Patches.Agents))
	}
	p := cfg.Patches.Agents[0]
	if p.Dir != "my-rig" || p.Name != "polecat" {
		t.Errorf("patch target = %s/%s, want my-rig/polecat", p.Dir, p.Name)
	}
	if p.Suspended == nil || !*p.Suspended {
		t.Error("expected suspended=true in patch")
	}
}

func TestAddOrUpdateAgentPatch_Existing(t *testing.T) {
	suspended := false
	cfg := &config.City{
		Patches: config.Patches{
			Agents: []config.AgentPatch{
				{Dir: "my-rig", Name: "polecat", Suspended: &suspended},
			},
		},
	}
	err := configedit.AddOrUpdateAgentPatch(cfg, "my-rig/polecat", func(p *config.AgentPatch) {
		s := true
		p.Suspended = &s
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Agents) != 1 {
		t.Fatalf("expected 1 patch (updated), got %d", len(cfg.Patches.Agents))
	}
	if cfg.Patches.Agents[0].Suspended == nil || !*cfg.Patches.Agents[0].Suspended {
		t.Error("expected suspended=true after update")
	}
}

func TestAddOrUpdateRigPatch(t *testing.T) {
	cfg := &config.City{}
	err := configedit.AddOrUpdateRigPatch(cfg, "my-rig", func(p *config.RigPatch) {
		s := true
		p.Suspended = &s
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Patches.Rigs) != 1 {
		t.Fatalf("expected 1 rig patch, got %d", len(cfg.Patches.Rigs))
	}
	if cfg.Patches.Rigs[0].Name != "my-rig" {
		t.Errorf("patch target = %s, want my-rig", cfg.Patches.Rigs[0].Name)
	}
}

func TestEdit_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	// Successful edit should leave no temp files.
	err := ed.Edit(func(cfg *config.City) error {
		cfg.Agents[0].Suspended = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "city.toml" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestSuspendAgent_Inline(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendAgent("mayor"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Agents[0].Suspended {
		t.Error("expected mayor to be suspended")
	}
}

func TestResumeAgent_Inline(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agents]]
name = "mayor"
provider = "claude"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeAgent("mayor"); err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Suspended {
		t.Error("expected mayor to not be suspended")
	}
}

func TestSuspendAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendAgent("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSuspendRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendRig("my-rig"); err != nil {
		t.Fatalf("SuspendRig: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Rigs[0].Suspended {
		t.Error("expected my-rig to be suspended")
	}
}

func TestResumeRig(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeRig("my-rig"); err != nil {
		t.Fatalf("ResumeRig: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Rigs[0].Suspended {
		t.Error("expected my-rig to not be suspended")
	}
}

func TestSuspendCity(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.SuspendCity(); err != nil {
		t.Fatalf("SuspendCity: %v", err)
	}

	cfg := readTOML(t, path)
	if !cfg.Workspace.Suspended {
		t.Error("expected workspace to be suspended")
	}
}

func TestResumeCity(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"
suspended = true
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.ResumeCity(); err != nil {
		t.Fatalf("ResumeCity: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Workspace.Suspended {
		t.Error("expected workspace to not be suspended")
	}
}

func TestCreateAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "coder", Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "coder" {
			found = true
		}
	}
	if !found {
		t.Error("agent 'coder' not found after create")
	}
}

func TestCreateAgent_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateAgent(config.Agent{Name: "mayor", Provider: "claude"})
	if err == nil {
		t.Error("expected error for duplicate agent")
	}
}

func TestUpdateAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("mayor", config.Agent{Name: "mayor", Provider: "gemini"})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Agents[0].Provider != "gemini" {
		t.Errorf("provider = %q, want %q", cfg.Agents[0].Provider, "gemini")
	}
}

func TestUpdateAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateAgent("nonexistent", config.Agent{Name: "nonexistent", Provider: "claude"})
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestDeleteAgent(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("mayor"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	cfg := readTOML(t, path)
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			t.Error("agent 'mayor' still exists after delete")
		}
	}
}

func TestDeleteAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteAgent("nonexistent"); err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestCreateRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateRig(config.Rig{Name: "new-rig", Path: "/tmp/new-rig"})
	if err != nil {
		t.Fatalf("CreateRig: %v", err)
	}

	cfg := readTOML(t, path)
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == "new-rig" {
			found = true
		}
	}
	if !found {
		t.Error("rig 'new-rig' not found after create")
	}
}

func TestCreateRig_Duplicate(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.CreateRig(config.Rig{Name: "my-rig", Path: "/tmp/x"})
	if err == nil {
		t.Error("expected error for duplicate rig")
	}
}

func TestUpdateRig(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, cityWithRig())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	err := ed.UpdateRig("my-rig", config.Rig{Path: "/tmp/updated"})
	if err != nil {
		t.Fatalf("UpdateRig: %v", err)
	}

	cfg := readTOML(t, path)
	if cfg.Rigs[0].Path != "/tmp/updated" {
		t.Errorf("path = %q, want %q", cfg.Rigs[0].Path, "/tmp/updated")
	}
}

func TestDeleteRig(t *testing.T) {
	dir := t.TempDir()
	city := `[workspace]
name = "test-city"

[[agents]]
name = "mayor"
provider = "claude"

[[agents]]
name = "polecat"
dir = "my-rig"
provider = "claude"

[[rigs]]
name = "my-rig"
path = "/tmp/my-rig"
`
	path := writeTOML(t, dir, city)
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("my-rig"); err != nil {
		t.Fatalf("DeleteRig: %v", err)
	}

	cfg := readTOML(t, path)
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			t.Error("rig 'my-rig' still exists after delete")
		}
	}
	// Rig-scoped agents should also be removed.
	for _, a := range cfg.Agents {
		if a.Dir == "my-rig" {
			t.Errorf("rig-scoped agent %q still exists after rig delete", a.QualifiedName())
		}
	}
	// City-scoped agent should remain.
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			found = true
		}
	}
	if !found {
		t.Error("city-scoped agent 'mayor' was incorrectly removed")
	}
}

func TestDeleteRig_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := writeTOML(t, dir, minimalCity())
	ed := configedit.NewEditor(fsys.OSFS{}, path)

	if err := ed.DeleteRig("nonexistent"); err == nil {
		t.Error("expected error for nonexistent rig")
	}
}
