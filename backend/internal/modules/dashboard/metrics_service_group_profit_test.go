package dashboard

import (
	"context"
	"math"
	"testing"

	"transithub/backend/internal/modules/upstream"
)

// TestGroupProfitToday_OwnCostFromMappedUpstreamKeys 验证：
// 1) 自有分组成本 = 调价映射关联的上游 key 实耗之和（不再 1x 归一化分摊）
// 2) 只返回 revenue > 0 或 cost > 0 的分组
// 3) 利润 = 营收 − 关联上游成本
func TestGroupProfitToday_OwnCostFromMappedUpstreamKeys(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}

	multA := 2.0
	multB := 1.0
	multC := 0.72
	platform := &fakePlatformClient{
		groups: []upstream.GroupInfo{
			{Name: "vip", Multiplier: &multA},
			{Name: "default", Multiplier: &multB},
			{Name: "Claude Max 混合", Multiplier: &multC},
		},
		dailyStats: []upstream.GroupDailyStat{
			{GroupName: "vip", TodayActualCost: 100},
			{GroupName: "default", TodayActualCost: 100},
			{GroupName: "Claude Max 混合", TodayActualCost: 7.19},
		},
	}

	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{ID: "site-a", Name: "Upstream A", RechargeRate: 2},
		},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			// vip 关联 cheap：成本 40
			{
				SiteID: "site-a", SiteName: "Upstream A", Platform: upstream.PlatformSub2API,
				KeyID: "k1", GroupName: "cheap", TodayAmount: 40, RechargeRate: 2,
			},
			// default 关联 mid：成本 80
			{
				SiteID: "site-a", SiteName: "Upstream A", Platform: upstream.PlatformSub2API,
				KeyID: "k2", GroupName: "mid", TodayAmount: 80, RechargeRate: 2,
			},
			// Claude Max 关联 Claude Code 特价：成本 6.74（与分组健康上游列对齐）
			{
				SiteID: "site-a", SiteName: "Upstream A", Platform: upstream.PlatformSub2API,
				KeyID: "k3", GroupName: "Claude Code 特价", TodayAmount: 6.74, RechargeRate: 2,
			},
		},
	}

	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "vip", SiteID: "site-a", GroupName: "cheap"},
			{OwnGroup: "default", SiteID: "site-a", GroupName: "mid"},
			{OwnGroup: "Claude Max 混合", SiteID: "site-a", GroupName: "Claude Code 特价"},
		},
	})

	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}

	if len(resp.Groups) != 3 {
		t.Fatalf("expected 3 groups, got %d: %+v", len(resp.Groups), resp.Groups)
	}
	// totalCost = key 合计 40+80+6.74
	if math.Abs(resp.TotalCost-126.74) > 1e-9 {
		t.Fatalf("totalCost = %v, want 126.74", resp.TotalCost)
	}
	if math.Abs(resp.TotalRevenue-207.19) > 1e-9 {
		t.Fatalf("totalRevenue = %v, want 207.19", resp.TotalRevenue)
	}

	byName := map[string]GroupProfitTodayItem{}
	for _, g := range resp.Groups {
		byName[g.GroupName] = g
	}

	vip := byName["vip"]
	if math.Abs(vip.Cost-40) > 1e-9 {
		t.Fatalf("vip.cost = %v, want 40 (mapped upstream key sum)", vip.Cost)
	}
	if math.Abs(vip.Profit-60) > 1e-9 {
		t.Fatalf("vip.profit = %v, want 60", vip.Profit)
	}

	def := byName["default"]
	if math.Abs(def.Cost-80) > 1e-9 {
		t.Fatalf("default.cost = %v, want 80", def.Cost)
	}
	if math.Abs(def.Profit-20) > 1e-9 {
		t.Fatalf("default.profit = %v, want 20", def.Profit)
	}

	claude := byName["Claude Max 混合"]
	if math.Abs(claude.Cost-6.74) > 1e-9 {
		t.Fatalf("Claude Max cost = %v, want 6.74", claude.Cost)
	}
	if math.Abs(claude.Profit-(7.19-6.74)) > 1e-9 {
		t.Fatalf("Claude Max profit = %v, want 0.45", claude.Profit)
	}
}

// TestGroupProfitToday_SplitCostWhenUpstreamMapsToMultipleOwn 一上游映射多自有时均分成本。
func TestGroupProfitToday_SplitCostWhenUpstreamMapsToMultipleOwn(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups: []upstream.GroupInfo{
			{Name: "a", Multiplier: &m},
			{Name: "b", Multiplier: &m},
		},
		dailyStats: []upstream.GroupDailyStat{
			{GroupName: "a", TodayActualCost: 50},
			{GroupName: "b", TodayActualCost: 50},
		},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{{ID: "site-a", Name: "A", RechargeRate: 1}},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "site-a", SiteName: "A", KeyID: "k1", GroupName: "shared", TodayAmount: 100, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "a", SiteID: "site-a", GroupName: "shared"},
			{OwnGroup: "b", SiteID: "site-a", GroupName: "shared"},
		},
	})
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	byName := map[string]GroupProfitTodayItem{}
	for _, g := range resp.Groups {
		byName[g.GroupName] = g
	}
	if math.Abs(byName["a"].Cost-50) > 1e-9 || math.Abs(byName["b"].Cost-50) > 1e-9 {
		t.Fatalf("expected 50/50 split, got a=%v b=%v", byName["a"].Cost, byName["b"].Cost)
	}
}

// TestGroupProfitToday_ZeroRevenueEmpty 验证今日无营收且无上游成本时返回空列表。
func TestGroupProfitToday_ZeroRevenueEmpty(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	platform := &fakePlatformClient{
		dailyStats: []upstream.GroupDailyStat{
			{GroupName: "idle", TodayActualCost: 0},
		},
	}
	service := NewMetricsService(store, platform, &fakeUpstreamLister{}, nil, accounts)
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	if len(resp.Groups) != 0 {
		t.Fatalf("expected empty groups, got %+v", resp.Groups)
	}
	if resp.TotalRevenue != 0 || resp.TotalProfit != 0 || resp.TotalMargin != 0 {
		t.Fatalf("expected zero totals, got %+v", resp)
	}
}

type fakePricingMappings struct {
	links []PricingTargetLink
}

func (f *fakePricingMappings) ListPricingTargetLinks(ctx context.Context, userID string) ([]PricingTargetLink, error) {
	return f.links, nil
}

// TestGroupProfitToday_UpstreamGroupsFromKeyUsage 验证有消耗的上游分组会进入 upstreamGroups，
// 并用 (sale-costMult)/sale 估算利润率（与调价映射预算毛利率同公式）。
func TestGroupProfitToday_UpstreamGroupsFromKeyUsage(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	sale := 2.0
	platform := &fakePlatformClient{
		groups: []upstream.GroupInfo{
			{Name: "vip", Multiplier: &sale},
		},
		dailyStats: []upstream.GroupDailyStat{
			{GroupName: "vip", TodayActualCost: 100},
		},
	}

	upstreamGroupMult := 0.5
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{
				ID:           "site-a",
				Name:         "Upstream A",
				RechargeRate: 2,
				Metrics: upstream.Metrics{
					Groups: []upstream.GroupInfo{
						{Name: "cheap", Multiplier: &upstreamGroupMult},
					},
				},
			},
		},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{
				SiteID:       "site-a",
				SiteName:     "Upstream A",
				Platform:     upstream.PlatformSub2API,
				KeyID:        "k1",
				KeyName:      "key-1",
				GroupName:    "cheap",
				TodayAmount:  40, // 已 × rechargeRate
				RawAmount:    20,
				RechargeRate: 2,
			},
			{
				SiteID:       "site-a",
				SiteName:     "Upstream A",
				Platform:     upstream.PlatformSub2API,
				KeyID:        "k2",
				KeyName:      "key-2",
				GroupName:    "cheap",
				TodayAmount:  20,
				RawAmount:    10,
				RechargeRate: 2,
			},
			// 零消耗不进入列表
			{
				SiteID: "site-a", SiteName: "Upstream A", KeyID: "k3", GroupName: "idle", TodayAmount: 0, RechargeRate: 2,
			},
		},
	}

	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "vip", SiteID: "site-a", GroupName: "cheap"},
		},
	})

	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	if len(resp.UpstreamGroups) != 1 {
		t.Fatalf("expected 1 upstream group, got %+v", resp.UpstreamGroups)
	}
	u := resp.UpstreamGroups[0]
	if u.GroupName != "cheap" || u.SiteID != "site-a" {
		t.Fatalf("unexpected upstream row: %+v", u)
	}
	if math.Abs(u.Cost-60) > 1e-9 {
		t.Fatalf("upstream cost = %v, want 60", u.Cost)
	}
	// costMult = 0.5 * 2 = 1.0, sale = 2.0
	// margin = (2-1)/2 = 0.5
	// revenue = 60 * 2 / 1 = 120, profit = 60
	if math.Abs(u.ProfitMargin-0.5) > 1e-9 {
		t.Fatalf("upstream margin = %v, want 0.5", u.ProfitMargin)
	}
	if math.Abs(u.Revenue-120) > 1e-9 {
		t.Fatalf("upstream revenue = %v, want 120", u.Revenue)
	}
	if math.Abs(u.Profit-60) > 1e-9 {
		t.Fatalf("upstream profit = %v, want 60", u.Profit)
	}
	if len(u.MappedOwnGroups) != 1 || u.MappedOwnGroups[0] != "vip" {
		t.Fatalf("mapped own groups = %v, want [vip]", u.MappedOwnGroups)
	}
	// 自有 vip 成本应等于关联上游 cheap 的 60
	if len(resp.Groups) != 1 || math.Abs(resp.Groups[0].Cost-60) > 1e-9 {
		t.Fatalf("own vip cost = %+v, want cost 60", resp.Groups)
	}
}
