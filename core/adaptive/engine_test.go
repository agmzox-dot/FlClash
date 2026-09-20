package adaptive

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time      { return c.t }
func (c *fakeClock) Add(d time.Duration) { c.t = c.t.Add(d) }

func validKey() Key {
	return Key{Profile: "net-1", Package: "com.example.app", Host: "img.example.com", Port: 443}
}

func learnRescue(t *testing.T, e *Engine, k Key) {
	t.Helper()
	if e.RecordDirectFailure(k) {
		t.Fatal("single direct failure learned a route")
	}
	if !e.RecordDirectFailure(k) {
		t.Fatal("second final direct failure did not create Rescue")
	}
}

func TestNormalizeKeyRejectsUnsafeTargets(t *testing.T) {
	tests := []Key{
		{Profile: "", Package: "p", Host: "a.example", Port: 443},
		{Profile: "n", Package: "", Host: "a.example", Port: 443},
		{Profile: "n", Package: "p", Host: "203.0.113.1", Port: 443},
		{Profile: "n", Package: "p", Host: "localhost", Port: 443},
		{Profile: "n", Package: "p", Host: "printer.local", Port: 443},
		{Profile: "n", Package: "p", Host: "example.com", Port: 22},
		{Profile: "n", Package: "p", Host: "*.example.com", Port: 443},
	}
	for _, tc := range tests {
		if _, ok := NormalizeKey(tc); ok {
			t.Fatalf("unsafe key accepted: %#v", tc)
		}
	}
}

func TestSingleFailureNeverLearns(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k := validKey()
	if e.RecordDirectFailure(k) {
		t.Fatal("single failure learned")
	}
	if e.ShouldUseAdaptive(k) {
		t.Fatal("single failure routed adaptively")
	}
	if got := e.Snapshot(k).PendingStrike; got != 1 {
		t.Fatalf("pending strikes=%d want 1", got)
	}
}

func TestFailureWindowExpires(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k := validKey()
	e.RecordDirectFailure(k)
	c.Add(FailureWindow + time.Second)
	if e.RecordDirectFailure(k) {
		t.Fatal("stale strike was reused")
	}
	if e.ShouldUseAdaptive(k) {
		t.Fatal("stale strike created route")
	}
}

func TestRescueAndEphemeralSuccessNeverRetains(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 9, 17, 9, 0, 0, 0, time.Local)}
	e := newWithClock(c.Now)
	k := validKey()
	learnRescue(t, e, k)
	if !e.ShouldUseAdaptive(k) {
		t.Fatal("rescue did not route")
	}
	if e.ConfirmAdaptiveSuccess(k) {
		t.Fatal("ephemeral Android-style profile became Retained")
	}
	if got := e.Snapshot(k).Retained; got != 0 {
		t.Fatalf("retained=%d want 0", got)
	}
	c.Add(RescueTTL - time.Second)
	if !e.ShouldUseAdaptive(k) {
		t.Fatal("confirmed Rescue was not refreshed")
	}
	c.Add(2 * time.Second)
	if e.ShouldUseAdaptive(k) {
		t.Fatal("expired Rescue still active")
	}
}

func TestStableProfileCanRetain(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k := validKey()
	k.CanRetain = true
	learnRescue(t, e, k)
	if !e.ConfirmAdaptiveSuccess(k) {
		t.Fatal("stable profile did not retain")
	}
	if !e.ShouldUseAdaptive(k) {
		t.Fatal("retained route missing")
	}
}

func TestNetworkPackageAndHostIsolation(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k := validKey()
	learnRescue(t, e, k)

	otherNetwork := k
	otherNetwork.Profile = "net-2"
	otherPackage := k
	otherPackage.Package = "com.other.app"
	otherHost := k
	otherHost.Host = "api.example.com"
	for _, candidate := range []Key{otherNetwork, otherPackage, otherHost} {
		if e.ShouldUseAdaptive(candidate) {
			t.Fatalf("learning leaked to %#v", candidate)
		}
	}
}

func TestAdaptiveFailureIsPerKeyAndRemovesRoute(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k1 := validKey()
	k2 := k1
	k2.Host = "api.example.com"
	learnRescue(t, e, k1)
	learnRescue(t, e, k2)

	e.RecordAdaptiveFailure(k1)
	if e.ShouldUseAdaptive(k1) {
		t.Fatal("failed adaptive route survived")
	}
	if !e.ShouldUseAdaptive(k2) {
		t.Fatal("one target failure disabled another target")
	}
	c.Add(BreakerTTL + time.Second)
	if e.ShouldUseAdaptive(k1) {
		t.Fatal("route should require two fresh direct failures after adaptive failure")
	}
}

func TestQuotaProtectionAcrossNetworkProfiles(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k1 := validKey()
	k2 := k1
	k2.Profile = "net-2"
	learnRescue(t, e, k1)

	if e.RecordTraffic(k1, RuleDailyLimit-1, 0) {
		t.Fatal("quota tripped too early")
	}
	if !e.RecordTraffic(k2, 1, 0) {
		t.Fatal("rule quota did not persist across network sessions")
	}
	if e.ShouldUseAdaptive(k1) {
		t.Fatal("quota did not disable learned route")
	}
}

func TestHostQuotaAggregatesAcrossPackages(t *testing.T) {
	c := &fakeClock{t: time.Now()}
	e := newWithClock(c.Now)
	k1 := validKey()
	k2 := k1
	k2.Package = "com.other.app"
	k3 := k1
	k3.Package = "com.third.app"
	if e.RecordTraffic(k1, (127 << 20), 0) {
		t.Fatal("host quota tripped on first package")
	}
	if e.RecordTraffic(k2, (127 << 20), 0) {
		t.Fatal("host quota tripped on second package")
	}
	if !e.RecordTraffic(k3, 2<<20, 0) {
		t.Fatal("host quota did not aggregate across packages")
	}
}

func TestQuotaStatePersistsRuleHostAndTotalWithoutLearning(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)}
	e := newWithClock(c.Now)
	k := validKey()
	learnRescue(t, e, k)
	if e.RecordTraffic(k, 7<<20, 11<<20) {
		t.Fatal("quota tripped too early")
	}
	state := e.ExportQuotaState()

	restored := newWithClock(c.Now)
	if !restored.RestoreQuotaState(state) {
		t.Fatal("valid quota state was rejected")
	}
	snap := restored.Snapshot(k)
	if snap.RuleUsed != 18<<20 || snap.HostUsed != 18<<20 || snap.TotalUsed != 18<<20 {
		t.Fatalf("restored quota mismatch: %#v", snap)
	}
	if snap.Rescue != 0 || snap.Retained != 0 || restored.ShouldUseAdaptive(k) {
		t.Fatal("quota restore resurrected learned routing state")
	}
}

func TestQuotaStatePriorDayResetsInsteadOfFailClosing(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)}
	e := newWithClock(c.Now)
	state := QuotaState{
		Day:       "2026-09-16",
		TotalUsed: 18 << 20,
		Rules:     []RuleQuota{{Package: "p", Host: "example.com", Port: 443, Used: 18 << 20}},
		Hosts:     []HostQuota{{Host: "example.com", Used: 18 << 20}},
	}
	if !e.RestoreQuotaState(state) {
		t.Fatal("valid prior-day quota state was treated as corruption")
	}
	if got := e.ExportQuotaState(); got.TotalUsed != 0 || len(got.Rules) != 0 || len(got.Hosts) != 0 {
		t.Fatalf("prior-day quota did not roll over cleanly: %#v", got)
	}
}

func TestQuotaStateRejectsMalformedFutureDuplicateOrInconsistentData(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)}
	e := newWithClock(c.Now)
	bad := []QuotaState{
		{Day: "not-a-day"},
		{Day: "2026-09-18"},
		{Day: "2026-09-17", TotalUsed: -1},
		{Day: "2026-09-17", TotalUsed: 1, Rules: []RuleQuota{{Package: "p", Host: "example.com", Port: 22, Used: 1}}, Hosts: []HostQuota{{Host: "example.com", Used: 1}}},
		{Day: "2026-09-17", TotalUsed: 2, Rules: []RuleQuota{{Package: "p", Host: "example.com", Port: 443, Used: 1}, {Package: "p", Host: "example.com", Port: 443, Used: 1}}, Hosts: []HostQuota{{Host: "example.com", Used: 2}}},
		{Day: "2026-09-17", TotalUsed: 2, Rules: []RuleQuota{{Package: "p", Host: "example.com", Port: 443, Used: 2}}, Hosts: []HostQuota{{Host: "example.com", Used: 1}}},
	}
	for _, state := range bad {
		if e.RestoreQuotaState(state) {
			t.Fatalf("malformed quota state accepted: %#v", state)
		}
	}
}

func TestQuotaStateRollsOverOnNewLocalDay(t *testing.T) {
	c := &fakeClock{t: time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)}
	e := newWithClock(c.Now)
	k := validKey()
	e.RecordTraffic(k, 3<<20, 0)
	c.Add(24 * time.Hour)
	state := e.ExportQuotaState()
	if state.TotalUsed != 0 || len(state.Rules) != 0 || len(state.Hosts) != 0 {
		t.Fatalf("new-day quota not reset: %#v", state)
	}
}

func TestHardFailureClassifier(t *testing.T) {
	hard := []error{
		context.DeadlineExceeded,
		syscall.ETIMEDOUT,
		syscall.ECONNRESET,
		syscall.ECONNREFUSED,
		fmt.Errorf("dial: %w", syscall.ENETUNREACH),
		errors.New("read: i/o timeout"),
	}
	for _, err := range hard {
		if !IsHardFailure(err) {
			t.Fatalf("hard failure rejected: %v", err)
		}
	}
	soft := []error{nil, context.Canceled, errors.New("certificate verify failed"), errors.New("dns lookup failed")}
	for _, err := range soft {
		if IsHardFailure(err) {
			t.Fatalf("soft failure accepted: %v", err)
		}
	}
}
