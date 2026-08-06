package dashboard

import (
	"context"
	"math"
	"testing"

	"transithub/backend/internal/modules/upstream"
)

// TestGroupProfitToday_VolumeBasedAllocation 验证：
// 1) 只返回 revenue > 0 的分组
// 2) 成本按 1x 归一化用量分摊
// 3) 各分组 profit 之和 = totalRevenue - totalCost
// 4) 高倍率分组利润率更高（同等营收下承担更少成本）
func TestGroupProfitToday_VolumeBasedAllocation(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}

	multA := 2.0
	multB := 1.0
	platform := &fakePlatformClient{
		groups: []upstream.GroupInfo{
			{Name: "vip", Multiplier: &multA},
			{Name: "default", Multiplier: &multB},
			{Name: "idle", Multiplier: &multB},
		},
		dailyStats: []upstream.GroupDailyStat{
			{GroupName: "vip", TodayActualCost: 100},
			{GroupName: "default", TodayActualCost: 100},
			{GroupName: "idle", TodayActualCost: 0},
		},
	}

	consume := 60.0
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{
				RechargeRate: 2,
				Metrics: upstream.Metrics{
					TodayConsume: upstream.MetricValue{Value: &consume},
				},
			},
		},
	}

	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}

	if len(resp.Groups) != 2 {
		t.Fatalf("expected 2 groups with revenue, got %d: %+v", len(resp.Groups), resp.Groups)
	}
	if resp.TotalRevenue != 200 {
		t.Fatalf("totalRevenue = %v, want 200", resp.TotalRevenue)
	}
	// totalCost = 60 * 2 = 120
	if resp.TotalCost != 120 {
		t.Fatalf("totalCost = %v, want 120", resp.TotalCost)
	}
	if resp.TotalProfit != 80 {
		t.Fatalf("totalProfit = %v, want 80", resp.TotalProfit)
	}

	byName := map[string]GroupProfitTodayItem{}
	var sumProfit, sumCost float64
	for _, g := range resp.Groups {
		byName[g.GroupName] = g
		sumProfit += g.Profit
		sumCost += g.Cost
	}
	if math.Abs(sumProfit-resp.TotalProfit) > 1e-9 {
		t.Fatalf("sum(profit)=%v totalProfit=%v", sumProfit, resp.TotalProfit)
	}
	if math.Abs(sumCost-resp.TotalCost) > 1e-9 {
		t.Fatalf("sum(cost)=%v totalCost=%v", sumCost, resp.TotalCost)
	}

	// volume vip=100/2=50, default=100/1=100, totalV=150
	// cost vip=120*50/150=40, default=120*100/150=80
	vip := byName["vip"]
	def := byName["default"]
	if math.Abs(vip.Cost-40) > 1e-9 {
		t.Fatalf("vip.cost = %v, want 40", vip.Cost)
	}
	if math.Abs(def.Cost-80) > 1e-9 {
		t.Fatalf("default.cost = %v, want 80", def.Cost)
	}
	if math.Abs(vip.Profit-60) > 1e-9 {
		t.Fatalf("vip.profit = %v, want 60", vip.Profit)
	}
	if math.Abs(def.Profit-20) > 1e-9 {
		t.Fatalf("default.profit = %v, want 20", def.Profit)
	}
	// vip margin 0.6 > default margin 0.2
	if vip.ProfitMargin <= def.ProfitMargin {
		t.Fatalf("expected vip margin > default margin, got vip=%v default=%v", vip.ProfitMargin, def.ProfitMargin)
	}
	// Sorted by profit desc: vip first
	if resp.Groups[0].GroupName != "vip" {
		t.Fatalf("expected vip first by profit, got %q", resp.Groups[0].GroupName)
	}
}

// TestGroupProfitToday_ZeroRevenueEmpty 验证今日无营收时返回空列表且合计为 0。
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
	consume := 40.0
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{
				ID:           "site-a",
				Name:         "Upstream A",
				RechargeRate: 2,
				Metrics: upstream.Metrics{
					TodayConsume: upstream.MetricValue{Value: &consume},
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
}
