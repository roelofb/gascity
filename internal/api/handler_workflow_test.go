package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestWorkflowGetBuildsSnapshot(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	session, err := cityStore.Create(beads.Bead{
		Title:  "Mayor Session",
		Type:   "task",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name":  "alpha--mayor",
			"agent_name":    "alpha/mayor",
			"mc_project_id": "proj-123",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	root, err := cityStore.Create(beads.Bead{
		Title: "Adopt PR",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_123",
			"gc.routed_to":        "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	body, err := cityStore.Create(beads.Bead{
		Title: "Review Scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.body",
			"gc.scope_role":   "body",
		},
	})
	if err != nil {
		t.Fatalf("Create(body): %v", err)
	}

	logical, err := cityStore.Create(beads.Bead{
		Title: "Review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.scope_ref":    "body",
			"gc.max_attempts": "3",
		},
	})
	if err != nil {
		t.Fatalf("Create(logical): %v", err)
	}

	run, err := cityStore.Create(beads.Bead{
		Title:    "Review attempt 1",
		Type:     "task",
		Assignee: "alpha--mayor",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.routed_to":       "alpha/mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(run): %v", err)
	}
	inProgress := "in_progress"
	if err := cityStore.Update(run.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(run status): %v", err)
	}

	eval, err := cityStore.Create(beads.Bead{
		Title: "Evaluate review attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
		},
	})
	if err != nil {
		t.Fatalf("Create(eval): %v", err)
	}

	control, err := cityStore.Create(beads.Bead{
		Title: "Finalize review scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review-scope-check",
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	if err != nil {
		t.Fatalf("Create(control): %v", err)
	}

	if err := cityStore.DepAdd(logical.ID, body.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(logical, body): %v", err)
	}
	if err := cityStore.DepAdd(eval.ID, run.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(eval, run): %v", err)
	}
	if err := cityStore.DepAdd(logical.ID, eval.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(logical, eval): %v", err)
	}
	if err := cityStore.DepAdd(control.ID, logical.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(control, logical): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_123?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if snapshot.WorkflowID != "wf_123" {
		t.Fatalf("workflow_id = %q, want wf_123", snapshot.WorkflowID)
	}
	if snapshot.RootBeadID != root.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, root.ID)
	}
	if snapshot.RootStoreRef != "city:test-city" {
		t.Fatalf("root_store_ref = %q, want city:test-city", snapshot.RootStoreRef)
	}
	if snapshot.ScopeKind != "city" || snapshot.ScopeRef != "test-city" {
		t.Fatalf("scope = %s:%s, want city:test-city", snapshot.ScopeKind, snapshot.ScopeRef)
	}

	logicalNode := findLogicalNode(snapshot.LogicalNodes, logical.ID)
	if logicalNode == nil {
		t.Fatalf("logical node %q not found", logical.ID)
	}
	if logicalNode.ScopeRef != "demo.body" {
		t.Fatalf("logical scope_ref = %q, want demo.body", logicalNode.ScopeRef)
	}
	if logicalNode.CurrentBeadID != run.ID {
		t.Fatalf("logical current_bead_id = %q, want %q", logicalNode.CurrentBeadID, run.ID)
	}
	if logicalNode.AttemptBadge != "1/3" {
		t.Fatalf("logical attempt_badge = %q, want 1/3", logicalNode.AttemptBadge)
	}
	if logicalNode.SessionLink == nil || logicalNode.SessionLink.SessionID != session.ID {
		t.Fatalf("logical session_link = %+v, want session %s", logicalNode.SessionLink, session.ID)
	}
	if logicalNode.SessionLink.ProjectID != "proj-123" {
		t.Fatalf("logical session_link.project_id = %q, want proj-123", logicalNode.SessionLink.ProjectID)
	}
	if logicalNode.SessionLink.SessionName != "alpha--mayor" {
		t.Fatalf("logical session_link.session_name = %q, want alpha--mayor", logicalNode.SessionLink.SessionName)
	}
	if logicalNode.SessionLink.Assignee != "alpha--mayor" {
		t.Fatalf("logical session_link.assignee = %q, want alpha--mayor", logicalNode.SessionLink.Assignee)
	}

	scopeGroup := findScopeGroup(snapshot.ScopeGroups, "demo.body")
	if scopeGroup == nil {
		t.Fatalf("scope group demo.body not found: %+v", snapshot.ScopeGroups)
	}
	if scopeGroup.Label != body.Title {
		t.Fatalf("scope group label = %q, want %q", scopeGroup.Label, body.Title)
	}
	if !containsString(scopeGroup.MemberLogicalNodeIDs, logical.ID) {
		t.Fatalf("scope group members = %v, want %s", scopeGroup.MemberLogicalNodeIDs, logical.ID)
	}

	if !hasEdge(snapshot.LogicalEdges, body.ID, logical.ID, "blocks") {
		t.Fatalf("logical edges = %+v, want %s -> %s", snapshot.LogicalEdges, body.ID, logical.ID)
	}
}

func TestLogicalStepRefForAttemptBeadPrefersNestedAttemptOverOuterRalphScope(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Metadata: map[string]string{
			"gc.kind":     "scope-check",
			"gc.attempt":  "3",
			"gc.step_ref": "mol-adopt-pr-v2.review-loop.run.3.review-pipeline.review-codex.eval.1-scope-check",
		},
	}

	if got := logicalStepRefForAttemptBead(bead); got != "mol-adopt-pr-v2.review-loop.run.3.review-pipeline.review-codex" {
		t.Fatalf("logicalStepRefForAttemptBead(scope-check) = %q, want nested retry logical step", got)
	}
}

func TestLogicalStepRefForAttemptBeadMapsFlatScopeCheckToControlStep(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Metadata: map[string]string{
			"gc.kind":     "scope-check",
			"gc.step_ref": "demo.review-scope-check",
		},
	}

	if got := logicalStepRefForAttemptBead(bead); got != "demo.review" {
		t.Fatalf("logicalStepRefForAttemptBead(flat scope-check) = %q, want demo.review", got)
	}
}

func TestLogicalGroupIDPrefersExactScopeCheckTargetStep(t *testing.T) {
	t.Parallel()

	workflowBeads := []beads.Bead{
		{
			ID: "parent",
			Metadata: map[string]string{
				"gc.kind":     "ralph",
				"gc.step_ref": "demo.loop",
			},
		},
		{
			ID: "child",
			Metadata: map[string]string{
				"gc.kind":     "retry",
				"gc.step_ref": "demo.loop.run.1.child",
			},
		},
	}
	scopeCheck := beads.Bead{
		ID: "child-scope-check",
		Metadata: map[string]string{
			"gc.kind":     "scope-check",
			"gc.attempt":  "1",
			"gc.step_ref": "demo.loop.run.1.child-scope-check",
		},
	}

	if got := logicalGroupID(workflowBeads, scopeCheck); got != "child" {
		t.Fatalf("logicalGroupID(child scope-check) = %q, want child", got)
	}
}

func TestWorkflowGetSelectsScopedRootMatch(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.stores = map[string]beads.Store{"alpha": rigStore}

	_, err := cityStore.Create(beads.Bead{
		Title: "City workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_shared",
		},
	})
	if err != nil {
		t.Fatalf("Create(cityRoot): %v", err)
	}
	rigRoot, err := rigStore.Create(beads.Bead{
		Title: "Rig workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_shared",
		},
	})
	if err != nil {
		t.Fatalf("Create(rigRoot): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_shared?scope_kind=rig&scope_ref=alpha", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if snapshot.RootBeadID != rigRoot.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, rigRoot.ID)
	}
	if snapshot.RootStoreRef != "rig:alpha" {
		t.Fatalf("root_store_ref = %q, want rig:alpha", snapshot.RootStoreRef)
	}
	if snapshot.ScopeKind != "rig" || snapshot.ScopeRef != "alpha" {
		t.Fatalf("scope = %s:%s, want rig:alpha", snapshot.ScopeKind, snapshot.ScopeRef)
	}
	if len(snapshot.Beads) == 0 || snapshot.Beads[0].Title != rigRoot.Title {
		t.Fatalf("selected workflow title = %q, want %q", firstWorkflowBeadTitle(snapshot.Beads), rigRoot.Title)
	}
}

func TestWorkflowGetPreservesRequestedScopeForUniqueCrossStoreWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Cross-store workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_city_scope",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_city_scope?scope_kind=city&scope_ref=gascity", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if snapshot.RootBeadID != root.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, root.ID)
	}
	if snapshot.RootStoreRef != "rig:alpha" {
		t.Fatalf("root_store_ref = %q, want rig:alpha", snapshot.RootStoreRef)
	}
	if snapshot.ScopeKind != "city" || snapshot.ScopeRef != "gascity" {
		t.Fatalf("scope = %s:%s, want city:gascity", snapshot.ScopeKind, snapshot.ScopeRef)
	}
}

func TestWorkflowGetRejectsMismatchedCityScopeForUniqueCrossStoreWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	if _, err := rigStore.Create(beads.Bead{
		Title: "Cross-store workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_wrong_city_scope",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_wrong_city_scope?scope_kind=city&scope_ref=other-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowGetRejectsInvalidScopeKind(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	if _, err := cityStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_invalid_scope",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_invalid_scope?scope_kind=workspace&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowGetRejectsMismatchedRigScopeForUniqueCrossStoreWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	if _, err := rigStore.Create(beads.Bead{
		Title: "Rig workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_rig_only",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_rig_only?scope_kind=rig&scope_ref=beta", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowGetMarksSnapshotPartialWhenDepListFails(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	memStore := beads.NewMemStore()
	state.cityBeadStore = depListFailStore{Store: memStore}

	root, err := memStore.Create(beads.Bead{
		Title: "Workflow with dep errors",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_partial",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if _, err := memStore.Create(beads.Bead{
		Title: "Work",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.work",
		},
	}); err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_partial?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}
	if !snapshot.Partial {
		t.Fatalf("partial = %v, want true", snapshot.Partial)
	}
}

func TestWorkflowGetRequiresScopeFields(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	state.cityBeadStore = beads.NewMemStore()

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_missing_scope", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "scope_kind and scope_ref are required") {
		t.Fatalf("body = %s, want scope requirement error", rec.Body.String())
	}
}

func TestWorkflowGetScopedRequestSurvivesUnrelatedStoreListFailure(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.stores = map[string]beads.Store{
		"alpha": failListStore{Store: beads.NewMemStore()},
	}

	if _, err := cityStore.Create(beads.Bead{
		Title: "City workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_city_partial",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_city_partial?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}
	if snapshot.RootStoreRef != "city:test-city" {
		t.Fatalf("root_store_ref = %q, want city:test-city", snapshot.RootStoreRef)
	}
	if !snapshot.Partial {
		t.Fatalf("partial = %v, want true", snapshot.Partial)
	}
}

func TestWorkflowGetUsesSingleSnapshotIndexForHeaderAndBody(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	state.cityBeadStore = beads.NewMemStore()
	state.eventProv = &incrementingLatestSeqProvider{}

	if _, err := state.cityBeadStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_index",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_index?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if got := rec.Header().Get("X-GC-Index"); got != "1" {
		t.Fatalf("X-GC-Index = %q, want 1", got)
	}
	if snapshot.SnapshotVersion != 1 {
		t.Fatalf("snapshot_version = %d, want 1", snapshot.SnapshotVersion)
	}
	if snapshot.SnapshotEventSeq == nil || *snapshot.SnapshotEventSeq != 1 {
		t.Fatalf("snapshot_event_seq = %v, want 1", snapshot.SnapshotEventSeq)
	}
}

func TestWorkflowStoreByRef(t *testing.T) {
	state := newFakeState(t)
	state.cityName = ""
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{
		"alpha": beads.NewMemStore(),
	}

	cityInfo, ok := workflowStoreByRef(state, "city:city")
	if !ok {
		t.Fatal("workflowStoreByRef(city:city) = false, want true")
	}
	if cityInfo.ref != "city:city" || cityInfo.scopeKind != "city" || cityInfo.scopeRef != "city" {
		t.Fatalf("city info = %+v, want city:city", cityInfo)
	}
	if cityInfo.store != state.cityBeadStore {
		t.Fatal("city store mismatch")
	}

	rigInfo, ok := workflowStoreByRef(state, "rig:alpha")
	if !ok {
		t.Fatal("workflowStoreByRef(rig:alpha) = false, want true")
	}
	if rigInfo.ref != "rig:alpha" || rigInfo.scopeKind != "rig" || rigInfo.scopeRef != "alpha" {
		t.Fatalf("rig info = %+v, want rig:alpha", rigInfo)
	}
	if rigInfo.store != state.stores["alpha"] {
		t.Fatal("rig store mismatch")
	}

	if _, ok := workflowStoreByRef(state, "city:bright-lights"); ok {
		t.Fatal("workflowStoreByRef(city:bright-lights) = true, want false")
	}
	if _, ok := workflowStoreByRef(state, "rig:missing"); ok {
		t.Fatal("workflowStoreByRef(rig:missing) = true, want false")
	}
}

func TestWorkflowStoresSkipsCityStoreEntriesFromBeadStoreMap(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: t.TempDir()},
	}
	state.stores = map[string]beads.Store{
		"bright-lights": cityStore,
		"alpha":         beads.NewMemStore(),
	}

	stores := workflowStores(state)
	if len(stores) != 2 {
		t.Fatalf("workflowStores() returned %d entries, want 2", len(stores))
	}
	if stores[0].ref != "city:bright-lights" {
		t.Fatalf("stores[0].ref = %q, want city:bright-lights", stores[0].ref)
	}
	if stores[1].ref != "rig:alpha" {
		t.Fatalf("stores[1].ref = %q, want rig:alpha", stores[1].ref)
	}
}

func TestWorkflowGetNormalizesShortScopeRefs(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	root, err := cityStore.Create(beads.Bead{
		Title: "Scoped workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	_, err = cityStore.Create(beads.Bead{
		Title: "Worktree scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "expansion.worktree",
			"gc.scope_role":   "body",
		},
	})
	if err != nil {
		t.Fatalf("Create(body): %v", err)
	}

	member, err := cityStore.Create(beads.Bead{
		Title: "Implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "expansion.implement",
			"gc.scope_ref":    "worktree",
			"gc.scope_role":   "member",
		},
	})
	if err != nil {
		t.Fatalf("Create(member): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/"+root.ID+"?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	memberNode := findLogicalNode(snapshot.LogicalNodes, member.ID)
	if memberNode == nil {
		t.Fatalf("member node %q not found", member.ID)
	}
	if memberNode.ScopeRef != "expansion.worktree" {
		t.Fatalf("member scope_ref = %q, want expansion.worktree", memberNode.ScopeRef)
	}

	scopeGroup := findScopeGroup(snapshot.ScopeGroups, "expansion.worktree")
	if scopeGroup == nil {
		t.Fatalf("scope groups = %+v, want expansion.worktree", snapshot.ScopeGroups)
	}
}

func TestWorkflowGetTracksMultiAttemptRetryState(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	root, err := cityStore.Create(beads.Bead{
		Title: "Retry workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_retry",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	logical, err := cityStore.Create(beads.Bead{
		Title: "Review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
		},
	})
	if err != nil {
		t.Fatalf("Create(logical): %v", err)
	}

	run1, err := cityStore.Create(beads.Bead{
		Title: "Review attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.outcome":         "fail",
		},
	})
	if err != nil {
		t.Fatalf("Create(run1): %v", err)
	}
	if err := cityStore.Close(run1.ID); err != nil {
		t.Fatalf("Close(run1): %v", err)
	}

	eval1, err := cityStore.Create(beads.Bead{
		Title: "Evaluate review attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
		},
	})
	if err != nil {
		t.Fatalf("Create(eval1): %v", err)
	}
	if err := cityStore.Close(eval1.ID); err != nil {
		t.Fatalf("Close(eval1): %v", err)
	}

	run2, err := cityStore.Create(beads.Bead{
		Title: "Review attempt 2",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review.run.2",
			"gc.attempt":         "2",
			"gc.max_attempts":    "3",
		},
	})
	if err != nil {
		t.Fatalf("Create(run2): %v", err)
	}
	if err := cityStore.Close(run2.ID); err != nil {
		t.Fatalf("Close(run2): %v", err)
	}

	if _, err := cityStore.Create(beads.Bead{
		Title:  "Evaluate review attempt 2",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review.eval.2",
			"gc.attempt":         "2",
			"gc.max_attempts":    "3",
		},
	}); err != nil {
		t.Fatalf("Create(eval2): %v", err)
	}

	run3, err := cityStore.Create(beads.Bead{
		Title:    "Review attempt 3",
		Type:     "task",
		Assignee: "mayor",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review.run.3",
			"gc.attempt":         "3",
			"gc.max_attempts":    "3",
			"gc.routed_to":       "mayor",
		},
	})
	if err != nil {
		t.Fatalf("Create(run3): %v", err)
	}
	inProgress := "in_progress"
	if err := cityStore.Update(run3.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update(run3 status): %v", err)
	}

	if _, err := cityStore.Create(beads.Bead{
		Title:  "Evaluate review attempt 3",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.step_ref":        "demo.review.eval.3",
			"gc.attempt":         "3",
			"gc.max_attempts":    "3",
		},
	}); err != nil {
		t.Fatalf("Create(eval3): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_retry?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	logicalNode := findLogicalNode(snapshot.LogicalNodes, logical.ID)
	if logicalNode == nil {
		t.Fatalf("logical node %q not found", logical.ID)
	}
	if logicalNode.CurrentBeadID != run3.ID {
		t.Fatalf("current_bead_id = %q, want %q", logicalNode.CurrentBeadID, run3.ID)
	}
	if logicalNode.Status != "active" {
		t.Fatalf("status = %q, want active", logicalNode.Status)
	}
	if logicalNode.AttemptBadge != "3/3" {
		t.Fatalf("attempt_badge = %q, want 3/3", logicalNode.AttemptBadge)
	}
	if logicalNode.AttemptCount == nil || *logicalNode.AttemptCount != 3 {
		t.Fatalf("attempt_count = %v, want 3", logicalNode.AttemptCount)
	}
	if logicalNode.ActiveAttempt == nil || *logicalNode.ActiveAttempt != 3 {
		t.Fatalf("active_attempt = %v, want 3", logicalNode.ActiveAttempt)
	}
	if len(logicalNode.Attempts) != 3 {
		t.Fatalf("attempts = %+v, want 3 entries", logicalNode.Attempts)
	}
	if logicalNode.Attempts[0].Attempt != 1 || logicalNode.Attempts[0].Status != "failed" || logicalNode.Attempts[0].BeadID != run1.ID {
		t.Fatalf("attempt[0] = %+v, want failed attempt 1 rooted at %s", logicalNode.Attempts[0], run1.ID)
	}
	if logicalNode.Attempts[1].Attempt != 2 || logicalNode.Attempts[1].Status != "pending" || logicalNode.Attempts[1].BeadID != run2.ID {
		t.Fatalf("attempt[1] = %+v, want pending attempt 2 rooted at %s", logicalNode.Attempts[1], run2.ID)
	}
	if logicalNode.Attempts[2].Attempt != 3 || logicalNode.Attempts[2].Status != "active" || logicalNode.Attempts[2].BeadID != run3.ID {
		t.Fatalf("attempt[2] = %+v, want active attempt 3 rooted at %s", logicalNode.Attempts[2], run3.ID)
	}
}

func TestWorkflowStatusTreatsOpenAssignedWorkAsPending(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status:   "open",
		Assignee: "mayor",
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}

	if got := workflowStatus(bead); got != "pending" {
		t.Fatalf("workflowStatus(open assigned) = %q, want pending", got)
	}
}

func TestWorkflowStatusRequiresAssignmentForActive(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status: "in_progress",
	}

	if got := workflowStatus(bead); got != "pending" {
		t.Fatalf("workflowStatus(in_progress unassigned) = %q, want pending", got)
	}
}

func TestWorkflowStatusDoesNotTreatRoutedOnlyWorkAsActive(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}

	if got := workflowStatus(bead); got != "pending" {
		t.Fatalf("workflowStatus(in_progress routed-only) = %q, want pending", got)
	}
}

func TestWorkflowGetRejectsNonWorkflowRoot(t *testing.T) {
	state := newFakeState(t)
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	bead, err := cityStore.Create(beads.Bead{
		Title: "Not a workflow",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/"+bead.ID+"?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func findLogicalNode(nodes []logicalNodeResponse, id string) *logicalNodeResponse {
	for i := range nodes {
		if nodes[i].ID == id {
			return &nodes[i]
		}
	}
	return nil
}

func findScopeGroup(groups []scopeGroupResponse, scopeRef string) *scopeGroupResponse {
	for i := range groups {
		if groups[i].ScopeRef == scopeRef {
			return &groups[i]
		}
	}
	return nil
}

func hasEdge(edges []workflowDepResponse, from, to, kind string) bool {
	for _, edge := range edges {
		if edge.From == from && edge.To == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func firstWorkflowBeadTitle(beads []workflowBeadResponse) string {
	if len(beads) == 0 {
		return ""
	}
	return beads[0].Title
}

type depListFailStore struct {
	beads.Store
}

func (s depListFailStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("dep list failed")
}

type failListStore struct {
	beads.Store
}

func (s failListStore) List() ([]beads.Bead, error) {
	return nil, errors.New("list failed")
}

type incrementingLatestSeqProvider struct {
	seq uint64
}

func (p *incrementingLatestSeqProvider) Record(events.Event) {}

func (p *incrementingLatestSeqProvider) List(events.Filter) ([]events.Event, error) {
	return nil, nil
}

func (p *incrementingLatestSeqProvider) LatestSeq() (uint64, error) {
	p.seq++
	return p.seq, nil
}

func (p *incrementingLatestSeqProvider) Watch(context.Context, uint64) (events.Watcher, error) {
	return nil, errors.New("not implemented")
}

func (p *incrementingLatestSeqProvider) Close() error {
	return nil
}
