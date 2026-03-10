# Session Identity and Work Dispatch

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-08 |
| Author(s) | Chris Sells, Claude |
| Issue | — |
| Supersedes | — |

**Companion to:** [Unified Session Model](unified-sessions.md). This doc
refines session naming, work dispatch, and clone semantics left unspecified
in the parent design.

## Summary

Replace deterministic session names (`SessionNameFor`) with bead-derived
names (`s-{beadID}`) universally. Make the reconciler the sole process
lifecycle manager — `gc session new` creates a bead, the reconciler starts
the process. Introduce per-session work queues for fixed agents (sling
assigns to a specific session, not a template) while preserving pool
semantics (sling labels for the pool, any member claims). Add resolvable
common names so `gc sling <bead> mayor` routes to the auto-created
session and `gc sling <bead> mayor-convo1` routes to a named clone.

## Motivation

### Problem 1: Two session naming schemes

The reconciler creates sessions with deterministic names derived from
config (`mayor`, `worker-1`) via `agent.SessionNameFor()`. The session
manager creates sessions with bead-derived names (`s-gc-42`). This means:

- User-created sessions are invisible to the reconciler (different
  naming scheme, different bead labels).
- The reconciler actively destroys user-created sessions as "orphaned"
  because their names aren't in `desiredState`.
- `gc session new mayor` followed by `gc sling <bead> mayor` doesn't
  route work to the user's session — it routes to the reconciler's
  separate `mayor` session.

### Problem 2: Sling routes to templates, not sessions

For fixed agents, `gc sling <bead> mayor` runs
`bd update <bead> --assignee=mayor` (the template name). Every session
sharing that template runs `bd ready --assignee=mayor` in its work query.
This is pool semantics applied to non-pool agents — if a user creates
clones of `mayor`, all clones race to claim the same work. There's no way
to direct work to a specific session.

### Problem 3: `gc session new` bypasses the reconciler

`gc session new` calls `sp.Start` directly, duplicating process lifecycle
logic. It doesn't inject `GC_AGENT`, `GC_TEMPLATE`, `GC_CITY`, `GC_DIR`,
or `GC_SESSION_NAME` into the env. Sessions it creates don't benefit from
crash-loop protection, config-drift detection, or work-driven wake.

## Guide-Level Explanation

### Bead ID is the session identity

Every session — whether auto-created from config or user-created via
CLI — gets its runtime name from its bead ID:

```
Bead gc-1  →  tmux session s-gc-1   (auto-created for overseer template)
Bead gc-42 →  tmux session s-gc-42  (user-created clone)
```

There is no `SessionNameFor()`. The bead store is the source of truth for
session names. The runtime provider receives opaque names it doesn't
interpret.

### Reconciler auto-creates config sessions

When the reconciler sees `[[agents]] name = "overseer"` and no open bead
with `template=overseer` exists, it creates one. The bead gets an ID
(e.g., `gc-1`), the session name becomes `s-gc-1`, and the reconciler
starts the process.

The auto-created session gets the template name as its **common name**
(stored in bead metadata as `common_name`). This makes it resolvable by
template name:

```bash
$ gc session list
ID       TEMPLATE    STATE    NAME        TITLE
gc-1     overseer    awake    overseer    —
gc-42    overseer    asleep   dbg-auth    debugging auth
gc-43    overseer    awake    refactor    refactoring config
```

### User clones

```bash
$ gc session new overseer --name dbg-auth --title "debugging auth"
Session gc-42 created from template "overseer".
# Reconciler starts it on next tick (or immediately via poke).
```

The clone is a full session: same template, same env, same provider,
same hook integration. It has its own work queue. It benefits from the
full reconciler infrastructure (crash-loop protection, config-drift
detection, idle timeout).

### Work dispatch: per-session for fixed, per-pool for pools

**Fixed agents** — work is assigned to a specific session:

```bash
$ gc sling BL-42 overseer           # routes to auto-created session (gc-1)
$ gc sling BL-42 dbg-auth           # routes to the clone (gc-42)
$ gc sling BL-42 gc-42              # routes by bead ID (same as above)
```

Under the hood, `gc sling BL-42 overseer` resolves `overseer` to
bead `gc-1`, then runs:
```
bd update BL-42 --assignee=s-gc-1
```

The session's work query checks its own queue:
```
bd ready --assignee=s-gc-1
```

Each session has an independent work queue keyed by its session name.
No race conditions between clones.

**Pool agents** — work is labeled for the pool, any member claims:

```bash
$ gc sling BL-42 worker             # labels for pool, any worker claims
```

Under the hood:
```
bd update BL-42 --add-label=pool:worker
```

Each pool member's work query checks the shared pool:
```
bd ready --label=pool:worker --limit=1
```

This is unchanged from today's pool behavior.

### `gc session new` doesn't start processes

`gc session new` creates a bead with the right metadata and pokes the
controller. The reconciler starts the process on its next tick. This
means:

- One code path for process lifecycle (the reconciler).
- Full env parity — `GC_SESSION_NAME`, `GC_TEMPLATE`, `GC_AGENT`,
  `GC_CITY`, `GC_DIR`, `GC_RIG` are always set by the reconciler.
- Crash-loop protection, config-drift detection, and work-driven wake
  apply to all sessions automatically.

Similarly, `gc session suspend` writes bead metadata (sets user hold)
and the reconciler drains the session. `gc session wake` clears the
hold and the reconciler wakes it.

## Reference-Level Explanation

### Common name resolution

Bead metadata gains a `common_name` field:

```
Metadata: {
    "common_name": "overseer"    // resolvable alias
    "template":    "overseer"    // agent template
    "session_name": "s-gc-1"    // runtime session name (= "s-" + beadID)
    ...
}
```

**Auto-created sessions:** `common_name` = template name. For pool
instances: `common_name` = `{template}-{slot}` (e.g., `worker-1`).

**User-created sessions:** `common_name` = value of `--name` flag. If
omitted, no common name is set (session is only addressable by bead ID).

**Resolution order** (used by `gc sling`, `gc session attach`, etc.):

1. Direct bead ID lookup (`gc-42`).
2. Common name lookup — query open beads where
   `common_name = <input>`. Must be unambiguous (exactly one match).
3. Template name lookup — query open beads where
   `template = <input>`. Must be unambiguous for fixed agents
   (exactly one auto-created session). Ambiguous for pools (error —
   use bead ID or common name).

This replaces `resolveAgentIdentity` for dispatch and
`resolveSessionID` for session management. Both collapse into one
resolution function.

### Sling query and work query changes

`EffectiveSlingQuery` and `EffectiveWorkQuery` in `config.Agent` change
their defaults based on agent type:

```go
func (a *Agent) EffectiveWorkQuery() string {
    if a.WorkQuery != "" {
        return a.WorkQuery  // explicit override
    }
    if a.IsPool() {
        label := a.QualifiedName()
        if a.PoolName != "" {
            label = a.PoolName
        }
        return "bd ready --label=pool:" + label + " --limit=1"
    }
    // Fixed agent: per-session work queue.
    return "bd ready --assignee=$GC_SESSION_NAME"
}

func (a *Agent) EffectiveSlingQuery() string {
    if a.SlingQuery != "" {
        return a.SlingQuery  // explicit override
    }
    if a.IsPool() {
        label := a.QualifiedName()
        if a.PoolName != "" {
            label = a.PoolName
        }
        return "bd update {} --add-label=pool:" + label
    }
    // Fixed agent: assign to the specific session.
    // The {} placeholder is replaced by the bead ID at sling time.
    // $GC_SLING_TARGET is the resolved session name of the target.
    return "bd update {} --assignee=$GC_SLING_TARGET"
}
```

**Key change:** Fixed agents use `$GC_SESSION_NAME` (bead-derived,
per-session) instead of the template name. This gives each clone its
own work queue.

For sling, the target session name is resolved before the sling query
runs and injected as `$GC_SLING_TARGET`. This way the sling query
doesn't need to know about name resolution — it just assigns to the
resolved target.

### `gc session new` becomes bead-only

```go
func cmdSessionNew(args []string, name, title string, noAttach bool,
    stdout, stderr io.Writer) int {

    // Resolve template from config.
    found, ok := resolveAgentIdentity(cfg, templateName, ...)
    if !ok { ... }

    // Create session bead with metadata.
    meta := map[string]string{
        "template":    found.QualifiedName(),
        "common_name": name,    // from --name flag (empty if not set)
        "state":       "asleep",
        "provider":    resolved.Name,
        "work_dir":    workDir,
        "command":     resolved.CommandString(),
    }
    bead := store.Create(beads.Bead{
        Title:    title,
        Type:     "session",
        Labels:   []string{"gc:session", "template:" + found.QualifiedName()},
        Metadata: meta,
    })

    // Derive and store session name from bead ID.
    sessName := "s-" + bead.ID
    store.SetMetadata(bead.ID, "session_name", sessName)

    // Poke controller for immediate reconciler tick.
    pokeController(cityPath)

    fmt.Fprintf(stdout, "Session %s created. Reconciler will start it.\n", bead.ID)

    if !noAttach {
        // Wait for reconciler to start the process, then attach.
        waitForSessionAlive(sp, sessName, 10*time.Second)
        sp.Attach(sessName)
    }
    return 0
}
```

The reconciler's `Phase 1` sync sees the new bead, finds it has a valid
template and no process running, computes wake reasons (`WakeConfig` if
it matches a config agent, or `WakeWork` if it has assigned work), and
starts it.

### `gc session suspend` and `gc session wake` become metadata-only

```go
func cmdSessionSuspend(args []string, ...) int {
    // Set user hold — reconciler will drain on next tick.
    store.SetMetadataBatch(id, map[string]string{
        "held_until":   "9999-12-31T23:59:59Z",  // indefinite
        "sleep_reason": "user-hold",
    })
    pokeController(cityPath)
}

func cmdSessionWake(args []string, ...) int {
    // Clear hold and quarantine — reconciler will wake on next tick.
    store.SetMetadataBatch(id, map[string]string{
        "held_until":        "",
        "quarantined_until": "",
        "wake_attempts":     "0",
    })
    pokeController(cityPath)
}
```

### `desiredState` discovers existing beads

Today `buildDesiredState` computes desired sessions purely from config.
After this change, it also discovers existing session beads with valid
templates:

```go
func buildDesiredState(...) map[string]TemplateParams {
    desired := map[string]TemplateParams{}

    // Step 1: Config-driven entries (as today).
    // For each [[agents]] entry, ensure at least one session bead exists.
    // If no bead with this template exists, create one (auto-create).
    for _, agent := range cfg.Agents {
        beads := findOpenBeadsByTemplate(store, agent.QualifiedName())
        if len(beads) == 0 && !agent.Suspended {
            // Auto-create the first session bead.
            bead := createSessionBead(agent)
            bead.Metadata["common_name"] = agent.QualifiedName()
            beads = []beads.Bead{bead}
        }

        // Step 2: Build TemplateParams for each bead.
        for _, b := range beads {
            sessName := b.Metadata["session_name"]
            tp := resolveTemplateForBead(agent, b)
            desired[sessName] = tp
        }
    }

    return desired
}
```

This means:
- Config agents always have at least one session (auto-created).
- User-created clones with matching templates are adopted into
  `desiredState` and managed by the reconciler.
- Beads for templates no longer in config are NOT in `desiredState`
  — the reconciler closes them as orphaned (as today).

### `SessionNameFor` removal

All ~25 call sites that compute `agent.SessionNameFor(cityName, name, template)`
are replaced with bead store lookups:

| Call site pattern | Replacement |
|---|---|
| `SessionNameFor` for provider operations | Read `session_name` from bead metadata |
| `SessionNameFor` in `doSlingNudge` | Resolve common name → bead → `session_name` |
| `SessionNameFor` in `configuredSessionNames` | Query beads by template label |
| `SessionNameFor` in `allDependenciesAlive` | Query beads by template, check any alive |
| `SessionNameFor` in prompt template `session` func | Query bead store |
| `SessionNameFor` in adoption barrier | Match by `session_name` metadata directly |

Most call sites already have a bead store handle or can receive one.
The `prompt.go` template function is the trickiest — it needs store
access at render time.

### Env vars

The reconciler builds env for all sessions uniformly (no change needed
in `gc session new` since the reconciler starts the process):

```go
env := map[string]string{
    "GC_SESSION_ID":   session.ID,           // bead ID (e.g., "gc-1")
    "GC_SESSION_NAME": session.Metadata["session_name"],  // "s-gc-1"
    "GC_TEMPLATE":     session.Metadata["template"],      // "overseer"
    "GC_AGENT":        session.Metadata["template"],      // legacy compat
    "GC_CITY":         cityPath,
    "GC_DIR":          workDir,
}
```

Since the reconciler is the sole starter, env parity is automatic.

## Primitive Test

Not applicable — this proposal refactors existing primitives (session
beads, work dispatch) without adding new ones. The bead, sling, and
reconciler are all existing mechanisms. Common names are a derived
convenience (bead metadata query), not a new primitive.

## Drawbacks

1. **Opaque session names.** `tmux ls` shows `s-gc-42` instead of
   `mayor`. Debugging requires `gc session list` to map IDs to
   templates. Mitigated by the `common_name` field and `gc session list`
   output.

2. **Store dependency for all session operations.** Every CLI command
   that interacts with a session needs a bead store handle. Today some
   commands compute the session name from config alone. This adds I/O
   to paths that were previously pure.

3. **Breaking change for `sling_query` / `work_query` defaults.**
   Fixed agents that rely on the current default
   (`--assignee=<template-name>`) will break if they have existing
   routed beads using the old assignee value. Migration requires
   re-routing or a compatibility shim.

4. **Latency on `gc session new --attach`.** Since the reconciler
   starts the process (not `gc session new` directly), there's a delay
   between bead creation and process availability. The CLI must poll
   or wait for the reconciler tick. With controller poke, this is
   typically < 1 second, but it's a new waiting step.

## Alternatives

### A: Keep deterministic names, fix orphan handling

Keep `SessionNameFor` for reconciler sessions. Teach the reconciler to
skip beads with `type=session` (the session manager's type). User
sessions and reconciler sessions remain separate planes.

- **Advantage:** No migration of 25+ call sites. No store dependency
  for simple CLI commands.
- **Rejected because:** User sessions can't benefit from reconciler
  infrastructure. Work can't be slung to user sessions. The two-system
  problem from unified-sessions.md remains.

### B: Template-scoped work queues (current behavior extended)

Keep `--assignee=<template-name>` for fixed agents. All sessions
sharing a template share one work queue (pool semantics for everyone).

- **Advantage:** Simpler dispatch model. No per-session routing.
- **Rejected because:** No way to direct work to a specific clone.
  User creates two sessions for different tasks, slings work to one,
  and the other might claim it. Pool semantics are wrong for
  interactive/conversational sessions where context matters.

### C: Do nothing

Keep the current two-system model. `gc session new` remains separate
from the reconciler.

- **Advantage:** No code changes.
- **Rejected because:** The unified-sessions design doc was approved
  specifically to solve this problem. Sessions can't receive work,
  reconciler actively destroys user sessions, env vars are missing.

## Resolved Questions

1. **Common name uniqueness.** Global. No two open sessions can share a
   common name. `store.Create` rejects duplicates. If someone wants two
   "debug" sessions from different templates, they name them `debug-auth`
   and `debug-perf`.

2. **Auto-create timing.** `syncSessionBeads` (separate phase, before
   `buildDesiredState`). Keeps `buildDesiredState` as a pure computation.
   Matches the current architecture.

3. **Migration of existing `--assignee` beads.** Not needed — the old
   behavior (template-name assignees) was never shipped. No backward
   compatibility shim required.

## Unresolved Questions

### During implementation

1. **Prompt template `session` function.** Needs store access at render
   time. Either pass the store to the template renderer or pre-compute
   a session name map.

2. **`gc session new --attach` wait behavior.** How long to wait for
   reconciler startup? Fixed timeout? Exponential backoff? Event-driven
   (watch for `session.woke` event)?

3. **Pool common names.** Should pool instances get automatic common
   names (`worker-1`, `worker-2`) or only be addressable by bead ID?

## Implementation Plan

### Phase 1: Bead-derived session names (medium)

- Change `syncSessionBeads` / auto-create to use `s-{beadID}` names.
- Add `common_name` metadata field.
- Auto-created sessions get `common_name` = template name.
- Update `resolveSessionID` to support common name lookup.
- Migrate `SessionNameFor` call sites to bead store lookups.
- Delete `agent.SessionNameFor`.
- All tests pass with new naming.

**Delivers:** Uniform session naming. Prerequisite for all other phases.

### Phase 2: Reconciler-only process lifecycle (medium)

- `gc session new` becomes bead-only (no `sp.Start`).
- `gc session suspend` / `wake` become metadata-only.
- `buildDesiredState` discovers existing session beads.
- Reconciler starts/stops all sessions.
- Env parity is automatic (reconciler builds env).

**Delivers:** One code path for process lifecycle. User sessions get
full reconciler infrastructure.

### Phase 3: Per-session work dispatch (small)

- `EffectiveSlingQuery` default for fixed agents: `--assignee=$GC_SLING_TARGET`.
- `EffectiveWorkQuery` default for fixed agents: `--assignee=$GC_SESSION_NAME`.
- `gc sling` resolves target name → bead → session name before
  running sling query.
- Pool dispatch unchanged.

**Delivers:** Per-session work queues. Clones don't race for work.
