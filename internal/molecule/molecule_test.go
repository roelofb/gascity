package molecule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestInstantiateSimple(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name:        "test-formula",
		Description: "A test formula",
		Steps: []formula.RecipeStep{
			{ID: "test-formula", Title: "{{title}}", Type: "epic", IsRoot: true},
			{ID: "test-formula.step-a", Title: "Step A", Type: "task"},
			{ID: "test-formula.step-b", Title: "Step B: {{feature}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "test-formula.step-a", DependsOnID: "test-formula", Type: "parent-child"},
			{StepID: "test-formula.step-b", DependsOnID: "test-formula", Type: "parent-child"},
			{StepID: "test-formula.step-b", DependsOnID: "test-formula.step-a", Type: "blocks"},
		},
		Vars: map[string]*formula.VarDef{
			"title":   {Description: "Title"},
			"feature": {Description: "Feature name"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{
		Title: "My Feature",
		Vars:  map[string]string{"feature": "auth"},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	if result.Created != 3 {
		t.Errorf("Created = %d, want 3", result.Created)
	}
	if result.RootID == "" {
		t.Fatal("RootID is empty")
	}

	// Verify root bead
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if root.Title != "My Feature" {
		t.Errorf("root.Title = %q, want %q", root.Title, "My Feature")
	}
	if root.Type != "molecule" {
		t.Errorf("root.Type = %q, want %q", root.Type, "molecule")
	}
	if root.Ref != "test-formula" {
		t.Errorf("root.Ref = %q, want %q", root.Ref, "test-formula")
	}

	// Verify step-b has variable substitution
	stepBID := result.IDMapping["test-formula.step-b"]
	stepB, err := store.Get(stepBID)
	if err != nil {
		t.Fatalf("Get step-b: %v", err)
	}
	if stepB.Title != "Step B: auth" {
		t.Errorf("step-b.Title = %q, want %q", stepB.Title, "Step B: auth")
	}
	if stepB.ParentID != result.RootID {
		t.Errorf("step-b.ParentID = %q, want %q", stepB.ParentID, result.RootID)
	}
}

func TestInstantiateWithParentID(t *testing.T) {
	store := beads.NewMemStore()

	// Create a parent bead first
	parent, err := store.Create(beads.Bead{Title: "Parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	recipe := &formula.Recipe{
		Name: "child-formula",
		Steps: []formula.RecipeStep{
			{ID: "child-formula", Title: "Child", Type: "epic", IsRoot: true},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{
		ParentID: parent.ID,
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	root, _ := store.Get(result.RootID)
	if root.ParentID != parent.ID {
		t.Errorf("root.ParentID = %q, want %q", root.ParentID, parent.ID)
	}
}

func TestInstantiateGraphWorkflowIgnoresParentIDOnRoot(t *testing.T) {
	store := beads.NewMemStore()

	parent, err := store.Create(beads.Bead{Title: "Source", Type: "task"})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	recipe := &formula.Recipe{
		Name: "graph-demo",
		Steps: []formula.RecipeStep{
			{
				ID:       "graph-demo",
				Title:    "Graph Demo",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "graph-demo.step",
				Title:    "Step",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "run"},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "graph-demo", DependsOnID: "graph-demo.step", Type: "blocks"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{ParentID: parent.ID})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if root.ParentID != "" {
		t.Fatalf("graph workflow root ParentID = %q, want empty", root.ParentID)
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
}

type recordingStore struct {
	beads.Store
	created []beads.Bead
	updates []struct {
		ID   string
		Opts beads.UpdateOpts
	}
}

func (r *recordingStore) Create(b beads.Bead) (beads.Bead, error) {
	r.created = append(r.created, b)
	return r.Store.Create(b)
}

func (r *recordingStore) Update(id string, opts beads.UpdateOpts) error {
	r.updates = append(r.updates, struct {
		ID   string
		Opts beads.UpdateOpts
	}{ID: id, Opts: opts})
	return r.Store.Update(id, opts)
}

func TestInstantiateGraphWorkflowDefersAssignmentsOnlyForFutureBlockers(t *testing.T) {
	base := beads.NewMemStore()
	store := &recordingStore{Store: base}

	recipe := &formula.Recipe{
		Name: "graph-assign",
		Steps: []formula.RecipeStep{
			{
				ID:       "graph-assign",
				Title:    "Graph Assign",
				Type:     "task",
				IsRoot:   true,
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			},
			{
				ID:       "graph-assign.run",
				Title:    "Run",
				Type:     "task",
				Assignee: "worker",
			},
			{
				ID:       "graph-assign.setup",
				Title:    "Setup",
				Type:     "task",
				Assignee: "worker",
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "graph-assign", DependsOnID: "graph-assign.run", Type: "blocks"},
			{StepID: "graph-assign.run", DependsOnID: "graph-assign.setup", Type: "blocks"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	createdByRef := make(map[string]beads.Bead, len(store.created))
	for _, created := range store.created {
		createdByRef[created.Ref] = created
	}
	if got := createdByRef["graph-assign.setup"].Assignee; got != "worker" {
		t.Fatalf("setup created assignee = %q, want worker", got)
	}
	if got := createdByRef["graph-assign.run"].Assignee; got != "" {
		t.Fatalf("run created assignee = %q, want empty until blocker wiring completes", got)
	}

	setup, err := base.Get(result.IDMapping["graph-assign.setup"])
	if err != nil {
		t.Fatalf("get setup: %v", err)
	}
	run, err := base.Get(result.IDMapping["graph-assign.run"])
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if setup.Assignee != "worker" {
		t.Fatalf("setup assignee = %q, want worker", setup.Assignee)
	}
	if run.Assignee != "worker" {
		t.Fatalf("run assignee = %q, want worker", run.Assignee)
	}

	if len(store.updates) < 1 {
		t.Fatalf("expected deferred assignment update for graph bead, got %d", len(store.updates))
	}
}

func TestInstantiateWithIdempotencyKey(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name: "idem-formula",
		Steps: []formula.RecipeStep{
			{ID: "idem-formula", Title: "Root", Type: "epic", IsRoot: true},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{
		IdempotencyKey: "converge:abc:iter:1",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	root, _ := store.Get(result.RootID)
	if root.Metadata["idempotency_key"] != "converge:abc:iter:1" {
		t.Errorf("idempotency_key = %q, want %q", root.Metadata["idempotency_key"], "converge:abc:iter:1")
	}
}

func TestInstantiateRootOnly(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{
		Name:     "patrol",
		RootOnly: true,
		Steps: []formula.RecipeStep{
			{ID: "patrol", Title: "Patrol", Type: "epic", IsRoot: true},
			{ID: "patrol.scan", Title: "Scan", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "patrol.scan", DependsOnID: "patrol", Type: "parent-child"},
		},
	}

	result, err := Instantiate(context.Background(), store, recipe, Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	if result.Created != 1 {
		t.Errorf("Created = %d, want 1 (root only)", result.Created)
	}

	all, _ := store.List()
	if len(all) != 1 {
		t.Errorf("store has %d beads, want 1", len(all))
	}
}

func TestInstantiateVarDefaults(t *testing.T) {
	store := beads.NewMemStore()
	defaultVal := "default-branch"
	recipe := &formula.Recipe{
		Name: "var-test",
		Steps: []formula.RecipeStep{
			{ID: "var-test", Title: "{{title}}", Type: "epic", IsRoot: true},
			{ID: "var-test.step", Title: "Branch: {{branch}}", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "var-test.step", DependsOnID: "var-test", Type: "parent-child"},
		},
		Vars: map[string]*formula.VarDef{
			"title":  {Description: "Title"},
			"branch": {Description: "Branch", Default: &defaultVal},
		},
	}

	// Don't provide "branch" — should use default
	result, err := Instantiate(context.Background(), store, recipe, Options{
		Vars: map[string]string{"title": "My Thing"},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	stepID := result.IDMapping["var-test.step"]
	step, _ := store.Get(stepID)
	if step.Title != "Branch: default-branch" {
		t.Errorf("step.Title = %q, want %q", step.Title, "Branch: default-branch")
	}
}

func TestInstantiateNilRecipe(t *testing.T) {
	store := beads.NewMemStore()
	_, err := Instantiate(context.Background(), store, nil, Options{})
	if err == nil {
		t.Fatal("expected error for nil recipe")
	}
}

func TestInstantiateEmptyRecipe(t *testing.T) {
	store := beads.NewMemStore()
	_, err := Instantiate(context.Background(), store, &formula.Recipe{Name: "empty"}, Options{})
	if err == nil {
		t.Fatal("expected error for empty recipe")
	}
}

// errStore fails on the Nth Create call.
type errStore struct {
	beads.Store
	failOnCreate int
	createCount  int
}

func (e *errStore) Create(b beads.Bead) (beads.Bead, error) {
	e.createCount++
	if e.createCount == e.failOnCreate {
		return beads.Bead{}, fmt.Errorf("injected create failure")
	}
	return e.Store.Create(b)
}

func TestInstantiateCreateFailure(t *testing.T) {
	base := beads.NewMemStore()
	store := &errStore{Store: base, failOnCreate: 2} // fail on second create (first step)

	recipe := &formula.Recipe{
		Name: "fail-test",
		Steps: []formula.RecipeStep{
			{ID: "fail-test", Title: "Root", Type: "epic", IsRoot: true},
			{ID: "fail-test.step", Title: "Step", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "fail-test.step", DependsOnID: "fail-test", Type: "parent-child"},
		},
	}

	_, err := Instantiate(context.Background(), store, recipe, Options{})
	if err == nil {
		t.Fatal("expected error on create failure")
	}

	// Root bead should exist but be marked as failed
	all, _ := base.List()
	if len(all) != 1 {
		t.Fatalf("expected 1 bead (root), got %d", len(all))
	}
	root, _ := base.Get(all[0].ID)
	if root.Metadata["molecule_failed"] != "true" {
		t.Error("root bead not marked as molecule_failed")
	}
}

// errDepStore fails on DepAdd.
type errDepStore struct {
	beads.Store
}

func (e *errDepStore) DepAdd(_, _, _ string) error {
	return fmt.Errorf("injected dep failure")
}

func TestInstantiateDepFailure(t *testing.T) {
	base := beads.NewMemStore()
	store := &errDepStore{Store: base}

	recipe := &formula.Recipe{
		Name: "dep-fail",
		Steps: []formula.RecipeStep{
			{ID: "dep-fail", Title: "Root", Type: "epic", IsRoot: true},
			{ID: "dep-fail.b", Title: "B", Type: "task"},
			{ID: "dep-fail.a", Title: "A", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "dep-fail.b", DependsOnID: "dep-fail", Type: "parent-child"},
			{StepID: "dep-fail.a", DependsOnID: "dep-fail", Type: "parent-child"},
			{StepID: "dep-fail.b", DependsOnID: "dep-fail.a", Type: "blocks"},
		},
	}

	_, err := Instantiate(context.Background(), store, recipe, Options{})
	if err == nil {
		t.Fatal("expected error on dep failure")
	}

	// All beads should be marked as failed
	all, _ := base.List()
	for _, b := range all {
		full, _ := base.Get(b.ID)
		if full.Metadata["molecule_failed"] != "true" {
			t.Errorf("bead %s not marked as molecule_failed", b.ID)
		}
	}
}

func TestCookOnRequiresParentID(t *testing.T) {
	store := beads.NewMemStore()
	_, err := CookOn(context.Background(), store, "x", nil, Options{})
	if err == nil {
		t.Fatal("expected error when ParentID is empty")
	}
}

func TestCookEndToEnd(t *testing.T) {
	// Write a minimal formula TOML to a temp directory.
	dir := t.TempDir()
	toml := `
formula = "e2e-test"
description = "End-to-end Cook test"

[vars.title]
description = "Title"

[[steps]]
id = "implement"
title = "Implement {{title}}"

[[steps]]
id = "verify"
title = "Verify {{title}}"
depends_on = ["implement"]
`
	if err := os.WriteFile(filepath.Join(dir, "e2e-test.formula.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "e2e-test", []string{dir}, Options{
		Title: "Auth Flow",
		Vars:  map[string]string{"title": "Auth Flow"},
	})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}

	if result.RootID == "" {
		t.Fatal("RootID is empty")
	}
	if result.Created != 3 {
		t.Errorf("Created = %d, want 3 (root + 2 steps)", result.Created)
	}

	// Verify root bead.
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if root.Title != "Auth Flow" {
		t.Errorf("root.Title = %q, want %q", root.Title, "Auth Flow")
	}
	if root.Type != "molecule" {
		t.Errorf("root.Type = %q, want %q", root.Type, "molecule")
	}

	// Verify step substitution.
	implID := result.IDMapping["e2e-test.implement"]
	impl, err := store.Get(implID)
	if err != nil {
		t.Fatalf("Get implement: %v", err)
	}
	if impl.Title != "Implement Auth Flow" {
		t.Errorf("implement.Title = %q, want %q", impl.Title, "Implement Auth Flow")
	}

	// Verify dependency wiring.
	verifyID := result.IDMapping["e2e-test.verify"]
	verify, err := store.Get(verifyID)
	if err != nil {
		t.Fatalf("Get verify: %v", err)
	}
	if verify.ParentID != result.RootID {
		t.Errorf("verify.ParentID = %q, want %q", verify.ParentID, result.RootID)
	}
}

func TestCookEndToEndRalph(t *testing.T) {
	dir := t.TempDir()
	toml := `
formula = "ralph-demo"
description = "Ralph cook test"

[[steps]]
id = "design"
title = "Design"

[[steps]]
id = "implement"
title = "Implement"
needs = ["design"]

[steps.metadata]
custom = "value"

[steps.ralph]
max_attempts = 3

[steps.ralph.check]
mode = "exec"
path = ".gascity/checks/widget.sh"
timeout = "2m"
`
	if err := os.WriteFile(filepath.Join(dir, "ralph-demo.formula.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "ralph-demo", []string{dir}, Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}

	if result.Created != 5 {
		t.Fatalf("Created = %d, want 5 (root + design + logical + run + check)", result.Created)
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	logical, err := store.Get(result.IDMapping["ralph-demo.implement"])
	if err != nil {
		t.Fatalf("get logical: %v", err)
	}
	run, err := store.Get(result.IDMapping["ralph-demo.implement.run.1"])
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	check, err := store.Get(result.IDMapping["ralph-demo.implement.check.1"])
	if err != nil {
		t.Fatalf("get check: %v", err)
	}

	if logical.Metadata["gc.kind"] != "ralph" {
		t.Fatalf("logical gc.kind = %q, want ralph", logical.Metadata["gc.kind"])
	}
	if root.Metadata["gc.kind"] != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", root.Metadata["gc.kind"])
	}
	if root.Type != "task" {
		t.Fatalf("root type = %q, want task", root.Type)
	}
	if run.Metadata["gc.kind"] != "run" {
		t.Fatalf("run gc.kind = %q, want run", run.Metadata["gc.kind"])
	}
	if run.ParentID != "" {
		t.Fatalf("run ParentID = %q, want detached graph node", run.ParentID)
	}
	if run.Metadata["gc.logical_bead_id"] != logical.ID {
		t.Fatalf("run gc.logical_bead_id = %q, want %q", run.Metadata["gc.logical_bead_id"], logical.ID)
	}
	if run.Metadata["gc.root_bead_id"] != result.RootID {
		t.Fatalf("run gc.root_bead_id = %q, want %q", run.Metadata["gc.root_bead_id"], result.RootID)
	}
	if run.Metadata["custom"] != "value" {
		t.Fatalf("run custom metadata = %q, want value", run.Metadata["custom"])
	}
	if check.Metadata["gc.kind"] != "check" {
		t.Fatalf("check gc.kind = %q, want check", check.Metadata["gc.kind"])
	}
	if check.ParentID != "" {
		t.Fatalf("check ParentID = %q, want detached graph node", check.ParentID)
	}
	if check.Metadata["gc.logical_bead_id"] != logical.ID {
		t.Fatalf("check gc.logical_bead_id = %q, want %q", check.Metadata["gc.logical_bead_id"], logical.ID)
	}
	if check.Metadata["gc.root_bead_id"] != result.RootID {
		t.Fatalf("check gc.root_bead_id = %q, want %q", check.Metadata["gc.root_bead_id"], result.RootID)
	}
	if check.Metadata["gc.check_path"] != ".gascity/checks/widget.sh" {
		t.Fatalf("check gc.check_path = %q, want .gascity/checks/widget.sh", check.Metadata["gc.check_path"])
	}

	checkDeps, err := store.DepList(check.ID, "down")
	if err != nil {
		t.Fatalf("dep list check: %v", err)
	}
	foundRunBlock := false
	for _, dep := range checkDeps {
		if dep.Type == "blocks" && dep.DependsOnID == run.ID {
			foundRunBlock = true
			break
		}
	}
	if !foundRunBlock {
		t.Fatalf("check bead does not block on run bead; deps=%v", checkDeps)
	}

	logicalDeps, err := store.DepList(logical.ID, "down")
	if err != nil {
		t.Fatalf("dep list logical: %v", err)
	}
	foundCheckBlock := false
	for _, dep := range logicalDeps {
		if dep.Type == "blocks" && dep.DependsOnID == check.ID {
			foundCheckBlock = true
			break
		}
	}
	if !foundCheckBlock {
		t.Fatalf("logical bead does not block on check bead; deps=%v", logicalDeps)
	}

	rootDeps, err := store.DepList(root.ID, "down")
	if err != nil {
		t.Fatalf("dep list root: %v", err)
	}
	foundDesignBlock := false
	foundLogicalBlock := false
	for _, dep := range rootDeps {
		if dep.Type != "blocks" {
			continue
		}
		switch dep.DependsOnID {
		case result.IDMapping["ralph-demo.design"]:
			foundDesignBlock = true
		case logical.ID:
			foundLogicalBlock = true
		}
	}
	if !foundDesignBlock {
		t.Fatalf("root bead does not block on design bead; deps=%v", rootDeps)
	}
	if !foundLogicalBlock {
		t.Fatalf("root bead does not block on logical bead; deps=%v", rootDeps)
	}
}

func TestCookEndToEndScopedWorkflowStampsRootAndScopeMetadata(t *testing.T) {
	dir := t.TempDir()
	toml := `
formula = "scoped-demo"
version = 2

[[steps]]
id = "body"
title = "Body"
needs = ["work"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "worktree" }

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope", "gc.continuation_group" = "main" }

[[steps]]
id = "cleanup"
title = "Cleanup"
needs = ["body"]
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "teardown", "gc.kind" = "cleanup" }
`
	if err := os.WriteFile(filepath.Join(dir, "scoped-demo.formula.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	store := beads.NewMemStore()
	result, err := Cook(context.Background(), store, "scoped-demo", []string{dir}, Options{})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	if !result.GraphWorkflow {
		t.Fatal("result.GraphWorkflow = false, want true")
	}

	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got := root.Metadata["gc.kind"]; got != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", got)
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}

	body, err := store.Get(result.IDMapping["scoped-demo.body"])
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if got := body.Metadata["gc.step_ref"]; got != "scoped-demo.body" {
		t.Fatalf("body gc.step_ref = %q, want %q", got, "scoped-demo.body")
	}

	work, err := store.Get(result.IDMapping["scoped-demo.work"])
	if err != nil {
		t.Fatalf("get work: %v", err)
	}
	if got := work.Metadata["gc.root_bead_id"]; got != result.RootID {
		t.Fatalf("work gc.root_bead_id = %q, want %q", got, result.RootID)
	}
	if got := work.Metadata["gc.step_ref"]; got != "scoped-demo.work" {
		t.Fatalf("work gc.step_ref = %q, want %q", got, "scoped-demo.work")
	}
	if got := work.Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("work gc.scope_ref = %q, want %q", got, "body")
	}

	cleanup, err := store.Get(result.IDMapping["scoped-demo.cleanup"])
	if err != nil {
		t.Fatalf("get cleanup: %v", err)
	}
	if got := cleanup.Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("cleanup gc.scope_ref = %q, want %q", got, "body")
	}

	scopeCheck, err := store.Get(result.IDMapping["scoped-demo.work-scope-check"])
	if err != nil {
		t.Fatalf("get scope-check: %v", err)
	}
	if got := scopeCheck.Metadata["gc.scope_ref"]; got != "body" {
		t.Fatalf("scope-check gc.scope_ref = %q, want %q", got, "body")
	}
	if got := scopeCheck.Metadata["gc.root_bead_id"]; got != result.RootID {
		t.Fatalf("scope-check gc.root_bead_id = %q, want %q", got, result.RootID)
	}
	if got := scopeCheck.Metadata["gc.step_ref"]; got != "scoped-demo.work-scope-check" {
		t.Fatalf("scope-check gc.step_ref = %q, want %q", got, "scoped-demo.work-scope-check")
	}

	finalizer, err := store.Get(result.IDMapping["scoped-demo.workflow-finalize"])
	if err != nil {
		t.Fatalf("get workflow-finalize: %v", err)
	}
	if got := finalizer.Metadata["gc.root_bead_id"]; got != result.RootID {
		t.Fatalf("workflow-finalize gc.root_bead_id = %q, want %q", got, result.RootID)
	}
}
