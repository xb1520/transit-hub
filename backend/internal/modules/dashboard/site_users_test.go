package dashboard

import (
	"testing"

	"transithub/backend/internal/modules/upstream"
)

func TestMergeGiftAmounts(t *testing.T) {
	manual := map[string]float64{"1": 10, "2": 5}
	marked := map[string]float64{"1": 30, "3": 8}
	got := mergeGiftAmounts(manual, marked)
	if got["1"] != 30 {
		t.Fatalf("user1: want max(10,30)=30 got %v", got["1"])
	}
	if got["2"] != 5 {
		t.Fatalf("user2: want manual 5 got %v", got["2"])
	}
	if got["3"] != 8 {
		t.Fatalf("user3: want marked 8 got %v", got["3"])
	}
}

func TestNormalizeSiteUserTag(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"recharge", SiteUserTagRecharge, true},
		{"gift", SiteUserTagGift, true},
		{"rebate", SiteUserTagRebate, true},
		{"paid", SiteUserTagRecharge, true},
		{"nope", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeSiteUserTag(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("%q: got (%q,%v) want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCmpUserID(t *testing.T) {
	if cmpUserID("11", "2") <= 0 {
		t.Fatal("numeric: 11 should be > 2")
	}
	if cmpUserID("2", "11") >= 0 {
		t.Fatal("numeric: 2 should be < 11")
	}
	if cmpUserID("a", "b") >= 0 {
		t.Fatal("string: a < b")
	}
}

func TestBuildUsagePeriodPercent(t *testing.T) {
	period := buildUsagePeriod("today", "2026-08-01", "2026-08-01", []upstream.Sub2APIUserGroupUsage{
		{GroupName: "A", ActualCost: 30},
		{GroupName: "B", ActualCost: 70},
	}, 100, nil)
	if period.TotalActualCost != 100 {
		t.Fatalf("total: got %v", period.TotalActualCost)
	}
	if len(period.Groups) != 2 {
		t.Fatalf("groups: %d", len(period.Groups))
	}
	// order preserved from input (already sorted by cost desc in fetch, here A then B)
	if period.Groups[0].Percent < 29 || period.Groups[0].Percent > 31 {
		t.Fatalf("A percent: %v", period.Groups[0].Percent)
	}
	if period.Groups[1].Percent < 69 || period.Groups[1].Percent > 71 {
		t.Fatalf("B percent: %v", period.Groups[1].Percent)
	}
}

func TestSiteUserSortNeedsLocal(t *testing.T) {
	if !siteUserSortNeedsLocal("id") || !siteUserSortNeedsLocal("recharge") {
		t.Fatal("expected local sorts")
	}
	if siteUserSortNeedsLocal("email") {
		t.Fatal("email can use upstream sort")
	}
	if !siteUserSortNeedsUsage("today_tokens") || !siteUserSortNeedsUsage("total_cost") {
		t.Fatal("expected usage sorts")
	}
	if siteUserSortNeedsUsage("balance_cny") {
		t.Fatal("balance is not usage sort")
	}
}

func TestSortSiteUsersByUsage(t *testing.T) {
	items := []SiteUserItem{
		{ID: "1", TodayTokens: 100, TotalCost: 5},
		{ID: "2", TodayTokens: 500, TotalCost: 1},
		{ID: "3", TodayTokens: 200, TotalCost: 9},
	}
	sortSiteUsers(items, "today_tokens", "desc")
	if items[0].ID != "2" || items[1].ID != "3" {
		t.Fatalf("today_tokens desc: %+v", []string{items[0].ID, items[1].ID, items[2].ID})
	}
	sortSiteUsers(items, "total_cost", "asc")
	if items[0].ID != "2" || items[2].ID != "3" {
		t.Fatalf("total_cost asc: %+v", []string{items[0].ID, items[1].ID, items[2].ID})
	}
}
