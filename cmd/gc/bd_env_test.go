package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestOpenStoreAtForCityUsesExplicitCityForExternalRig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	externalRig := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(externalRig, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "file")

	store, err := openStoreAtForCity(externalRig, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "external rig bead", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cityStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := cityStore.Get(created.ID); err != nil {
		t.Fatalf("city store should see created bead %s: %v", created.ID, err)
	}
}

func TestMergeRuntimeEnvReplacesInheritedRuntimeKeys(t *testing.T) {
	env := mergeRuntimeEnv([]string{
		"PATH=/bin",
		"GC_CITY_PATH=/wrong",
		"GC_DOLT_PORT=9999",
		"GC_PACK_STATE_DIR=/wrong/.gc/runtime/packs/dolt",
	}, map[string]string{
		"GC_CITY_PATH": "/city",
		"GC_DOLT_PORT": "31364",
	})

	got := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}

	if got["GC_CITY_PATH"] != "/city" {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got["GC_CITY_PATH"], "/city")
	}
	if got["GC_DOLT_PORT"] != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got["GC_DOLT_PORT"], "31364")
	}
	if _, ok := got["GC_PACK_STATE_DIR"]; ok {
		t.Fatalf("GC_PACK_STATE_DIR should be removed, env = %#v", got)
	}
}
