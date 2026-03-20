package workflow

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestProcessScopeCheckClosesScopeOnSuccess(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "demo.body",
		},
	})
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "implement",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "pass",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "cleanup",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "cleanup",
			"gc.root_bead_id": workflow.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "teardown",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, control.ID, step.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, cleanup.ID, body.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")

	result, err := ProcessControl(store, control)
	if err != nil {
		t.Fatalf("ProcessControl(scope-check): %v", err)
	}
	if !result.Processed || result.Action != "scope-pass" {
		t.Fatalf("scope result = %+v, want processed scope-pass", result)
	}

	bodyAfter, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("body outcome = %q, want pass", got)
	}

	cleanupReady := mustReadyContains(t, store, cleanup.ID)
	if !cleanupReady {
		t.Fatalf("cleanup %s should be ready after body closes", cleanup.ID)
	}
}

func TestProcessScopeCheckAbortsScopeOnFailure(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.scope_role":   "body",
			"gc.root_bead_id": "wf-1",
			"gc.step_ref":     "demo.body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	control = mustGetBead(t, store, control.ID)
	futureControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	futureMember := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "cleanup",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "cleanup",
			"gc.root_bead_id": "wf-1",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "teardown",
		},
	})

	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, cleanup.ID, body.ID, "blocks")
	mustDepAdd(t, store, futureMember.ID, control.ID, "blocks")
	mustDepAdd(t, store, futureControl.ID, futureMember.ID, "blocks")

	result, err := ProcessControl(store, control)
	if err != nil {
		t.Fatalf("ProcessControl(scope-check fail): %v", err)
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("scope result = %+v, want processed scope-fail", result)
	}
	if result.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", result.Skipped)
	}

	bodyAfter, err := store.Get(body.ID)
	if err != nil {
		t.Fatalf("get body: %v", err)
	}
	if bodyAfter.Status != "closed" {
		t.Fatalf("body status = %q, want closed", bodyAfter.Status)
	}
	if got := bodyAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}

	for _, beadID := range []string{futureMember.ID, futureControl.ID} {
		member, err := store.Get(beadID)
		if err != nil {
			t.Fatalf("get skipped member %s: %v", beadID, err)
		}
		if member.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, member.Status)
		}
		if got := member.Metadata["gc.outcome"]; got != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", beadID, got)
		}
	}

	cleanupReady := mustReadyContains(t, store, cleanup.ID)
	if !cleanupReady {
		t.Fatalf("cleanup %s should be ready after body fails closed", cleanup.ID)
	}
}

type strictCloseStore struct {
	*beads.MemStore
}

func newStrictCloseStore() *strictCloseStore {
	return &strictCloseStore{MemStore: beads.NewMemStore()}
}

func (s *strictCloseStore) Close(id string) error {
	deps, err := s.DepList(id, "down")
	if err != nil {
		return err
	}
	var openBlockers []string
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		blocker, err := s.Get(dep.DependsOnID)
		if err != nil {
			return err
		}
		if blocker.Status == "open" {
			openBlockers = append(openBlockers, blocker.ID)
		}
	}
	if len(openBlockers) > 0 {
		return fmt.Errorf("cannot close %s: blocked by open issues %v", id, openBlockers)
	}
	return s.MemStore.Close(id)
}

func TestProcessWorkflowFinalizeClosesWorkflow(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "cleanup",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "fail",
		},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})

	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	result, err := ProcessControl(store, finalizer)
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("workflow result = %+v, want processed workflow-fail", result)
	}

	rootAfter, err := store.Get(workflow.ID)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if rootAfter.Status != "closed" {
		t.Fatalf("workflow status = %q, want closed", rootAfter.Status)
	}
	if got := rootAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("workflow outcome = %q, want fail", got)
	}
}

func mustCreateWorkflowBead(t *testing.T, store beads.Store, bead beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("create bead %q: %v", bead.Title, err)
	}
	if bead.Status == "closed" {
		if err := store.Close(created.ID); err != nil {
			t.Fatalf("close bead %q: %v", bead.Title, err)
		}
		created, err = store.Get(created.ID)
		if err != nil {
			t.Fatalf("reload closed bead %q: %v", bead.Title, err)
		}
	}
	for k, v := range bead.Metadata {
		if err := store.SetMetadata(created.ID, k, v); err != nil {
			t.Fatalf("set metadata on %q: %v", bead.Title, err)
		}
	}
	if len(bead.Labels) > 0 {
		if err := store.Update(created.ID, beads.UpdateOpts{Labels: bead.Labels}); err != nil {
			t.Fatalf("add labels to %q: %v", bead.Title, err)
		}
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("reload bead %q: %v", bead.Title, err)
	}
	return created
}

func mustDepAdd(t *testing.T, store beads.Store, issueID, dependsOnID, depType string) {
	t.Helper()
	if err := store.DepAdd(issueID, dependsOnID, depType); err != nil {
		t.Fatalf("dep add %s --%s--> %s: %v", issueID, depType, dependsOnID, err)
	}
}

func mustReadyContains(t *testing.T, store beads.Store, beadID string) bool {
	t.Helper()
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("store.Ready: %v", err)
	}
	for _, bead := range ready {
		if bead.ID == beadID {
			return true
		}
	}
	return false
}

func mustGetBead(t *testing.T, store beads.Store, beadID string) beads.Bead {
	t.Helper()
	bead, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get bead %s: %v", beadID, err)
	}
	return bead
}
