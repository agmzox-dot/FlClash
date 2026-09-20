//go:build android

package main

import (
	"context"
	"core/adaptive"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/component/process"
	"github.com/metacubex/mihomo/config"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

const (
	adaptiveFixedProxyName  = "ByteVirt-LA-24443"
	adaptiveFixedServer     = "38.49.36.36"
	adaptiveFixedPort       = 24443
	adaptivePort            = 24445
	adaptiveProxyName       = "__FlClash-Adaptive-Mobile-24445"
	adaptiveSelfPackageRoot = "com.follow.clash.adaptive"
	adaptiveQuotaStateName  = "adaptive-mobile-quota-v1.json"
)

var (
	adaptiveEngine = adaptive.New()
	adaptiveReady  atomic.Bool

	adaptiveProxyMu sync.RWMutex
	adaptiveProxy   C.Proxy

	adaptiveProfileMu      sync.RWMutex
	adaptiveNetworkProfile string
	adaptiveConfigID       string

	adaptiveQuotaLoadOnce sync.Once
	adaptiveQuotaWriteMu  sync.Mutex
	adaptiveLastPersistAt time.Time
	adaptiveLastPersisted int64
)

type adaptiveQuotaState struct {
	Version int                 `json:"version"`
	Quota   adaptive.QuotaState `json:"quota"`
}

func adaptiveSetNetworkProfile(profile string) {
	adaptiveProfileMu.Lock()
	adaptiveNetworkProfile = strings.TrimSpace(profile)
	adaptiveProfileMu.Unlock()
}

func adaptiveSetConfigID(buf []byte) {
	sum := sha256.Sum256(buf)
	adaptiveProfileMu.Lock()
	adaptiveConfigID = hex.EncodeToString(sum[:8])
	adaptiveProfileMu.Unlock()
}

func adaptiveCurrentProfile() string {
	adaptiveProfileMu.RLock()
	defer adaptiveProfileMu.RUnlock()
	if adaptiveNetworkProfile == "" || adaptiveConfigID == "" {
		return ""
	}
	return adaptiveNetworkProfile + "/" + adaptiveConfigID
}

func adaptiveAnyInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		if uint64(int(v)) == v {
			return int(v), true
		}
	case float64:
		if float64(int(v)) == v {
			return int(v), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

func adaptiveCloneMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for k, v := range source {
		clone[k] = v
	}
	return clone
}

func adaptiveBuildProxy(raw *config.RawConfig) (C.Proxy, bool) {
	for _, mapping := range raw.Proxy {
		typ, _ := mapping["type"].(string)
		server, _ := mapping["server"].(string)
		port, portOK := adaptiveAnyInt(mapping["port"])
		name, _ := mapping["name"].(string)
		if name != adaptiveFixedProxyName || !strings.EqualFold(typ, "ss") || server != adaptiveFixedServer || !portOK || port != adaptiveFixedPort {
			continue
		}
		clone := adaptiveCloneMap(mapping)
		clone["name"] = adaptiveProxyName
		clone["port"] = adaptivePort
		clone["udp"] = false
		proxy, err := adapter.ParseProxy(clone, adapter.WithTunnelForAPI(tunnel.Tunnel))
		if err != nil {
			log.Warnln("[ADAPTIVE] hidden mobile proxy parse failed: %v", err)
			return nil, false
		}
		return proxy, true
	}
	return nil, false
}

func adaptiveSetProxy(proxy C.Proxy) {
	adaptiveProxyMu.Lock()
	adaptiveProxy = proxy
	adaptiveProxyMu.Unlock()
}

func adaptiveGetProxy() C.Proxy {
	adaptiveProxyMu.RLock()
	defer adaptiveProxyMu.RUnlock()
	return adaptiveProxy
}

func adaptiveQuotaStatePath() string {
	return C.Path.Resolve(adaptiveQuotaStateName)
}

func adaptiveQuarantineQuotaState(path string, data []byte) {
	corrupt := fmt.Sprintf("%s.corrupt.%d", path, time.Now().Unix())
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		log.Warnln("[ADAPTIVE] cannot quarantine corrupt quota state: %v", err)
	}
}

func adaptiveLoadQuotaState() {
	adaptiveQuotaLoadOnce.Do(func() {
		path := adaptiveQuotaStatePath()
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			// Keep the unreadable file in place. A later process restart will see
			// the same failure instead of silently resetting quota accounting.
			log.Warnln("[ADAPTIVE] quota state unreadable; disabling Adaptive for today: %v", err)
			adaptiveEngine.ForceDailyTotalAtLeast(adaptive.TotalDailyLimit)
			return
		}
		var state adaptiveQuotaState
		if err := json.Unmarshal(data, &state); err != nil || state.Version != 1 || !adaptiveEngine.RestoreQuotaState(state.Quota) {
			log.Warnln("[ADAPTIVE] quota state corrupt; disabling Adaptive for today")
			// Copy the damaged payload aside first, but leave the original path in
			// place until a fresh fail-closed state is durably committed. If that
			// write fails, the next process still sees corruption and fails closed.
			adaptiveQuarantineQuotaState(path, data)
			adaptiveEngine.ForceDailyTotalAtLeast(adaptive.TotalDailyLimit)
			_ = adaptivePersistQuotaState(true)
			return
		}
		current := adaptiveEngine.ExportQuotaState()
		adaptiveLastPersisted = current.TotalUsed
		adaptiveLastPersistAt = time.Now()
		if current.Day != state.Quota.Day {
			// A valid prior-day file rolled over cleanly. Persist the fresh day
			// immediately so a restart cannot resurrect yesterday's counters.
			_ = adaptivePersistQuotaState(true)
		}
	})
}

func adaptivePersistQuotaState(force bool) bool {
	adaptiveQuotaWriteMu.Lock()
	defer adaptiveQuotaWriteMu.Unlock()

	quota := adaptiveEngine.ExportQuotaState()
	now := time.Now()
	if !force && quota.TotalUsed-adaptiveLastPersisted < 1<<20 && now.Sub(adaptiveLastPersistAt) < 10*time.Second {
		return true
	}
	path := adaptiveQuotaStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Warnln("[ADAPTIVE] quota state directory failure: %v", err)
		adaptiveEngine.ForceDailyTotalAtLeast(adaptive.TotalDailyLimit)
		return false
	}
	state := adaptiveQuotaState{Version: 1, Quota: quota}
	data, err := json.Marshal(state)
	if err != nil {
		adaptiveEngine.ForceDailyTotalAtLeast(adaptive.TotalDailyLimit)
		return false
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".adaptive-quota-*.tmp")
	if err != nil {
		log.Warnln("[ADAPTIVE] quota state temp-file failure: %v", err)
		adaptiveEngine.ForceDailyTotalAtLeast(adaptive.TotalDailyLimit)
		return false
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, path)
	}
	if err == nil {
		// Best-effort directory sync closes the rename durability window on
		// filesystems that support fsync on directories.
		if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
			if syncErr := dir.Sync(); syncErr != nil {
				log.Debugln("[ADAPTIVE] quota directory fsync unavailable: %v", syncErr)
			}
			_ = dir.Close()
		}
	}
	if err != nil {
		log.Warnln("[ADAPTIVE] quota persistence failed; disabling Adaptive for today: %v", err)
		adaptiveEngine.ForceDailyTotalAtLeast(adaptive.TotalDailyLimit)
		return false
	}
	ok = true
	adaptiveLastPersistAt = now
	adaptiveLastPersisted = quota.TotalUsed
	return true
}

func adaptiveLoadConfig(buf []byte, _ func([]byte) (*config.Config, error)) (*config.Config, error) {
	adaptiveReady.Store(false)
	adaptiveProfileMu.Lock()
	adaptiveConfigID = ""
	adaptiveProfileMu.Unlock()

	adaptiveSetProxy(nil)
	raw, err := config.UnmarshalRawConfig(buf)
	if err != nil {
		return nil, err
	}
	hiddenProxy, ready := adaptiveBuildProxy(raw)
	cfg, err := config.ParseRawConfig(raw)
	if err != nil {
		return nil, err
	}
	adaptiveSetConfigID(buf)
	adaptiveLoadQuotaState()
	adaptiveSetProxy(hiddenProxy)
	adaptiveReady.Store(ready)
	if ready {
		log.Infoln("[ADAPTIVE] mobile backend armed on %s:%d", adaptiveFixedServer, adaptivePort)
	} else {
		log.Warnln("[ADAPTIVE] disabled: inline SS source %s:%d was not found", adaptiveFixedServer, adaptiveFixedPort)
	}
	return cfg, nil
}

func adaptiveRouteEndsDirect(proxy C.Proxy, metadata *C.Metadata) bool {
	for depth := 0; proxy != nil && depth < 16; depth++ {
		if proxy.Type() == C.Direct || proxy.Name() == "DIRECT" {
			return true
		}
		next := proxy.Unwrap(metadata, false)
		if next == nil || next == proxy {
			return false
		}
		proxy = next
	}
	return false
}

func adaptiveEligibleOriginalRoute(proxy C.Proxy, rule C.Rule, metadata *C.Metadata) bool {
	// Adaptive is a fallback only for the catch-all/default route. Explicit
	// DOMAIN/RULE-SET/GEOIP/DIRECT rules and any fixed proxy rule always win,
	// even when their selected group currently unwraps to DIRECT.
	return rule != nil && rule.RuleType() == C.MATCH && adaptiveRouteEndsDirect(proxy, metadata)
}

func adaptiveKey(metadata *C.Metadata) (adaptive.Key, bool) {
	if metadata == nil {
		return adaptive.Key{}, false
	}
	pkg := strings.TrimSpace(metadata.Process)
	if pkg == "" {
		if resolved, err := process.FindPackageName(metadata); err == nil {
			pkg = strings.TrimSpace(resolved)
			if pkg != "" {
				metadata.Process = pkg
			}
		}
	}
	if pkg == adaptiveSelfPackageRoot || strings.HasPrefix(pkg, adaptiveSelfPackageRoot+".") {
		return adaptive.Key{}, false
	}
	return adaptive.NormalizeKey(adaptive.Key{
		Profile:   adaptiveCurrentProfile(),
		Package:   pkg,
		Host:      metadata.RuleHost(),
		Port:      metadata.DstPort,
		CanRetain: false,
	})
}

func adaptiveOverride(metadata *C.Metadata, original C.Proxy, rule C.Rule) (C.Proxy, bool) {
	if !adaptiveReady.Load() || !adaptiveEligibleOriginalRoute(original, rule, metadata) {
		return original, false
	}
	key, ok := adaptiveKey(metadata)
	if !ok || !adaptiveEngine.ShouldUseAdaptive(key) {
		return original, false
	}
	proxy := adaptiveGetProxy()
	if proxy == nil {
		adaptiveEngine.RecordAdaptiveFailure(key)
		return original, false
	}
	return proxy, true
}

func adaptiveDialFailure(metadata *C.Metadata, original C.Proxy, selected C.Proxy, rule C.Rule, err error, overridden bool) {
	if !adaptiveReady.Load() || err == nil {
		return
	}
	key, keyOK := adaptiveKey(metadata)
	if overridden {
		if keyOK && !errors.Is(err, context.Canceled) {
			adaptiveEngine.RecordAdaptiveFailure(key)
			log.Warnln("[ADAPTIVE] %s failed for %s %s:%d; exact target returned to DIRECT for 60s: %v", adaptiveProxyName, key.Package, key.Host, key.Port, err)
		}
		return
	}
	if !keyOK || !adaptiveEligibleOriginalRoute(original, rule, metadata) || !adaptive.IsHardFailure(err) {
		return
	}
	if adaptiveEngine.RecordDirectFailure(key) {
		log.Infoln("[ADAPTIVE] Rescue armed after two final DIRECT failures: %s %s:%d", key.Package, key.Host, key.Port)
	}
}

var adaptiveConfirmedConnections sync.Map

func adaptiveTrackerKey(tracker statistic.Tracker) (adaptive.Key, bool) {
	if !adaptiveReady.Load() || tracker == nil || tracker.Chains().Last() != adaptiveProxyName {
		return adaptive.Key{}, false
	}
	info := tracker.Info()
	if info == nil {
		return adaptive.Key{}, false
	}
	return adaptiveKey(info.Metadata)
}

// adaptiveTrackTraffic runs synchronously from Mihomo's TCP tracker at the
// same points where its own byte counters are incremented. This avoids polling,
// cannot miss short-lived connections, and lets a quota breach close the
// underlying adaptive socket immediately.
func adaptiveTrackTraffic(tracker statistic.Tracker, uploadDelta, downloadDelta int64) bool {
	key, ok := adaptiveTrackerKey(tracker)
	if !ok {
		// The hidden proxy is reachable only through Adaptive. If its metadata can
		// no longer be attributed safely, stop that connection rather than let it
		// bypass accounting.
		return tracker != nil && tracker.Chains().Last() == adaptiveProxyName
	}

	quotaExceeded := adaptiveEngine.RecordTraffic(key, uploadDelta, downloadDelta)
	if uploadDelta+downloadDelta > 0 && !adaptivePersistQuotaState(false) {
		quotaExceeded = true
	}

	if downloadDelta > 0 {
		if _, loaded := adaptiveConfirmedConnections.LoadOrStore(tracker.ID(), struct{}{}); !loaded {
			retained := adaptiveEngine.ConfirmAdaptiveSuccess(key)
			if retained {
				log.Infoln("[ADAPTIVE] Retained %s %s:%d", key.Package, key.Host, key.Port)
			} else {
				log.Infoln("[ADAPTIVE] Rescue confirmed by response traffic for ephemeral network session: %s %s:%d", key.Package, key.Host, key.Port)
			}
		}
	}

	if quotaExceeded {
		// The statistic hook returns true; Mihomo then closes the underlying
		// adaptive socket before any further application traffic can pass.
		log.Warnln("[ADAPTIVE] quota stopped %s %s:%d", key.Package, key.Host, key.Port)
		return true
	}
	return false
}

func adaptiveTrackClose(tracker statistic.Tracker) {
	if tracker == nil || tracker.Chains().Last() != adaptiveProxyName {
		return
	}
	adaptiveConfirmedConnections.Delete(tracker.ID())
	// Manager.Leave calls this only after an atomic LoadAndDelete, so exactly
	// one close notification forces the final durable quota snapshot.
	adaptivePersistQuotaState(true)
}

func init() {
	tunnel.AdaptiveTCPOverrideHook = adaptiveOverride
	tunnel.AdaptiveTCPFailureHook = adaptiveDialFailure
	statistic.DefaultRequestTrafficNotify = adaptiveTrackTraffic
	statistic.DefaultRequestCloseNotify = adaptiveTrackClose
}

