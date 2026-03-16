package runtime

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"sort"
	"strings"
)

// ConfigFingerprint returns a deterministic hash of the Config fields that
// define an agent's behavioral identity. Changes to these fields indicate
// the agent should be restarted (via drain when drain ops are available).
//
// Included: Command, Env, FingerprintExtra (pool config, etc.),
// Nudge, PreStart, SessionSetup, SessionSetupScript, OverlayDir, CopyFiles,
// SessionLive.
//
// Excluded (observation-only hints): WorkDir, ReadyPromptPrefix,
// ReadyDelayMs, ProcessNames, EmitsPermissionWarning.
//
// The hash is a hex-encoded SHA-256. Same config always produces the same
// hash regardless of map iteration order.
func ConfigFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFields(h, cfg)
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// CoreFingerprint returns a hash of only the "core" config fields —
// everything except SessionLive. A change to core fields triggers a
// drain + restart. A change to only SessionLive triggers re-apply
// without restart.
func CoreFingerprint(cfg Config) string {
	h := sha256.New()
	hashCoreFields(h, cfg)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// LiveFingerprint returns a hash of only the SessionLive fields.
// Used by the reconciler to detect live-only drift.
func LiveFingerprint(cfg Config) string {
	h := sha256.New()
	hashLiveFields(h, cfg)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// envFingerprintIncludePrefix lists the prefixes of env keys that SHOULD
// contribute to the config fingerprint. Only env vars matching one of
// these prefixes are hashed. Everything else (PATH, CLAUDECODE,
// CLAUDE_CODE_ENTRYPOINT, OTel vars, etc.) is excluded because those
// are transport/runtime details that vary between the process that
// created the session (gc init inside Claude Code) and the supervisor's
// reconciler process, causing spurious config-drift restarts.
var envFingerprintIncludePrefix = []string{"GC_"}

// envFingerprintInclude returns true if the key should contribute to the
// config fingerprint. Only GC_* prefixed vars are included — all others
// are ambient environment that varies between processes.
func envFingerprintInclude(key string) bool {
	for _, prefix := range envFingerprintIncludePrefix {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// hashCoreFields writes all config fields except SessionLive to the hash.
func hashCoreFields(h hash.Hash, cfg Config) {
	h.Write([]byte(cfg.Command)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})           //nolint:errcheck // hash.Write never errors

	hashSortedMapIncluded(h, cfg.Env, envFingerprintInclude)

	// FingerprintExtra carries additional identity fields (pool config, etc.)
	// that aren't part of the session command but should
	// trigger a restart on change. Prefixed with "fp:" to avoid collisions
	// with Env keys.
	if len(cfg.FingerprintExtra) > 0 {
		h.Write([]byte("fp")) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
		hashSortedMap(h, cfg.FingerprintExtra)
	}

	// Nudge
	h.Write([]byte(cfg.Nudge)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})         //nolint:errcheck // hash.Write never errors

	// PreStart
	for _, ps := range cfg.PreStart {
		h.Write([]byte(ps)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel between slices

	// SessionSetup
	for _, ss := range cfg.SessionSetup {
		h.Write([]byte(ss)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel between slices

	h.Write([]byte(cfg.SessionSetupScript)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})                      //nolint:errcheck // hash.Write never errors

	h.Write([]byte(cfg.OverlayDir)) //nolint:errcheck // hash.Write never errors
	h.Write([]byte{0})              //nolint:errcheck // hash.Write never errors

	// CopyFiles
	for _, cf := range cfg.CopyFiles {
		h.Write([]byte(cf.Src))    //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})         //nolint:errcheck // separator between Src and RelDst
		h.Write([]byte(cf.RelDst)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})         //nolint:errcheck // separator between entries
	}
}

// hashLiveFields writes SessionLive fields to the hash.
func hashLiveFields(h hash.Hash, cfg Config) {
	for _, sl := range cfg.SessionLive {
		h.Write([]byte(sl)) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})  //nolint:errcheck // hash.Write never errors
	}
	h.Write([]byte{1}) //nolint:errcheck // sentinel
}

// hashSortedMapIncluded writes map entries to h in deterministic sorted-key
// order, only including keys for which the include function returns true.
func hashSortedMapIncluded(h hash.Hash, m map[string]string, include func(string) bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if include(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))    //nolint:errcheck // hash.Write never errors
		h.Write([]byte{'='})  //nolint:errcheck // hash.Write never errors
		h.Write([]byte(m[k])) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}
}

// hashSortedMap writes map entries to h in deterministic sorted-key order.
func hashSortedMap(h hash.Hash, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))    //nolint:errcheck // hash.Write never errors
		h.Write([]byte{'='})  //nolint:errcheck // hash.Write never errors
		h.Write([]byte(m[k])) //nolint:errcheck // hash.Write never errors
		h.Write([]byte{0})    //nolint:errcheck // hash.Write never errors
	}
}
