package keel

import (
	"testing"
	"time"
)

func testReplica(name string, weight int64, lag time.Duration, replay Position) *replica {
	rep := &replica{name: name, weight: weight}
	rep.healthy.Store(true)
	rep.lag.Store(int64(lag))
	rep.replay.Store(uint64(replay))
	rep.checked.Store(time.Now().UnixNano())
	return rep
}

func TestPickSharesReadsByWeight(t *testing.T) {
	a := testReplica("a", 3, 0, 100)
	b := testReplica("b", 1, 0, 100)
	rs := &replicaSet{list: []*replica{a, b}, maxLag: time.Second}
	got := map[string]int{}
	for range 400 {
		rep := rs.pick(routeMark{}, false)
		got[rep.name]++
		rep.release()
	}
	if got["a"] != 300 || got["b"] != 100 {
		t.Fatalf("reads %v, want a 300 and b 100", got)
	}
}

func TestPickPrefersTheLeastLoaded(t *testing.T) {
	a := testReplica("a", 1, 0, 100)
	b := testReplica("b", 1, 0, 100)
	rs := &replicaSet{list: []*replica{a, b}, maxLag: time.Second}
	a.inflight.Store(5)
	for range 4 {
		if rep := rs.pick(routeMark{}, false); rep != b {
			t.Fatalf("picked %s with a loaded", rep.name)
		}
	}
	if b.inflight.Load() != 4 {
		t.Fatalf("b in flight %d, want 4", b.inflight.Load())
	}
	b.inflight.Store(10)
	if rep := rs.pick(routeMark{}, false); rep != a {
		t.Fatalf("picked %s, want the less loaded a", rep.name)
	}
}

func TestPickSkipsReplicasThatDoNotQualify(t *testing.T) {
	lagging := testReplica("lagging", 1, 3*time.Second, 500)
	behind := testReplica("behind", 1, 0, 90)
	down := testReplica("down", 1, 0, 500)
	down.healthy.Store(false)
	stale := testReplica("stale", 1, 0, 500)
	stale.checked.Store(time.Now().Add(-time.Minute).UnixNano())
	rs := &replicaSet{list: []*replica{lagging, behind, down, stale}, maxLag: time.Second}
	if rep := rs.pick(routeMark{pos: 100}, false); rep != nil {
		t.Fatalf("picked %s", rep.name)
	}
	if rs.fallbacks.Load() != 1 {
		t.Fatalf("fallbacks %d, want 1", rs.fallbacks.Load())
	}
	if rep := rs.pick(routeMark{pos: 90}, false); rep != behind {
		t.Fatalf("picked %v, want behind once its position is enough", rep)
	}
}

func TestGuardedPickTriesReplicasNotYetSeenAtTheFloor(t *testing.T) {
	seen := testReplica("seen", 1, 0, 100)
	unseen := testReplica("unseen", 1, 50*time.Millisecond, 90)
	rs := &replicaSet{list: []*replica{unseen, seen}, maxLag: time.Second}
	seen.inflight.Store(10)
	if rep := rs.pick(routeMark{pos: 100, at: time.Now()}, true); rep != seen {
		t.Fatalf("picked %v, want the replica seen at the floor even when busier", rep)
	}
	seen.healthy.Store(false)
	if rep := rs.pick(routeMark{pos: 100, at: time.Now()}, true); rep != nil {
		t.Fatalf("picked %s for a write newer than its lag", rep.name)
	}
	old := routeMark{pos: 100, at: time.Now().Add(-time.Second)}
	if rep := rs.pick(old, false); rep != nil {
		t.Fatalf("picked %s without a guard", rep.name)
	}
	if rep := rs.pick(old, true); rep != unseen {
		t.Fatalf("picked %v for a write older than the replica's lag", rep)
	}
	if rep := rs.pick(routeMark{pos: 100}, true); rep != unseen {
		t.Fatalf("picked %v for a position given without a time", rep)
	}
	if rep := rs.pick(routeMark{pos: unreachable}, true); rep != nil {
		t.Fatalf("picked %s for an unreachable floor", rep.name)
	}
}

func TestLagIsTheAgeOfTheNewestSampleReplayed(t *testing.T) {
	now := time.Now()
	rs := &replicaSet{maxLag: 2 * time.Second}
	for i, pos := range []Position{100, 200, 300, 400} {
		rs.addSample(walSample{at: now.Add(time.Duration(i-3) * time.Second), pos: pos})
	}
	cases := []struct {
		replay Position
		want   time.Duration
	}{
		{400, 0},
		{450, 0},
		{399, time.Second},
		{250, 2 * time.Second},
		{50, 3 * time.Second},
	}
	for _, c := range cases {
		if got := rs.lagOf(c.replay, now, 0); got != c.want {
			t.Fatalf("lag at %d: %v, want %v", c.replay, got, c.want)
		}
	}
	if got := rs.lagOf(50, now, 10*time.Second); got != 10*time.Second {
		t.Fatalf("lag before every sample: %v, want the replay timestamp's 10s", got)
	}
	for range 10 {
		rs.addSample(walSample{at: now, pos: 500})
	}
	if len(rs.samples) != 5 {
		t.Fatalf("kept %d samples, want 5", len(rs.samples))
	}
}

func TestNormalizeMovesHeaderPositionsToTheBoundary(t *testing.T) {
	rs := &replicaSet{}
	seg := Position(defaultWALSegment)
	cases := map[Position]Position{
		8192 + 24:         8192,
		8192 + 48:         8192 + 48,
		3*seg + 40:        3 * seg,
		3*seg + 8192*5:    3*seg + 8192*5,
		3*seg + 8192 + 24: 3*seg + 8192,
		3*seg + 8192 + 40: 3*seg + 8192 + 40,
	}
	for in, want := range cases {
		if got := rs.normalize(in); got != want {
			t.Fatalf("normalize %v: %v, want %v", in, got, want)
		}
	}
}

func TestPositionText(t *testing.T) {
	p := Position(0x16_B374D848)
	if p.String() != "16/B374D848" {
		t.Fatalf("String %q", p.String())
	}
	back, err := ParsePosition("16/B374D848")
	if err != nil || back != p {
		t.Fatalf("parse: %v %v", back, err)
	}
	for _, bad := range []string{"", "16", "x/1", "1/x", "1/100000000"} {
		if _, err := ParsePosition(bad); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
}

func TestRouteFloorIsPerDB(t *testing.T) {
	a, b := &DB{}, &DB{}
	rt := &route{}
	at := time.Now()
	rt.raise(a, 10, at)
	rt.raise(a, 5, time.Time{})
	rt.raise(b, 7, time.Time{})
	if m := rt.mark(a); m.pos != 10 || !m.at.Equal(at) {
		t.Fatalf("floor of a %+v", m)
	}
	if rt.mark(b).pos != 7 || rt.mark(&DB{}).pos != 0 {
		t.Fatalf("floors %v %v", rt.mark(b), rt.mark(&DB{}))
	}
	var none *route
	if none.mark(a).pos != 0 {
		t.Fatal("a nil route has a floor")
	}
}

func TestTenantKeys(t *testing.T) {
	type orgID string
	if got := TenantKeyOf(orgID("org_1"), "bu_1"); got != "org_1/bu_1" {
		t.Fatalf("key %q", got)
	}
	if got := TenantKeyOf(int64(7), int32(8), true); got != "7/8/true" {
		t.Fatalf("key %q", got)
	}
	esc := TenantKeyOf(`a/b`, `c\`)
	if esc != `a\/b/c\\` {
		t.Fatalf("escaped key %q", esc)
	}
	if TenantKeyOf("a/b", "c") == TenantKeyOf("a", "b/c") {
		t.Fatal("keys collide")
	}
	want := []string{`a\/b/c\\`, `a\/b`, ""}
	got := tenantPrefixes(esc)
	if len(got) != len(want) {
		t.Fatalf("prefixes %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("prefixes %q, want %q", got, want)
		}
	}
	if got := tenantPrefixes(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("prefixes of the empty key %q", got)
	}
}
