package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// acquireControllerLock takes an exclusive flock on .gc/controller.lock.
// Returns the locked file (caller must defer Close) or an error if another
// controller is already running.
func acquireControllerLock(cityPath string) (*os.File, error) {
	path := filepath.Join(cityPath, ".gc", "controller.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening controller lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck // closing after flock failure
		return nil, fmt.Errorf("controller already running")
	}
	return f, nil
}

// startControllerSocket listens on a Unix socket at .gc/controller.sock.
// When a client sends "stop\n", cancelFn is called to shut down the
// controller loop. Returns the listener for cleanup.
func startControllerSocket(cityPath string, cancelFn context.CancelFunc) (net.Listener, error) {
	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	// Remove stale socket from a previous crash.
	os.Remove(sockPath) //nolint:errcheck // stale socket cleanup
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on controller socket: %w", err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return // listener closed
			}
			go handleControllerConn(conn, cancelFn)
		}
	}()
	return lis, nil
}

// handleControllerConn reads from a connection and dispatches commands.
// Supported commands: "stop" (shutdown), "ping" (liveness check, returns PID).
func handleControllerConn(conn net.Conn, cancelFn context.CancelFunc) {
	defer conn.Close()                                     //nolint:errcheck // best-effort cleanup
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck // best-effort deadline
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		switch scanner.Text() {
		case "stop":
			cancelFn()
			conn.Write([]byte("ok\n")) //nolint:errcheck // best-effort ack
		case "ping":
			fmt.Fprintf(conn, "%d\n", os.Getpid()) //nolint:errcheck // best-effort
		}
	}
}

// controllerAlive checks whether a controller is running by connecting
// to the controller.sock and sending a "ping". Returns the PID if alive,
// or 0 if not reachable.
func controllerAlive(cityPath string) int {
	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return 0
	}
	defer conn.Close()                                    //nolint:errcheck // best-effort
	conn.Write([]byte("ping\n"))                          //nolint:errcheck // best-effort
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck // best-effort
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}

// debounceDelay is the coalesce window for filesystem events. Multiple
// events within this window (vim atomic saves, git checkouts) produce a
// single dirty signal. Tests may override this for faster response.
var debounceDelay = 200 * time.Millisecond

// watchConfigDirs starts an fsnotify watcher on the given directories and
// sets dirty to true after a debounce window. Watches directories instead
// of individual files to handle vim/emacs rename-swap atomic saves.
// Returns a cleanup function. If the watcher cannot be created, returns a
// no-op cleanup (degraded to tick-only, no file watching).
func watchConfigDirs(dirs []string, dirty *atomic.Bool, stderr io.Writer) func() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(stderr, "gc start: config watcher: %v (reload on tick only)\n", err) //nolint:errcheck // best-effort stderr
		return func() {}
	}
	for _, dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			fmt.Fprintf(stderr, "gc start: config watcher: cannot watch %s: %v\n", dir, err) //nolint:errcheck // best-effort stderr
		}
	}
	go func() {
		var debounce *time.Timer
		for {
			select {
			case _, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Debounce: reset timer on each event, fire after quiet period.
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(debounceDelay, func() {
					dirty.Store(true)
				})
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return func() { watcher.Close() } //nolint:errcheck // best-effort cleanup
}

// reloadResult holds the result of a config reload attempt.
type reloadResult struct {
	Cfg      *config.City
	Prov     *config.Provenance
	Revision string
}

// tryReloadConfig attempts to reload city.toml with includes and patches.
// Returns the new config, provenance, and revision on success, or an error
// on failure (parse error, validation error, cityName changed). Callers
// should keep the old config on error. Warnings are written to stderr;
// strict mode (default) makes them fatal — use --no-strict to disable.
func tryReloadConfig(tomlPath, lockedCityName, cityRoot string, stderr io.Writer) (*reloadResult, error) {
	// Auto-fetch remote packs before full config load (mirrors cmd_start).
	if quickCfg, qErr := config.Load(fsys.OSFS{}, tomlPath); qErr == nil && len(quickCfg.Packs) > 0 {
		if fErr := config.FetchPacks(quickCfg.Packs, cityRoot); fErr != nil {
			return nil, fmt.Errorf("fetching packs: %w", fErr)
		}
	}

	newCfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, tomlPath, extraConfigFiles...)
	if err != nil {
		return nil, fmt.Errorf("parsing city.toml: %w", err)
	}
	if strictMode && len(prov.Warnings) > 0 {
		for _, w := range prov.Warnings {
			fmt.Fprintf(stderr, "gc start: strict: %s\n", w) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr, "gc start: use --no-strict to disable strict checking") //nolint:errcheck // best-effort stderr
		return nil, fmt.Errorf("strict mode: %d collision warning(s)", len(prov.Warnings))
	}
	for _, w := range prov.Warnings {
		fmt.Fprintf(stderr, "gc start: warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}
	if err := config.ValidateAgents(newCfg.Agents); err != nil {
		return nil, fmt.Errorf("validating agents: %w", err)
	}
	newName := newCfg.Workspace.Name
	if newName == "" {
		newName = filepath.Base(filepath.Dir(tomlPath))
	}
	if newName != lockedCityName {
		return nil, fmt.Errorf("workspace.name changed from %q to %q (restart controller to apply)", lockedCityName, newName)
	}
	rev := config.Revision(fsys.OSFS{}, prov, newCfg, cityRoot)
	return &reloadResult{Cfg: newCfg, Prov: prov, Revision: rev}, nil
}

// gracefulStopAll performs two-pass graceful shutdown:
//  1. Send Interrupt (Ctrl-C) to all sessions
//  2. Wait shutdown_timeout
//  3. Stop (force-kill) any survivors
func gracefulStopAll(
	names []string,
	sp session.Provider,
	timeout time.Duration,
	rec events.Recorder,
	stdout, stderr io.Writer,
) {
	if timeout <= 0 || len(names) == 0 {
		// Immediate kill (no grace period).
		for _, name := range names {
			if err := sp.Stop(name); err != nil {
				fmt.Fprintf(stderr, "gc stop: stopping %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stdout, "Stopped agent '%s'\n", name) //nolint:errcheck // best-effort stdout
				rec.Record(events.Event{
					Type: events.AgentStopped, Actor: "gc", Subject: name,
				})
			}
		}
		return
	}

	// Pass 1: interrupt all.
	for _, name := range names {
		_ = sp.Interrupt(name) // best-effort
	}
	fmt.Fprintf(stdout, "Sent interrupt to %d agent(s), waiting %s...\n", //nolint:errcheck // best-effort stdout
		len(names), timeout)

	// Poll until all agents exit or timeout expires (avoid sleeping full duration).
	pollInterval := 500 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allExited := true
		for _, name := range names {
			if sp.IsRunning(name) {
				allExited = false
				break
			}
		}
		if allExited {
			break
		}
		remaining := time.Until(deadline)
		if remaining < pollInterval {
			time.Sleep(remaining)
		} else {
			time.Sleep(pollInterval)
		}
	}

	// Pass 2: kill survivors.
	for _, name := range names {
		if !sp.IsRunning(name) {
			fmt.Fprintf(stdout, "Agent '%s' exited gracefully\n", name) //nolint:errcheck // best-effort stdout
			rec.Record(events.Event{
				Type: events.AgentStopped, Actor: "gc", Subject: name,
			})
			continue
		}
		if err := sp.Stop(name); err != nil {
			fmt.Fprintf(stderr, "gc stop: stopping %s: %v\n", name, err) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stdout, "Stopped agent '%s'\n", name) //nolint:errcheck // best-effort stdout
			rec.Record(events.Event{
				Type: events.AgentStopped, Actor: "gc", Subject: name,
			})
		}
	}
}

// controllerLoop runs reconciliation periodically until ctx is canceled.
// buildFn is called on each tick to re-evaluate the desired agent set
// (pool check commands are re-run). If tomlPath is non-empty, the loop
// watches config directories for changes and reloads config on the next tick.
// watchDirs is the initial set of directories to watch; it is updated on
// config reload.
func controllerLoop(
	ctx context.Context,
	interval time.Duration,
	cfg *config.City,
	cityName string,
	tomlPath string,
	watchDirs []string,
	buildFn func(*config.City, session.Provider) []agent.Agent,
	sp session.Provider,
	rops reconcileOps,
	dops drainOps,
	ct crashTracker,
	it idleTracker,
	wg wispGC,
	ad automationDispatcher,
	rec events.Recorder,
	poolSessions map[string]time.Duration,
	poolDeathHandlers map[string]poolDeathInfo,
	suspendedNames map[string]bool,
	cs *controllerState, // nil when API disabled
	stdout, stderr io.Writer,
) {
	dirty := &atomic.Bool{}
	if tomlPath != "" {
		// Fall back to watching the directory containing city.toml if no
		// explicit watch dirs were provided.
		dirs := watchDirs
		if len(dirs) == 0 {
			dirs = []string{filepath.Dir(tomlPath)}
		}
		cleanup := watchConfigDirs(dirs, dirty, stderr)
		defer cleanup()
	}

	// Track effective provider name for hot-reload detection.
	// GC_SESSION env var overrides config — if set, config changes won't trigger a swap.
	lastProviderName := cfg.Session.Provider
	if v := os.Getenv("GC_SESSION"); v != "" {
		lastProviderName = v
	}

	// Compute observation search paths for agent output reading.
	observePaths := observeSearchPaths(cfg.Daemon.ObservePaths)

	// Initial reconciliation.
	agents := buildFn(cfg, sp)
	doReconcileAgents(agents, sp, rops, dops, ct, it, rec, poolSessions, suspendedNames, cfg.Daemon.DriftDrainTimeoutDuration(), cfg.Session.StartupTimeoutDuration(), stdout, stderr, ctx)
	ensureObservers(agents, observePaths)
	fmt.Fprintln(stdout, "City started.") //nolint:errcheck // best-effort stdout

	cityRoot := filepath.Dir(tomlPath)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Track pool instance liveness for death detection.
	// nil on first tick — on_boot already handled startup orphans.
	var prevPoolRunning map[string]bool

	for {
		select {
		case <-ticker.C:
			// Detect pool instance deaths since last tick.
			if len(poolDeathHandlers) > 0 {
				currentRunning, _ := rops.listRunning("")
				currentSet := make(map[string]bool, len(currentRunning))
				for _, name := range currentRunning {
					currentSet[name] = true
				}
				if prevPoolRunning != nil {
					for sn, info := range poolDeathHandlers {
						if prevPoolRunning[sn] && !currentSet[sn] {
							if _, err := shellScaleCheck(info.Command, info.Dir); err != nil {
								fmt.Fprintf(stderr, "on_death %s: %v\n", sn, err) //nolint:errcheck // best-effort stderr
							}
						}
					}
				}
				prevPoolRunning = make(map[string]bool)
				for sn := range poolDeathHandlers {
					if currentSet[sn] {
						prevPoolRunning[sn] = true
					}
				}
			}
			if dirty.Swap(false) {
				result, err := tryReloadConfig(tomlPath, cityName, cityRoot, stderr)
				if err != nil {
					fmt.Fprintf(stderr, "gc start: config reload: %v (keeping old config)\n", err) //nolint:errcheck // best-effort stderr
					telemetry.RecordConfigReload(ctx, "", err)
				} else {
					oldAgentCount := len(cfg.Agents)
					oldRigCount := len(cfg.Rigs)
					cfg = result.Cfg
					// Detect session provider change.
					newProviderName := cfg.Session.Provider
					if v := os.Getenv("GC_SESSION"); v != "" {
						newProviderName = v // env always wins — no swap when env is set
					}
					if newProviderName != lastProviderName {
						// Stop all agents on the current provider.
						if running, lErr := rops.listRunning(""); lErr == nil && len(running) > 0 {
							fmt.Fprintf(stdout, "Provider changed (%s → %s), stopping %d agent(s)...\n", //nolint:errcheck // best-effort stdout
								displayProviderName(lastProviderName), displayProviderName(newProviderName), len(running))
							gracefulStopAll(running, sp, cfg.Daemon.ShutdownTimeoutDuration(), rec, stdout, stderr)
						}
						// Construct new provider.
						newSp, spErr := newSessionProviderByName(newProviderName, cfg.Session, cityName)
						if spErr != nil {
							fmt.Fprintf(stderr, "gc start: new session provider %q: %v (keeping old provider)\n", //nolint:errcheck // best-effort stderr
								newProviderName, spErr)
						} else {
							sp = newSp
							rops = newReconcileOps(sp)
							dops = newDrainOps(sp)
							rec.Record(events.Event{
								Type:    events.ProviderSwapped,
								Actor:   "gc",
								Message: fmt.Sprintf("%s → %s", displayProviderName(lastProviderName), displayProviderName(newProviderName)),
							})
							fmt.Fprintf(stdout, "Session provider swapped to %s.\n", displayProviderName(newProviderName)) //nolint:errcheck // best-effort stdout
							lastProviderName = newProviderName
						}
					}
					// Re-materialize and prepend system formulas (not included in LoadWithIncludes).
					sysDir, _ := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityRoot)
					if sysDir != "" {
						cfg.FormulaLayers.City = append([]string{sysDir}, cfg.FormulaLayers.City...)
						for rigName, layers := range cfg.FormulaLayers.Rigs {
							cfg.FormulaLayers.Rigs[rigName] = append([]string{sysDir}, layers...)
						}
					}
					// Validate rigs (prefix collisions, missing fields).
					if err := config.ValidateRigs(cfg.Rigs, cityName); err != nil {
						fmt.Fprintf(stderr, "gc start: config reload: %v\n", err) //nolint:errcheck // best-effort stderr
					}
					// Resolve rig paths and init beads for any new/changed rigs.
					resolveRigPaths(cityRoot, cfg.Rigs)
					if err := startBeadsLifecycle(cityRoot, cityName, cfg, stderr); err != nil {
						fmt.Fprintf(stderr, "gc start: config reload: %v\n", err) //nolint:errcheck // best-effort stderr
					}
					// Resolve formula symlinks for newly activated packs.
					if len(cfg.FormulaLayers.City) > 0 {
						if err := ResolveFormulas(cityRoot, cfg.FormulaLayers.City); err != nil {
							fmt.Fprintf(stderr, "gc start: config reload: city formulas: %v\n", err) //nolint:errcheck // best-effort stderr
						}
					}
					for _, r := range cfg.Rigs {
						if layers, ok := cfg.FormulaLayers.Rigs[r.Name]; ok && len(layers) > 0 {
							if err := ResolveFormulas(r.Path, layers); err != nil {
								fmt.Fprintf(stderr, "gc start: config reload: rig %q formulas: %v\n", r.Name, err) //nolint:errcheck // best-effort stderr
							}
						}
					}
					poolSessions = computePoolSessions(cfg, cityName, sp)
					poolDeathHandlers = computePoolDeathHandlers(cfg, cityName, cityRoot, sp)
					suspendedNames = computeSuspendedNames(cfg, cityName, cityRoot)
					// Rebuild crash tracker only if config values changed.
					newMaxR := cfg.Daemon.MaxRestartsOrDefault()
					newWindow := cfg.Daemon.RestartWindowDuration()
					switch {
					case newMaxR <= 0:
						ct = nil
					case ct == nil:
						ct = newCrashTracker(newMaxR, newWindow)
					default:
						// Compare against existing tracker limits; rebuild if changed.
						oldMaxR, oldWindow := ct.limits()
						if newMaxR != oldMaxR || newWindow != oldWindow {
							ct = newCrashTracker(newMaxR, newWindow)
						}
					}
					// Update API state with new crash tracker.
					if cs != nil {
						cs.mu.Lock()
						cs.ct = ct
						cs.mu.Unlock()
					}
					// Rebuild idle tracker with new config timeouts.
					it = buildIdleTracker(cfg, cityName, sp)
					// Rebuild wisp GC from new config.
					if cfg.Daemon.WispGCEnabled() {
						wg = newWispGC(cfg.Daemon.WispGCIntervalDuration(),
							cfg.Daemon.WispTTLDuration(), beads.ExecCommandRunner())
					} else {
						wg = nil
					}
					// Rebuild automation dispatcher from new config.
					ad = buildAutomationDispatcher(cityRoot, cfg, beads.ExecCommandRunner(), rec, stderr)
					observePaths = observeSearchPaths(cfg.Daemon.ObservePaths)
					// Update API server state if running.
					if cs != nil {
						cs.update(cfg, sp)
					}
					fmt.Fprintf(stdout, "Config reloaded: %s (rev %s)\n", //nolint:errcheck // best-effort stdout
						configReloadSummary(oldAgentCount, oldRigCount, len(cfg.Agents), len(cfg.Rigs)),
						shortRev(result.Revision))
					telemetry.RecordConfigReload(ctx, result.Revision, nil)
				}
			}
			agents = buildFn(cfg, sp)
			doReconcileAgents(agents, sp, rops, dops, ct, it, rec, poolSessions, suspendedNames, cfg.Daemon.DriftDrainTimeoutDuration(), cfg.Session.StartupTimeoutDuration(), stdout, stderr, ctx)
			ensureObservers(agents, observePaths)
			// Wisp GC: purge expired closed molecules.
			if wg != nil && wg.shouldRun(time.Now()) {
				purged, gcErr := wg.runGC(filepath.Dir(tomlPath), time.Now())
				if gcErr != nil {
					fmt.Fprintf(stderr, "gc start: wisp gc: %v\n", gcErr) //nolint:errcheck // best-effort stderr
				} else if purged > 0 {
					fmt.Fprintf(stdout, "Bead GC: purged %d expired bead(s)\n", purged) //nolint:errcheck // best-effort stdout
				}
			}
			// Automation dispatch: evaluate gates and fire due automations.
			if ad != nil {
				ad.dispatch(ctx, filepath.Dir(tomlPath), time.Now())
			}
			// Chat session auto-suspend: suspend detached idle sessions.
			if idleTimeout := cfg.ChatSessions.IdleTimeoutDuration(); idleTimeout > 0 {
				autoSuspendChatSessions(filepath.Dir(tomlPath), sp, idleTimeout, stdout, stderr)
			}
		case <-ctx.Done():
			return
		}
	}
}

// shortRev returns the first 12 characters of a revision hash.
func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// configReloadSummary returns a human-readable summary of what changed
// between config reloads.
func configReloadSummary(oldAgents, oldRigs, newAgents, newRigs int) string {
	var parts []string
	switch {
	case newAgents > oldAgents:
		parts = append(parts, fmt.Sprintf("%d agents (+%d)", newAgents, newAgents-oldAgents))
	case newAgents < oldAgents:
		parts = append(parts, fmt.Sprintf("%d agents (-%d)", newAgents, oldAgents-newAgents))
	default:
		parts = append(parts, fmt.Sprintf("%d agents", newAgents))
	}
	switch {
	case newRigs > oldRigs:
		parts = append(parts, fmt.Sprintf("%d rigs (+%d)", newRigs, newRigs-oldRigs))
	case newRigs < oldRigs:
		parts = append(parts, fmt.Sprintf("%d rigs (-%d)", newRigs, oldRigs-newRigs))
	default:
		parts = append(parts, fmt.Sprintf("%d rigs", newRigs))
	}
	return strings.Join(parts, ", ")
}

// runController runs the persistent controller loop. It acquires a lock,
// opens a control socket, runs the reconciliation loop, and on shutdown
// stops all agents. Returns an exit code. initialWatchDirs is the set of
// directories to watch for config changes (from initial provenance).
func runController(
	cityPath string,
	tomlPath string,
	cfg *config.City,
	buildFn func(*config.City, session.Provider) []agent.Agent,
	sp session.Provider,
	dops drainOps,
	poolSessions map[string]time.Duration,
	poolDeathHandlers map[string]poolDeathInfo,
	initialWatchDirs []string,
	rec events.Recorder,
	eventProv events.Provider,
	stdout, stderr io.Writer,
) int {
	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer lock.Close() //nolint:errcheck // best-effort cleanup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Signal handler: SIGINT/SIGTERM → cancel.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := startControllerSocket(cityPath, cancel)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defer lis.Close()         //nolint:errcheck // best-effort cleanup
	defer os.Remove(sockPath) //nolint:errcheck // best-effort cleanup

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	rec.Record(events.Event{Type: events.ControllerStarted, Actor: "gc"})
	telemetry.RecordControllerLifecycle(context.Background(), "started")
	fmt.Fprintln(stdout, "Controller started.") //nolint:errcheck // best-effort stdout

	rops := newReconcileOps(sp)

	// Build crash tracker from config — must happen before API server
	// starts so IsQuarantined reads are race-free.
	var ct crashTracker
	maxR := cfg.Daemon.MaxRestartsOrDefault()
	if maxR > 0 {
		ct = newCrashTracker(maxR, cfg.Daemon.RestartWindowDuration())
	}

	// Start API server if configured.
	var cs *controllerState
	if cfg.API.Port > 0 {
		cs = newControllerState(cfg, sp, eventProv, cityName, cityPath)
		cs.ct = ct
		bind := cfg.API.BindOrDefault()
		nonLocal := bind != "127.0.0.1" && bind != "localhost" && bind != "::1"
		var apiSrv *api.Server
		if nonLocal {
			apiSrv = api.NewReadOnly(cs)
			fmt.Fprintf(stderr, "api: binding to %s — mutation endpoints disabled (non-localhost)\n", bind) //nolint:errcheck
		} else {
			apiSrv = api.New(cs)
		}
		addr := net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
		apiLis, apiErr := net.Listen("tcp", addr)
		if apiErr != nil {
			fmt.Fprintf(stderr, "api: WARNING: listen %s failed: %v — continuing without API server\n", addr, apiErr) //nolint:errcheck // best-effort stderr
		} else {
			go func() {
				if err := apiSrv.Serve(apiLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
					fmt.Fprintf(stderr, "api: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			}()
			defer func() {
				shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				apiSrv.Shutdown(shutCtx) //nolint:errcheck // best-effort cleanup
			}()
			fmt.Fprintf(stdout, "API server listening on http://%s\n", addr) //nolint:errcheck // best-effort stdout
		}
	}

	// Build idle tracker from config.
	it := buildIdleTracker(cfg, cityName, sp)

	// Build wisp GC from config.
	var wg wispGC
	if cfg.Daemon.WispGCEnabled() {
		wg = newWispGC(cfg.Daemon.WispGCIntervalDuration(),
			cfg.Daemon.WispTTLDuration(), beads.ExecCommandRunner())
	}

	// Build automation dispatcher from config.
	ad := buildAutomationDispatcher(cityPath, cfg, beads.ExecCommandRunner(), rec, stderr)

	suspendedNames := computeSuspendedNames(cfg, cityName, cityPath)
	runPoolOnBoot(cfg, cityPath, shellScaleCheck, stderr)
	controllerLoop(ctx, cfg.Daemon.PatrolIntervalDuration(),
		cfg, cityName, tomlPath, initialWatchDirs,
		buildFn, sp, rops, dops, ct, it, wg, ad, rec, poolSessions, poolDeathHandlers, suspendedNames, cs, stdout, stderr)

	// Shutdown: graceful stop all sessions on this city's socket.
	timeout := cfg.Daemon.ShutdownTimeoutDuration()
	if rops != nil {
		running, _ := rops.listRunning("")
		gracefulStopAll(running, sp, timeout, rec, stdout, stderr)
	} else {
		var names []string
		for _, a := range buildFn(cfg, sp) {
			if a.IsRunning() {
				names = append(names, a.SessionName())
			}
		}
		gracefulStopAll(names, sp, timeout, rec, stdout, stderr)
	}

	rec.Record(events.Event{Type: events.ControllerStopped, Actor: "gc"})
	telemetry.RecordControllerLifecycle(context.Background(), "stopped")
	fmt.Fprintln(stdout, "Controller stopped.") //nolint:errcheck // best-effort stdout
	return 0
}
