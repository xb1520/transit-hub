package group_rates

import (
	"testing"
	"time"
)

func floatPtr(v float64) *float64 { return &v }

func TestGroupPresenceKey(t *testing.T) {
	if got := groupPresenceKey("54", "vip"); got != "54" {
		t.Fatalf("prefer group id: got %q", got)
	}
	if got := groupPresenceKey("", "vip"); got != "vip" {
		t.Fatalf("fallback to name: got %q", got)
	}
	if got := groupPresenceKey("  ", "  vip  "); got != "vip" {
		t.Fatalf("trim whitespace: got %q", got)
	}
}

func TestPlanSiteSnapshotKeepsNilMultiplierPresent(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	existing := []latestGroupKey{
		{ID: "snap-auto", GroupID: "auto", GroupName: "auto", Multiplier: 1, Deleted: false},
		{ID: "snap-vip", GroupID: "2", GroupName: "vip", Multiplier: 1.5, Deleted: false},
		{ID: "snap-gone", GroupID: "3", GroupName: "gone", Multiplier: 2, Deleted: false},
	}
	groups := []SnapshotGroup{
		{ID: "auto", Name: "auto", Multiplier: nil}, // still present upstream, no numeric rate
		{ID: "2", Name: "vip", Multiplier: floatPtr(1.5)},
		// "gone" omitted → truly disappeared
	}

	plan, err := planSiteSnapshot("user", "ws", "site-a", "Site A", "sub2api", groups, existing, now)
	if err != nil {
		t.Fatalf("planSiteSnapshot: %v", err)
	}

	if len(plan.toDelete) != 1 || plan.toDelete[0] != "snap-gone" {
		t.Fatalf("toDelete = %#v, want only snap-gone", plan.toDelete)
	}

	// nil-multiplier "auto" must be touched, not deleted.
	// vip rate unchanged → touched.
	touchSet := map[string]struct{}{}
	for _, id := range plan.toTouch {
		touchSet[id] = struct{}{}
	}
	if _, ok := touchSet["snap-auto"]; !ok {
		t.Fatalf("expected nil-multiplier present group to be touched, toTouch=%#v", plan.toTouch)
	}
	if _, ok := touchSet["snap-vip"]; !ok {
		t.Fatalf("expected unchanged rate group to be touched, toTouch=%#v", plan.toTouch)
	}
	if len(plan.toInsert) != 0 {
		t.Fatalf("toInsert = %#v, want empty", plan.toInsert)
	}
}

func TestPlanSiteSnapshotInsertsRateChangeAndDoesNotDeleteNilMultiplier(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	existing := []latestGroupKey{
		{ID: "snap-auto", GroupID: "auto", GroupName: "auto", Multiplier: 1, Deleted: false},
		{ID: "snap-vip", GroupID: "2", GroupName: "vip", Multiplier: 1.5, Deleted: false},
	}
	groups := []SnapshotGroup{
		{ID: "auto", Name: "auto", Multiplier: nil},
		{ID: "2", Name: "vip", Multiplier: floatPtr(2.0)}, // rate changed
	}

	plan, err := planSiteSnapshot("user", "ws", "site-a", "Site A", "newapi", groups, existing, now)
	if err != nil {
		t.Fatalf("planSiteSnapshot: %v", err)
	}
	if len(plan.toDelete) != 0 {
		t.Fatalf("toDelete = %#v, want empty (nil multiplier must not soft-delete)", plan.toDelete)
	}
	if len(plan.toInsert) != 1 {
		t.Fatalf("toInsert len = %d, want 1 rate-change row", len(plan.toInsert))
	}
	if plan.toInsert[0].GroupID != "2" || plan.toInsert[0].Multiplier != 2.0 {
		t.Fatalf("unexpected insert: %#v", plan.toInsert[0])
	}
	if len(plan.toTouch) != 1 || plan.toTouch[0] != "snap-auto" {
		t.Fatalf("toTouch = %#v, want only snap-auto", plan.toTouch)
	}
}

func TestPlanSiteSnapshotEmptyPlatformStillKeepsPresentGroups(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	existing := []latestGroupKey{
		{ID: "snap-vip", GroupID: "2", GroupName: "vip", Multiplier: 1.5, Deleted: false},
	}
	// Platform empty → cannot insert, but group is still present so must not delete.
	groups := []SnapshotGroup{
		{ID: "2", Name: "vip", Multiplier: floatPtr(1.5)},
	}

	plan, err := planSiteSnapshot("user", "ws", "site-a", "Site A", "", groups, existing, now)
	if err != nil {
		t.Fatalf("planSiteSnapshot: %v", err)
	}
	if len(plan.toDelete) != 0 {
		t.Fatalf("toDelete = %#v, want empty when platform empty but group present", plan.toDelete)
	}
	if len(plan.toInsert) != 0 {
		t.Fatalf("toInsert = %#v, want empty without platform", plan.toInsert)
	}
	if len(plan.toTouch) != 1 || plan.toTouch[0] != "snap-vip" {
		t.Fatalf("toTouch = %#v, want snap-vip", plan.toTouch)
	}
}

func TestPlanSiteSnapshotSkipsEmptyNames(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	existing := []latestGroupKey{
		{ID: "snap-x", GroupID: "1", GroupName: "kept", Multiplier: 1, Deleted: false},
	}
	groups := []SnapshotGroup{
		{ID: "1", Name: "kept", Multiplier: floatPtr(1)},
		{ID: "2", Name: "   ", Multiplier: floatPtr(3)}, // empty after trim → ignored
	}

	plan, err := planSiteSnapshot("user", "ws", "site-a", "Site A", "sub2api", groups, existing, now)
	if err != nil {
		t.Fatalf("planSiteSnapshot: %v", err)
	}
	if len(plan.toInsert) != 0 {
		t.Fatalf("blank name must not insert: %#v", plan.toInsert)
	}
	if len(plan.toDelete) != 0 {
		t.Fatalf("toDelete = %#v", plan.toDelete)
	}
}
