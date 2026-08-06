package dashboard

import "time"

// MetricsResponse 是 GET /api/dashboard/metrics 返回的实时指标数据。
// 所有金额均以成本口径计价，上游指标已乘以站点的 rechargeRate。
type MetricsResponse struct {
	TodayProfit     float64 `json:"todayProfit"`     // 今日盈利额度：管理员站点今日总实际消费
	SiteBalance     float64 `json:"siteBalance"`     // 站点用户成本侧需兑付余额（含赠送/返利剩余）
	TodayPurchase   float64 `json:"todayPurchase"`   // 兼容字段 = TodayCost（历史趋势列 today_purchase）
	TodayCost       float64 `json:"todayCost"`       // 今日成本：上游今日消耗 × 倍率之和
	TodayInbound    float64 `json:"todayInbound"`    // 今日进货：账本 confirmed topup_* + 预授信当日结算
	NetProfit       float64 `json:"netProfit"`       // 今日净利润：todayProfit - todayCost
	UpstreamBalance float64 `json:"upstreamBalance"` // 上游预存备付（不含预授信账面）
	GroupCount      int     `json:"groupCount"`      // 管理员站点分组总数

	// MVP 扩展字段：双口径与覆盖率
	SiteRevenueBalance       float64  `json:"siteRevenueBalance"`       // 营收侧用户余额（扣赠送等，成本/CNY）
	UpstreamPrepaidBalance   float64  `json:"upstreamPrepaidBalance"`   // = UpstreamBalance
	UpstreamCreditReference  float64  `json:"upstreamCreditReference"`  // 预授信站平台账面参考合计（不进覆盖率）
	CoverageApplicable       bool     `json:"coverageApplicable"`       // 站点成本侧余额>0 时才有参考意义
	CoverageRatio            *float64 `json:"coverageRatio,omitempty"`  // prepaid / siteCost * 100
	// SiteRechargeRate 工作区站点充值倍率（平台用户余额 → 成本/CNY）。
	SiteRechargeRate float64 `json:"siteRechargeRate"`
	// SiteBalancePlatform / SiteRevenueBalancePlatform 为倍率换算前的平台单位，便于前端双币展示。
	SiteBalancePlatform        float64 `json:"siteBalancePlatform"`
	SiteRevenueBalancePlatform float64 `json:"siteRevenueBalancePlatform"`

	// 全站用户流水标记合计（已 × 站点充值倍率 → CNY；Platform 为原始 USD）。
	// SiteGiftTotal / SiteRebateTotal：累计赠送 / 累计返利（标记金额，不是当前剩余拆分）
	// SiteRechargeTotal：累计充值（不含赠送+返利）
	SiteGiftTotal              float64 `json:"siteGiftTotal"`
	SiteRebateTotal            float64 `json:"siteRebateTotal"`
	SiteRechargeTotal          float64 `json:"siteRechargeTotal"`
	SiteGiftTotalPlatform      float64 `json:"siteGiftTotalPlatform"`
	SiteRebateTotalPlatform    float64 `json:"siteRebateTotalPlatform"`
	SiteRechargeTotalPlatform  float64 `json:"siteRechargeTotalPlatform"`
}

// TrendResponse 是 GET /api/dashboard/trends 返回的历史趋势数据。
type TrendResponse struct {
	Points []TrendPoint `json:"points"`
}

// TrendPoint 代表一天的指标快照，用于趋势图渲染。
type TrendPoint struct {
	Date            string  `json:"date"` // 日期，格式 "2006-01-02"
	TodayProfit     float64 `json:"todayProfit"`
	SiteBalance     float64 `json:"siteBalance"`
	TodayPurchase   float64 `json:"todayPurchase"` // = TodayCost，兼容旧前端
	TodayCost       float64 `json:"todayCost"`
	TodayInbound    float64 `json:"todayInbound"`
	NetProfit       float64 `json:"netProfit"`
	UpstreamBalance float64 `json:"upstreamBalance"`
}

// DailySnapshot 是 dashboard_daily_stats 表的行结构。
// 每天至多一行（user_id + admin_account_id + date 唯一），
// 通过 LiveMetrics 调用时的 upsert 和午夜调度器持续更新。
type DailySnapshot struct {
	ID              string
	UserID          string
	AdminAccountID  string
	Date            time.Time
	TodayProfit     float64
	SiteBalance     float64
	TodayPurchase   float64 // 存库列 today_purchase，语义=今日成本
	TodayInbound    float64 // 存库列 today_inbound，语义=今日进货
	NetProfit       float64
	UpstreamBalance float64
	CreatedAt       time.Time
}

// AdminGroupsResponse 是 GET /api/dashboard/groups 返回的管理员站点分组数据。
type AdminGroupsResponse struct {
	Count  int              `json:"count"`
	Groups []AdminGroupItem `json:"groups"`
}

// AdminGroupItem 是管理员站点中单个分组的展示数据。
type AdminGroupItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	Multiplier string `json:"multiplier"`
}

// GroupUsageTodayResponse 是 GET /api/dashboard/group-usage-today 返回的分组今日用量明细。
type GroupUsageTodayResponse struct {
	Date   string                `json:"date"`
	Total  float64               `json:"total"`
	Groups []GroupUsageTodayItem `json:"groups"`
}

// GroupUsageTodayItem 是单个分组的今日使用额度。
type GroupUsageTodayItem struct {
	GroupName   string  `json:"groupName"`
	TodayAmount float64 `json:"todayAmount"`
}

// GroupProfitTodayResponse 是 GET /api/dashboard/group-profit-today 返回的
// 「今日净利润 / 今日利润率」下钻：今天有消耗的自有分组与上游分组利润/利润率。
// 自有分组成本按「1x 归一化用量」分摊今日总成本，使各分组合计与仪表盘净利口径一致。
// 上游分组以 key 今日实际成本为准，营收按售卖/成本倍率估算（与调价映射预算毛利率同公式）。
type GroupProfitTodayResponse struct {
	Date         string                 `json:"date"`
	TotalRevenue float64                `json:"totalRevenue"`
	TotalCost    float64                `json:"totalCost"`
	TotalProfit  float64                `json:"totalProfit"`
	// TotalMargin 为 totalProfit / totalRevenue；无营收时为 0。
	TotalMargin float64 `json:"totalMargin"`
	// Groups 自有（admin）分组，仅 revenue > 0。
	Groups []GroupProfitTodayItem `json:"groups"`
	// UpstreamGroups 已接入且今日有消耗的上游分组。
	UpstreamGroups []UpstreamGroupProfitTodayItem `json:"upstreamGroups"`
	// UpstreamPartial 表示部分上游站点 key 用量采集失败，上游列表可能不完整。
	UpstreamPartial bool `json:"upstreamPartial,omitempty"`
}

// GroupProfitTodayItem 是单个有今日营收的自有分组利润明细。
// ProfitMargin 为利润/营收（0~1 比例，前端再格式化为百分比）。
type GroupProfitTodayItem struct {
	GroupName      string   `json:"groupName"`
	Revenue        float64  `json:"revenue"`
	Cost           float64  `json:"cost"`
	Profit         float64  `json:"profit"`
	ProfitMargin   float64  `json:"profitMargin"`
	SaleMultiplier *float64 `json:"saleMultiplier,omitempty"`
}

// UpstreamGroupProfitTodayItem 是单个今日有消耗的上游分组利润明细。
// Cost 为 key 用量合计（已 × 站点 rechargeRate）；Revenue/Profit 在缺少倍率时按全站利润率回退估算。
type UpstreamGroupProfitTodayItem struct {
	SiteID         string   `json:"siteId"`
	SiteName       string   `json:"siteName"`
	Platform       string   `json:"platform"`
	GroupName      string   `json:"groupName"`
	Cost           float64  `json:"cost"`
	Revenue        float64  `json:"revenue"`
	Profit         float64  `json:"profit"`
	ProfitMargin   float64  `json:"profitMargin"`
	SaleMultiplier *float64 `json:"saleMultiplier,omitempty"`
	CostMultiplier *float64 `json:"costMultiplier,omitempty"`
	// MappedOwnGroups 参与售卖倍率推断的自有分组名（来自调价映射 / 真实对接）。
	MappedOwnGroups []string `json:"mappedOwnGroups,omitempty"`
}

// PricingTargetLink 是仪表盘读取的「自有分组 → 上游分组」映射边（与 my_sites 结构对齐）。
type PricingTargetLink struct {
	OwnGroup  string
	SiteID    string
	GroupName string
}

// UpstreamKeyUsageTodayResponse 是 GET /api/dashboard/upstream-key-usage-today 返回的
// 「今日成本」下钻明细：当前工作区所有上游站点中，今天有消费的 key 列表。
type UpstreamKeyUsageTodayResponse struct {
	Date        string                      `json:"date"`
	Total       float64                     `json:"total"`
	Keys        []UpstreamKeyUsageTodayItem `json:"keys"`
	FailedSites int                         `json:"failedSites,omitempty"`
	TotalSites  int                         `json:"totalSites,omitempty"`
}

// UpstreamKeyUsageTodayItem 是单个 key 的今日消费明细。
// TodayAmount 已乘以所属站点的 rechargeRate，口径与仪表盘「今日成本」卡片一致；RawAmount 为上游平台原始金额。
type UpstreamKeyUsageTodayItem struct {
	SiteID       string  `json:"siteId"`
	SiteName     string  `json:"siteName"`
	Platform     string  `json:"platform"`
	KeyID        string  `json:"keyId"`
	KeyName      string  `json:"keyName"`
	GroupName    string  `json:"groupName"`
	TodayAmount  float64 `json:"todayAmount"`
	RawAmount    float64 `json:"rawAmount"`
	RechargeRate float64 `json:"rechargeRate"`
}

// TodayInboundBreakdownResponse 是 GET /api/dashboard/today-inbound-breakdown 返回的
// 「今日进货」下钻：按上游站点汇总已确认充值进货（成本口径）。
type TodayInboundBreakdownResponse struct {
	Date  string                      `json:"date"`
	Total float64                     `json:"total"`
	Sites []TodayInboundBreakdownItem `json:"sites"`
}

// TodayInboundBreakdownItem 单个上游站点的今日进货汇总。
type TodayInboundBreakdownItem struct {
	SiteID       string  `json:"siteId"`
	SiteName     string  `json:"siteName"`
	Platform     string  `json:"platform"`
	AmountCost   float64 `json:"amountCost"`
	EntryCount   int     `json:"entryCount"`
	RechargeRate float64 `json:"rechargeRate"`
}

// UpstreamBalanceBreakdownResponse 是 GET /api/dashboard/upstream-balance-breakdown 返回的
// 「上游总余额」下钻明细：当前工作区所有上游站点的缓存余额列表。
type UpstreamBalanceBreakdownResponse struct {
	Total float64                        `json:"total"`
	Sites []UpstreamBalanceBreakdownItem `json:"sites"`
}

// UpstreamBalanceBreakdownItem 是单个上游站点的余额明细。
// Balance/RawBalance 为 null 表示该站点余额尚未同步或未配置 rechargeRate。
type UpstreamBalanceBreakdownItem struct {
	SiteID         string   `json:"siteId"`
	SiteName       string   `json:"siteName"`
	Platform       string   `json:"platform"`
	Balance        *float64 `json:"balance"`
	RawBalance     *float64 `json:"rawBalance"`
	RechargeRate   float64  `json:"rechargeRate"`
	LastSyncedAt   *int64   `json:"lastSyncedAt"`
	Status         string   `json:"status"`
	SettlementMode string   `json:"settlementMode,omitempty"`
	// ReserveKind: prepaid | credit_reference | excluded
	ReserveKind string `json:"reserveKind,omitempty"`
}

// BalanceFilterConfig 是用户自定义的站点用户余额筛选条件，持久化在 dashboard_balance_filter 表中。
// 每个 (user_id, admin_account_id) 最多一行配置，控制 LiveMetrics 计算 siteBalance 时的过滤行为。
//
// 双口径：
//   - 成本侧（siteBalance）：含赠送剩余，用于覆盖率；金额 = 平台余额 × SiteRechargeRate
//   - 营收侧（siteRevenueBalance）：扣掉 gift/rebate 标记与 userGiftAmounts
type BalanceFilterConfig struct {
	UserID          string             `json:"-"`
	AdminAccountID  string             `json:"-"`
	ExcludeAdmin    bool               `json:"excludeAdmin"`    // 是否排除 admin 角色用户（默认 true）
	ExcludeBalances []float64          `json:"excludeBalances"` // 需要排除的精确余额值列表
	ExcludeUserIDs  []string           `json:"excludeUserIds"`  // 整户排除（成本与营收都不计）
	UserGiftAmounts map[string]float64 `json:"userGiftAmounts"` // 用户ID → 赠送不计营收额度
	// SiteRechargeRate 工作区站点充值倍率：平台余额单位 → 成本/CNY 口径（默认 1）。
	SiteRechargeRate float64 `json:"siteRechargeRate"`
}
