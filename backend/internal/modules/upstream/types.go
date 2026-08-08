package upstream

import (
	"context"
	"strings"
	"time"
)

type Platform string

const (
	PlatformAuto    Platform = "auto"
	PlatformNewAPI  Platform = "newapi"
	PlatformSub2API Platform = "sub2api"
)

type Status string

const (
	StatusConnecting Status = "connecting"
	StatusSyncing    Status = "syncing"
	StatusConnected  Status = "connected"
	StatusError      Status = "error"
)

const (
	ErrorNotFound        = "admin.upstream.errors.notFound"
	ErrorInvalidURL      = "admin.upstream.errors.invalidUrl"
	ErrorAuth            = "admin.upstream.errors.auth"
	ErrorNetwork         = "admin.upstream.errors.network"
	ErrorRequest         = "admin.upstream.errors.request"
	ErrorInvalidResponse = "admin.upstream.errors.invalidResponse"
	ErrorUnknown         = "admin.upstream.errors.unknown"
	// ErrorSub2APIBulkUpdateUnsupported 表示当前 Sub2API 站点没有字段级批量更新能力。
	// 调度优先级/状态更新遇到该能力缺失时必须要求升级，绝不回退到整对象回写。
	ErrorSub2APIBulkUpdateUnsupported = "admin.upstream.errors.sub2APIBulkUpdateUnsupported"
)

// SSE 同步流事件类型。
const (
	SyncEventSyncing  = "syncing"
	SyncEventDone     = "done"
	SyncEventError    = "error"
	SyncEventComplete = "complete"
)

// SyncEvent 是 SSE 同步流中每个 data: 行的 JSON 载荷。
// 前端通过 event 字段判断当前阶段并更新对应站点卡片的进度。
type SyncEvent struct {
	Event    string    `json:"event"`
	SiteID   string    `json:"siteId"`
	Attempt  int       `json:"attempt,omitempty"`
	MaxRetry int       `json:"maxRetry,omitempty"`
	Site     *Response `json:"site,omitempty"`
	ErrorKey string    `json:"errorKey,omitempty"`
}

// SyncEventCallback 是 SSE 事件的推送回调，由 handler 注入，
// 负责将事件序列化并写入 ResponseWriter。
type SyncEventCallback func(SyncEvent)

type AuthMode string

const (
	AuthModePassword AuthMode = "password"
	AuthModeToken    AuthMode = "token"
	AuthModeUserKey  AuthMode = "user_key"
)

type MetricValue struct {
	Value   *float64 `json:"value"`
	Display string   `json:"display"`
}

type GroupInfo struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Platform          *string  `json:"platform"`
	Multiplier        *float64 `json:"multiplier"`
	MultiplierDisplay string   `json:"multiplierDisplay"`
	MultiplierMode    string   `json:"multiplierMode,omitempty"`
	// 以下字段为 sub2api 专属倍率合并规则新增的向后兼容字段：/groups/available 默认倍率
	// 与 /groups/rates 专属倍率覆盖后，Multiplier 始终表示最终生效倍率；这些字段仅供前端
	// 展示"默认倍率 -> 专属倍率"提示，不参与业务计算。旧数据缺少这些字段时 omitempty 生效，
	// 前端按无专属倍率处理。
	DefaultMultiplier          *float64 `json:"defaultMultiplier,omitempty"`
	DefaultMultiplierDisplay   string   `json:"defaultMultiplierDisplay,omitempty"`
	DedicatedMultiplier        *float64 `json:"dedicatedMultiplier,omitempty"`
	DedicatedMultiplierDisplay string   `json:"dedicatedMultiplierDisplay,omitempty"`
	HasDedicatedMultiplier     bool     `json:"hasDedicatedMultiplier"`
}

// SnapshotGroup and SnapshotWriter keep upstream decoupled from the group_rates
// module while still allowing successful metric refreshes to publish multiplier
// history. Any implementation can be injected by the HTTP server assembly layer.
type SnapshotGroup struct {
	ID         string
	Name       string
	Platform   *string
	Multiplier *float64
}

type SnapshotWriter interface {
	SaveSiteSnapshot(ctx context.Context, userID string, adminAccountID string, siteID string, siteName string, sitePlatform Platform, groups []SnapshotGroup) error
}

type Metrics struct {
	Balance         MetricValue `json:"balance"`
	TodayConsume    MetricValue `json:"todayConsume"`
	HistoryRecharge MetricValue `json:"historyRecharge"`
	// LifetimeConsume 是平台累计实际消耗（上游原始单位，未乘 rechargeRate）。
	// 预授信「累计消耗(成本)」= LifetimeConsume × rechargeRate。
	LifetimeConsume MetricValue        `json:"lifetimeConsume"`
	Group           GroupInfo          `json:"group"`
	Groups          []GroupInfo        `json:"groups"`
	Subscriptions   []SubscriptionInfo `json:"subscriptions,omitempty"`
}

// SubscriptionInfo 是上游订阅资产（与钱包余额分开展示）。
type SubscriptionInfo struct {
	ID               string   `json:"id"`
	GroupID          string   `json:"groupId"`
	GroupName        string   `json:"groupName"`
	Status           string   `json:"status"`
	StartsAt         string   `json:"startsAt,omitempty"`
	ExpiresAt        string   `json:"expiresAt,omitempty"`
	RateMultiplier   *float64 `json:"rateMultiplier,omitempty"`
	DailyLimitUSD    *float64 `json:"dailyLimitUsd,omitempty"`
	WeeklyLimitUSD   *float64 `json:"weeklyLimitUsd,omitempty"`
	MonthlyLimitUSD  *float64 `json:"monthlyLimitUsd,omitempty"`
	DailyUsageUSD    float64  `json:"dailyUsageUsd"`
	WeeklyUsageUSD   float64  `json:"weeklyUsageUsd"`
	MonthlyUsageUSD  float64  `json:"monthlyUsageUsd"`
	DailyRemaining   *float64 `json:"dailyRemainingUsd,omitempty"`
	WeeklyRemaining  *float64 `json:"weeklyRemainingUsd,omitempty"`
	MonthlyRemaining *float64 `json:"monthlyRemainingUsd,omitempty"`
	// TodayMaxConsumableUSD 今日在日/周/月限额约束下最多还能消耗的额度（取剩余的最小值）。
	TodayMaxConsumableUSD *float64 `json:"todayMaxConsumableUsd,omitempty"`
}

type CreateRequest struct {
	Name         string   `json:"name"`
	SiteURL      string   `json:"siteUrl"`
	Platform     Platform `json:"platform"`
	AuthMode     AuthMode `json:"authMode"`
	Account      string   `json:"account"`
	Password     string   `json:"password"`
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	TokenType    string   `json:"tokenType"`
	UserID       string   `json:"userId"`
	Remark       string   `json:"remark"`
	RechargeRate float64  `json:"rechargeRate"`
}

type UpdateRequest struct {
	Name         string   `json:"name"`
	SiteURL      string   `json:"siteUrl"`
	Platform     Platform `json:"platform"`
	AuthMode     AuthMode `json:"authMode"`
	Account      string   `json:"account"`
	Password     string   `json:"password"`
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	TokenType    string   `json:"tokenType"`
	UserID       string   `json:"userId"`
	Remark       string   `json:"remark"`
	RechargeRate float64  `json:"rechargeRate"`
}

// 上游结算/备付模式。
const (
	SettlementModePrepaidWallet = "prepaid_wallet" // 真预存，余额可计入预存备付
	SettlementModeCreditLine    = "credit_line"    // 预授信，先用后结；账面余额仅参考
)

// SiteSettings 站点级配置：预警覆盖 + 结算模式（预存/预授信）等。
// 指针/空字符串表示使用默认值。
type SiteSettings struct {
	BalanceThreshold *float64 `json:"balanceThreshold"`
	// SettlementMode: prepaid_wallet（默认）| credit_line
	SettlementMode string `json:"settlementMode,omitempty"`
	// CreditLimit 预授信额度，默认按成本口径（已含充值倍率后的单位）。
	CreditLimit *float64 `json:"creditLimit,omitempty"`
	// SettlementCurrency 结算展示币种，默认 CNY。
	SettlementCurrency string `json:"settlementCurrency,omitempty"`
}

// NormalizeSettlementMode 返回规范化后的结算模式。
func NormalizeSettlementMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case SettlementModeCreditLine:
		return SettlementModeCreditLine
	default:
		return SettlementModePrepaidWallet
	}
}

// IsCreditLine 是否为预授信结算模式。
func (s SiteSettings) IsCreditLine() bool {
	return NormalizeSettlementMode(s.SettlementMode) == SettlementModeCreditLine
}

// CountsAsPrepaidReserve 是否计入「上游预存备付」（覆盖率分子）。
// 预授信站点的平台账面不进入预存合计。
func (s SiteSettings) CountsAsPrepaidReserve() bool {
	return !s.IsCreditLine()
}

type Site struct {
	ID                string       `json:"id"`
	UserID            string       `json:"-"`
	AdminAccountID    string       `json:"-"`
	Name              string       `json:"name"`
	BaseURL           string       `json:"baseUrl"`
	Platform          Platform     `json:"platform"`
	RequestedPlatform Platform     `json:"requestedPlatform"`
	Account           string       `json:"account"`
	Remark            string       `json:"remark"`
	RechargeRate      float64      `json:"rechargeRate"`
	Status            Status       `json:"status"`
	ErrorKey          *string      `json:"errorKey"`
	Metrics           Metrics      `json:"metrics"`
	Settings          SiteSettings `json:"settings"`
	LastSyncedAt      *int64       `json:"lastSyncedAt"`
	Session           *Session     `json:"-"`
}

type Response struct {
	ID                string       `json:"id"`
	UserID            string       `json:"-"`
	AdminAccountID    string       `json:"-"`
	Name              string       `json:"name"`
	BaseURL           string       `json:"baseUrl"`
	Platform          Platform     `json:"platform"`
	RequestedPlatform Platform     `json:"requestedPlatform"`
	Account           string       `json:"account"`
	Remark            string       `json:"remark"`
	RechargeRate      float64      `json:"rechargeRate"`
	Status            Status       `json:"status"`
	ErrorKey          *string      `json:"errorKey"`
	Metrics           Metrics      `json:"metrics"`
	Settings          SiteSettings `json:"settings"`
	LastSyncedAt      *int64       `json:"lastSyncedAt"`
	// Settlement 预授信/结算汇总；列表接口在有 settlementRepo 时填充。
	Settlement *SettlementSummary `json:"settlement,omitempty"`
}

type Session struct {
	Platform    Platform
	BaseURL     string
	Cookie      string
	UserID      string
	AccessToken string
	// AdminAPIKey 是 Sub2API 管理路由使用的 Admin API Key，通过 x-api-key 发送。
	// 它与用户 JWT/AccessToken 分开保存，避免被误发到普通用户路由。
	AdminAPIKey  string `json:",omitempty"`
	RefreshToken string
	TokenType    string
	// ExpiresAt 是 access token 过期的毫秒时间戳，来自登录/刷新响应的 expires_in。
	// 临期时由 refreshIfNeeded 用 refresh token 自动换新（refresh token 本身无过期时间）。
	ExpiresAt *int64
	// QuotaPerUnit 是 new-api 的 quota 换算单位（来自 /api/status 的 quota_per_unit 字段）。
	// sub2api 不使用此字段。为 0 时回退到默认值 500000。
	QuotaPerUnit float64
}

// IsAuthenticated 按平台判断会话是否有效（已持有登录凭证）。
// sub2api 支持用户 AccessToken 或 Admin API Key；new-api 需要 UserID，
// 并支持 Cookie 会话或“个人设置 -> 系统访问令牌”生成的 Access Token。
func (s Session) IsAuthenticated() bool {
	switch s.Platform {
	case PlatformNewAPI:
		return strings.TrimSpace(s.UserID) != "" &&
			(strings.TrimSpace(s.Cookie) != "" || strings.TrimSpace(s.AccessToken) != "")
	case PlatformSub2API:
		return strings.TrimSpace(s.AccessToken) != "" || strings.TrimSpace(s.AdminAPIKey) != ""
	default:
		return strings.TrimSpace(s.AccessToken) != "" || strings.TrimSpace(s.AdminAPIKey) != "" ||
			(strings.TrimSpace(s.Cookie) != "" && strings.TrimSpace(s.UserID) != "")
	}
}

// Sub2APIKeyItem 表示从上游 Sub2API 站点获取的单个 API Key 信息。
// 用于手动绑定时展示 key 列表供用户选择。
type Sub2APIKeyItem struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"`
	Status    string `json:"status"`
}

type LoginResult struct {
	Platform Platform
	Session  Session
	Metrics  Metrics
}

type GroupDailyStat struct {
	GroupName       string  `json:"groupName"`
	TodayActualCost float64 `json:"todayActualCost"`
}

// BalanceFilter 控制统计站点用户余额时的过滤条件。
// 由仪表盘模块创建，传递给 PlatformService 在分页遍历用户时应用。
//
// 双口径：
//   - CostBalance：成本/兑付压力（含赠送与返利剩余，覆盖率用这个）
//   - RevenueBalance：营收相关负债（扣掉赠送额度等不计营收部分）
type BalanceFilter struct {
	ExcludeAdmin    bool      // 是否排除 admin 角色用户
	ExcludeBalances []float64 // 需要排除的精确余额值（如 0、0.1、1 等）
	ExcludeUserIDs  []string  // 整户排除（测试号等）：成本与营收都不计
	// UserGiftAmounts: 用户 ID → 赠送不计营收额度。只从营收侧扣除，成本侧保留。
	UserGiftAmounts map[string]float64
}

// AdminSiteBalance 站点用户余额汇总（双口径）。
// Balance 与 CostBalance 相同，保留 Balance 字段兼容旧调用方。
type AdminSiteBalance struct {
	Balance        float64 `json:"balance"`        // = CostBalance，兼容旧字段
	CostBalance    float64 `json:"costBalance"`    // 成本/兑付侧
	RevenueBalance float64 `json:"revenueBalance"` // 营收侧
}

// SettlementRecord 上游站点手动结算流水。
type SettlementRecord struct {
	ID             string    `json:"id"`
	UserID         string    `json:"-"`
	AdminAccountID string    `json:"-"`
	SiteID         string    `json:"siteId"`
	Amount         float64   `json:"amount"` // 成本口径实付/记账金额
	Note           string    `json:"note"`
	SettledAt      time.Time `json:"settledAt"`
	Status         string    `json:"status"` // active | voided
	OperatorUserID string    `json:"operatorUserId"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

const (
	SettlementStatusActive = "active"
	SettlementStatusVoided = "voided"
)

// SettlementSummary 站点结算汇总（预授信）。
type SettlementSummary struct {
	Mode               string   `json:"mode"`
	CreditLimit        *float64 `json:"creditLimit,omitempty"`
	SettlementCurrency string   `json:"settlementCurrency,omitempty"`
	ConsumedCost       float64  `json:"consumedCost"`
	SettledCost        float64  `json:"settledCost"`
	Outstanding        float64  `json:"outstanding"`
	CreditRemaining    *float64 `json:"creditRemaining,omitempty"`
	PlatformBalance    *float64 `json:"platformBalance,omitempty"` // 仅参考
}

// Sub2APIAdminUser 是 GET /api/v1/admin/users/:id 返回的用户详情中，工单模块"Sub2API 用户
// 资料"弹窗需要展示的只读字段。字段在远端响应中不存在或类型不匹配时保持零值/nil，
// 由调用方（tickets.Service）按需降级展示，不在这里伪造数据。
type Sub2APIAdminUser struct {
	ID       string
	Email    string
	Username string
	Role     string
	Status   string
	// Notes 是 Sub2API 后台用户备注（notes / remark 等字段）。
	Notes         string
	Balance       *float64
	FrozenBalance *float64
	Concurrency   *int
	RPMLimit      *int
	CreatedAt     *time.Time
	LastUsedAt    *time.Time
}

// Sub2APIAdminUsersQuery 是 Sub2API admin 用户分页列表的安全查询对象。
// 调用方只能通过这些显式字段影响远端查询，PlatformService 会继续做白名单和分页夹紧。
type Sub2APIAdminUsersQuery struct {
	Page      int
	PageSize  int
	Status    string
	Role      string
	Search    string
	SortBy    string
	SortOrder string
	Timezone  string
}

type Sub2APIAdminUsersPage struct {
	Items    []Sub2APIAdminUser
	Total    int
	Page     int
	PageSize int
	Pages    int
	// TotalKnown/PagesKnown let batch jobs distinguish real upstream pagination
	// metadata from local fallbacks, so all-mode jobs never silently truncate an
	// unknown-length user stream.
	TotalKnown bool
	PagesKnown bool
}

// Sub2APIUserBreakdownQuery is the explicit contract for the Sub2API admin
// leaderboard source endpoint. The upstream end_date is exclusive.
type Sub2APIUserBreakdownQuery struct {
	StartDate string
	EndDate   string
	SortBy    string
	Limit     int
	Timezone  string
}

// Sub2APIUserBreakdownItem is one user row from
// /api/v1/admin/dashboard/user-breakdown. Optional token/cost fields stay at
// zero when older Sub2API deployments omit them.
type Sub2APIUserBreakdownItem struct {
	UserID       string
	Email        string
	Requests     int
	InputTokens  int64
	OutputTokens int64
	CacheTokens  int64
	TotalTokens  int64
	Cost         float64
	ActualCost   float64
}

type Sub2APIUserBreakdown struct {
	Users     []Sub2APIUserBreakdownItem
	StartDate string
	EndDate   string
}

// Sub2APIUserGroupUsage 是某用户在日期区间内按分组汇总的用量（admin dashboard/groups + user_id）。
type Sub2APIUserGroupUsage struct {
	GroupID     string
	GroupName   string
	ActualCost  float64
	Cost        float64
	Requests    int
	TotalTokens int64
}

// Sub2APIBatchUserUsage 来自 POST /api/v1/admin/dashboard/users-usage 的单用户汇总。
// 上游主字段为今日/累计实际消费；部分版本可能附带 token 字段，解析时尽量兼容。
type Sub2APIBatchUserUsage struct {
	UserID          string
	TodayActualCost float64
	TotalActualCost float64
	TodayTokens     int64
	TotalTokens     int64
}

// Sub2APIAccountTodayStats 来自 GET/POST /api/v1/admin/accounts/.../today-stats 的账号今日用量。
// 真实对接创建的是 admin accounts（不是 users），营收必须走账号接口。
//
// ActualCost = 用户侧总消费/实际（管理站绿色「总消费」），用作营收；
// Cost = 上游/账号成本（管理站橙色「成本」），仅参考，不参与营收回填。
type Sub2APIAccountTodayStats struct {
	AccountID   string
	ActualCost  float64 // 用户侧计费，→ 子行营收
	Cost        float64 // 上游成本参考，禁止当作营收
	Requests    int
	TotalTokens int64
}

// Sub2APIBalanceHistoryItem 是上游用户侧入账流水的统一结构（sub2api / new-api 共用）。
type Sub2APIBalanceHistoryItem struct {
	ID        string
	Type      string
	Amount    *float64
	Note      string
	CreatedAt *time.Time
}

// Sub2APIUserBalanceHistory 是上游入账流水拉取结果（命名保留 Sub2API 前缀以兼容既有调用）。
// Items 为全量（后端分页拉齐后合并）；Total 为条数。
// 注意：sub2api /auth/me 的 total_recharged 只是用户表上的累计数值，不含明细。
type Sub2APIUserBalanceHistory struct {
	Items          []Sub2APIBalanceHistoryItem
	Total          int
	TotalRecharged *float64
	Platform       Platform // 实际拉取所用平台
}

// KeyUsageTodayStat 是平台层返回的单个 key 今日消费统计（上游平台原始金额，未乘以站点 rechargeRate）。
type KeyUsageTodayStat struct {
	KeyID       string
	KeyName     string
	GroupName   string
	TodayAmount float64
}

// KeyUsageTodayItem 是仪表盘「今日成本」下钻明细中单个 key 的聚合结果（已按站点 rechargeRate 换算）。
type KeyUsageTodayItem struct {
	SiteID       string
	SiteName     string
	Platform     Platform
	KeyID        string
	KeyName      string
	GroupName    string
	TodayAmount  float64
	RawAmount    float64
	RechargeRate float64
}

// KeyUsageCollectionError 表示跨多个上游站点采集 Key 用量时有站点失败。
// Items 仍由调用方通过正常返回值获得；FailedSites < TotalSites 时属于部分成功，
// 调用方可以展示已成功数据并明确标注缺失范围，而不必把失败站点静默当成零消费。
type KeyUsageCollectionError struct {
	FailedSites int
	TotalSites  int
	Cause       error
}

func (e *KeyUsageCollectionError) Error() string {
	if e == nil || e.Cause == nil {
		return ErrorRequest
	}
	return e.Cause.Error()
}

func (e *KeyUsageCollectionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// BalanceBreakdownItem 是仪表盘「上游总余额」下钻明细中单个站点的余额展示数据。
// Balance/RawBalance 为 nil 表示该站点余额未知（未配置 rechargeRate 或尚未同步成功）。
type BalanceBreakdownItem struct {
	SiteID         string
	SiteName       string
	Platform       Platform
	Balance        *float64
	RawBalance     *float64
	RechargeRate   float64
	LastSyncedAt   *int64
	Status         Status
	SettlementMode string
	ReserveKind    string // prepaid | credit_reference | excluded
}
