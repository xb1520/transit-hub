package dashboard

import (
	"context"
	"math"
	"strings"
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

// TestGroupProfitToday_AppliesSiteRechargeRateToOwnRevenue 验证：
// 自有营收 = 平台分组消费 × 管理站充值倍率，再与上游成本（已是成本/CNY）相减。
// 双币种展示时 CNY 用本字段，USD = CNY / rate（或 revenuePlatform）。
func TestGroupProfitToday_AppliesSiteRechargeRateToOwnRevenue(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups: []upstream.GroupInfo{
			{Name: "GPT Pro 混合", Multiplier: &m},
		},
		dailyStats: []upstream.GroupDailyStat{
			// 平台单位 100；倍率 0.08 → 营收 CNY 8
			{GroupName: "GPT Pro 混合", TodayActualCost: 100},
		},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{{ID: "site-a", Name: "ruoli", RechargeRate: 1}},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			// 上游成本已是成本/CNY
			{SiteID: "site-a", SiteName: "ruoli", KeyID: "k1", GroupName: "gpt-pro", TodayAmount: 3, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.siteRechargeRateOverride = 0.08
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "GPT Pro 混合", SiteID: "site-a", GroupName: "gpt-pro"},
		},
	})

	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	if math.Abs(resp.SiteRechargeRate-0.08) > 1e-12 {
		t.Fatalf("siteRechargeRate = %v, want 0.08", resp.SiteRechargeRate)
	}
	// 营收 100×0.08=8，成本 3，利润 5，利润率 5/8=62.5%
	if math.Abs(resp.TotalRevenue-8) > 1e-9 {
		t.Fatalf("totalRevenue = %v, want 8 (100×0.08)", resp.TotalRevenue)
	}
	if math.Abs(resp.TotalCost-3) > 1e-9 {
		t.Fatalf("totalCost = %v, want 3", resp.TotalCost)
	}
	if math.Abs(resp.TotalProfit-5) > 1e-9 {
		t.Fatalf("totalProfit = %v, want 5", resp.TotalProfit)
	}
	if math.Abs(resp.TotalMargin-0.625) > 1e-9 {
		t.Fatalf("totalMargin = %v, want 0.625", resp.TotalMargin)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("expected 1 own group, got %+v", resp.Groups)
	}
	g := resp.Groups[0]
	if math.Abs(g.Revenue-8) > 1e-9 || math.Abs(g.RevenuePlatform-100) > 1e-9 {
		t.Fatalf("revenue/platform = %v/%v, want 8/100", g.Revenue, g.RevenuePlatform)
	}
	if math.Abs(g.Cost-3) > 1e-9 || math.Abs(g.Profit-5) > 1e-9 {
		t.Fatalf("cost/profit = %v/%v, want 3/5", g.Cost, g.Profit)
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

// TestGroupProfitToday_UpstreamNestedUnderOwnWithRealRevenueShare 验证：
// 1) 上游嵌套在自有分组 Upstreams 下
// 2) 子行营收 = 母行真实营收 × (子成本/母成本)，非倍率估算
// 3) 子行利润之和 = 母行利润
func TestGroupProfitToday_UpstreamNestedUnderOwnWithRealRevenueShare(t *testing.T) {
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

	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{ID: "site-a", Name: "Upstream A", RechargeRate: 2},
		},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{
				SiteID: "site-a", SiteName: "Upstream A", Platform: upstream.PlatformSub2API,
				KeyID: "k1", KeyName: "key-1", GroupName: "cheap",
				TodayAmount: 40, RawAmount: 20, RechargeRate: 2,
			},
			{
				SiteID: "site-a", SiteName: "Upstream A", Platform: upstream.PlatformSub2API,
				KeyID: "k2", KeyName: "key-2", GroupName: "cheap",
				TodayAmount: 20, RawAmount: 10, RechargeRate: 2,
			},
			// 零消耗不进入列表
			{
				SiteID: "site-a", SiteName: "Upstream A", KeyID: "k3", GroupName: "idle", TodayAmount: 0, RechargeRate: 2,
			},
			// 未映射上游：进 unmapped
			{
				SiteID: "site-a", SiteName: "Upstream A", Platform: upstream.PlatformSub2API,
				KeyID: "k4", GroupName: "orphan", TodayAmount: 10, RechargeRate: 2,
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
	if len(resp.Groups) != 1 {
		t.Fatalf("expected 1 own group, got %+v", resp.Groups)
	}
	own := resp.Groups[0]
	// 母行：营收 100，成本 60，利润 40
	if math.Abs(own.Revenue-100) > 1e-9 || math.Abs(own.Cost-60) > 1e-9 || math.Abs(own.Profit-40) > 1e-9 {
		t.Fatalf("own vip = %+v, want rev100 cost60 profit40", own)
	}
	if len(own.Upstreams) != 1 {
		t.Fatalf("expected 1 nested upstream, got %+v", own.Upstreams)
	}
	child := own.Upstreams[0]
	if child.GroupName != "cheap" || child.SiteID != "site-a" {
		t.Fatalf("unexpected child: %+v", child)
	}
	// 子行：成本 60，营收 = 100×(60/60)=100，利润 40（与母行一致）
	if math.Abs(child.Cost-60) > 1e-9 || math.Abs(child.Revenue-100) > 1e-9 || math.Abs(child.Profit-40) > 1e-9 {
		t.Fatalf("child = %+v, want cost60 rev100 profit40", child)
	}
	// 扁平列表含 cheap + orphan
	if len(resp.UpstreamGroups) != 2 {
		t.Fatalf("expected 2 flat upstreams, got %+v", resp.UpstreamGroups)
	}
	if len(resp.UnmappedUpstreams) != 1 || resp.UnmappedUpstreams[0].GroupName != "orphan" {
		t.Fatalf("unmapped = %+v, want [orphan]", resp.UnmappedUpstreams)
	}
	if math.Abs(resp.UnmappedUpstreams[0].Cost-10) > 1e-9 || resp.UnmappedUpstreams[0].Revenue != 0 {
		t.Fatalf("orphan should be cost10 rev0, got %+v", resp.UnmappedUpstreams[0])
	}
}

// TestGroupProfitToday_RemapKeyUsageGroupByKeyID 上游 key 用量组名与对接组名不一致时（特惠 vs 正价），
// 按 upstreamKeyId 归并成本到对接组名，避免成本进未映射、营收挂空壳。
func TestGroupProfitToday_RemapKeyUsageGroupByKeyID(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &m}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 20}},
		batchUsage: map[string]upstream.Sub2APIBatchUserUsage{
			"67": {UserID: "67", TodayActualCost: 10},
		},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{{ID: "paidaxing", Name: "派大星", RechargeRate: 1}},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			// 上游接口组名是「特惠」，对接记的是「正价」
			{
				SiteID: "paidaxing", SiteName: "派大星", KeyID: "1936",
				KeyName: "Conduit-派大星-Codex-Pro 正价Pro渠道A",
				GroupName: "Codex-Pro 特惠Pro渠道A", TodayAmount: 5.16, RechargeRate: 1,
			},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "GPT Pro 混合", SiteID: "paidaxing", GroupName: "Codex-Pro 正价Pro渠道A"},
		},
	})
	service.SetRealConnectionSource(&fakeRealConnections{
		links: []RealConnectionLink{
			{
				SiteID: "paidaxing", UpstreamGroup: "Codex-Pro 正价Pro渠道A", UpstreamKeyID: "1936",
				AdminAccountID: "67", AdminPlatform: "sub2api", OwnGroupNames: []string{"GPT Pro 混合"},
			},
		},
	})
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	// 未映射不应再有「特惠」
	for _, u := range resp.UnmappedUpstreams {
		if strings.Contains(u.GroupName, "特惠") {
			t.Fatalf("特惠 should be remapped, still unmapped: %+v", u)
		}
	}
	own := resp.Groups[0]
	if math.Abs(own.Cost-5.16) > 1e-9 {
		t.Fatalf("own cost = %v, want 5.16 (remapped key cost)", own.Cost)
	}
	found := false
	for _, u := range own.Upstreams {
		if u.GroupName == "Codex-Pro 正价Pro渠道A" {
			found = true
			if math.Abs(u.Cost-5.16) > 1e-9 {
				t.Fatalf("正价 cost = %v, want 5.16", u.Cost)
			}
		}
		if strings.Contains(u.GroupName, "特惠") {
			t.Fatalf("child should not use 特惠 name: %s", u.GroupName)
		}
	}
	if !found {
		t.Fatalf("expected child 正价Pro渠道A, got %+v", own.Upstreams)
	}
}

// TestGroupProfitToday_IncludesProbeCostInCost 探测消耗并入自有与上游成本。
func TestGroupProfitToday_IncludesProbeCostInCost(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &m}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 20}},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{{ID: "ruoli", Name: "ruoli", RechargeRate: 1}},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "ruoli", SiteName: "ruoli", GroupName: "gpt-pro", TodayAmount: 10, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "gpt-pro"}},
	})
	service.SetRealConnectionSource(&fakeRealConnections{
		links: []RealConnectionLink{
			{
				ID: "conn-1", SiteID: "ruoli", UpstreamGroup: "gpt-pro", AdminAccountID: "acc-9",
				AdminPlatform: "sub2api", OwnGroupNames: []string{"GPT Pro 混合"},
			},
		},
	})
	service.SetProbeCostSource(&fakeProbeCosts{
		entries: []ProbeCostEntry{
			// targetId 形态：platform:ws:accountID
			{ConnectionID: "sub2api:account-1:acc-9", OwnGroupName: "GPT Pro 混合", CostCNY: 1.5},
		},
	})
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	own := resp.Groups[0]
	// 成本 = key 10 + 探测 1.5
	if math.Abs(own.Cost-11.5) > 1e-9 || math.Abs(own.ProbeCost-1.5) > 1e-9 {
		t.Fatalf("own cost/probe = %v/%v, want 11.5/1.5", own.Cost, own.ProbeCost)
	}
	if math.Abs(own.Profit-(20-11.5)) > 1e-9 {
		t.Fatalf("own profit = %v, want 8.5", own.Profit)
	}
	child := own.Upstreams[0]
	if math.Abs(child.Cost-11.5) > 1e-9 || math.Abs(child.ProbeCost-1.5) > 1e-9 {
		t.Fatalf("child cost/probe = %v/%v, want 11.5/1.5", child.Cost, child.ProbeCost)
	}
}

type fakeProbeCosts struct {
	entries []ProbeCostEntry
}

func (f *fakeProbeCosts) ListTodayProbeCosts(ctx context.Context, userID string) ([]ProbeCostEntry, error) {
	return f.entries, nil
}

// TestGroupProfitToday_BudgetMarginDiffersFromAllocatedActual 成本分摊时实际利润率=母行，
// 但预算利润率仍按售卖/成本倍率独立计算（对照 ruoli 类「预算 vs 实际」差）。
func TestGroupProfitToday_BudgetMarginDiffersFromAllocatedActual(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	sale := 1.0
	upMult := 0.75 // costMult = 0.75 * 1 = 0.75 → budget margin = 25%
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &sale}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 100}},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{
				ID: "ruoli", Name: "ruoli", RechargeRate: 1,
				Metrics: upstream.Metrics{Groups: []upstream.GroupInfo{{Name: "gpt-pro", Multiplier: &upMult}}},
			},
		},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "ruoli", SiteName: "ruoli", GroupName: "gpt-pro", TodayAmount: 80, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "gpt-pro"}},
	})
	// 无真实对接 → 成本分摊
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	child := resp.Groups[0].Upstreams[0]
	// 实际：100 营收 80 成本 → 利润率 20%
	if math.Abs(child.ProfitMargin-0.2) > 1e-9 {
		t.Fatalf("actual margin = %v, want 0.2", child.ProfitMargin)
	}
	if child.RevenueSource != "allocated" {
		t.Fatalf("source = %q, want allocated", child.RevenueSource)
	}
	// 预算：(1-0.75)/1 = 25%
	if child.BudgetMargin == nil || math.Abs(*child.BudgetMargin-0.25) > 1e-9 {
		t.Fatalf("budget margin = %v, want 0.25", child.BudgetMargin)
	}
}

// TestGroupProfitToday_UpstreamRevenueFromNewAPIChannel 管理站 new-api 时按 channel 拉今日消费。
func TestGroupProfitToday_UpstreamRevenueFromNewAPIChannel(t *testing.T) {
	store := newFakeSessionStore()
	sess := upstream.Session{Platform: upstream.PlatformNewAPI, BaseURL: "https://admin.example", Cookie: "c", UserID: "1", QuotaPerUnit: 500000}
	store.set("user-1", "account-1", AdminSession{Session: sess})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &m}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 20}},
		batchUsage: map[string]upstream.Sub2APIBatchUserUsage{
			"ch-42": {UserID: "ch-42", TodayActualCost: 14.99}, // channel 今日消费
		},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{{ID: "ruoli", Name: "ruoli", RechargeRate: 1}},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "ruoli", SiteName: "ruoli", Platform: upstream.PlatformNewAPI, GroupName: "gpt-pro", TodayAmount: 13.74, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "gpt-pro"}},
	})
	service.SetRealConnectionSource(&fakeRealConnections{
		links: []RealConnectionLink{
			{
				SiteID: "ruoli", UpstreamGroup: "gpt-pro", AdminAccountID: "ch-42",
				AdminPlatform: string(upstream.PlatformNewAPI), OwnGroupNames: []string{"GPT Pro 混合"},
			},
		},
	})
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	child := resp.Groups[0].Upstreams[0]
	if child.RevenueSource != "account" {
		t.Fatalf("source = %q, want account", child.RevenueSource)
	}
	if math.Abs(child.Revenue-14.99) > 1e-9 || math.Abs(child.Profit-(14.99-13.74)) > 1e-9 {
		t.Fatalf("child = %+v, want rev14.99 profit1.25", child)
	}
	// 实际利润率 ≈ 1.25/14.99 ≈ 8.3%，不再被母行强行拉齐
	wantMargin := (14.99 - 13.74) / 14.99
	if math.Abs(child.ProfitMargin-wantMargin) > 1e-9 {
		t.Fatalf("margin = %v, want %v", child.ProfitMargin, wantMargin)
	}
}

// TestGroupProfitToday_UpstreamRevenueFromAdminAccount 子行营收优先用真实对接管理站账号今日消费。
func TestGroupProfitToday_UpstreamRevenueFromAdminAccount(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &m}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 15.55}},
		batchUsage: map[string]upstream.Sub2APIBatchUserUsage{
			// 管理站账号 1001 = 真实对接 gpt-pro 的转发账号（用户侧营收）
			"1001": {UserID: "1001", TodayActualCost: 14.0},
		},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{{ID: "ruoli", Name: "ruoli", RechargeRate: 1}},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "ruoli", SiteName: "ruoli", GroupName: "gpt-pro", TodayAmount: 11.90, RechargeRate: 1},
			{SiteID: "ruoli", SiteName: "ruoli", GroupName: "extra", TodayAmount: 2.63, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "gpt-pro"},
			{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "extra"},
		},
	})
	service.SetRealConnectionSource(&fakeRealConnections{
		links: []RealConnectionLink{
			{
				SiteID: "ruoli", UpstreamGroup: "gpt-pro", AdminAccountID: "1001",
				AdminPlatform: string(upstream.PlatformSub2API), OwnGroupNames: []string{"GPT Pro 混合"},
			},
		},
	})

	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	own := resp.Groups[0]
	byName := map[string]UpstreamGroupProfitTodayItem{}
	for _, c := range own.Upstreams {
		byName[c.GroupName] = c
	}
	gpt := byName["gpt-pro"]
	// 真实账号营收 14，成本 11.90，利润 2.10
	if math.Abs(gpt.Revenue-14) > 1e-9 || math.Abs(gpt.Profit-(14-11.90)) > 1e-9 {
		t.Fatalf("gpt-pro = %+v, want rev14 profit2.10", gpt)
	}
	if gpt.RevenueSource != "account" {
		t.Fatalf("gpt-pro revenueSource = %q, want account", gpt.RevenueSource)
	}
	// extra 无对接账号：分摊剩余母行营收 (15.55-14)=1.55
	extra := byName["extra"]
	if extra.RevenueSource != "allocated" {
		t.Fatalf("extra revenueSource = %q, want allocated", extra.RevenueSource)
	}
	if math.Abs(extra.Revenue-1.55) > 1e-9 {
		t.Fatalf("extra revenue = %v, want 1.55 (parent remainder)", extra.Revenue)
	}
}

// TestGroupProfitToday_ConnectedZeroUsageNotAllocated 有真实对接但今日用量 0：
// 营收记 0（账号真实），不吃母行剩余分摊（星辰幻语类假利润）。
func TestGroupProfitToday_ConnectedZeroUsageNotAllocated(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &m}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 100}},
		batchUsage: map[string]upstream.Sub2APIBatchUserUsage{
			"50": {UserID: "50", TodayActualCost: 80}, // ruoli 有量
			"28": {UserID: "28", TodayActualCost: 0},  // 星辰幻语 0
		},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{ID: "ruoli", Name: "ruoli", RechargeRate: 1},
			{ID: "xc", Name: "星辰幻语", RechargeRate: 1},
		},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "ruoli", SiteName: "ruoli", GroupName: "gpt-pro", TodayAmount: 70, RechargeRate: 1},
			{SiteID: "xc", SiteName: "星辰幻语", GroupName: "openai-pro-下游分组", TodayAmount: 0.5, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "gpt-pro"},
			{OwnGroup: "GPT Pro 混合", SiteID: "xc", GroupName: "openai-pro-下游分组"},
		},
	})
	service.SetRealConnectionSource(&fakeRealConnections{
		links: []RealConnectionLink{
			{SiteID: "ruoli", UpstreamGroup: "gpt-pro", AdminAccountID: "50", AdminPlatform: "sub2api", OwnGroupNames: []string{"GPT Pro 混合"}},
			{SiteID: "xc", UpstreamGroup: "openai-pro-下游分组", AdminAccountID: "28", AdminPlatform: "sub2api", OwnGroupNames: []string{"GPT Pro 混合"}},
		},
	})
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	byName := map[string]UpstreamGroupProfitTodayItem{}
	for _, c := range resp.Groups[0].Upstreams {
		byName[c.GroupName] = c
	}
	xc := byName["openai-pro-下游分组"]
	if xc.RevenueSource != "account" {
		t.Fatalf("星辰幻语 source=%q, want account", xc.RevenueSource)
	}
	if xc.Revenue != 0 {
		t.Fatalf("星辰幻语 rev=%v, want 0 (zero usage, not allocated)", xc.Revenue)
	}
	// 利润 = 0 - 0.5 = -0.5，不应出现 +40 虚高
	if xc.Profit >= 0 {
		t.Fatalf("星辰幻语 profit=%v, want negative (cost only)", xc.Profit)
	}
	gpt := byName["gpt-pro"]
	if math.Abs(gpt.Revenue-80) > 1e-9 {
		t.Fatalf("gpt-pro rev=%v, want 80", gpt.Revenue)
	}
}

type fakeRealConnections struct {
	links []RealConnectionLink
}

func (f *fakeRealConnections) ListRealConnectionLinks(ctx context.Context, userID string) ([]RealConnectionLink, error) {
	return f.links, nil
}

// TestGroupProfitToday_ChildRevenueSharesParentByCost 两个上游映射同一自有时按成本占比分摊真实营收。
func TestGroupProfitToday_ChildRevenueSharesParentByCost(t *testing.T) {
	store := newFakeSessionStore()
	store.set("user-1", "account-1", AdminSession{Session: authenticatedSession()})
	accounts := &fakeAdminAccounts{current: map[string]string{"user-1": "account-1"}}
	m := 1.0
	platform := &fakePlatformClient{
		groups:     []upstream.GroupInfo{{Name: "GPT Pro 混合", Multiplier: &m}},
		dailyStats: []upstream.GroupDailyStat{{GroupName: "GPT Pro 混合", TodayActualCost: 15.55}},
	}
	upstreams := &fakeUpstreamLister{
		listItems: []upstream.Response{
			{ID: "ruoli", Name: "ruoli", RechargeRate: 1},
			{ID: "other", Name: "other", RechargeRate: 1},
		},
		keyUsageItems: []upstream.KeyUsageTodayItem{
			{SiteID: "ruoli", SiteName: "ruoli", GroupName: "gpt-pro", TodayAmount: 11.90, RechargeRate: 1},
			{SiteID: "other", SiteName: "other", GroupName: "extra", TodayAmount: 2.63, RechargeRate: 1},
		},
	}
	service := NewMetricsService(store, platform, upstreams, nil, accounts)
	service.SetPricingMappingSource(&fakePricingMappings{
		links: []PricingTargetLink{
			{OwnGroup: "GPT Pro 混合", SiteID: "ruoli", GroupName: "gpt-pro"},
			{OwnGroup: "GPT Pro 混合", SiteID: "other", GroupName: "extra"},
		},
	})
	resp, err := service.GroupProfitToday(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("GroupProfitToday failed: %v", err)
	}
	own := resp.Groups[0]
	// 母：rev 15.55, cost 14.53, profit 1.02
	if math.Abs(own.Cost-14.53) > 1e-9 || math.Abs(own.Profit-(15.55-14.53)) > 1e-9 {
		t.Fatalf("own = %+v", own)
	}
	if len(own.Upstreams) != 2 {
		t.Fatalf("want 2 children, got %+v", own.Upstreams)
	}
	byName := map[string]UpstreamGroupProfitTodayItem{}
	var childProfitSum, childRevSum float64
	for _, c := range own.Upstreams {
		byName[c.GroupName] = c
		childProfitSum += c.Profit
		childRevSum += c.Revenue
	}
	// gpt-pro: rev = 15.55 * (11.90/14.53) ≈ 12.737, profit ≈ 0.837
	gpt := byName["gpt-pro"]
	wantRev := 15.55 * (11.90 / 14.53)
	wantProfit := wantRev - 11.90
	if math.Abs(gpt.Revenue-wantRev) > 1e-9 || math.Abs(gpt.Profit-wantProfit) > 1e-9 {
		t.Fatalf("gpt-pro = %+v, want rev %v profit %v", gpt, wantRev, wantProfit)
	}
	// 子行之和 = 母行
	if math.Abs(childRevSum-own.Revenue) > 1e-9 || math.Abs(childProfitSum-own.Profit) > 1e-9 {
		t.Fatalf("children sum rev=%v profit=%v, own rev=%v profit=%v", childRevSum, childProfitSum, own.Revenue, own.Profit)
	}
}
