package adaptive

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	RuleDailyLimit  int64 = 128 << 20
	HostDailyLimit  int64 = 256 << 20
	TotalDailyLimit int64 = 512 << 20

	FailureThreshold = 2
	FailureWindow    = 2 * time.Minute
	RescueTTL        = 15 * time.Minute
	RetainedTTL      = 30 * 24 * time.Hour
	BreakerTTL       = 60 * time.Second
)

type Key struct {
	Profile   string
	Package   string
	Host      string
	Port      uint16
	CanRetain bool
}

type entry struct {
	ExpiresAt time.Time
}

type failureStrike struct {
	Count int
	Last  time.Time
}

type ruleQuotaKey struct {
	Package string
	Host    string
	Port    uint16
}

type hostQuotaKey struct {
	Host string
}

type Engine struct {
	mu sync.Mutex

	now func() time.Time

	rescue   map[Key]entry
	retained map[Key]entry
	strikes  map[Key]failureStrike
	breaker  map[Key]time.Time

	quotaDay  string
	ruleUsed  map[ruleQuotaKey]int64
	hostUsed  map[hostQuotaKey]int64
	totalUsed int64
}

type Snapshot struct {
	Rescue        int
	Retained      int
	PendingStrike int
	RuleUsed      int64
	HostUsed      int64
	TotalUsed     int64
	BreakerActive bool
}

type RuleQuota struct {
	Package string `json:"package"`
	Host    string `json:"host"`
	Port    uint16 `json:"port"`
	Used    int64  `json:"used"`
}

type HostQuota struct {
	Host string `json:"host"`
	Used int64  `json:"used"`
}

type QuotaState struct {
	Day       string      `json:"day"`
	Rules     []RuleQuota `json:"rules"`
	Hosts     []HostQuota `json:"hosts"`
	TotalUsed int64       `json:"total_used"`
}

func New() *Engine {
	return newWithClock(time.Now)
}

func newWithClock(now func() time.Time) *Engine {
	return &Engine{
		now:      now,
		rescue:   make(map[Key]entry),
		retained: make(map[Key]entry),
		strikes:  make(map[Key]failureStrike),
		breaker:  make(map[Key]time.Time),
		ruleUsed: make(map[ruleQuotaKey]int64),
		hostUsed: make(map[hostQuotaKey]int64),
	}
}

func NormalizeKey(k Key) (Key, bool) {
	k.Profile = strings.TrimSpace(k.Profile)
	k.Package = strings.TrimSpace(k.Package)
	k.Host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(k.Host), "."))

	if k.Profile == "" || k.Package == "" || k.Host == "" {
		return Key{}, false
	}
	if k.Port != 80 && k.Port != 443 {
		return Key{}, false
	}
	if len(k.Host) > 253 || !strings.Contains(k.Host, ".") || strings.Contains(k.Host, "*") {
		return Key{}, false
	}
	if net.ParseIP(k.Host) != nil || k.Host == "localhost" || strings.HasSuffix(k.Host, ".local") {
		return Key{}, false
	}
	return k, true
}

func ruleQuotaFor(k Key) ruleQuotaKey {
	return ruleQuotaKey{Package: k.Package, Host: k.Host, Port: k.Port}
}

func hostQuotaFor(k Key) hostQuotaKey {
	return hostQuotaKey{Host: k.Host}
}

// RecordDirectFailure records one final failed connection attempt. Mihomo calls
// this only after its own internal retry loop has finished, so one connection
// cannot manufacture multiple strikes. A single failure never learns a route.
func (e *Engine) RecordDirectFailure(k Key) bool {
	k, ok := NormalizeKey(k)
	if !ok {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.rollQuotaDayLocked(now)
	e.pruneLocked(now)
	if e.breakerActiveLocked(k, now) || e.quotaExceededLocked(k) {
		return false
	}
	if _, ok := e.rescue[k]; ok {
		return false
	}
	if _, ok := e.retained[k]; ok {
		return false
	}

	strike := e.strikes[k]
	if strike.Last.IsZero() || now.Sub(strike.Last) > FailureWindow {
		strike.Count = 0
	}
	strike.Count++
	strike.Last = now
	if strike.Count < FailureThreshold {
		e.strikes[k] = strike
		return false
	}

	delete(e.strikes, k)
	e.rescue[k] = entry{ExpiresAt: now.Add(RescueTTL)}
	return true
}

func (e *Engine) ShouldUseAdaptive(k Key) bool {
	k, ok := NormalizeKey(k)
	if !ok {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.rollQuotaDayLocked(now)
	e.pruneLocked(now)
	if e.breakerActiveLocked(k, now) || e.quotaExceededLocked(k) {
		return false
	}
	if _, ok := e.retained[k]; ok {
		return true
	}
	_, ok = e.rescue[k]
	return ok
}

// ConfirmAdaptiveSuccess is called only after real response bytes arrive.
// Ephemeral Android network sessions refresh Rescue but never become Retained.
func (e *Engine) ConfirmAdaptiveSuccess(k Key) bool {
	k, ok := NormalizeKey(k)
	if !ok {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.pruneLocked(now)
	rescue, ok := e.rescue[k]
	if !ok {
		return false
	}
	rescue.ExpiresAt = now.Add(RescueTTL)
	e.rescue[k] = rescue
	if !k.CanRetain {
		return false
	}
	delete(e.rescue, k)
	e.retained[k] = entry{ExpiresAt: now.Add(RetainedTTL)}
	return true
}

// RecordAdaptiveFailure removes the learned route and suppresses this exact
// target briefly. It is intentionally per-key: one destination failure must
// not disable Adaptive for unrelated applications or hosts.
func (e *Engine) RecordAdaptiveFailure(k Key) {
	k, ok := NormalizeKey(k)
	if !ok {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	delete(e.rescue, k)
	delete(e.retained, k)
	delete(e.strikes, k)
	until := now.Add(BreakerTTL)
	if current := e.breaker[k]; until.After(current) {
		e.breaker[k] = until
	}
}

// RecordTraffic records deltas, not cumulative counters. It returns true when
// any daily protection limit has been reached and the current adaptive
// connection should be closed immediately.
func (e *Engine) RecordTraffic(k Key, uploadDelta, downloadDelta int64) bool {
	k, ok := NormalizeKey(k)
	if !ok {
		return true
	}
	if uploadDelta < 0 {
		uploadDelta = 0
	}
	if downloadDelta < 0 {
		downloadDelta = 0
	}
	delta := uploadDelta + downloadDelta

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.rollQuotaDayLocked(now)
	if delta > 0 {
		e.ruleUsed[ruleQuotaFor(k)] += delta
		e.hostUsed[hostQuotaFor(k)] += delta
		e.totalUsed += delta
	}
	return e.quotaExceededLocked(k)
}

// ExportQuotaState persists quota accounting only. Learned routes, strikes and
// breakers are deliberately never exported because Android v0.1 network
// identities are ephemeral per VPN/network session.
func (e *Engine) ExportQuotaState() QuotaState {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.rollQuotaDayLocked(now)

	state := QuotaState{Day: e.quotaDay, TotalUsed: e.totalUsed}
	state.Rules = make([]RuleQuota, 0, len(e.ruleUsed))
	for k, used := range e.ruleUsed {
		if used <= 0 {
			continue
		}
		state.Rules = append(state.Rules, RuleQuota{Package: k.Package, Host: k.Host, Port: k.Port, Used: used})
	}
	state.Hosts = make([]HostQuota, 0, len(e.hostUsed))
	for k, used := range e.hostUsed {
		if used <= 0 {
			continue
		}
		state.Hosts = append(state.Hosts, HostQuota{Host: k.Host, Used: used})
	}
	return state
}

// RestoreQuotaState restores only quota counters for the current local day.
// It rejects malformed entries instead of partially restoring them.
func (e *Engine) RestoreQuotaState(state QuotaState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.rollQuotaDayLocked(now)
	if state.TotalUsed < 0 {
		return false
	}

	savedDay, err := time.ParseInLocation("2006-01-02", state.Day, now.Location())
	if err != nil {
		return false
	}
	currentDay, err := time.ParseInLocation("2006-01-02", e.quotaDay, now.Location())
	if err != nil {
		return false
	}
	if savedDay.After(currentDay) {
		return false
	}
	if savedDay.Before(currentDay) {
		// A valid prior-day state is not corruption. Quotas have legitimately
		// rolled over, so start the new local day at zero.
		clear(e.ruleUsed)
		clear(e.hostUsed)
		e.totalUsed = 0
		return true
	}

	rules := make(map[ruleQuotaKey]int64, len(state.Rules))
	var ruleSum int64
	for _, q := range state.Rules {
		k := Key{Profile: "quota-restore", Package: q.Package, Host: q.Host, Port: q.Port}
		nk, ok := NormalizeKey(k)
		key := ruleQuotaFor(nk)
		if !ok || q.Used < 0 {
			return false
		}
		if _, duplicate := rules[key]; duplicate {
			return false
		}
		if q.Used > int64(^uint64(0)>>1)-ruleSum {
			return false
		}
		rules[key] = q.Used
		ruleSum += q.Used
	}
	hosts := make(map[hostQuotaKey]int64, len(state.Hosts))
	var hostSum int64
	for _, q := range state.Hosts {
		k := Key{Profile: "quota-restore", Package: "quota-restore", Host: q.Host, Port: 443}
		nk, ok := NormalizeKey(k)
		key := hostQuotaFor(nk)
		if !ok || q.Used < 0 {
			return false
		}
		if _, duplicate := hosts[key]; duplicate {
			return false
		}
		if q.Used > int64(^uint64(0)>>1)-hostSum {
			return false
		}
		hosts[key] = q.Used
		hostSum += q.Used
	}
	if ruleSum != state.TotalUsed || hostSum != state.TotalUsed {
		return false
	}
	e.ruleUsed = rules
	e.hostUsed = hosts
	e.totalUsed = state.TotalUsed
	return true
}

func (e *Engine) ForceDailyTotalAtLeast(used int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rollQuotaDayLocked(e.now())
	if used > e.totalUsed {
		e.totalUsed = used
	}
}

func (e *Engine) Snapshot(k Key) Snapshot {
	nk, valid := NormalizeKey(k)

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.rollQuotaDayLocked(now)
	e.pruneLocked(now)
	s := Snapshot{
		Rescue:    len(e.rescue),
		Retained:  len(e.retained),
		TotalUsed: e.totalUsed,
	}
	if valid {
		s.RuleUsed = e.ruleUsed[ruleQuotaFor(nk)]
		s.HostUsed = e.hostUsed[hostQuotaFor(nk)]
		s.PendingStrike = e.strikes[nk].Count
		s.BreakerActive = e.breakerActiveLocked(nk, now)
	}
	return s
}

func (e *Engine) quotaExceededLocked(k Key) bool {
	return e.ruleUsed[ruleQuotaFor(k)] >= RuleDailyLimit ||
		e.hostUsed[hostQuotaFor(k)] >= HostDailyLimit ||
		e.totalUsed >= TotalDailyLimit
}

func (e *Engine) breakerActiveLocked(k Key, now time.Time) bool {
	until := e.breaker[k]
	return now.Before(until)
}

func (e *Engine) rollQuotaDayLocked(now time.Time) {
	day := now.Local().Format("2006-01-02")
	if e.quotaDay == day {
		return
	}
	e.quotaDay = day
	clear(e.ruleUsed)
	clear(e.hostUsed)
	e.totalUsed = 0
}

func (e *Engine) pruneLocked(now time.Time) {
	for k, v := range e.rescue {
		if !now.Before(v.ExpiresAt) {
			delete(e.rescue, k)
		}
	}
	for k, v := range e.retained {
		if !now.Before(v.ExpiresAt) {
			delete(e.retained, k)
		}
	}
	for k, v := range e.strikes {
		if v.Last.IsZero() || now.Sub(v.Last) > FailureWindow {
			delete(e.strikes, k)
		}
	}
	for k, until := range e.breaker {
		if !now.Before(until) {
			delete(e.breaker, k)
		}
	}
}

func IsHardFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	hard := []string{
		"i/o timeout",
		"connection timed out",
		"connection reset",
		"connection refused",
		"network is unreachable",
		"no route to host",
		"host is unreachable",
	}
	for _, token := range hard {
		if strings.Contains(msg, token) {
			return true
		}
	}
	return false
}

