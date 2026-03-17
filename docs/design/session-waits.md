# Durable Session Waits

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-15 |
| Author(s) | Codex |
| Issue | — |
| Supersedes | — |

## Summary

Add a durable **wait** subsystem so a session can register "wake me when this condition becomes true," drain to sleep, and later resume with the same conversation when the condition is satisfied. A wait is a dynamic, per-session instance stored as a bead; the controller evaluates waits using three trigger kinds: event, bead dependency, and external probe. External probes resolve against a controller-owned built-in waiter registry by default, with optional city-local and pack-defined extensions for custom integrations. When a wait becomes ready, the controller keeps it in durable `ready` state, wakes the session through `WakeWait`, enqueues a synthetic nudge entry referencing the wait bead into the async nudge queue, and closes or fails the wait only after the async nudge subsystem advances the authoritative `gc:nudge` bead to a terminal state. This preserves durable continuation handoff semantics up to the strongest provider commit boundary available for long pauses such as async reviews and external approvals, while freeing the agent process and most of its memory during the wait.

<!-- REVIEW: added per Blocker 1 — delivery delegated to async nudge queue -->

The proposal deliberately does **not** overload existing Orders. Orders are static, global automation definitions; waits are dynamic, targeted continuations for one blocked session. The implementation should reuse order-like evaluation patterns — specifically, a shared evaluator layer for event cursor matching and exec probe execution — but not the Order type itself.

<!-- REVIEW: added per Major 10 — shared evaluator layer -->

## Motivation

Today Gas City can put sessions to sleep, and it can also dispatch background work, but it has no durable way to say "this exact session is blocked on X; wake it later when X finishes."

That causes two concrete failures:

1. A worker session starts two async review jobs, then has nothing useful to do until both complete. The session can keep running, wasting memory and a provider slot, or it can die and lose the exact continuation point that should consume the review results.

2. A session opens a pull request and must wait for an external actor to approve or reject it. The result is not produced by the local event bus, so neither bead dependencies nor a plain idle timeout solve the problem. The current system has no durable "sleep until approval decision changes" contract.

The naive fix is "just leave the process up and poll from inside the agent." That fails the resource-allocation goal and duplicates polling logic across prompts. Another naive fix is "just use Orders." That also misses the target:

- Orders are config-defined, not created dynamically by a blocked session.
- Orders are fire-and-forget; they do not own per-session delivery or acknowledgment.
- Orders do not solve the sleep-wakeup race for a session that is draining while the awaited condition flips.

This proposal follows NDI directly: the blocked continuation is persisted outside the runtime process, and the controller converges it to the correct outcome after crashes or restarts. The OS analogy is useful here: a session should be able to sleep on a wait channel, and the scheduler should wake it when the condition is true, without requiring the sleeping process to keep spinning.

## Guide-Level Explanation

### Waiting on internal work

A session that kicked off async work can arm a wait and then sleep:

```bash
$ gc session wait s-42 \
    --on-beads gc-201,gc-202 \
    --all \
    --note "Both async reviews finished. Reconcile findings and continue." \
    --sleep

Registered wait w-17 for session s-42.
Session s-42 draining to sleep.
```

While the reviews are running, `gc session list` can show the session as asleep with a waiting annotation:

```text
ID     TEMPLATE  STATE   REASON
s-42   worker    asleep  waiting (2 blockers)
```

When both review beads reach their terminal state, the controller marks the wait ready, enqueues the reminder into the async nudge queue, wakes the session if needed, and delivers the reminder on the next safe boundary:

```text
<system-reminder>
Wait satisfied:
- Both async reviews finished. Reconcile findings and continue.
</system-reminder>
```

The worker session resumes with its original conversation instead of starting fresh in a different process.

### Waiting on an external actor

External waits can use a built-in waiter without any pack:

```bash
$ gc session wait s-42 \
    --watch github-pr-approval \
    --arg repo=acme/widgets \
    --arg pr=128 \
    --note "Approval decision changed for PR 128." \
    --sleep
```

High-value built-ins are controller-owned implementations:

```text
github-pr-approval
github-check-suite
http-json
bead-status
```

Custom integrations can still come from city-local or pack-defined waiters. Those references are explicit:

```toml
# packs/ops/waiters/approval-status/waiter.toml
description = "Poll a pull request approval decision"
mode = "probe"
exec = "scripts/approval-status.sh"
default_interval = "30s"
timeout = "10s"
```

```bash
$ gc session wait s-42 \
    --watch pack:ops/approval-status \
    --arg repo=acme/widgets \
    --arg pr=128 \
    --sleep
```

The blocked session does **not** hand the controller an arbitrary shell string. It references either a built-in waiter or a trusted named extension that the city already ships.

### Operator view

<!-- REVIEW: added per Blocker 6 — expanded operator tooling -->

Operators can inspect and manage waits:

```bash
$ gc wait list
WAIT   SESSION  STATE    KIND   FAILURES  NEXT CHECK  AGE   NOTE
w-17   s-42     pending  deps   0         —           4m    Both async reviews finished...
w-18   s-51     pending  probe  3         22s         9m    Approval decision changed...
```

Filter by state:

```bash
$ gc wait list --state pending
$ gc wait list --state ready
$ gc wait list --session s-42
```

Inspect full details of a wait:

```bash
$ gc wait inspect w-18
Wait:       w-18
Session:    s-51 (continuation epoch 7)
State:      pending
Kind:       probe
Waiter:     approval-status
Args:       {"repo":"acme/widgets","pr":"128"}
Created:    2026-03-15T12:30:00Z
Expires:    2026-03-16T12:30:00Z
Failures:   3 (backoff: 2m, next: 2026-03-15T12:38:22Z)
Note:       Approval decision changed for PR 128.
```

Cancel a wait:

```bash
$ gc wait cancel w-18
Canceled wait w-18.
```

Manually satisfy a wait whose probe is broken:

```bash
$ gc wait ready w-18
Marked wait w-18 ready.
```

### What the user learns

Gas City gets one explicit concept for blocked continuations:

- **session**: the resumable conversation identity
- **wait**: a durable condition that can wake that session later

The common pattern becomes:

1. Register a wait.
2. Sleep the session.
3. Let the controller wake it when the wait is ready.

## Reference-Level Explanation

### 1) New types and ownership

Add a new package:

```go
// Package waits defines durable blocked-continuation instances.
package waits

type State string

const (
    StatePending    State = "pending"
    StateReady      State = "ready"
    StateClosed     State = "closed"
    StateExpired    State = "expired"
    StateCanceled   State = "canceled"
    StateFailed     State = "failed"
)
```

<!-- REVIEW: added per Blocker 1 — removed StateDelivering; added per Blocker 4 — added StateFailed -->

```go
type Spec struct {
    SchemaVersion     int
    SessionID         string
    ContinuationEpoch uint64
    CreatedBySession  string
    Note              string
    ExpiresAt         time.Time
    Trigger           TriggerSpec
}
```

<!-- REVIEW: added per Major 11 — SessionGeneration binding; added per Blocker 7 — CreatedBySession provenance -->

```go
type TriggerSpec struct {
    Event *EventTrigger
    Deps  *DepsTrigger
    Probe *ProbeTrigger
}

type EventTrigger struct {
    Type     string
    Actor    string
    Subject  string
    AfterSeq uint64
}

type DepsTrigger struct {
    BeadIDs []string
    Mode    string // "all" or "any"
}

type ProbeTrigger struct {
    Waiter              string
    Args                map[string]string
    NextCheckAt         time.Time
    Interval            time.Duration
    ConsecutiveFailures int
    LastError           string
}

type ProbeResult struct {
    Ready        bool
    Summary      string
    RecheckAfter time.Duration
}
```

<!-- REVIEW: added per Minor 18 — ConsecutiveFailures and LastError in ProbeTrigger -->

The controller-side orchestration lives in `cmd/gc/wait_dispatch.go`, analogous to order dispatch, but the persistent model belongs in `internal/waits`.

### 2) Persistence model

A wait is a bead with `type = "wait"`:

<!-- REVIEW: added per Major 12 — hoisted key trigger fields to top-level metadata -->

```go
beads.Bead{
    Type:   "wait",
    Title:  "wait:s-42",
    Labels: []string{"gc:wait", "wait:pending", "session:s-42", "kind:deps"},
    Metadata: map[string]string{
        "session_id":         "s-42",
        "schema_version":     "1",
        "continuation_epoch": "7",
        "created_by_session": "s-42",
        "state":              "pending",
        "delivery_attempt":   "1",
        "trigger_kind":       "deps",
        "dep_bead_ids":       "gc-201,gc-202",
        "dep_mode":           "all",
        "event_type":         "",
        "event_after_seq":    "",
        "probe_waiter":       "",
        "probe_args_json":    "",
        "probe_next_check_at":"",
        "probe_failures":     "0",
        "probe_last_error":   "",
        "spec_json":          "{...}",
        "result_json":        "",
        "expires_at":         "2026-03-16T12:34:56Z",
    },
}
```

Key trigger fields are hoisted to top-level metadata for visibility in `bd list`, `bd show`, and label queries. These top-level metadata fields are the canonical source of truth for evaluation. The `spec_json` blob is write-once archival data for debugging and manual repair; the hot path never reads it.

The bead metadata map is string-only today, so the structured trigger and result are stored as JSON blobs under `spec_json` and `result_json`. That is acceptable for v0 because waits are a derived mechanism and not a new storage primitive.

`delivery_attempt` starts at `1` and is part of the synthetic nudge
identity. It remains stable across crash recovery for one delivery
attempt and increments only when an operator explicitly retries a
failed wait-originated nudge via `gc wait retry`.

### 3) Built-in and extension waiter definitions

<!-- REVIEW: added per security feedback and follow-up discussion — built-in waiters work without packs -->

External waits resolve waiter references from two sources:

- **Built-in waiters**: controller-owned implementations available in every city with no pack dependency.
- **Extension waiters**: city-local or pack-defined waiters for custom systems.

Recommended reference forms:

- `github-pr-approval` — built-in waiter
- `pack:ops/approval-status` — pack waiter
- `city:staging-approval` — city-local waiter

Built-in waiters are the default path for high-value common integrations. Pack and city waiters are extensions, not prerequisites. Bare waiter names resolve only against the built-in registry. Custom waiters must use explicit `pack:` or `city:` prefixes so the trust boundary is visible at the CLI.

#### 3a) Built-in waiters

Built-in waiters are registered in Go and do not execute shell by default. Initial built-ins should include:

- `github-pr-approval` — wake when a PR approval decision changes
- `github-check-suite` — wake when a check suite reaches a terminal conclusion

These built-ins work without any pack installed. They are controller-owned so they can use typed config, explicit auth contracts, and deterministic tests instead of shell conventions.

`http-json` is intentionally excluded from the session-facing v0 built-in set. A generic controller HTTP client becomes an SSRF surface unless endpoints and auth profiles are operator-defined and allowlisted.

`bead-status` is also excluded from the v0 built-in set because dependency waits already cover the same territory with less concept count.

#### 3b) Extension waiters

Extension waiters are disabled by default for session-authored requests. They become callable only when the operator enables them explicitly via `[session.waits].allow_extension_waiters = true`.

External probe waits are backed by pack-owned waiter definitions under:

```text
packs/<pack>/waiters/<name>/waiter.toml
```

City-local waiters live under:

```text
waiters/<name>/waiter.toml
```

<!-- REVIEW: added per Minor 15 — waiter name validation -->

Pack and city waiter names must match `^[a-zA-Z0-9][a-zA-Z0-9_-]*$`. The controller rejects names containing `/`, `..`, or path separators before constructing the filesystem path. The `pack:` prefix must specify both pack and waiter name (`pack:<pack>/<name>`); both segments use the same validation rule.

Suggested schema:

```toml
description = "Human-readable description"
mode = "probe"
exec = "scripts/check.sh"
default_interval = "30s"
timeout = "10s"
max_backoff = "10m"
max_failures = 10
```

<!-- REVIEW: added per Minor 18 — max_failures field -->

Extension waiters print JSON shaped like `ProbeResult`. The command receives stable environment variables:

- `GC_WAIT_ID`
- `GC_WAIT_SESSION_ID`
- `GC_WAIT_ARGS_JSON`
- normal city runtime env from `citylayout.CityRuntimeEnv`

This keeps controller-executed code in trusted named definitions instead of arbitrary session-authored shell. For critical common integrations, built-ins are preferred over extension waiters because they avoid shell portability, shadowing ambiguity, and pack installation as a prerequisite.

<!-- REVIEW: added per Minor 17 — waiter args schema validation -->

Extension waiter definitions may include an optional `[args]` section declaring expected argument names and types. When present, the controller validates `--arg` values against the schema before persisting the wait bead. Regardless of schema, `GC_WAIT_ARGS_JSON` is always a flat `map[string]string` — no nested structures.

Every built-in waiter declares an argument schema in Go. The API rejects more than 10 args, arg values longer than 1024 bytes, and arg names outside `^[a-zA-Z0-9][a-zA-Z0-9_-]*$`.

For extension waiters, `exec` is resolved relative to the `waiter.toml` parent directory. Absolute paths are rejected. After symlink resolution, the final path must remain within the waiter directory tree. Execution uses structured argv, never `sh -c`.

Extension waiters must be read-only, side-effect-free, and idempotent. They receive a minimal environment only: documented `GC_WAIT_*` variables, city runtime env, and `PATH`. The controller's ambient environment is not inherited.

Built-in waiters authenticate using controller-side secret profiles declared in `city.toml`. Sessions never pass raw API tokens in wait args, and they do not choose arbitrary credential handles. Each built-in waiter binds to an operator-declared integration profile plus optional scope rules such as allowed orgs/repos or allowed API resource prefixes. Registration is rejected if the requested args fall outside that allowlisted scope.
`city.toml` references secret profile names only. Actual credential values live under `.gc/secrets/waiters/` with `0600` permissions.

### 4) Session wake integration

Extend `WakeReason` with:

```go
const WakeWait WakeReason = "wait"
```

`wakeReasons()` stays pure. The reconciler precomputes a `readyWaitSet` keyed by session ID and passes it in, the same way it already precomputes work presence.

If any `ready` wait targets the session, `WakeWait` is present.

#### Wait evaluation phase ordering

The reconciler processes waits in three distinct phases:

1. **Phase 2pre:** consume terminal `gc:nudge` bead states for wait-originated nudges, clear stale `wait_hold`, and build `readyWaitSet` from waits already in `ready` state.
2. **Phase 2a:** compute wake reasons; `readyWaitSet` contributes `WakeWait`.
3. **Phase 2c:** evaluate `pending` waits, transition newly satisfied waits to `ready`, ensure synthetic nudges exist for awake `ready` waits, refresh `readyWaitSet`, and clear `wait_hold`/`sleep_intent=wait-hold` for sessions that just became ready.
4. **Phase 2d:** advance drains only after the Phase 2c delta has had a chance to cancel them.

This means a wait that becomes `ready` during Phase 2c of tick N is visible before drains advance in Phase 2d of that same tick. Sleeping sessions therefore always have a durable wake reason before the wait lifecycle can end, and ready-during-drain races resolve in favor of keeping the session alive.

`readyWaitSet` is built from the wait bead metadata field `state=ready`, then filtered by `session_id` and `continuation_epoch` against the live session table. Labels are advisory only.

<!-- REVIEW: added per Major 8 — wait_hold for config agents and pool workers -->

#### Interaction with standing wake reasons

Config agents and in-count pool workers always have `WakeConfig`, so `wakeReasons()` never returns empty and these sessions cannot drain to idle sleep. To support wait-initiated sleep for such sessions, the registration flow sets a `wait_hold` metadata field on the session bead.

`wait_hold` suppresses `WakeConfig` and `WakeAttached` but **not** `WakeWait`. Once `WakeWork` ships, it must not suppress `WakeWork` either. The reconciler checks `wait_hold` before computing config/attached reasons but after checking user hold. This allows:

- A config agent to sleep pending a wait, without its standing config reason preventing the drain.
- `WakeWait` to still wake the session immediately.
- Future `WakeWork` support to remain compatible with wait-held sessions instead of stranding work.
- User hold (`held_until`) to take full precedence if set — it suppresses all wake reasons including `WakeWait`.

`--sleep` without any pending waits targeting the session is rejected. The CLI returns an error: sessions should not enter wait-hold with nothing to wait for.

When a wait becomes `ready`, clearing `wait_hold` is sticky for that
waking period. If the same session still has other pending waits after
the delivered continuation is handled, the controller does not re-arm
`wait_hold` immediately when the first wait closes. It waits until the
session next reaches its normal idle boundary, then re-arms
`wait_hold`/`sleep_intent=wait-hold` before draining again. This avoids
draining a just-woken session before it can process the delivered
continuation.

When all waits targeting a session are in terminal states (`closed`, `expired`, `canceled`, `failed`), the controller clears `wait_hold` in Phase 2pre of the next tick, before `readyWaitSet` is built. If a session has `wait_hold` but no matching non-terminal waits, the controller treats that as corruption or crash residue and clears the hold automatically.

`wait_hold` is paired with the session bead's durable `sleep_intent = "wait-hold"`. Registration writes both fields before starting the drain. If the controller crashes after persisting the hold but before signaling the process, the next tick sees `sleep_intent` and begins the drain immediately rather than waiting for idle timeout.

The precedence chain is explicit:

1. `held_until`
2. `quarantined_until`
3. `wait_hold`
4. normal wake computation

Manual `gc session wake` clears `held_until`, `quarantined_until`, `wait_hold`, and `sleep_intent`. Manual attach does not override `wait_hold` by itself; the operator must wake the session first.

`gc session attach` detects `wait_hold` and prints an explicit message explaining that the session is intentionally sleeping for waits. It offers the operator the equivalent of `gc session wake` rather than hanging opaquely.

<!-- REVIEW: added per Major 11 — continuation epoch binding -->

#### Continuation epoch binding

Wait registration captures the target session's current `continuation_epoch`, not its per-wake runtime generation. `continuation_epoch` changes only when the conversation identity changes: fresh-mode restart, explicit session reset, or provider session-key reset. Ordinary sleep/wake cycles do not change it.

The `readyWaitSet` only includes waits whose stored continuation epoch matches the session's current continuation epoch. If a session resets its conversation identity, waits bound to the old continuation epoch transition to `canceled` with reason `continuation-stale`. This prevents a wait armed for one conversation from delivering into a different one.

Waits targeting `wake_mode=fresh` sessions are rejected at registration. Fresh-mode sessions discard conversation state on restart, so continuation semantics are undefined.

This is the key integration point: the existing wake/drain machinery already knows how to:

- wake a sleeping session
- cancel an in-progress drain when a wake reason reappears
- fence stale stops using generation and instance token

The wait subsystem should reuse that behavior instead of inventing another lifecycle path.

### 5) Registration contract and the sleep-wakeup race

<!-- REVIEW: added per Blocker 7 — authorization model -->

#### Authorization

By default, a session can only register waits targeting itself. The CLI verifies that `GC_SESSION_ID` matches the target session ID and must also provide the session's current `instance_token`. The controller verifies that token against the target session bead before authorizing any non-operator wait mutation. Cross-session wait registration requires explicit operator authorization via `gc wait create --target <session>`, which is only available from the controller socket (not from within a session). All wait beads persist `created_by_session` provenance.

Until the controller socket grows stronger in-band auth, "operator" here means "a local caller that can open `controller.sock` under the city's OS-level permissions." This design does not claim a stronger privilege boundary between same-UID local processes.

Authorization matrix:

| Command | Session caller | Operator |
|---|---|---|
| `gc session wait --sleep` | Allowed only for same `GC_SESSION_ID` + matching `instance_token` | Allowed |
| `gc wait cancel <id>` | Allowed only when `created_by_session` matches caller and `instance_token` matches | Allowed |
| `gc wait ready <id>` | Not allowed | Allowed |
| `gc wait retry <id>` | Not allowed | Allowed |
| `gc wait clear-hold <session>` | Not allowed | Allowed |

<!-- REVIEW: added per Minor 16 — drain reason specified -->

`gc session wait --sleep` must perform these steps in order:

1. Resolve the target session bead.
2. Verify authorization: `GC_SESSION_ID` must match the target, or the caller must have operator-level access.
3. Build the wait spec and capture any needed cursor or snapshot.
   Event waits capture `LatestSeq()` into `AfterSeq`.
   Dependency waits capture the bead IDs to inspect.
   Capture the target session's current `continuation_epoch`.
   Resolve whether the target session/provider has a validated deferred nudge path. If `AsyncNudgeDrainMode=none`, reject the wait request rather than allowing a wait that can never deliver its continuation.
   Reject the request if the target session already has 20 non-terminal waits (configurable cap).
4. Persist the wait bead.
5. Re-check the trigger once immediately.
   If already satisfied, mark the wait `ready` and do **not** force the session all the way to sleep. The normal ready-finalization path handles wake/no-wake and synthetic nudge creation.
6. Write `wait_hold` and `sleep_intent = "wait-hold"` to the session bead only if the session is actually proceeding to drain.
7. Begin the normal session drain with `drain_reason = "wait-sleep"`. This drain reason is cancelable (unlike `config-drift` drains, which are exempt from cancellation).

<!-- REVIEW: added per Minor 17 — note sanitization -->

The `--note` value is sanitized before storage: XML-like tags (including `</system-reminder>`) are stripped, and the note is capped at 280 characters to align with the synthetic nudge message budget. The note is reminder text, not a prompt injection surface.

This ordering avoids lost wakeups:

- If the condition flips after step 4 but before the drain completes, the wait becomes `ready`, `WakeWait` appears, and the existing drain-cancel path keeps the session alive.
- If the controller crashes after step 4, the wait bead remains and the next tick reevaluates it.

#### CLI-to-controller communication

The `gc session wait` CLI command sends a structured request over the controller's unix socket (`controller.sock`). The controller performs steps 2–7 within one reconciler-owned critical section before returning success to the CLI. The CLI does not write wait beads directly.

### 6) Wait state machine

<!-- REVIEW: added per Blocker 1 — simplified state machine, removed delivering state -->

```text
register
   |
   v
 pending -----> expired
   |        |
   |        +-> failed (probe exhausted or terminal nudge failure)
   |
   | condition satisfied
   v
 ready ----terminal nudge success----> closed

pending/ready ----cancel----> canceled
```

Rules:

- `pending`: not yet satisfied; the controller evaluates triggers each tick
- `ready`: satisfied; durable wake reason for sleeping sessions, remains until the async nudge pipeline reports a terminal outcome
- `closed`: the synthetic nudge reached a terminal success outcome at the strongest provider commit boundary available for that delivery path
- `expired`: deadline elapsed before satisfaction
- `canceled`: operator, session closure, or continuation-epoch staleness invalidated the wait before delivery crossed the provider attempt boundary
- `failed`: probe exhausted its failure budget or async nudge delivery reached a terminal failure outcome

The `ready` state is not merely transient. It persists across ticks for sleeping sessions so that `WakeWait` can be observed by the next wake pass, and it stays durable until the nudge pipeline reports terminal success. Synthetic nudges that reach terminal failure transition the wait to `failed`, not `closed`.

The former `delivering` state has been eliminated. Delivery claim, terminalization, and safe-boundary injection semantics are fully owned by the existing async nudge pipeline. This avoids a dual-substrate claim problem where crashes between nudge transport files and wait-bead metadata updates could produce partial delivery.

### 7) Evaluation paths

<!-- REVIEW: added per Major 10 — shared evaluator layer with orders -->

Event cursor matching uses a shared evaluator layer extracted from the order gate machinery. Probe execution lives in the same package but is wait-specific because orders use exit-code gates while waits need structured JSON results.

The relevant functions are:

- `evalgate.MatchEvent(cursor uint64, filter EventFilter, bus EventReader) (matched bool, seq uint64)` — shared event cursor matching with `AfterSeq` + type/actor/subject filter.
- `evalgate.RunProbe(cmd string, env []string, timeout time.Duration) (ProbeResult, error)` — wait-specific exec probe runner with timeout and JSON result parsing.

Wait and order types remain separate. Shared logic must not fork, but the design does not claim that every evaluator path is identical.

#### Event waits

Event waits are the cheap path. They should use the event bus cursor already captured in the wait spec.

Event waits are only admitted for event types with a durable reconstruction contract. A best-effort bus event is not enough by itself. Each supported event family must satisfy one of:

- the event is durably emitted and replayable from the captured cursor, or
- the waited-on condition can be recomputed from durable state on restart

Registration rejects event types that do not declare one of those stories. This keeps event waits first-class without pretending the current best-effort bus is sufficient for every event.

Evaluation rule:

- query events with `Seq > AfterSeq`
- match exact `Type`, optional `Actor`, optional `Subject`
- first match transitions the wait to `ready`

For low latency, the controller keeps one event watcher and fans out new events to candidate waits in memory. This is normative, not optional. On restart, a normal full scan of open wait beads plus cursor-based reevaluation restores correctness.

#### Dependency waits

Dependency waits inspect bead state directly:

- `all`: all listed beads reached terminal state
- `any`: at least one listed bead reached terminal state

For v0, "terminal" should mean bead status `closed`. Follow-on work can add richer terminal predicates if needed.

<!-- REVIEW: added per Major 13 — batch lookup for dependency waits -->

Dependency evaluation uses a single `ListByIDs` call per tick to batch-resolve all dependency bead states, rather than individual lookups per wait. `ListByIDs` (or an equivalent batched read) is a prerequisite for Phase B.

#### Probe waits

Probe waits are the expensive path and need explicit pacing:

- only evaluate when `NextCheckAt <= now`
- run the waiter command under timeout via `evalgate.RunProbe`
- on `Ready=true`, transition to `ready`
- on `Ready=false`, store the next check time using `max(RecheckAfter, default_interval)` or the waiter's default interval if `RecheckAfter` is empty
- on failure, emit `wait.probe_failed`, increment `ConsecutiveFailures`, record the error, apply exponential backoff capped at `max_backoff` from the waiter TOML, and remain `pending`

<!-- REVIEW: added per Minor 18 — probe backoff and failure limits -->

Probe backoff follows exponential growth: `min(default_interval * 2^failures, max_backoff)`. `ConsecutiveFailures` resets to 0 on any successful probe execution, even when `Ready=false`. After `max_failures` consecutive failures (default: 10, configurable per waiter), the wait transitions to `failed` and emits `wait.probe_exhausted`. This prevents indefinite polling of broken probes.

V0 probe waits are restricted to level-trigger predicates only: "is approved now", "is terminal now", "does status equal X now". Edge-trigger predicates like "changed since last check" are out of scope until the wait schema grows a durable probe checkpoint.

### 8) Delivery path

<!-- REVIEW: added per Blocker 1 — delivery delegated to async nudge queue -->

When a wait becomes `ready`, delivery is delegated to the async nudge queue. The wait does **not** close on enqueue. The lifecycle is:

1. The controller verifies the target session is awake in the current continuation epoch. "Awake" here means the reconciler observed `ProbeAlive` for that session on the current tick; `ProbeUnknown` defers synthetic nudge creation until a later tick.
2. The controller ensures a synthetic nudge exists for the target session using a deterministic nudge ID derived from the wait bead ID, continuation epoch, and `delivery_attempt`.
3. The synthetic nudge carries `session_id`, `continuation_epoch`, `source="wait"`, `reference={kind:"bead", id:<wait_id>}`, and a wait-specific TTL that exceeds typical wake latency.
4. The wait remains `ready` while the nudge is queued, claimed, and delivered. `WakeWait` stays active the entire time.
5. The async nudge pipeline keeps an authoritative `gc:nudge` bead keyed by the deterministic nudge ID label. The bead advances to a terminal state before transport cleanup.
6. When Phase 2pre observes the terminal `gc:nudge` bead state, it updates the wait bead and stores the nudge ID, delivery status, and commit boundary in `result_json`.

The controller performs synthetic nudge creation through the nudge dispatcher API, not by writing queue files directly. The dispatcher receives `mode=queue`, `source="wait"`, the deterministic nudge ID, the wait bead reference, the session fence metadata above, and a wait-specific TTL override.

Ownership is split deliberately: queue files are transient transport, while the `gc:nudge` bead is authoritative delivery state. There is no ambiguity about which side wins after a crash: terminal bead state wins, and any lingering queue file is cleaned up as transport residue.

If the session is sleeping, `WakeWait` from the durable `ready` state wakes it on the next tick. Synthetic nudge creation happens only after the session is awake, but the wait remains `ready` until delivery reaches a terminal outcome.

If the session is already awake, the nudge is delivered on the next safe boundary alongside any other queued nudges.

The deterministic nudge ID makes enqueue idempotent across crashes for one delivery attempt. If the controller dies after writing the nudge file, the next tick derives the same ID and sees that the synthetic nudge already exists. Re-enqueue is therefore a no-op rather than a duplicate delivery. `gc wait retry` increments `delivery_attempt`, which deliberately creates a fresh nudge ID while preserving prior failure evidence.

At injection time, the async nudge drainer validates `session_id` and `continuation_epoch` on the synthetic nudge against the live session. On mismatch, it terminalizes the `gc:nudge` bead as `failed` with reason `epoch_mismatch` instead of delivering into the wrong conversation.

Synthetic wait nudges are session-scoped, not merely agent-scoped, but they still live inside the agent-scoped async queue. `QueuedNudge` grows optional `SessionID` and `ContinuationEpoch` fields. `gc notify drain --inject` reads `$GC_SESSION_ID` and skips synthetic wait nudges whose `SessionID` does not match the current session. Legacy nudges without `SessionID` remain agent-scoped.

The drainer also validates that the referenced wait bead is still in `ready` state before injection. If the wait is already in any other terminal state, it terminalizes the `gc:nudge` bead as `failed` with reason `wait_not_ready` instead of delivering stale continuation text.

#### Async nudge outcome mapping

If the dispatcher cannot create the synthetic nudge at all, the wait remains `ready` and Phase 2c retries on the next tick. Typical reasons are queue saturation or a temporary inability to admit the current contract.

Once a synthetic nudge exists, Phase 2pre maps terminal `gc:nudge` bead states back into wait states as follows:

| Nudge outcome | Wait state/result |
|---|---|
| `injected` or `accepted_for_injection` | `closed` |
| `expired` | `failed` with reason `nudge-expired` |
| `failed` with `terminal_reason != ambiguous_post_attempt_crash` | `failed` with that reason |
| `failed` with `terminal_reason = ambiguous_post_attempt_crash` | `failed` with reason `ambiguous_post_attempt_crash` |

Terminal-state precedence is strict: once a wait reaches `closed`, `expired`, `canceled`, or `failed`, no later transition is permitted.

`closed` is intentionally defined against the provider commit boundary,
not end-to-end model observability. For direct `Provider.Nudge()` paths
that boundary is `provider-nudge-return`; for hook-backed paths it is
`accepted_for_injection`, which proves durable transport acceptance but
not that the runtime later exposed the reminder to the model. The
controller records the exact commit boundary in `result_json` so callers
can distinguish those cases.

Authority transfers at the provider attempt boundary. Before the async
nudge drainer writes `LastAttemptAt`/`AttemptBoundary`, the wait bead may
still cancel, expire, or go continuation-stale and step 17's
pre-injection validation will stop delivery. After that boundary, the
`gc:nudge` bead owns the final delivery outcome for that attempt.
`gc wait cancel` returns `delivery-in-flight` instead of forcing
`canceled`, and `default_ready_ttl` no longer expires that attempt.

#### Nudge envelope packing

Wait-originated nudges appear in the async reminder envelope in normal FIFO order. Each wait nudge renders as:

```text
Wait satisfied (w-17):
- <note text>
```

Wait nudges share the existing byte-budget allocation with other nudge items. V1 preserves the async nudge design's FIFO semantics; there is no wait-specific priority reordering.

#### Future interaction with WakeWork

Once `WakeWork` ships, a session may wake for both `WakeWait` and `WakeWork` in the same tick. In that case the session receives both the wait nudge and any pending work. The wait nudge does not suppress or delay work processing. The session prompt should handle both.

### 9) Error handling and recovery

#### Probe failures

Probe execution failures do **not** wake the session. They:

- emit `wait.probe_failed` with the error message
- increment `ConsecutiveFailures` and record `LastError` in the wait bead
- update `next_check_at` using exponential backoff capped at `max_backoff`
- leave the wait in `pending`

After `max_failures` consecutive failures, the wait transitions to `failed` and emits `wait.probe_exhausted`. Operators can use `gc wait ready <id>` to manually satisfy a wait whose probe is broken, `gc wait retry <id>` to increment `delivery_attempt` and reset a failed wait to `pending`, or `gc wait cancel <id>` to discard it when delivery is not already in flight.

Repeated probe failure is therefore noisy and eventually self-limiting, not an infinite loop.

#### Target session closed

If the target session bead is closed, open waits targeting it transition to `canceled` with reason `session-closed`. A closed session has no wake semantics.

#### Continuation epoch change

<!-- REVIEW: added per Major 11 — stale continuation handling -->

If the target session resets its conversation identity and increments `continuation_epoch`, open waits bound to the old epoch transition to `canceled` with reason `continuation-stale` and emit `wait.canceled`. Ordinary sleep/wake cycles do not affect wait validity.

#### Expiration

Expired waits transition to `expired`, emit `wait.expired`, and optionally enqueue a reminder only if the session is already awake. They do not self-wake a sleeping session by default; expiration is operational information, not success.

Within a single tick, trigger satisfaction is evaluated before expiration. If both become true at the same tick boundary, the wait is treated as satisfied.

#### Metadata parse failures

If evaluation-critical metadata (`state`, `continuation_epoch`, `dep_bead_ids`, `probe_waiter`) fails to parse, the controller emits `wait.evaluation_error`, skips that wait for the tick, and makes no state transition. Operators repair the bead manually.

#### Controller crash

Crash recovery is straight NDI:

- `pending` waits remain `pending` and are reevaluated on the next tick
- `ready` waits remain `ready`; the next tick re-ensures the synthetic nudge exists and/or consumes the terminal `gc:nudge` bead state
- terminal states (`closed`, `expired`, `canceled`, `failed`) are inert

No lease-based recovery is needed for delivery because the `delivering` state has been eliminated. Probe leases remain: a probe claim persists as `probe_claimed_until` in the wait bead to prevent duplicate long-running probes if the controller crashes mid-check. Expired probe claims are reclaimed on the next scan.

#### Worked crash-recovery example

Suppose the controller crashes after marking wait `w-17` as `ready` but before enqueueing the synthetic nudge:

1. **On disk after crash:** The wait bead has `state=ready` in its metadata. A synthetic nudge may or may not already exist under its deterministic ID.
2. **Next controller tick:** The reconciler scans open wait beads, finds `w-17` in `ready` state.
3. **Decision:** If session `s-42` is sleeping, `WakeWait` from the durable `ready` state wakes it in Phase 2a. On the first tick where `s-42` is awake in the same continuation epoch, the reconciler ensures the synthetic nudge exists. The wait stays `ready` until the nudge pipeline reports terminal success or failure.
4. **Result:** The deterministic nudge ID makes re-enqueue idempotent across crashes within one delivery attempt. The wait never loses its wake reason or continuation metadata while delivery is still in flight.

### 10) Concurrency model

Gas City still assumes one active controller per city, enforced by the existing lock. The wait subsystem preserves that assumption.

<!-- REVIEW: added per Major 9 — dedicated probe execution pool -->

Required concurrency controls:

- **Probe execution pool:** Probes run in a dedicated bounded pool (default: 2 workers), separate from provider I/O and the reconciler's main tick. This prevents long-running probes from starving session lifecycle phases.
- **Probe lease:** Prevents duplicate probes if the controller crashes mid-check. Stored as `probe_claimed_until` in the wait bead metadata.
- **Tick budget:** Wait evaluation runs as Phase 2c of the reconciler, after the wake/sleep passes but before drain advancement, with a wall-clock sub-budget check at entry (`max_probe_wall_time_per_tick`, default: 2s). This is a sub-budget of the reconciler's overall tick budget, not additive to it. The reconciler reserves a minimum 500ms slice for Phase 2c on every tick so wait evaluation cannot be starved indefinitely by earlier phases. Probes that cannot start within the remaining sub-budget are deferred to the next tick.

Phase 2c never blocks on external probe execution. Its algorithm is:

1. Drain a thread-safe probe completion queue and apply completed results to wait beads.
2. Evaluate cheap waits (ready finalization, dependency checks, event checks).
3. Dispatch newly due probes into the bounded worker pool until the Phase 2c sub-budget is exhausted.

Pool workers do not mutate wait beads directly. They write only `{wait_id, probe_attempt, result}` records into the completion queue. Phase 2pre and Phase 2c are the sole writers of wait bead metadata: Phase 2pre consumes terminal `gc:nudge` bead states, and Phase 2c applies probe completions plus newly satisfied waits. Each dispatched probe increments a monotonic `probe_attempt` field, and Phase 2c applies a completion only when the attempt matches the current wait metadata.

No extra mutexes are needed beyond existing controller single-threaded reconciliation plus the in-memory event fanout path, if implemented.

### 11) Performance

The design should optimize for the common case: many sleeping sessions, few due checks.

<!-- REVIEW: added per Major 13 — indexed selection for due waits -->

- Event waits scale with new events, not with total waits. The in-memory event fanout only touches waits with matching event type filters.
- Dependency waits use a single `ListByIDs` call per tick and can be further optimized via event-driven invalidation using `bead.closed` events.
- Probe waits use a min-heap keyed by `next_check_at` so only due probes are evaluated each tick. The cost target is O(due_waits) per tick, not O(total_waits).
- Probe execution is bounded by `max_probes_per_tick` and `max_probe_wall_time_per_tick` so external polling cannot starve the controller.

Suggested config:

```toml
[session.waits]
enabled = false
allow_extension_waiters = false
default_probe_interval = "30s"
probe_timeout = "10s"
max_probes_per_tick = 10
max_probe_wall_time_per_tick = "2s"
probe_pool_size = 2
probe_claim_ttl = "2m"
default_ready_ttl = "24h"
wait_nudge_ttl = "24h"
max_waits_per_session = 20
wait_retention = "7d"
gc_deletes_per_tick = 20
```

`wait_nudge_ttl` defaults to `default_ready_ttl` and is the TTL passed to
the async nudge dispatcher for wait-originated synthetic nudges.

`default_ready_ttl` is the maximum time a wait may remain in `ready`
state without reaching a terminal nudge outcome and before any provider
attempt boundary has been crossed. When exceeded, the synthetic nudge is
treated as expired and the wait transitions to `failed(nudge-expired)`.

<!-- REVIEW: added per Major 9, Major 14 — probe pool, wall-time budget, retention -->

### 12) Observability

Add event types:

- `wait.registered`
- `wait.ready`
- `wait.nudge_queued` (emitted when the synthetic nudge is enqueued)
- `wait.expired`
- `wait.canceled`
- `wait.failed`
- `wait.probe_failed`
- `wait.probe_exhausted`
- `wait.evaluation_skipped`
- `wait.evaluation_error`

<!-- REVIEW: added per Minor 18 — probe_failed and probe_exhausted events -->

CLI surfaces:

- `gc wait list [--state <state>]` — with probe failure count and backoff in output
- `gc wait list --session <id>` — filter to one session
- `gc wait inspect <id>` — full trigger spec, probe history, backoff state, lease status
- `gc wait cancel <id>` — destructive: discards the wait unless delivery is already in flight, in which case it returns `delivery-in-flight`
- `gc wait ready <id>` — manual satisfaction for broken probes
- `gc wait retry <id>` — increment `delivery_attempt` and reset a failed wait back to pending
- `gc wait clear-hold <session>` — clear `wait_hold` without touching other session state
- `gc wait clear-hold --all` — operator rollback tool: clear every active wait hold in the city
- `gc wait create --target <session>` — operator-level cross-session wait creation
- `gc session list` shows waiting/ready-wait annotations, including `wait-hold` as a distinct reason

<!-- REVIEW: added per Blocker 6 — expanded CLI surfaces -->

Dashboard/operator surfaces should show:

- number of pending waits
- oldest ready wait (metric: `gc_wait_oldest_ready_age_seconds`)
- probe failure count per wait
- probe backoff interval per wait
- wait evaluation duration per tick (metric: `gc_wait_evaluation_duration_seconds`)
- wait evaluation skips (metric: `gc_wait_evaluation_skipped_total`)
- end-to-end wait latency (metric: `gc_wait_ready_to_delivered_seconds`)

`gc wait inspect` surfaces the synthetic nudge ID for `closed` waits so operators can trace delivery into `gc nudge status`.

If Phase 2c is skipped because earlier reconciler phases consumed the tick budget, the controller emits `wait.evaluation_skipped` and increments `gc_wait_evaluation_skipped_total`.

### 13) Backward compatibility

Existing cities keep working unchanged.

- No wait definitions means no new behavior.
- Sessions that never register waits continue to sleep and wake exactly as they do today.
- Existing Orders remain unchanged.

The only controller-visible change is a new optional wake reason when ready waits exist.

<!-- REVIEW: added per Major 14 — garbage collection for terminal wait beads -->

### 14) Garbage collection

Terminal wait beads (`closed`, `expired`, `canceled`, `failed`) are auto-pruned by the controller after `wait_retention` (default: 7d, configurable). This mirrors the 7d retention policy for deconfigured session beads and prevents unbounded accumulation from frequent short-lived waits.

Wait GC runs as bounded Phase 2e with `gc_deletes_per_tick` (default: 20) so retention cleanup cannot starve lifecycle or probe work.

<!-- REVIEW: added per Blocker 2 — prerequisite gates and rollout phasing -->

### 15) Prerequisites and rollout phasing

The wait subsystem depends on two designs that are currently Draft:

- **Unified sessions Phase 2+** — provides `WakeWait` in `wakeReasons()`, session bead model, drain-cancel machinery, runtime generation/instance token fencing, and a stable `continuation_epoch`.
- **Async nudge delivery Phase 1** — provides the claim/terminalization/safe-boundary pipeline that waits delegate delivery to.

#### Hard prerequisites

The controller MUST assert at startup that session bead support, the async nudge delivery path, and batched dependency reads (`ListByIDs` or equivalent) are available before enabling wait evaluation. If any are missing, the wait subsystem is disabled and `gc session wait` returns an error explaining the missing prerequisite.

Rollback to a controller version that predates `wait_hold` handling is not transparent. Before downgrade, operators must clear all active wait holds with `gc wait clear-hold --all` or disable waits entirely; otherwise older controllers may wake sessions unexpectedly.

#### Rollout phases

| Phase | Ships after | Scope |
|---|---|---|
| **A: Storage only** | This design approved | Wait bead CRUD, CLI (`gc wait list/inspect/cancel/ready`), waiter name validation, shared evaluator extraction. No wake integration. |
| **B1: Dependency waits with wake** | Unified sessions Phase 2 merged + async nudge delivery Phase 1 merged | `WakeWait` integration, `wait_hold`, continuation-epoch binding, dependency trigger evaluation, session-scoped synthetic nudges, wait finalization on delivery outcome, sleep-wakeup race protocol. |
| **B2: Operator recovery and tracing** | B1 proven | `gc wait retry`, `gc wait inspect` delivery tracing, explicit recovery workflow for failed wait-originated nudges. |
| **C1: Built-in probe waits** | B2 proven | Probe execution pool, built-in waiter registry, probe backoff/failure limits, `gc wait create --target`. |
| **C2: Extension waiters** | C1 proven in production + operator allowlist enabled | Pack/city waiter definitions, extension args schema validation, custom probe integrations. |
| **D: Event waits** | Allowlisted event families with durable reconstruction contracts | Event trigger evaluation, event fanout, restart-safe reconstruction from durable state. |

Each phase has its own test gate (see §16).

<!-- REVIEW: added per Blocker 3 — mandatory test scenario matrix -->

### 16) Mandatory test scenarios

The following scenarios MUST have deterministic tests before the corresponding phase can merge. Each test uses the fake session/event/bead infrastructure — no real tmux or filesystem timing.

#### Phase A (storage)

| # | Scenario |
|---|---|
| A1 | Wait bead CRUD: create, read, update state, labels updated |
| A2 | Waiter name validation: accept `foo-bar_1`, reject `../etc`, reject empty |
| A3 | Note sanitization: strip `</system-reminder>` tags, enforce 280-char cap |
| A4 | Top-level metadata fields match spec_json content after creation |
| A5 | Existing order gate and order dispatch tests pass unchanged after `evalgate.MatchEvent` extraction |

#### Phase B (deps & wake mechanics)

| # | Scenario |
|---|---|
| B1 | Register-then-immediate-satisfaction: condition already true at step 5, wait goes `ready → closed`, session does not drain |
| B2 | Register-sleep-wake cycle: condition false → sleep → condition becomes true → `WakeWait` appears → session wakes → nudge delivered |
| B3 | Ready-during-drain cancels drain: condition flips while drain is in progress, drain is canceled |
| B4 | Deps-mode "all" with partial completion then crash: 1 of 2 deps closed → controller crash → restart → remaining dep closes → wait fires |
| B5 | Deps-mode "any": first dep closed → wait fires immediately |
| B7 | Target session closed while wait pending: wait transitions to `canceled` |
| B8 | Continuation epoch change: session resets conversation identity → wait transitions to `canceled` with reason `continuation-stale` |
| B9 | Expiration before satisfaction: deadline elapses → wait transitions to `expired`, session not woken |
| B10 | Cancel during ready state (between ready and nudge enqueue): wait transitions to `canceled`, no nudge enqueued |
| B11 | Two ready waits for one session: both produce nudges, session receives both |
| B12 | `wait_hold` suppresses `WakeConfig` but not `WakeWait` |
| B13 | Sleeping session with satisfied wait wakes before wait closes |
| B14 | Crash after nudge enqueue but before wait close reuses deterministic nudge ID and does not duplicate delivery |
| B15 | Synthetic nudge with stale continuation epoch terminalizes as `failed(reason=epoch_mismatch)` instead of being delivered |
| B16 | `gc session wake` clears `wait_hold`/`sleep_intent` and wakes the session |
| B17 | `gc wait cancel` after provider attempt boundary returns `delivery-in-flight` and does not override terminal nudge outcome |

#### Phase C (probe waits)

| # | Scenario |
|---|---|
| C1 | Probe success on first check: `Ready=true` → wait fires |
| C2 | Probe failure backoff and recovery: 3 failures with exponential backoff → success on 4th → wait fires |
| C3 | Probe exhaustion: `max_failures` consecutive failures → wait transitions to `failed` |
| C4 | `gc wait ready` manual satisfaction after probe failure |
| C5 | Probe wall-time budget exceeded: deferred probes carry to next tick |
| C6 | Controller crash mid-probe with lease: lease expires → probe re-runs on next tick |
| C7 | Authorization: session cannot register wait targeting different session without operator access |
| C8 | Built-in waiter arg schema rejects oversized or malformed args |
| C9 | `gc wait retry` increments `delivery_attempt` and resets a failed wait to pending |

#### Phase D (event waits)

| # | Scenario |
|---|---|
| D1 | Event cursor correctness after controller restart: wait captures `AfterSeq`, events before cursor are ignored after restart |

## Primitive Test

This is a **derived mechanism**, not a new primitive.

It composes entirely from existing primitives:

- **beads** for durable wait instances
- **event bus** for event-triggered readiness
- **session wake/drain reconciler** for sleeping and waking processes
- **async nudge delivery** for safe post-wake notification (owns 100% of delivery semantics)

The proposal reuses those primitives to express a blocked continuation. It does not introduce a new storage engine, message bus, or scheduler primitive.

## Drawbacks

- This adds a new first-class concept. Users now need to understand not just sessions and nudges, but also waits and waiter definitions.

- Probe waits introduce controller-owned external polling. That is operationally useful, but it increases the amount of pack code whose correctness now affects scheduling. Probe exhaustion limits (`max_failures`) bound the damage from broken probes.

- A separate waiter-definition catalog is additional surface area. Reusing Orders directly would reduce concept count, but it would blur two different things: global automation and one-session continuation. This proposal chooses conceptual clarity over maximal reuse, while extracting shared evaluation code to prevent drift.

- The design assumes session resumption is the right continuation model. For some roles, waking the same session may be worse than creating a fresh worker bead for a new agent. That is a product choice each workflow will still need to make. Waits on `wake_mode=fresh` sessions are explicitly rejected.

## Known Limitations

<!-- REVIEW: added per review — collecting minor items and missing evidence not addressed inline -->

- **Rollback safety:** Downgrades across `wait_hold` semantics are operationally sensitive. Before rollback, operators must clear all active wait holds. Pending wait beads otherwise become inert until re-upgrade, and sleeping sessions may wake unexpectedly under older controllers.

- **`wake_mode=fresh` compatibility:** Waits targeting `wake_mode=fresh` sessions are rejected at registration. A future extension could support "wait then dispatch fresh" semantics, but that is a different abstraction (closer to Orders) and out of scope.

- **Extension waiter args schemas** remain optional in v0. Built-in waiters always validate args, but pack and city waiters only get schema-level validation when they declare an `[args]` block. API-level size caps still apply in all cases.
