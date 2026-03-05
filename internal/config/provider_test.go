package config

import (
	"strings"
	"testing"
)

func TestBuiltinProviders(t *testing.T) {
	providers := BuiltinProviders()
	order := BuiltinProviderOrder()

	// Must have exactly 7 built-in providers.
	if len(providers) != 7 {
		t.Fatalf("len(BuiltinProviders()) = %d, want 7", len(providers))
	}
	if len(order) != 7 {
		t.Fatalf("len(BuiltinProviderOrder()) = %d, want 7", len(order))
	}

	// Every entry in order must exist in providers.
	for _, name := range order {
		p, ok := providers[name]
		if !ok {
			t.Errorf("BuiltinProviders() missing %q", name)
			continue
		}
		if p.Command == "" {
			t.Errorf("provider %q has empty Command", name)
		}
		if p.DisplayName == "" {
			t.Errorf("provider %q has empty DisplayName", name)
		}
	}

	// Every provider must be in order.
	for name := range providers {
		found := false
		for _, o := range order {
			if o == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("provider %q not in BuiltinProviderOrder()", name)
		}
	}
}

func TestBuiltinProvidersClaude(t *testing.T) {
	p := BuiltinProviders()["claude"]
	if !strings.Contains(p.Command, "claude --dangerously-skip-permissions") {
		t.Errorf("Command should contain claude --dangerously-skip-permissions, got %q", p.Command)
	}
	if !strings.Contains(p.Command, "sh -c") {
		t.Errorf("Command should be a sh -c wrapper, got %q", p.Command)
	}
	if !strings.Contains(p.Command, "bd list") {
		t.Errorf("Command should contain bd list preamble, got %q", p.Command)
	}
	if len(p.Args) != 0 {
		t.Errorf("Args = %v, want empty (args baked into sh -c wrapper)", p.Args)
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
	if p.ReadyDelayMs != 10000 {
		t.Errorf("ReadyDelayMs = %d, want 10000", p.ReadyDelayMs)
	}
	if !p.EmitsPermissionWarning {
		t.Error("EmitsPermissionWarning = false, want true")
	}
}

func TestBuiltinClaudeCommandString(t *testing.T) {
	p := BuiltinProviders()["claude"]
	rp := &ResolvedProvider{
		Command: p.Command,
		Args:    p.Args,
	}
	cs := rp.CommandString()
	// With no args, CommandString should just return the command.
	if cs != p.Command {
		t.Errorf("CommandString() = %q, want %q", cs, p.Command)
	}
	// The wrapper should end with ' -- so prompt passthrough works.
	if !strings.HasSuffix(cs, "' --") {
		t.Errorf("CommandString() should end with \"' --\" for prompt passthrough, got %q", cs)
	}
}

func TestBuiltinProvidersCodex(t *testing.T) {
	p := BuiltinProviders()["codex"]
	if p.Command != "codex" {
		t.Errorf("Command = %q, want %q", p.Command, "codex")
	}
	if p.PromptMode != "none" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "none")
	}
	if p.ReadyDelayMs != 3000 {
		t.Errorf("ReadyDelayMs = %d, want 3000", p.ReadyDelayMs)
	}
	if p.EmitsPermissionWarning {
		t.Error("EmitsPermissionWarning = true, want false")
	}
}

func TestBuiltinProvidersGemini(t *testing.T) {
	p := BuiltinProviders()["gemini"]
	if p.Command != "gemini" {
		t.Errorf("Command = %q, want %q", p.Command, "gemini")
	}
	if len(p.Args) != 2 || p.Args[0] != "--approval-mode" || p.Args[1] != "yolo" {
		t.Errorf("Args = %v, want [--approval-mode yolo]", p.Args)
	}
	if p.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want %q", p.PromptMode, "arg")
	}
}

func TestBuiltinProvidersReturnsNewMap(t *testing.T) {
	a := BuiltinProviders()
	b := BuiltinProviders()
	a["claude"] = ProviderSpec{Command: "mutated"}
	if b["claude"].Command == "mutated" {
		t.Error("BuiltinProviders() should return a new map each time")
	}
}

func TestBuiltinProviderOrderReturnsNewSlice(t *testing.T) {
	a := BuiltinProviderOrder()
	b := BuiltinProviderOrder()
	a[0] = "mutated"
	if b[0] == "mutated" {
		t.Error("BuiltinProviderOrder() should return a new slice each time")
	}
}

func TestCommandStringNoArgs(t *testing.T) {
	rp := &ResolvedProvider{Command: "claude"}
	if got := rp.CommandString(); got != "claude" {
		t.Errorf("CommandString() = %q, want %q", got, "claude")
	}
}

func TestCommandStringWithArgs(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "claude",
		Args:    []string{"--dangerously-skip-permissions"},
	}
	want := "claude --dangerously-skip-permissions"
	if got := rp.CommandString(); got != want {
		t.Errorf("CommandString() = %q, want %q", got, want)
	}
}

func TestCommandStringMultipleArgs(t *testing.T) {
	rp := &ResolvedProvider{
		Command: "gemini",
		Args:    []string{"--approval-mode", "yolo"},
	}
	want := "gemini --approval-mode yolo"
	if got := rp.CommandString(); got != want {
		t.Errorf("CommandString() = %q, want %q", got, want)
	}
}
