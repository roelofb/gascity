# Async Nudge Delivery

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-13 |
| Author(s) | Codex |
| Issue | — |
| Supersedes | — |

## Summary

Add a city-scoped async nudge subsystem for agent notifications that are not direct chat: a provider-level `WaitForIdle` contract, a transient delivery queue under `.gc/`, a single hook-facing aggregator command for validated startup/turn-boundary injection, a poller path only for providers that can both run a background drainer and expose a safe idle boundary, and a bead-backed authoritative nudge record for every queued nudge. `gc session nudge`, `gc mail --notify`, sling nudges, and equivalent API paths default to `wait-idle -> queue`, while `internal/session.Manager.Send()` remains direct and immediate. The revised design makes five constraints explicit: exactly one automatic poller per running session runtime, queued delivery is allowed only when a safe eventual boundary is proven, terminal delivery outcome is persisted in beads before transport cleanup, queue files are transport replicas rather than authority, failed post-claim injections do not disappear silently, and operators get queue-health visibility without `state.json`-style status files.

## Motivation

Today Gas City has only two effective nudge behaviors:

- direct `runtime.Provider.Nudge()` delivery
- tmux `wait-idle`, followed by immediate send anyway if the wait fails

That is enough for a Claude tmux session with compatible prompt heuristics, but not for the broader provider matrix we now claim to support.

Three concrete failures exist today:

1. Busy non-Claude agents can receive text mid-turn.
   The tmux adapter still calls `NudgeSession()` after `WaitForIdle()` fails, and tmux idle detection is still Claude-specific. A Codex, Gemini, or Cursor session can therefore receive reminder text while the runtime is actively working.

2. Async reminders can be lost when the target is asleep.
   `gc mail send --notify` calls `sp.Nudge()` directly. For missing tmux sessions, the provider treats session-not-found as best-effort success, so the notify path can silently drop the reminder. `gc sling --nudge` is better because it pokes the controller when the target is asleep, but it still has no deferred reminder path.

3. The runtime exports activity, not a safe-delivery contract.
   `GetLastActivity()` is used for coarse idle timeout logic. ACP also has internal busy tracking, but it is intentionally not exposed as a generic provider capability. Gas City therefore has no provider-agnostic answer to "is it safe to inject an async reminder right now?"

This matters for Gastown parity because the current audit gap is larger than "add a queue." We need the full stack:

- a real runtime idle capability
- persisted queueing with deferred delivery
- drain on startup/turn-boundary hooks where available
- background drain for providers without a hook path
- migration of async reminder producers onto that subsystem
- observability strong enough to tell normal deferral from broken delivery

The naive fix, "teach tmux more prompt patterns," is not enough. That still fails for asleep sessions, does not help ACP, and does not give us persistence or deferred reminders.

## Guide-Level Explanation

### Two kinds of session input

After this change, Gas City will have two explicit message paths:

- **direct chat**: user text that should go straight to the session now
- **async nudges**: reminders, wakeups, and coordination messages that should not interrupt active work

Direct chat stays where it is today:

```bash
gc session chat reviewer "Explain the failing test"
```

Async nudges use the existing `gc session nudge` command, but with safe delivery semantics:

```bash
gc session nudge reviewer "Check your mail from overseer"
```

Default behavior:

1. If the runtime can reliably wait for idle, Gas City waits briefly.
2. If the agent becomes idle, the reminder is injected immediately.
3. If the agent stays busy, the reminder is queued.
4. If the session is asleep, the reminder is queued and delivered on a later wake.

The default is therefore "deliver soon without interrupting work," not "blind tmux send-keys."

Explicit override remains available:

```bash
gc session nudge reviewer "Stop and respond now" --delivery=immediate
gc session nudge reviewer "Review this after lunch" --delivery=queue --deliver-after=1h
```

`--delivery=immediate` is intentionally loud in CLI/API help: it is an
interruption path, not ordinary chat, and it bypasses deferred safety.

### What agents see

All async nudge deliveries, including `--delivery=immediate`, use a non-user reminder wrapper. Providers choose the exact wrapper shape, but every queued-capable provider must validate one concrete wrapper before opt-in. The default is:

```text
<system-reminder>
These are independent notifications, not a sequence of instructions.
Pending reminders:
- 2026-03-13T18:47:00Z [from overseer] Check your mail from overseer.
- 2026-03-13T18:49:11Z [urgent] Work was slung to you. Check your hook.
</system-reminder>
```

The delivery path never injects raw user-style text for async nudges. In v1, queued delivery is allowed only for providers whose wrapper has already been validated for async reminder use. Unvalidated wrappers remain immediate-only.

### What changes for hook-enabled providers

Providers that expose a verified async-safe startup/turn boundary keep using hooks, but they no longer run two separate injectors. Their installed startup and turn-boundary hooks call a single aggregator:

- `gc notify drain --inject`

That command:

- reads bounded unread mail summary
- reads due queued nudges

and merges them into one deterministic, provider-ready reminder envelope. Hook templates splice its stdout verbatim and do not add a second provider-specific wrapper. This avoids ordering bugs and provider-specific concatenation drift.

### What changes for poller-based providers

Some providers lack a usable per-turn hook path. They may still support safe deferred delivery if their runtime backend can answer `WaitForIdle()` reliably and their reminder wrapper is validated. Those providers use a hidden `gc nudge-poller <session> <agent>` process.

The poller:

- watches for queued reminders
- waits for a safe idle boundary
- drains and injects due reminders
- exits when the session stops

The controller reconciliation loop is the single owner of automatic poller liveness. It ensures a poller for each running session whose resolved deferred-delivery mode is `poller` and restarts one if it dies while that session stays up.

If a provider cannot safely support either hook drain or poller drain, queued delivery is rejected for that target. There is no "queue now, maybe never deliver" mode in the default path.

Queued reminders do not by themselves self-wake sleeping agents. Existing wake mechanisms such as session start, sling/controller poke, or an operator opening the session remain responsible for wake-up. The only exception is a session-wait synthetic nudge paired with a separate durable wait bead: the wait bead produces `WakeWait`, while the queued nudge remains transport only. The nudge queue is never the wake primitive by itself.

### Config

Async nudge tuning lives under `[session.async_nudges]`:

```toml
[session.async_nudges]
wait_idle_timeout = "15s"
normal_ttl = "30m"
urgent_ttl = "2h"
poll_interval = "10s"
poll_idle_timeout = "3s"
max_queue_depth = 50
max_drain_batch = 10
max_reminder_bytes = 4096
max_failed_retained = 1000
claim_stale_after = "5m"
```

Existing cities do not need to set any of these fields. Defaults are compiled in.

## Reference-Level Explanation

### Scope boundary

This proposal intentionally changes **async reminder producers only**:

- `gc session nudge`
- `gc mail send --notify`
- `gc mail reply --notify`
- sling nudges
- equivalent internal/API reminder producers

It does **not** change direct chat:

- `internal/session.Manager.Send()`
- interactive session chat API routes
- any path whose semantic contract is "send the user's message now"

That separation is mandatory. Async nudges want safe cooperative delivery; chat wants immediacy.

### New runtime contract

`internal/runtime.Provider` grows a first-class idle-wait method:

```go
var ErrIdleUnsupported = errors.New("session idle detection unsupported")
var ErrIdleTimeout = errors.New("agent not idle before timeout")

type Provider interface {
    // existing methods...
    WaitForIdle(name string, timeout time.Duration) error
}
```

`WaitForIdle` is the authoritative runtime capability surface. `ProviderCapabilities` does not grow a second idle boolean.

Semantics:

- `nil`: provider observed a verified safe idle boundary within `timeout`
- `ErrIdleTimeout`: provider supports idle waiting, but the session stayed busy
- `ErrIdleUnsupported`: provider cannot safely answer the question
- session-not-found / provider errors: terminal delivery errors, not queue hints

Minimum contract:

- a provider must not return `nil` based on "no output for a while"
- prompt-based implementations must use a multi-sample confirmation window, not a single snapshot
- callers must treat `ErrIdleUnsupported` as "do not inject mid-turn"

Implementations:

- `tmux`: uses provider/session metadata describing a verified prompt-based idle detector. If the session lacks one, returns `ErrIdleUnsupported`.
- `acp`: returns `ErrIdleUnsupported` in v1. ACP may opt in later only after it exposes a confirmed idle boundary with the same false-positive protection required of tmux.
- `auto` / `hybrid`: resolve the routed backend for the live session and delegate there. The resolved backend is fixed for the lifetime of that running session. A later sleep/wake cycle re-resolves from scratch.
- `exec`, `subprocess`, `k8s`, `fake`: return unsupported unless they can implement the real contract.

`poll_idle_timeout = 3s` is chosen for prompt-detector backends only: it is comfortably above the tmux 2-poll confirmation window and is not intended to cover ACP-like backends in v1 because those remain unsupported.

### Provider metadata

Built-in provider metadata gains explicit delivery metadata, but runtime delivery authority still lives in `WaitForIdle()`:

```go
type AsyncNudgeDrainMode string

const (
    AsyncNudgeDrainHook   AsyncNudgeDrainMode = "hook"
    AsyncNudgeDrainPoller AsyncNudgeDrainMode = "poller"
    AsyncNudgeDrainNone   AsyncNudgeDrainMode = "none"
)

type AsyncNudgeBoundary string

const (
    AsyncNudgeBoundaryNone        AsyncNudgeBoundary = "none"
    AsyncNudgeBoundaryTurnEndHook AsyncNudgeBoundary = "turn-end-hook"
)

type HookTransport string

const (
    HookTransportNone                 HookTransport = "none"
    HookTransportStdoutReminderSplice HookTransport = "stdout-reminder-splice"
)

type ReminderEnvelope string

const (
    ReminderEnvelopeSystemReminder ReminderEnvelope = "system-reminder"
    ReminderEnvelopeNoticeBlock    ReminderEnvelope = "notice-block"
)

type IdleDetector struct {
    Kind             string // "tmux-prompt"
    PromptPrefix     string
    BusyIndicator    string
    ConsecutivePolls int
}

type ProviderSpec struct {
    // existing fields...
    IdleDetector        *IdleDetector
    AsyncNudgeDrainMode AsyncNudgeDrainMode
    AsyncNudgeBoundary  AsyncNudgeBoundary
    HookTransport       HookTransport
    ReminderEnvelope    ReminderEnvelope
    MaxReminderBytes    int
}
```

Rules:

- `IdleDetector` is install-time/runtime-hint metadata for backends like tmux. It does not replace `WaitForIdle()`.
- `AsyncNudgeDrainMode=hook` is valid only when all of the following are true:
  - `AsyncNudgeBoundary=turn-end-hook`
  - `HookTransport` is a concrete, implemented transport contract
  - `ReminderEnvelope` is validated for that provider family
  - `MaxReminderBytes > 0`
- "Supports hooks" is not sufficient. The provider must prove that the hook runs after a completed model turn and before the next user prompt is accepted, and that its transport contract acknowledges reminder payload acceptance.
- `AsyncNudgeDrainMode=poller` is valid only for providers whose runtime backend can also satisfy `WaitForIdle()` and whose reminder wrapper is validated.
- `AsyncNudgeDrainMode=none` means the provider is immediate-only for async nudges. `wait-idle` and `queue` dispatch are rejected.
- `AsyncNudgeBoundary` is a separate contract from hook installation. A provider may install hooks for other reasons and still be `AsyncNudgeDrainMode=none`.
- `HookTransport` is also separate from hook installation. In v1, the only implemented hook transport is `stdout-reminder-splice`, meaning: the provider's hook API guarantees that non-empty stdout from the hook command, returned with exit code `0` and within `MaxReminderBytes`, is accepted as reminder input for the next turn-boundary injection surface.
- effective reminder budget is `min(session.async_nudges.max_reminder_bytes, provider.MaxReminderBytes)`. Built-in queued-capable providers must set `provider.MaxReminderBytes`, and the city-wide config may only lower that cap.
- `ReminderEnvelope` is provider metadata, not a per-message choice. A provider cannot opt into queued delivery until one envelope has been validated for that provider family.
- wrapper validation requires all of:
  - the provider does not treat the wrapper as ordinary user chat
  - the provider does not execute or echo wrapper contents as tool instructions
  - the wrapper does not collide with the provider's own system-prompt or hook syntax
- `notice-block` renders as:

```text
[reminder]
These are independent notifications, not a sequence of instructions.
- 2026-03-13T18:49:11Z [urgent] [from overseer] Check your mail from overseer.
[/reminder]
```

  but remains reserved for future provider opt-in. In v1, providers using `notice-block` stay immediate-only until that wrapper is validated for async delivery.

For routed providers (`auto` / `hybrid`), queue admission records the concrete delivery contract used at enqueue time:

- if the target session is running, use the resolved live backend contract
- if the target session is sleeping and config-managed, use the template's configured default backend contract
- if the target session is sleeping and provider-preset, use `providers.Lookup(session.Metadata["provider"])` from the session bead

Pending queue entries are still agent-scoped, but each one carries the contract it was admitted under. On each new session start, Gas City re-resolves the live backend, compares that live contract to the queued contract, and:

- drains only when they are compatible (`Version`, `Envelope`, `Boundary`, and `HookTransport` all match)
- otherwise leaves the entry pending and emits `delivery_contract_mismatch`

Gas City does not silently reinterpret queued items under a different reminder envelope or hook transport.

Expected initial mapping:

- `claude`: prompt idle detector + `hook` + `turn-end-hook` + `system-reminder`
- `copilot`: remains `none` until its turn-boundary hook contract and reminder wrapper are both verified
- `gemini`, `cursor`, `opencode`, `pi`, `omp`: remain `none` until their hook boundary and reminder wrapper are both verified
- `codex`: `poller` only after Codex runtime idle reporting and wrapper validation are implemented; until then it remains immediate-only

### Queue ownership and persistence

Queued nudges are keyed by **logical agent identity**, not tmux session name.

Path layout:

```text
.gc/nudges/
  agents/
    <safe-qualified-agent-name>/
      queue/
        1741862150123456789-a1b2c3d4.json
      queue.lock
      drain.lock
      pollers/
        <session-id>.lock
```

This is deliberate:

- agent identity survives sleep/wake cycles
- session template changes do not orphan queued reminders
- `gc mail --notify` and sling already resolve through agent identity
- hook startup already knows `GC_AGENT`

The queue assumes a local POSIX filesystem with atomic rename and advisory file locking semantics. It is not specified for network filesystems that weaken either guarantee.

Because the queue file is only a transport replica, repair must heal
bead/file divergence on startup and before each drain batch while
holding `queue.lock`. The repair helper is shared, but the mutation
rules are serialized by that lock so enqueue depth accounting and file
materialization cannot race.

- queued `gc:nudge` bead with no transport file (`queue/` or claimed) -> recreate the queue file from bead metadata
- terminal `gc:nudge` bead with lingering transport file -> delete the file without redelivery
- transport file with no matching `gc:nudge` bead -> delete the file as orphaned transport residue and emit queue-health degradation

Canonical ownership stays **agent-scoped** even in the unified-sessions
world:

- locks, queue depth, and backpressure remain per `agent`
- individual entries may additionally bind to `session_id` and `continuation_epoch`
- a drainer may claim only unscoped entries or entries bound to its own live session
- entries bound to some other live session are skipped in place and do not head-of-line block later deliverable items

Every queue state transition uses the same persistence protocol:

1. write the new JSON payload to a temp file in the destination
   directory
2. `fsync()` the temp file
3. `rename()` into place atomically
4. `fsync()` the parent directory

This protocol applies to enqueue, claim metadata updates, restore,
expiry removal, and transport-file cleanup.

The queue entry format is mechanical:

```go
type DeliveryContract struct {
    Version        int    `json:"version"`
    ProviderFamily string `json:"provider_family"`
    DrainMode      string `json:"drain_mode"`
    Boundary       string `json:"boundary"`
    HookTransport  string `json:"hook_transport"`
    Envelope       string `json:"envelope"`
}

type NudgeReference struct {
    Kind string `json:"kind"` // mail | bead | work | none
    ID   string `json:"id"`
}

type QueuedNudge struct {
    ID           string    `json:"id"`
    BeadID       string    `json:"bead_id"`
    Agent        string    `json:"agent"`
    SessionID    string    `json:"session_id,omitempty"`
    ContinuationEpoch string `json:"continuation_epoch,omitempty"`
    Message      string    `json:"message"` // reminder text only; max 280 chars
    Reference    *NudgeReference `json:"reference,omitempty"`
    Sender       string    `json:"sender,omitempty"`
    Source       string    `json:"source"`
    Priority     string    `json:"priority"` // display + ttl bucket only; never affects drain order
    CreatedAt    time.Time `json:"created_at"`
    DeliverAfter time.Time `json:"deliver_after,omitempty"`
    ExpiresAt    time.Time `json:"expires_at,omitempty"`
    Contract     DeliveryContract `json:"contract"`
}

type ClaimMeta struct {
    ClaimedAt        time.Time  `json:"claimed_at"`
    ClaimerPID       int        `json:"claimer_pid"`
    ClaimerStartTime uint64     `json:"claimer_start_time"`
    ClaimerToken     string     `json:"claimer_token"`
    LastAttemptAt    *time.Time `json:"last_attempt_at,omitempty"`
    AttemptBoundary  string     `json:"attempt_boundary,omitempty"` // provider-nudge-return | hook-transport-accepted
}

type NudgeTerminal struct {
    Outcome        string     `json:"outcome"`         // injected | accepted_for_injection | failed | expired
    TerminalAt     *time.Time `json:"terminal_at,omitempty"`
    TerminalReason string     `json:"terminal_reason,omitempty"`
    Provider       string     `json:"provider,omitempty"`
    DrainMode      string     `json:"drain_mode,omitempty"`
    CommitBoundary string     `json:"commit_boundary,omitempty"`
    LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
}
```

Authoritative persistence uses an internal bead per nudge:

```go
beads.Bead{
    Type:  "nudge",
    Title: "nudge:" + queued.ID,
    Labels: []string{
        "gc:nudge",
        "agent:" + queued.Agent,
        "nudge:" + queued.ID,
        "source:" + queued.Source,
    },
    Metadata: map[string]string{
        "nudge_id":            queued.ID,
        "agent":               queued.Agent,
        "session_id":          queued.SessionID,
        "continuation_epoch":  queued.ContinuationEpoch,
        "state":               "queued",
        "source":              queued.Source,
        "priority":            queued.Priority,
        "sender":              queued.Sender,
        "reference_json":      "{...}",
        "message":             queued.Message,
        "contract_json":       "{...}",
        "deliver_after":       "...",
        "expires_at":          "...",
        "terminal_json":       "",
    },
}
```

The queue file is a transport replica of that bead plus claim metadata.
If the two diverge after a crash, the bead is authoritative and the next
repair/drain pass reconciles the queue file to it by `nudge_id`.

Queue entries are a transient delivery buffer only:

- no inbox
- no read/unread
- no archive
- no threading
- no search
- no user-visible message history
- no authoritative task or decision record

Durable human-readable communication remains mail/beads. A queued nudge is only a wake/reminder pointer. If the content is substantive enough that losing the reminder wrapper would matter, it belongs in mail first and the nudge should point at that mail.

When a nudge is derived from mail, a bead, or routed work, the producer
must populate `Reference` with the authoritative object. `Message`
remains reminder/pointer text only. `gc nudge status` and `gc nudge
show` surface references before message text.

### Runtime epoch and drainer ownership

Every running session has a controller-owned `RuntimeEpoch` token. This
is not a new persisted field: it is the session bead's current
`generation`, exported under a nudge-specific name so drainer contracts
do not depend on unified-sessions internals. The controller mints a new
runtime epoch on every wake; any future backend-reroute flow that
replaces the runtime without a full sleep/wake must increment
`generation` first and therefore also mint a new runtime epoch. The
controller exposes that token to automatic drainers:

- pollers are launched with `--runtime-epoch <epoch>`
- hook templates receive `GC_RUNTIME_EPOCH=<epoch>`

Automatic drainers must verify all of the following before they may
claim any queue entries:

- the agent's current live `RuntimeEpoch` matches the drainer's epoch
- the live provider family matches the drainer's expected provider
- the live drain mode matches the calling context (`hook` vs `poller`)
- live readiness is satisfied for that drainer type

If any check fails, the drainer emits `epoch_mismatch`,
`mode_mismatch`, or `delivery_contract_mismatch` and exits without
claiming.

Changing effective drain mode for a running agent also mints a new
`RuntimeEpoch`. That forces any in-flight drainer from the old contract
to abort on its next epoch check before the new contract becomes
`hook_ready` or `poller_running`.

`RuntimeEpoch` and unified-sessions `continuation_epoch` are different
fences:

- `RuntimeEpoch` (the current `generation`) changes on every wake/runtime replacement and prevents stale drainers from touching a replaced runtime
- `continuation_epoch` changes only when the conversation identity changes and prevents a wake continuation from landing in the wrong conversation

Wait-generated synthetic nudges are checked against both fences in
practice: the live drainer context supplies `RuntimeEpoch`, and the
queued nudge carries `ContinuationEpoch`. `RuntimeEpoch` gates who may
drain right now; `ContinuationEpoch` gates which conversation may accept
the reminder.

### Queue state machine

```text
enqueue -> pending (.json)
pending -> claimed (.json.claimed.<pid>.<start>.<token>) by exclusive drainer
claimed -> injected                                 update nudge bead terminal state, then delete claim file
claimed -> accepted_for_injection                   update nudge bead terminal state, then delete claim file
claimed -> failed                                   update nudge bead terminal state, then delete claim file
claimed -> failed                                   update nudge bead terminal state on stale recovery if attempt metadata exists, then delete claim file
claimed -> pending                                  restore only when claimer is dead, age > claim_stale_after, and no attempt metadata exists
pending -> expired                                  update nudge bead terminal state, then delete queue file
```

Concurrency rules:

- enqueue writes a new unique JSON file in `queue/`
- a drainer must first hold `drain.lock`
- drain scans queue files in filename order for stable FIFO among deliverable items
- future-dated entries are skipped in place and do not head-of-line block later due items
- expired entries are terminalized in the nudge bead before their queue file is removed
- only the selected due batch is atomically renamed to claimed files
- stale-claim recovery compares PID and process start time before restoring
- hook drain, poller drain, and manual drain all share the same `drain.lock`

This proposal does **not** claim strict mathematical at-most-once delivery. Providers do not expose a stronger end-to-end ack handshake today. The design therefore records the commit boundary explicitly:

- `provider-nudge-return` means `Provider.Nudge()` returned success
- `hook-transport-accepted` means the hook adapter observed successful handoff through the provider's declared hook transport contract, but not that the runtime confirmed model insertion

Claims are deleted only after one of those boundaries is reached and the
authoritative nudge bead has been updated to the matching terminal
state. The async nudge subsystem is the sole owner of terminal and
ambiguous delivery classification: `ambiguous_post_attempt_crash`,
`expired`, `failed`, `injected`, and `accepted_for_injection` are all
authored here, not reinterpreted by wait consumers.
`delivery_contract_mismatch` is intentionally non-terminal: it leaves
the nudge pending with degraded queue-health until an operator
recontracts it or it expires. Stale claims with recorded attempt
metadata are terminalized as `failed`, not restored to `queue/`, so
ambiguous post-attempt crashes prefer observable failure over duplicate
redelivery.

Terminalization is a write-ordered protocol:

1. materialize `NudgeTerminal`
2. read the `gc:nudge` bead for `nudge_id`; if it is already terminal, skip the write and treat any lingering transport file as residue to delete
3. otherwise update the `gc:nudge` bead with terminal metadata
4. commit the bead-store write durably
5. only then delete the successful claim file or remove the expired queue file

The bead update is the authoritative success/failure record that waits
and operators consume. A lingering queue file after a terminal bead state
is transport residue, not delivery ambiguity; the next repair/drain pass
removes it without redelivery.

### Drain selection algorithm

Every drainer uses the same two-phase algorithm:

1. Gather bounded unread-mail summary first.
2. Acquire `drain.lock`.
3. Resolve the live drainer session context: `agent`, `session_id`, `continuation_epoch`, `RuntimeEpoch`, provider family, and drain mode.
4. Resolve stale claim files before any queue-file repair:
   - terminal bead + lingering claim file -> delete the claim file
   - no bead + lingering claim file -> delete the claim file as orphaned transport residue
   - live or reclaimable claim -> let stale-claim recovery own it
5. Scan `queue/` in filename order without claiming anything yet.
6. For expired entries, update the nudge bead to `expired` and remove the queue file.
7. Skip entries whose `DeliverAfter` is still in the future without renaming them.
8. Skip session-scoped entries whose `SessionID` does not match the live drainer session; leave them in place.
9. Collect up to `max_drain_batch` due entries in stable FIFO order.
10. Build one merged reminder envelope as a single batch unit from the bounded mail summary plus due nudge metadata.
11. Apply the envelope packing algorithm:
   - reserve wrapper/preamble overhead first
   - cap mail summary to at most one third of the effective byte budget
   - reserve the remaining budget for nudges in FIFO order
   - if the combined envelope is too large, shrink the nudge batch until it fits
   - if due nudges exist, mail summary may be truncated but due nudges are not dropped behind mail growth
   - if even one due nudge cannot fit with the wrapper, emit no nudge batch and leave all due nudges pending
12. Only after envelope assembly succeeds, rename the selected due entries to claimed files and write `ClaimMeta`.
13. Release `drain.lock`.
14. For poller delivery, call `WaitForIdle()` one more time after claim and before any attempt metadata is written. If that re-check fails, restore the claims because no provider attempt has begun yet.
15. For hook delivery, revalidate `RuntimeEpoch`, provider family, drain mode, and live hook readiness after claim and immediately before hook handoff. If the live contract changed, restore the claims because no provider attempt has begun yet; queue-health and the nudge bead's non-terminal contract metadata surface the mismatch.
16. For every claimed entry with `SessionID` or `ContinuationEpoch`, revalidate them against the live session. On mismatch, terminalize that nudge as `failed(reason=session_mismatch|epoch_mismatch)`, delete its claim file, and continue without attempting provider injection for that entry.
17. For every claimed entry with `Source="wait"` and `Reference.Kind="bead"`, re-read the referenced wait bead and require `state=ready`. If the wait is no longer ready, terminalize the nudge as `failed(reason=wait_not_ready)`, delete its claim file, and continue without injection.
18. Persist `LastAttemptAt=now` and `AttemptBoundary` durably to every remaining claimed entry before any provider call or hook handoff begins.
19. Attempt injection through the live provider contract.
20. On success, update the nudge bead to its terminal success state, delete claims, and emit the matching terminal event.
21. On failure, update the nudge bead to `failed`, delete claims, and emit the matching terminal event.

Once step 18 completes, the claimed `gc:nudge` bead is authoritative for
that delivery attempt. Higher-level subsystems such as waits may still
record that a cancel/reset raced with delivery, but they do not recall
or overwrite an in-flight attempt after the provider boundary has been
crossed.

If envelope assembly fails before step 9, queue state is unchanged except for expired-entry cleanup. Partial source handling is pre-claim only:

- if mail summary fetch fails but nudge batch renders, the drainer may inject a nudge-only envelope
- if nudge selection/render fails, the drainer may emit mail-only summary and leaves all nudges untouched

Once claims are taken, the merged envelope is committed as a single unit. There is no post-claim split where mail is accepted but claimed nudges are only partially committed.

### Dispatcher API

Async reminder producers call a dedicated dispatcher in `internal/nudge`, not `runtime.Provider.Nudge()` directly:

```go
type DeliveryMode string

const (
    DeliveryWaitIdle  DeliveryMode = "wait-idle"
    DeliveryQueue     DeliveryMode = "queue"
    DeliveryImmediate DeliveryMode = "immediate"
)

type Options struct {
    Mode              DeliveryMode
    Priority          string
    DeliverAfter      time.Time
    Source            string
    Sender            string
    NudgeID           string          // optional deterministic override for controller-owned flows
    SessionID         string          // optional session fence inside an agent-scoped queue
    ContinuationEpoch string          // optional conversation fence paired with SessionID
    Reference         *NudgeReference // optional authoritative pointer surfaced in status/show
    TTL               time.Duration   // optional expiry override for controller-owned flows
}

type Result struct {
    Outcome        string // injected | accepted_for_injection | queued | degraded | rejected
    DeliveryMode   DeliveryMode
    CommitBoundary string // provider-nudge-return | hook-transport-accepted | none
    Agent          string
}
```

Dispatch algorithm:

1. Resolve agent identity.
2. Validate message size (`<= 280` chars). Oversized or long-form content is rejected with guidance to use mail/beads instead.
3. Resolve the current runtime session/backend only if the target is running.
4. If `Options.NudgeID` is set, look up an existing `gc:nudge` bead labeled `nudge:<NudgeID>`. If one exists, treat the call as idempotent: return its current terminal/non-terminal outcome and do not enqueue a duplicate transport file.
5. Resolve the effective delivery contract to admit:
   - running target session: use the resolved live backend
   - sleeping config-managed session: use the template's configured default backend
   - sleeping provider-preset session: use `providers.Lookup(session.Metadata["provider"])` from the target session bead
6. Validate that the effective contract exposes either a live-safe `hook` path or a `poller` path with `WaitForIdle()`.
7. Validate `DeliverAfter < ExpiresAt`. When the caller omits `ExpiresAt`, compute it from `max(CreatedAt, DeliverAfter) + ttl`.
8. Persist the resolved `DeliveryContract` onto the queued entry if the reminder will be enqueued.
9. If `Mode=immediate`:
   - require the session to be running
   - inject the provider-specific reminder envelope directly
10. If `Mode=wait-idle`:
   - if the session is running, call `WaitForIdle`
   - on success, inject directly
   - on `ErrIdleTimeout`, enqueue
   - on `ErrIdleUnsupported`, enqueue only if the provider has a valid drain path that will eventually observe a safe boundary; otherwise return `degraded` error
11. If `Mode=queue`:
   - require a valid drain path (`hook` or `poller+WaitForIdle`)
   - enqueue regardless of whether the session is running
12. After enqueue:
   - `hook`: nothing else required
   - `poller`: emit queue-health immediately and rely on controller reconciliation to ensure the poller whenever the session is running
   - producers that also created work may still poke the controller

The dispatcher's public CLI/API contract is agent-based. Session-scoped
fields are controller-internal escape hatches used by waits and other
continuation mechanisms; ordinary producers do not choose them.

Enqueue holds `queue.lock` for queue-depth accounting. While holding
that lock, the producer:

- counts current pending entries
- rejects when `max_queue_depth` would be exceeded
- creates or updates the authoritative `gc:nudge` bead with `state=queued`
- writes the new queue entry durably with a pointer to that bead

`Provider.Nudge()` remains the low-level immediate send primitive after
this refactor. It no longer owns any wait-idle policy. The dispatcher
owns the `WaitForIdle() -> inject` sequence so the wait contract is
specified exactly once.

The dispatcher MUST NOT return success until both the authoritative
`gc:nudge` bead and the transport queue file have been durably written.
If queue-file persistence fails after the bead write, the dispatcher
must delete the newly created non-terminal nudge bead before returning
the error so a synchronous failure cannot later become an asynchronous
delivery. Reconstruction of a missing queue file is therefore only a
crash-recovery path for bead writes that completed before the process
died, not for calls that already returned failure. If the dispatcher
crashes after writing the queue file but before returning to the caller,
replay with the same `nudge_id` is idempotent because the dispatcher
first looks up an existing `gc:nudge` bead labeled `nudge:<id>`.

### Injection formatting

Direct wait-idle delivery and queue drain use the same formatter:

```go
func FormatForInjection(entries []QueuedNudge, envelope ReminderEnvelope) string
```

Rules:

- all async delivery modes, including `immediate`, use the same envelope
- immediate-mode envelopes include a visible `[delivery: immediate]` marker so operators and agents can distinguish emergency interruption from cooperative deferred delivery
- preserve FIFO order within the batch
- show timestamp on every line
- show priority only when not normal
- prefix sender only when present
- include a preamble that says the entries are independent notifications, not a sequence of instructions
- escape or strip nested reminder-envelope markers and XML-like tags from `QueuedNudge.Message` before assembly
- do not summarize or rewrite content
- emit nothing for an empty batch
- `Priority` does not affect drain order in v1; it only affects display and default TTL selection
- mail summary is summary-only: at most 5 unread items, sender + subject preview only, max 40 chars of subject preview, max 1 KiB total
- mail summary preamble says counts are approximate point-in-time observations

### Hidden and debug commands

Hidden helper commands:

- `gc notify drain [agent] --inject`
- `gc nudge-poller <session> <agent>`

Debug/operator commands:

- `gc nudge status <agent>`
- `gc nudge show <nudge-id>`
- `gc nudge drain <agent>`
- `gc nudge recontract <agent>`
- `gc nudge status <agent> --json`

`gc notify drain --inject`:

- resolves the target agent from `$GC_AGENT` or arg
- reads `$GC_SESSION_ID`, `$GC_CONTINUATION_EPOCH`, and `$GC_RUNTIME_EPOCH` from the live session context when present
- reads unread mail summary plus due queued nudges
- verifies live drainer readiness (`hook_ready`, `hook_unverified`, `poller_running`, `poller_missing`, `drainer_impossible`)
- enforces runtime-epoch and mode guards before any claim
- emits one merged provider-ready reminder envelope through the live provider's declared hook transport contract
- returns one of five result classes internally: `empty`, `rendered`, `partial`, `suppressed-error`, `mode-mismatch`
- in `--inject` mode, always exits 0 so hook runners do not break the session lifecycle
- on `suppressed-error`, attempts a one-line degraded reminder within `MaxReminderBytes` (`notification system degraded; run gc nudge status`) using the same `ReminderEnvelope` before falling back to empty output
- emits hook-drain observability for every invocation so "empty" and "broken but suppressed" are distinguishable
- does not write snapshot status files; operators reconstruct recent hook health from events and `gc:nudge` beads

`gc nudge-poller`:

- runs as a detached child process, similar to `gc daemon start`
- acquires `pollers/<session-id>.lock` for its target session
- checks queue health every `poll_interval`
- emits a heartbeat every `max(30s, 5*poll_interval)` while alive
- calls `WaitForIdle(session, poll_idle_timeout)`
- only drains after a verified idle boundary
- emits `session.nudge_poller_idle_miss` on `ErrIdleTimeout`
- counts consecutive `ErrIdleUnsupported`; after 3 strikes it emits a fatal poller error with reason `idle_permanently_unsupported` and exits
- holds its session-scoped poller lock for its lifetime, but acquires `drain.lock` only for queue scan/claim work
- exits if the session stops or the agent's resolved drain mode is no longer `poller`
- exits when the session stops

`gc nudge drain <agent>`:

- uses the same due-item selection, claim, and commit rules as automatic drain
- does not bypass idle-boundary rules in v1
- exists for operator diagnosis and safe replay workflows, not force injection

`gc nudge recontract <agent>`:

- rewrites pending `DeliveryContract` metadata to the current live contract
- is operator-only and requires a running session with a validated deferred path
- does not rewrite message text, references, TTLs, or priority
- exists for explicit recovery from `delivery_contract_mismatch`, not automatic rerouting

`gc nudge status <agent>` reports:

- queue depth
- oldest pending age
- oldest due age
- due count
- recent terminal success count
- recent terminal failure count
- drain mode
- drainer readiness state
- whether the session is running
- which poller locks/processes are active for this agent queue
- latest poller heartbeat event if known
- latest hook-drain result event if known
- latest terminal nudge failure from `gc:nudge` beads if known

`gc nudge status` derives its view from live queue scan, live
lock/session checks, recent `session.nudge_*` events, and recent
`gc:nudge` beads. There is no `state.json` authority file.
For session-scoped due nudges, readiness and heartbeat fields are
computed against the bound session; for unscoped nudges, any compatible
live drainer for the agent counts.

### Hook changes

Provider hook templates change only at the startup/turn-boundary injection points.

Every hook-capable provider calls the same aggregator:

- `gc notify drain --inject`

Observable contract:

- one command
- one reminder envelope
- mail summary first, then due nudges
- empty output when nothing is pending
- stderr is not part of the injected prompt
- only providers whose `HookTransport` is explicitly implemented may use this path
- in v1, only `stdout-reminder-splice` is implemented; providers with transform-style or JSON-return hooks remain `AsyncNudgeDrainMode=none` until an adapter is added
- queue claims are deleted only after the command reaches `hook-transport-accepted` and the authoritative `gc:nudge` bead is durably terminalized
- hook-mode success is recorded as `accepted_for_injection`, not `injected`, because the runtime does not give a stronger ack today

Live hook readiness requires all of:

- provider metadata says `hook`
- installed hooks are present for the agent
- installed hook version matches the async-nudge aggregator version
- payload fits `MaxReminderBytes`
- current runtime epoch and provider family match the hook invocation context

If any check fails, the effective drainer state becomes `hook_unverified`
or `drainer_impossible`, queue drain is skipped, and queue-health emits a
degraded reason.

Hook-capable providers must invoke the same aggregator once during
session startup after hook installation and before the session accepts
its first normal turn. This gives a session that woke because of a wait
one immediate safe delivery opportunity even if it would otherwise sit
idle with no natural turn boundary. Providers that cannot prove either
startup execution or a poller path remain ineligible for queued delivery
to sleeping sessions.

`gc hook --inject` remains work-only. It does not become a kitchen-sink notification command.

Legacy installed hook files must not keep the old split path alive
forever. Migration requirement:

- old `gc mail check --inject` hook payloads delegate to `gc notify drain --inject` when async nudges are enabled
- freshly installed hook configs carry an aggregator version marker
- `gc prime --hook` upgrades known older hook versions in place

### Producer migrations

The first migration set is:

- `cmd/gc/cmd_session.go`
  `gc session nudge` defaults to `--delivery=wait-idle`, adds `--delivery`, `--priority`, and `--deliver-after`, and always persists `SessionID` plus the session's current `ContinuationEpoch` so session-directed nudges cannot spill into sibling sessions that share the same template/agent queue.

- `cmd/gc/cmd_mail.go`
  `--notify` uses the async dispatcher instead of direct `sp.Nudge()`. The mail write remains the primary action; notify failure is surfaced as warning/error metadata, not a mail-send rollback. Agent-targeted notify paths remain unscoped unless the existing routing layer has already selected one live session.

- `cmd/gc/cmd_sling.go`
  `--nudge` uses the async dispatcher. Controller poke behavior stays, because routing work still needs a wake signal.

- `internal/api/handler_sling.go`
  uses the same async dispatcher to avoid API/CLI drift.

No change is made to `internal/session/chat.go`.

### Error handling

Expected failure modes and handling:

- **queue directory create/write fails**
  Return error to explicit CLI/API callers. Best-effort producers log warning and keep the primary action successful if that action already committed (for example, mail send or work routing).

- **queue full**
  Return error. Full queue is operator-visible backpressure, not a reason to silently interrupt the agent.

- **session not running for `immediate`**
  Return error. The caller explicitly requested non-deferred delivery.

- **provider has no valid deferred drain path**
  Return `degraded` error for `queue` and `wait-idle`. Default async delivery must not pretend to be durable without an eventual safe delivery path.

- **controller cannot ensure poller**
  The queue entry remains pending, controller reconciliation keeps retrying poller ownership for the specific target session while that session is running, and observability emits `session.nudge_poller_unavailable` plus queue-health degradation. The dispatcher does not spawn an unmanaged second owner.

- **poller wedges without exiting**
  Controller reconciliation treats the poller as stale only when both of
  these are true:
  - `last_poller_heartbeat` is older than `3 * heartbeat_interval`
  - `pollers/<session-id>.lock` is non-blocking acquirable by reconciliation

  On stale takeover, reconciliation emits `session.nudge_poller_error`
  and starts a new poller for that session's current runtime epoch.

- **inject fails after claim**
  Update the `gc:nudge` bead to `failed`, emit `session.nudge_failed`,
  delete the queue file, and do not auto-retry in v1. Operators can
  inspect and manually decide whether to requeue after diagnosis. This
  preserves evidence without creating retry storms.

- **stale claim recovered**
  Restore to `queue/` only after claimer-death verification with matching process start time and only when no `LastAttemptAt` is recorded. If a stale claim already has attempt metadata, update the `gc:nudge` bead to `failed` with reason `ambiguous_post_attempt_crash` and delete the queue file.

- **agent removed from config**
  Leave queue files on disk, emit queue-health degradation, and require operator cleanup or replay. Queue state is transport evidence and is not deleted silently on config edits.

- **drain contract disappears after reroute or config change**
  Leave pending entries untouched, emit queue-health degradation with `due_without_active_drainer`, and do not silently downgrade to immediate delivery. Operators may wait for the original contract to return, let TTL expire, or run `gc nudge recontract <agent>` to re-admit the pending entries under the current live contract.

### Observability

All lifecycle events include:

- `nudge_id`
- `agent`
- `source`
- `mode`
- `drain_mode`
- `provider`
- `commit_boundary`
- `reason` when not delivered immediately

New events:

- `session.nudge_queued`
- `session.nudge_injected`
- `session.nudge_accepted_for_injection`
- `session.nudge_expired`
- `session.nudge_failed`
- `session.nudge_queue_health`
- `session.nudge_claim_recovered`
- `session.nudge_queue_rejected`
- `session.nudge_poller_started`
- `session.nudge_poller_stopped`
- `session.nudge_poller_heartbeat`
- `session.nudge_poller_idle_miss`
- `session.nudge_poller_error`
- `session.nudge_poller_unavailable`
- `session.nudge_hook_drain`
- `session.nudge_drain_partial`

Required `reason` values:

- `session_not_running`
- `idle_timeout`
- `idle_unsupported`
- `deliver_after_not_reached`
- `expired`
- `queue_full`
- `poller_unavailable`
- `hook_not_available`
- `inject_failed`
- `claim_stale_restored`
- `ambiguous_post_attempt_crash`
- `idle_permanently_unsupported`
- `due_without_active_drainer`
- `delivery_contract_mismatch`
- `mode_mismatch`
- `session_mismatch`
- `epoch_mismatch`
- `wait_not_ready`
- `hook_unverified`
- `due_without_timely_drain`
- `failed_nudge_accumulation`
- `circuit_breaker`

`session.nudge_queue_health` is emitted by controller reconciliation every `max(30s, 5*poll_interval)` for each non-empty queue and immediately on threshold crossings or state changes. It includes:

- `queue_depth`
- `due_count`
- `oldest_pending_age`
- `oldest_due_age`
- `recent_failed_count`
- `recent_expired_count`
- `session_running`
- `drainer_readiness=hook_ready|hook_unverified|poller_running|poller_missing|drainer_impossible`
- `last_poller_heartbeat`
- `last_hook_drain_result`
- `degraded_reason` when present

Threshold crossings are explicit in v1:

- queue depth transitions from `0 -> N`
- due count transitions from `0 -> N`
- `oldest_due_age > wait_idle_timeout` => `due_without_timely_drain`
- `recent_failed_count > 0` => warning-only queue health
- `recent_failed_count >= max_failed_retained` => `failed_nudge_accumulation`
- `drainer_readiness=poller_missing` with `due_count > 0` => `due_without_active_drainer`
- repeated poller exits with `idle_permanently_unsupported` => `circuit_breaker`
- hook readiness regresses from ready to unverified
- first `delivery_contract_mismatch` for a queued item
- first `epoch_mismatch` or `mode_mismatch` on a drain attempt

`session.nudge_hook_drain` includes `result=empty|rendered|partial|suppressed-error|mode-mismatch`, making hook-path failures visible even though `gc notify drain --inject` exits `0`.

Controller reconciliation applies a poller circuit breaker: after 3
consecutive poller exits for one agent with reason
`idle_permanently_unsupported`, it stops restarting the poller for that
runtime epoch, marks `drainer_readiness=drainer_impossible`, and emits
`session.nudge_poller_unavailable` with reason `circuit_breaker`.

Degraded states are latched in recent events and `gc:nudge` beads until
cleared by a successful drain or explicit operator action. `gc nudge
status` reconstructs that degraded state after restart rather than
reading a snapshot file.

Telemetry here means existing session events plus in-process counters/log fields, not OpenTelemetry.

Retention:

- terminal `gc:nudge` beads with success outcomes retain for 24 hours
- terminal `gc:nudge` beads with failure outcomes retain for 7 days
- queue files are removed immediately after the authoritative bead reaches a terminal state

Counters extend the existing nudge counter with attributes:

- `mode=immediate|wait-idle|queue`
- `outcome=injected|accepted_for_injection|queued|failed|rejected|degraded|expired`
- `source=session-nudge|mail-notify|sling|api|wait`
- `provider=<provider>`
- `drain_mode=hook|poller|none`
- `reason=<reason>`

Operators diagnose problems via:

- `gc nudge status <agent>`
- `gc nudge show <nudge-id>`
- queue-health events
- queue files under `.gc/nudges/`
- `gc:nudge` beads for authoritative terminal records

### Configuration

`internal/config.SessionConfig` gains:

```go
type AsyncNudgesConfig struct {
    WaitIdleTimeout string `toml:"wait_idle_timeout,omitempty" jsonschema:"default=15s"`
    NormalTTL       string `toml:"normal_ttl,omitempty" jsonschema:"default=30m"`
    UrgentTTL       string `toml:"urgent_ttl,omitempty" jsonschema:"default=2h"`
    PollInterval    string `toml:"poll_interval,omitempty" jsonschema:"default=10s"`
    PollIdleTimeout string `toml:"poll_idle_timeout,omitempty" jsonschema:"default=3s"`
    MaxQueueDepth   int    `toml:"max_queue_depth,omitempty" jsonschema:"default=50"`
    MaxDrainBatch   int    `toml:"max_drain_batch,omitempty" jsonschema:"default=10"`
    MaxReminderBytes int   `toml:"max_reminder_bytes,omitempty" jsonschema:"default=4096"`
    MaxFailedRetained int  `toml:"max_failed_retained,omitempty" jsonschema:"default=1000"`
    ClaimStaleAfter string `toml:"claim_stale_after,omitempty" jsonschema:"default=5m"`
}

type SessionConfig struct {
    // existing fields...
    AsyncNudges AsyncNudgesConfig `toml:"async_nudges,omitempty"`
}
```

Validation:

- all durations must parse and be positive
- `max_queue_depth >= 1`
- `max_drain_batch >= 1`
- `max_reminder_bytes >= 512`
- `max_failed_retained >= max_queue_depth`
- `claim_stale_after >= wait_idle_timeout + poll_idle_timeout + 30s`
- computed `ExpiresAt` must be after `DeliverAfter`
- queued nudge messages must be `<= 280` chars

### Rollout phases

| Phase | Scope |
|---|---|
| **Phase 1: Wait-safe deferred delivery** | bead-backed `gc:nudge` authority, agent-scoped queue with optional session fences, deterministic `NudgeID` admission, hook delivery via `gc notify drain --inject`, terminal-state consumption by waits/operators, controller-owned poller lifecycle hooks, `gc nudge status/show` diagnostics |
| **Phase 2: Broader provider and producer coverage** | provider-specific poller hardening, expanded producer migrations, recontract/replay tooling, broader default-provider eligibility |

Wait-driven sleep/wake depends specifically on **Phase 1** plus a target
session/provider whose resolved async-nudge contract is not
`AsyncNudgeDrainMode=none`.

### Backward compatibility

Existing config continues to work unchanged.

Behavioral changes:

- `gc session nudge` no longer requires a running session for the default path; it may queue instead
- scripts that relied on immediate interruption must pass `--delivery=immediate`
- hook-generated reminders now use `gc notify drain --inject`
- `gc nudge status` gains `--json` because queue-health is intended to be scriptable from day one
- `docs/architecture/messaging.md` must be updated with the new "persistent async nudge buffer" semantics once implementation lands

No migration is required for persisted data because the queue is new.

## Primitive Test

This proposal adds a **derived mechanism**, not a new primitive.

Derivation:

- persistence: ordinary files under `.gc/`
- atomic claim: `os.Rename`
- exclusion: advisory file locks, same pattern class as controller locking
- direct delivery: existing `runtime.Provider.Nudge()`
- startup/turn-boundary execution: existing hook installation system
- background work: same detached-child pattern already used by `gc daemon start`

| Condition | Pass/Fail | Reasoning |
|---|---|---|
| Atomicity | Pass | Queue claim/delivery uses filesystem rename plus advisory lock exclusion; no new transactional primitive is introduced. |
| Bitter Lesson | Pass | A stronger model still cannot mechanically detect safe terminal injection without a runtime signal. |
| ZFC | Pass | Go code makes only transport decisions from explicit mode/capability/timeout inputs. Message meaning stays in prompts/callers. |

### Why Queue Plus Beads

Pure queue files would make `.gc/nudges/` a second durable state system,
which fights the layering guidance in `CLAUDE.md`. Pure beads would make
claim/lease semantics awkward in the current bead store and give up the
simple atomic rename protocol that makes local deferred delivery robust.

So the design splits responsibilities cleanly:

- `gc:nudge` beads are the authoritative persistence substrate
- queue files are transient transport replicas used only for admission, claim, and delivery
- the event bus remains the observation substrate for operators and tooling

This preserves Gas City's preferred substrates while keeping the one
filesystem trick that actually buys us atomic local claim.

## Drawbacks

- This adds real surface area: new config, queue files, lock handling, hidden commands, poller lifecycle, and hook updates.
- There are now two deferred drain strategies (`hook` and `poller`), which increases testing cost.
- A persistent queue can still produce stale reminders if TTLs are too generous or operators forget that asleep sessions can accumulate notifications.
- Internal `gc:nudge` beads add write volume and retention work even though the reminders are low-value.
- `WaitForIdle` as a provider interface change touches every runtime backend.
- Failure evidence is safer than silent drop, but it also means failed `gc:nudge` beads can accumulate until retention prunes them.
