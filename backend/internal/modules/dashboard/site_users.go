package dashboard

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// sub2APIUserReader 站点用户列表/流水所需的平台能力（由 PlatformService 实现）。
type sub2APIUserReader interface {
	FetchSub2APIAdminUsersPage(session upstream.Session, query upstream.Sub2APIAdminUsersQuery) (upstream.Sub2APIAdminUsersPage, error)
	FetchSub2APIAdminUserBalanceHistory(session upstream.Session, userID string, page int, pageSize int, codeType string) (upstream.Sub2APIUserBalanceHistory, error)
	FetchSub2APIAdminUser(session upstream.Session, userID string) (upstream.Sub2APIAdminUser, error)
	FetchSub2APIAdminUserGroupUsage(session upstream.Session, platformUserID, startDate, endDate string) ([]upstream.Sub2APIUserGroupUsage, error)
	FetchSub2APIAdminUsageStatsFiltered(session upstream.Session, startDate, endDate, platformUserID string) (float64, error)
	FetchSub2APIAdminBatchUsersUsage(session upstream.Session, platformUserIDs []string) (map[string]upstream.Sub2APIBatchUserUsage, error)
	FetchSub2APIAdminUserBreakdown(session upstream.Session, query upstream.Sub2APIUserBreakdownQuery) (upstream.Sub2APIUserBreakdown, error)
}

// SiteUserItem 站点用户列表项。
type SiteUserItem struct {
	ID            string   `json:"id"`
	Email         string   `json:"email"`
	Username      string   `json:"username"`
	Role          string   `json:"role"`
	Status        string   `json:"status"`
	// Notes 上游后台用户备注（Sub2API notes/remark）。
	Notes         string   `json:"notes,omitempty"`
	Balance       *float64 `json:"balance,omitempty"`       // 平台原始余额
	FrozenBalance *float64 `json:"frozenBalance,omitempty"`
	// LastUsedAt 上游最后使用时间（RFC3339）；未使用过则为空。
	LastUsedAt *string `json:"lastUsedAt,omitempty"`
	// CreatedAt 上游创建时间（RFC3339），用量历史区间起点参考。
	CreatedAt *string `json:"createdAt,omitempty"`
	// Marked* 来自本地打标汇总（平台原始单位）
	MarkedGift     float64 `json:"markedGift"`
	MarkedRebate   float64 `json:"markedRebate"`
	MarkedRecharge float64 `json:"markedRecharge"`
	// NonRevenue 营收侧将扣除的额度（gift+rebate，平台单位，且不超过余额）
	NonRevenue float64 `json:"nonRevenue"`
	// 成本/CNY 口径（平台 × siteRechargeRate）
	BalanceCny    *float64 `json:"balanceCny,omitempty"`
	NonRevenueCny float64  `json:"nonRevenueCny"`
	RevenueCny    *float64 `json:"revenueCny,omitempty"`
	// 用量（平台单位 / token）；按用量排序时填充，便于列表展示
	TodayTokens int64   `json:"todayTokens,omitempty"`
	TodayCost   float64 `json:"todayCost,omitempty"`  // 今日实际消费（平台）
	TotalTokens int64   `json:"totalTokens,omitempty"`
	TotalCost   float64 `json:"totalCost,omitempty"` // 累计实际消费（平台）
}

// SiteUserUsageGroup 用户在某区间内单个分组的用量与占比。
type SiteUserUsageGroup struct {
	GroupID     string  `json:"groupId,omitempty"`
	GroupName   string  `json:"groupName"`
	ActualCost  float64 `json:"actualCost"` // 平台原始金额
	Percent     float64 `json:"percent"`    // 0–100，相对本区间合计
	Requests    int     `json:"requests,omitempty"`
	TotalTokens int64   `json:"totalTokens,omitempty"`
}

// SiteUserUsagePeriod 今日或历史用量汇总。
type SiteUserUsagePeriod struct {
	Label           string               `json:"label"` // today | history
	StartDate       string               `json:"startDate"`
	EndDate         string               `json:"endDate"`
	TotalActualCost float64              `json:"totalActualCost"`
	Groups          []SiteUserUsageGroup `json:"groups"`
}

// SiteUserUsageResponse 用户用量详情（今日 + 历史，按分组拆分）。
type SiteUserUsageResponse struct {
	User             *SiteUserItem        `json:"user,omitempty"`
	Today            SiteUserUsagePeriod  `json:"today"`
	History          SiteUserUsagePeriod  `json:"history"`
	SiteRechargeRate float64              `json:"siteRechargeRate"`
	MessageKey       string               `json:"messageKey,omitempty"`
}

// SiteUsersResponse 分页用户列表。
type SiteUsersResponse struct {
	Items            []SiteUserItem `json:"items"`
	Total            int            `json:"total"`
	Page             int            `json:"page"`
	PageSize         int            `json:"pageSize"`
	Pages            int            `json:"pages"`
	Platform         string         `json:"platform,omitempty"`
	MessageKey       string         `json:"messageKey,omitempty"`
	SiteRechargeRate float64        `json:"siteRechargeRate"`
	ExcludeAdmin     bool           `json:"excludeAdmin"`
	// ExcludeUserIDs 整户排除（测试号等），成本与付费剩余统计都不计。
	ExcludeUserIDs []string `json:"excludeUserIds"`
	// 当前页合计（成本口径）；全站合计请用仪表盘 metrics
	PageCostTotal    float64 `json:"pageCostTotal"`
	PageRevenueTotal float64 `json:"pageRevenueTotal"`
	SortBy           string  `json:"sortBy,omitempty"`
	SortOrder        string  `json:"sortOrder,omitempty"`
}

// SiteBalanceSettingsInput 站点用户余额相关设置（倍率 + 排除规则）。
type SiteBalanceSettingsInput struct {
	SiteRechargeRate *float64 `json:"siteRechargeRate,omitempty"`
	ExcludeAdmin     *bool    `json:"excludeAdmin,omitempty"`
	// ExcludeUserIDs 非 nil 时整体替换排除名单（可传空数组清空）。
	ExcludeUserIDs *[]string `json:"excludeUserIds,omitempty"`
}

// SiteUserTopupCandidate 单条用户入账流水 + 本地标记。
type SiteUserTopupCandidate struct {
	PlatformID     string  `json:"platformId"`
	PlatformType   string  `json:"platformType,omitempty"`
	AmountPlatform float64 `json:"amountPlatform"`
	Note           string  `json:"note,omitempty"`
	CreatedAt      *string `json:"createdAt,omitempty"`
	SuggestedTag   string  `json:"suggestedTag,omitempty"`
	Tag            string  `json:"tag,omitempty"`
	MarkID         string  `json:"markId,omitempty"`
	BusinessDate   string  `json:"businessDate,omitempty"`
	CountsAsPaid   bool    `json:"countsAsPaid"` // tag=recharge 且金额>0（负向为退款）
}

// SiteUserTopupsResponse 用户充值流水（全量拉取后分页展示）。
type SiteUserTopupsResponse struct {
	Items            []SiteUserTopupCandidate `json:"items"`
	Available        bool                     `json:"available"`
	Page             int                      `json:"page"`
	PageSize         int                      `json:"pageSize"`
	Total            int                      `json:"total"`
	User             *SiteUserItem            `json:"user,omitempty"`
	PlatformLifetime *float64                 `json:"platformLifetime,omitempty"` // total_recharged（平台单位）
	DetailListSum    float64                  `json:"detailListSum"`
	MarkedRecharge   float64                  `json:"markedRecharge"`
	MarkedGift       float64                  `json:"markedGift"`
	MarkedRebate     float64                  `json:"markedRebate"`
	MessageKey       string                   `json:"messageKey,omitempty"`
	// SiteRechargeRate 用于前端把平台金额换算成成本/CNY 双币展示。
	SiteRechargeRate float64 `json:"siteRechargeRate"`
}

// MarkSiteUserTopupInput 标记/改标请求。
type MarkSiteUserTopupInput struct {
	PlatformRecordID string   `json:"platformRecordId"`
	Tag              string   `json:"tag"`
	AmountPlatform   *float64 `json:"amountPlatform,omitempty"`
	Note             string   `json:"note,omitempty"`
	CreatedAt        string   `json:"createdAt,omitempty"`
}

func (s *MetricsService) siteUserReader() (sub2APIUserReader, bool) {
	if s == nil || s.platform == nil {
		return nil, false
	}
	reader, ok := s.platform.(sub2APIUserReader)
	return reader, ok
}

func (s *MetricsService) requireAdminSession(ctx context.Context, userID string) (upstream.Session, string, error) {
	adminAccountID, err := s.accounts.RequireCurrentID(ctx, userID)
	if err != nil {
		return upstream.Session{}, "", err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return upstream.Session{}, "", err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return upstream.Session{}, "", requestError(ErrorAdminOnly)
	}
	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return upstream.Session{}, "", requestError(ErrorAdminOnly)
	}
	if err := s.platform.VerifyAdmin(session); err != nil {
		return upstream.Session{}, "", requestError(ErrorAdminOnly)
	}
	return session, adminAccountID, nil
}

// ListSiteUsersQuery 站点用户列表查询。
type ListSiteUsersQuery struct {
	Page      int
	PageSize  int
	Search    string
	SortBy    string // balance | non_revenue | email | username | created_at | role | status
	SortOrder string // asc | desc
	// HideExcluded 列表中隐藏整户排除用户（及可选 admin）；余额弹窗用 true，站点用户页用 false 便于仍可标记流水。
	HideExcluded bool
	// SkipMarkSync 跳过列表前自动打标（typeahead 搜索等轻量场景）；正式列表必须为 false。
	SkipMarkSync bool
}

// ListSiteUsers 分页列出当前 admin 站点用户；search 走上游 search（邮箱/ID/用户名）。
// balance / non_revenue 排序在本地完成（上游不支持）；其余透传上游。
func (s *MetricsService) ListSiteUsers(ctx context.Context, userID string, q ListSiteUsersQuery) (SiteUsersResponse, error) {
	page, pageSize := q.Page, q.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	sortBy, sortOrder := normalizeSiteUserSort(q.SortBy, q.SortOrder)
	resp := SiteUsersResponse{
		Items:     []SiteUserItem{},
		Page:      page,
		PageSize:  pageSize,
		SortBy:    sortBy,
		SortOrder: sortOrder,
	}
	session, adminAccountID, err := s.requireAdminSession(ctx, userID)
	if err != nil {
		return resp, err
	}
	resp.Platform = string(session.Platform)

	filterCfg, _ := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
	rate := filterCfg.SiteRechargeRate
	if rate <= 0 {
		rate = 1
	}
	resp.SiteRechargeRate = rate
	resp.ExcludeAdmin = filterCfg.ExcludeAdmin
	if filterCfg.ExcludeUserIDs != nil {
		resp.ExcludeUserIDs = append([]string{}, filterCfg.ExcludeUserIDs...)
	} else {
		resp.ExcludeUserIDs = []string{}
	}
	excludedIDs := stringSet(filterCfg.ExcludeUserIDs)

	reader, ok := s.siteUserReader()
	if !ok || session.Platform != upstream.PlatformSub2API {
		resp.MessageKey = "admin.siteUsers.errors.platformUnsupported"
		return resp, nil
	}

	// 本地字段排序 / 隐藏排除用户时强制全量拉齐再过滤分页
	localSort := q.HideExcluded || siteUserSortNeedsLocal(sortBy)

	var raw []upstream.Sub2APIAdminUser
	var total int
	var pages int

	if localSort {
		// 全量拉取后本地排序分页（上限 50 页 × 100）
		const fetchSize = 100
		const maxPages = 50
		for p := 1; p <= maxPages; p++ {
			pageData, err := reader.FetchSub2APIAdminUsersPage(session, upstream.Sub2APIAdminUsersQuery{
				Page:      p,
				PageSize:  fetchSize,
				Search:    strings.TrimSpace(q.Search),
				SortBy:    "created_at",
				SortOrder: "desc",
			})
			if err != nil {
				if p == 1 {
					log.Printf("dashboard site-users: list failed user_id=%s err=%v", userID, err)
					return resp, err
				}
				break
			}
			raw = append(raw, pageData.Items...)
			total = pageData.Total
			if len(pageData.Items) < fetchSize {
				break
			}
			if total > 0 && len(raw) >= total {
				break
			}
		}
		if total < len(raw) {
			total = len(raw)
		}
	} else {
		upstreamSort := sortBy
		if upstreamSort == "" {
			upstreamSort = "created_at"
		}
		pageData, err := reader.FetchSub2APIAdminUsersPage(session, upstream.Sub2APIAdminUsersQuery{
			Page:      page,
			PageSize:  pageSize,
			Search:    strings.TrimSpace(q.Search),
			SortBy:    upstreamSort,
			SortOrder: sortOrder,
		})
		if err != nil {
			log.Printf("dashboard site-users: list failed user_id=%s err=%v", userID, err)
			return resp, err
		}
		raw = pageData.Items
		total = pageData.Total
		pages = pageData.Pages
	}

	// 按累计充值/赠送/返利排序时必须先全量打标；否则只同步当前页，避免列表首屏过慢
	// 全量分页偶发重复时按 ID 去重
	raw = dedupeAdminUsersByID(raw)

	sortNeedsMarks := sortBy == "recharge" || sortBy == "gift" || sortBy == "rebate" ||
		sortBy == "revenue_cny" || sortBy == "non_revenue"

	if !q.SkipMarkSync && (!localSort || sortNeedsMarks) {
		platformIDs := make([]string, 0, len(raw))
		for _, u := range raw {
			if id := strings.TrimSpace(u.ID); id != "" {
				platformIDs = append(platformIDs, id)
			}
		}
		s.ensureTopupMarksForUsers(ctx, reader, session, userID, adminAccountID, platformIDs)
	}

	tagSums, _ := s.metricsRepo.MapTagSumsByPlatformUser(ctx, userID, adminAccountID)

	items := make([]SiteUserItem, 0, len(raw))
	for _, u := range raw {
		item := siteUserFromAdmin(u)
		// 余额统计/弹窗：隐藏已排除用户与（可选）admin
		if q.HideExcluded {
			if filterCfg.ExcludeAdmin && strings.EqualFold(strings.TrimSpace(item.Role), "admin") {
				continue
			}
			if excludedIDs[item.ID] {
				continue
			}
		}
		applyTagSumsToItem(&item, tagSums)
		applySiteUserCny(&item, rate)
		items = append(items, item)
	}

	// 按用量排序时批量补齐今日/累计 token 与消费，再本地排序
	if siteUserSortNeedsUsage(sortBy) {
		s.enrichSiteUsersUsage(reader, session, items, sortBy)
	}

	if localSort {
		sortSiteUsers(items, sortBy, sortOrder)
		// 分页切片
		total = len(items)
		start := (page - 1) * pageSize
		if start > total {
			start = total
		}
		end := start + pageSize
		if end > total {
			end = total
		}
		pageItems := items[start:end]
		// 默认余额排序：仅对本页用户同步打标后再回填，保证展示准确且不拖垮全站
		if !q.SkipMarkSync && !sortNeedsMarks {
			pageIDs := make([]string, 0, len(pageItems))
			for _, it := range pageItems {
				if id := strings.TrimSpace(it.ID); id != "" {
					pageIDs = append(pageIDs, id)
				}
			}
			s.ensureTopupMarksForUsers(ctx, reader, session, userID, adminAccountID, pageIDs)
			if fresh, err := s.metricsRepo.MapTagSumsByPlatformUser(ctx, userID, adminAccountID); err == nil {
				for i := range pageItems {
					applyTagSumsToItem(&pageItems[i], fresh)
					applySiteUserCny(&pageItems[i], rate)
				}
			}
		}
		resp.Items = pageItems
	} else {
		resp.Items = items
	}

	resp.Total = total
	if pages < 1 && pageSize > 0 && total > 0 {
		pages = (total + pageSize - 1) / pageSize
	}
	resp.Pages = pages
	if resp.Pages < 1 {
		resp.Pages = 1
	}

	for _, item := range resp.Items {
		// 本页合计与仪表盘一致：跳过 admin（若开启）与整户排除
		if filterCfg.ExcludeAdmin && strings.EqualFold(item.Role, "admin") {
			continue
		}
		if excludedIDs[item.ID] {
			continue
		}
		if item.BalanceCny != nil {
			resp.PageCostTotal += *item.BalanceCny
		}
		// PageRevenueTotal 语义改为本页累计充值（CNY）
		resp.PageRevenueTotal += item.MarkedRecharge * rate
	}
	return resp, nil
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out[trimmed] = true
		}
	}
	return out
}

func dedupeAdminUsersByID(raw []upstream.Sub2APIAdminUser) []upstream.Sub2APIAdminUser {
	if len(raw) < 2 {
		return raw
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]upstream.Sub2APIAdminUser, 0, len(raw))
	for _, u := range raw {
		id := strings.TrimSpace(u.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, u)
	}
	return out
}

func normalizeSiteUserSort(sortBy, sortOrder string) (string, string) {
	sortBy = strings.ToLower(strings.TrimSpace(sortBy))
	switch sortBy {
	case "id", "balance", "non_revenue", "balance_cny", "revenue_cny", "recharge", "gift", "rebate",
		"email", "username", "created_at", "last_used_at", "role", "status",
		"today_tokens", "today_cost", "total_tokens", "total_cost":
	default:
		sortBy = "balance"
	}
	sortOrder = strings.ToLower(strings.TrimSpace(sortOrder))
	if sortOrder != "asc" {
		sortOrder = "desc"
	}
	return sortBy, sortOrder
}

// siteUserSortNeedsLocal 标记类/兑付余额/ID/最后使用/用量等字段上游不支持，需本地排序。
func siteUserSortNeedsLocal(sortBy string) bool {
	switch sortBy {
	case "id", "balance", "non_revenue", "balance_cny", "revenue_cny",
		"recharge", "gift", "rebate", "last_used_at",
		"today_tokens", "today_cost", "total_tokens", "total_cost":
		return true
	default:
		return false
	}
}

func siteUserSortNeedsUsage(sortBy string) bool {
	switch sortBy {
	case "today_tokens", "today_cost", "total_tokens", "total_cost":
		return true
	default:
		return false
	}
}

// enrichSiteUsersUsage 按排序字段批量补齐用量：金额优先 users-usage，token 走 user-breakdown。
func (s *MetricsService) enrichSiteUsersUsage(
	reader sub2APIUserReader,
	session upstream.Session,
	items []SiteUserItem,
	sortBy string,
) {
	if reader == nil || len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if id := strings.TrimSpace(it.ID); id != "" {
			ids = append(ids, id)
		}
	}
	needCost := sortBy == "today_cost" || sortBy == "total_cost"
	needTokens := sortBy == "today_tokens" || sortBy == "total_tokens"

	var batch map[string]upstream.Sub2APIBatchUserUsage
	if needCost {
		if m, err := reader.FetchSub2APIAdminBatchUsersUsage(session, ids); err == nil {
			batch = m
		} else {
			log.Printf("dashboard site-users: batch usage failed err=%v", err)
		}
	}

	today := upstream.BusinessToday()
	// user-breakdown 的 end_date 为 exclusive
	tomorrow := time.Now().In(upstream.BusinessLocation()).AddDate(0, 0, 1).Format("2006-01-02")

	var todayBD, totalBD map[string]upstream.Sub2APIUserBreakdownItem
	if needTokens {
		// token 排序：拉今日 + 累计 breakdown（limit 200，无用量用户视为 0）
		todayBD = fetchBreakdownMap(reader, session, today, tomorrow, "total_tokens")
		totalBD = fetchBreakdownMap(reader, session, "2020-01-01", tomorrow, "total_tokens")
	} else if needCost && len(batch) == 0 {
		// batch 不可用时用 breakdown 兜底金额
		todayBD = fetchBreakdownMap(reader, session, today, tomorrow, "actual_cost")
		totalBD = fetchBreakdownMap(reader, session, "2020-01-01", tomorrow, "actual_cost")
	}

	for i := range items {
		id := items[i].ID
		if b, ok := batch[id]; ok {
			items[i].TodayCost = b.TodayActualCost
			items[i].TotalCost = b.TotalActualCost
			if b.TodayTokens > 0 {
				items[i].TodayTokens = b.TodayTokens
			}
			if b.TotalTokens > 0 {
				items[i].TotalTokens = b.TotalTokens
			}
		}
		if u, ok := todayBD[id]; ok {
			if items[i].TodayTokens == 0 {
				items[i].TodayTokens = u.TotalTokens
			}
			if items[i].TodayCost == 0 && u.ActualCost > 0 {
				items[i].TodayCost = u.ActualCost
			}
		}
		if u, ok := totalBD[id]; ok {
			if items[i].TotalTokens == 0 {
				items[i].TotalTokens = u.TotalTokens
			}
			if items[i].TotalCost == 0 && u.ActualCost > 0 {
				items[i].TotalCost = u.ActualCost
			}
		}
	}
}

func fetchBreakdownMap(
	reader sub2APIUserReader,
	session upstream.Session,
	startDate, endDate, sortBy string,
) map[string]upstream.Sub2APIUserBreakdownItem {
	out := make(map[string]upstream.Sub2APIUserBreakdownItem)
	sb := "total_tokens"
	if sortBy == "actual_cost" || sortBy == "today_cost" || sortBy == "total_cost" {
		sb = "actual_cost"
	}
	bd, err := reader.FetchSub2APIAdminUserBreakdown(session, upstream.Sub2APIUserBreakdownQuery{
		StartDate: startDate,
		EndDate:   endDate,
		SortBy:    sb,
		Limit:     200,
		Timezone:  upstream.BusinessTimezone,
	})
	if err != nil {
		log.Printf("dashboard site-users: user-breakdown failed start=%s end=%s err=%v", startDate, endDate, err)
		return out
	}
	for _, u := range bd.Users {
		if id := strings.TrimSpace(u.UserID); id != "" {
			out[id] = u
		}
	}
	return out
}

func applyTagSumsToItem(item *SiteUserItem, tagSums map[string]UserTagSums) {
	if item == nil {
		return
	}
	if sums, ok := tagSums[item.ID]; ok {
		item.MarkedGift = sums.Gift
		item.MarkedRebate = sums.Rebate
		item.MarkedRecharge = sums.Recharge
		// 净赠送+返利；负向收回会拉低合计，营收扣除不低于 0、不超过余额
		item.NonRevenue = sums.Gift + sums.Rebate
		if item.NonRevenue < 0 {
			item.NonRevenue = 0
		}
		if item.Balance != nil && item.NonRevenue > *item.Balance {
			item.NonRevenue = *item.Balance
		}
	} else {
		item.MarkedGift = 0
		item.MarkedRebate = 0
		item.MarkedRecharge = 0
		item.NonRevenue = 0
	}
}

func applySiteUserCny(item *SiteUserItem, rate float64) {
	if rate <= 0 {
		rate = 1
	}
	item.NonRevenueCny = item.NonRevenue * rate
	if item.Balance != nil {
		bc := *item.Balance * rate
		item.BalanceCny = &bc
		rev := bc - item.NonRevenueCny
		if rev < 0 {
			rev = 0
		}
		item.RevenueCny = &rev
	}
}

func sortSiteUsers(items []SiteUserItem, sortBy, sortOrder string) {
	asc := sortOrder == "asc"
	sort.SliceStable(items, func(i, j int) bool {
		var cmp int
		switch sortBy {
		case "id":
			cmp = cmpUserID(items[i].ID, items[j].ID)
		case "non_revenue", "non_revenue_cny", "gift":
			cmp = cmpFloat(items[i].MarkedGift, items[j].MarkedGift)
		case "rebate":
			cmp = cmpFloat(items[i].MarkedRebate, items[j].MarkedRebate)
		case "recharge", "revenue_cny":
			cmp = cmpFloat(items[i].MarkedRecharge, items[j].MarkedRecharge)
		case "last_used_at":
			cmp = cmpTimePtr(items[i].LastUsedAt, items[j].LastUsedAt)
		case "today_tokens":
			cmp = cmpInt64(items[i].TodayTokens, items[j].TodayTokens)
		case "today_cost":
			cmp = cmpFloat(items[i].TodayCost, items[j].TodayCost)
		case "total_tokens":
			cmp = cmpInt64(items[i].TotalTokens, items[j].TotalTokens)
		case "total_cost":
			cmp = cmpFloat(items[i].TotalCost, items[j].TotalCost)
		case "email":
			cmp = strings.Compare(strings.ToLower(items[i].Email), strings.ToLower(items[j].Email))
		case "username":
			cmp = strings.Compare(strings.ToLower(items[i].Username), strings.ToLower(items[j].Username))
		case "role":
			cmp = strings.Compare(items[i].Role, items[j].Role)
		case "status":
			cmp = strings.Compare(items[i].Status, items[j].Status)
		default: // balance / balance_cny — 兑付余额与平台余额同序（倍率常量）
			cmp = cmpFloat(floatOrZero(items[i].Balance), floatOrZero(items[j].Balance))
		}
		if cmp == 0 {
			return false
		}
		if asc {
			return cmp < 0
		}
		return cmp > 0
	})
}

func cmpUserID(a, b string) int {
	// 纯数字 ID 按数值比，否则字符串
	ai, aErr := parseIntID(a)
	bi, bErr := parseIntID(b)
	if aErr == nil && bErr == nil {
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func parseIntID(s string) (int64, error) {
	s = strings.TrimSpace(s)
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not int")
		}
		n = n*10 + int64(r-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

func cmpTimePtr(a, b *string) int {
	at := parseRFC3339OrZero(a)
	bt := parseRFC3339OrZero(b)
	if at.Before(bt) {
		return -1
	}
	if at.After(bt) {
		return 1
	}
	return 0
}

func parseRFC3339OrZero(v *string) time.Time {
	if v == nil || strings.TrimSpace(*v) == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(*v)); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*v)); err == nil {
		return t
	}
	return time.Time{}
}

func cmpFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func cmpInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func floatOrZero(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// SearchSiteUserCandidates 搜索候选（typeahead），默认每页 10 条。
func (s *MetricsService) SearchSiteUserCandidates(ctx context.Context, userID string, search string, limit int) (SiteUsersResponse, error) {
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	return s.ListSiteUsers(ctx, userID, ListSiteUsersQuery{
		Page: 1, PageSize: limit, Search: search, SortBy: "email", SortOrder: "asc",
		SkipMarkSync: true, // 候选搜索不需要打标，避免拖慢输入
	})
}

// UpdateSiteBalanceSettings 更新工作区站点充值倍率与排除规则。
func (s *MetricsService) UpdateSiteBalanceSettings(ctx context.Context, userID string, input SiteBalanceSettingsInput) (BalanceFilterConfig, error) {
	adminAccountID, err := s.accounts.RequireCurrentID(ctx, userID)
	if err != nil {
		return BalanceFilterConfig{}, err
	}
	cfg, err := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
	if err != nil {
		return BalanceFilterConfig{}, err
	}
	if input.SiteRechargeRate != nil {
		if *input.SiteRechargeRate <= 0 {
			return BalanceFilterConfig{}, requestError("admin.siteUsers.errors.invalidRate")
		}
		cfg.SiteRechargeRate = *input.SiteRechargeRate
	}
	if input.ExcludeAdmin != nil {
		cfg.ExcludeAdmin = *input.ExcludeAdmin
	}
	if input.ExcludeUserIDs != nil {
		cleaned := make([]string, 0, len(*input.ExcludeUserIDs))
		seen := map[string]bool{}
		for _, id := range *input.ExcludeUserIDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			cleaned = append(cleaned, id)
		}
		cfg.ExcludeUserIDs = cleaned
	}
	cfg.UserID = userID
	cfg.AdminAccountID = adminAccountID
	if err := s.metricsRepo.SaveBalanceFilter(ctx, cfg); err != nil {
		return BalanceFilterConfig{}, err
	}
	return cfg, nil
}

// ListSiteUserTopups 拉取用户入账流水，自动打标未标记项，合并本地标记后分页返回。
func (s *MetricsService) ListSiteUserTopups(ctx context.Context, userID, platformUserID string, page, pageSize int) (SiteUserTopupsResponse, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	platformUserID = strings.TrimSpace(platformUserID)
	resp := SiteUserTopupsResponse{Items: []SiteUserTopupCandidate{}, Page: page, PageSize: pageSize}
	if platformUserID == "" {
		return resp, requestError("admin.siteUsers.errors.invalidUser")
	}
	session, adminAccountID, err := s.requireAdminSession(ctx, userID)
	if err != nil {
		return resp, err
	}
	rate := 1.0
	if filterCfg, ferr := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID); ferr == nil && filterCfg.SiteRechargeRate > 0 {
		rate = filterCfg.SiteRechargeRate
	}
	resp.SiteRechargeRate = rate
	reader, ok := s.siteUserReader()
	if !ok || session.Platform != upstream.PlatformSub2API {
		resp.MessageKey = "admin.siteUsers.errors.platformUnsupported"
		return resp, nil
	}

	// 用户资料（标记合计在自动打标之后再填，避免左侧/头部仍是 0）
	var profileUser *SiteUserItem
	if profile, err := reader.FetchSub2APIAdminUser(session, platformUserID); err == nil && profile.ID != "" {
		u := siteUserFromAdmin(profile)
		profileUser = &u
	}

	// 全量分页拉取 balance-history
	all, lifetime, fetchErr := s.fetchAllUserBalanceHistory(reader, session, platformUserID)
	if fetchErr != nil {
		log.Printf("dashboard site-users: balance-history failed user_id=%s platform_user=%s err=%v", userID, platformUserID, fetchErr)
		resp.MessageKey = "admin.siteUsers.errors.historyUnavailable"
		return resp, nil
	}
	resp.Available = true
	resp.PlatformLifetime = lifetime

	// 自动打标未标记流水（写入 DB 后列表累计充值才能对上）
	s.autoMarkUserHistory(ctx, userID, adminAccountID, platformUserID, all)

	// 打标后从库汇总三类标记（与用户列表同源）
	if gift, rebate, recharge, sumErr := s.metricsRepo.SumTagsByPlatformUser(ctx, userID, adminAccountID, platformUserID); sumErr == nil {
		resp.MarkedGift = gift
		resp.MarkedRebate = rebate
		resp.MarkedRecharge = recharge
		if profileUser != nil {
			profileUser.MarkedGift = gift
			profileUser.MarkedRebate = rebate
			profileUser.MarkedRecharge = recharge
			profileUser.NonRevenue = gift + rebate
			if profileUser.NonRevenue < 0 {
				profileUser.NonRevenue = 0
			}
			if profileUser.Balance != nil && profileUser.NonRevenue > *profileUser.Balance {
				profileUser.NonRevenue = *profileUser.Balance
			}
			applySiteUserCny(profileUser, rate)
			resp.User = profileUser
		}
	} else if profileUser != nil {
		applySiteUserCny(profileUser, rate)
		resp.User = profileUser
	}

	marks, _ := s.metricsRepo.MapSiteUserMarksByRef(ctx, userID, adminAccountID, platformUserID)

	candidates := make([]SiteUserTopupCandidate, 0, len(all))
	for _, item := range all {
		// 展示正负余额变动；0 与非余额额度（并发/RPM）跳过
		if item.Amount == nil || *item.Amount == 0 {
			continue
		}
		if upstream.IsNonBalanceCreditHistoryType(item.Type, item.Note) {
			continue
		}
		platformID := strings.TrimSpace(item.ID)
		if platformID == "" {
			ts := ""
			if item.CreatedAt != nil {
				ts = item.CreatedAt.UTC().Format(time.RFC3339)
			}
			platformID = fmt.Sprintf("anon:%.6f:%s:%s", *item.Amount, ts, item.Note)
		}
		cand := SiteUserTopupCandidate{
			PlatformID:     platformID,
			PlatformType:   item.Type,
			AmountPlatform: *item.Amount,
			Note:           item.Note,
			SuggestedTag:   upstream.SuggestHistoryTag(item.Type, item.Note, *item.Amount),
		}
		if item.CreatedAt != nil {
			ts := item.CreatedAt.Format(time.RFC3339)
			cand.CreatedAt = &ts
			cand.BusinessDate = item.CreatedAt.In(upstream.BusinessLocation()).Format("2006-01-02")
		}
		ref := siteUserTopupRef(platformID)
		if mark, ok := marks[ref]; ok {
			cand.Tag = mark.Tag
			cand.MarkID = mark.ID
			// 付费入账仅计正向充值；负向 recharge 是退款，不算 paid
			cand.CountsAsPaid = mark.Tag == SiteUserTagRecharge && mark.AmountPlatform > 0
			cand.AmountPlatform = mark.AmountPlatform
			if mark.BusinessDate != "" {
				cand.BusinessDate = mark.BusinessDate
			}
		} else if cand.SuggestedTag != "" {
			// 自动打标失败时仍用建议标签展示（不计入 DB 汇总）
			cand.Tag = cand.SuggestedTag
		}
		candidates = append(candidates, cand)
		resp.DetailListSum += *item.Amount
	}

	// 汇总以 DB 为准（上面已填 Marked*）；此处仅作兜底
	if resp.MarkedRecharge == 0 && resp.MarkedGift == 0 && resp.MarkedRebate == 0 {
		for _, cand := range candidates {
			switch cand.Tag {
			case SiteUserTagRecharge:
				resp.MarkedRecharge += cand.AmountPlatform
			case SiteUserTagGift:
				resp.MarkedGift += cand.AmountPlatform
			case SiteUserTagRebate:
				resp.MarkedRebate += cand.AmountPlatform
			}
		}
	}

	resp.Total = len(candidates)
	start := (page - 1) * pageSize
	if start >= len(candidates) {
		resp.Items = []SiteUserTopupCandidate{}
		return resp, nil
	}
	end := start + pageSize
	if end > len(candidates) {
		end = len(candidates)
	}
	resp.Items = candidates[start:end]
	return resp, nil
}

// MarkSiteUserTopup 手动/改标用户入账流水。
func (s *MetricsService) MarkSiteUserTopup(ctx context.Context, userID, platformUserID string, input MarkSiteUserTopupInput) (SiteUserTopupMark, error) {
	platformUserID = strings.TrimSpace(platformUserID)
	platformRecordID := strings.TrimSpace(input.PlatformRecordID)
	if platformUserID == "" || platformRecordID == "" {
		return SiteUserTopupMark{}, requestError("admin.siteUsers.errors.invalidRequest")
	}
	tag, ok := normalizeSiteUserTag(input.Tag)
	if !ok {
		return SiteUserTopupMark{}, requestError("admin.siteUsers.errors.invalidTag")
	}
	_, adminAccountID, err := s.requireAdminSession(ctx, userID)
	if err != nil {
		return SiteUserTopupMark{}, err
	}
	// 允许负向金额：充值退款 / 赠送收回 / 返利收回与正向共用 tag，金额带符号
	var amount float64
	if input.AmountPlatform == nil || *input.AmountPlatform == 0 {
		return SiteUserTopupMark{}, requestError("admin.siteUsers.errors.invalidAmount")
	}
	amount = *input.AmountPlatform
	if amount != amount { // NaN
		return SiteUserTopupMark{}, requestError("admin.siteUsers.errors.invalidAmount")
	}
	bizDate := ""
	if ts := strings.TrimSpace(input.CreatedAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			bizDate = t.In(upstream.BusinessLocation()).Format("2006-01-02")
		} else if t, err := time.Parse("2006-01-02", ts); err == nil {
			bizDate = t.Format("2006-01-02")
		}
	}
	now := time.Now()
	id, err := newSiteUserMarkID()
	if err != nil {
		return SiteUserTopupMark{}, err
	}
	note := strings.TrimSpace(input.Note)
	mark := SiteUserTopupMark{
		ID:             id,
		UserID:         userID,
		AdminAccountID: adminAccountID,
		PlatformUserID: platformUserID,
		Ref:            siteUserTopupRef(platformRecordID),
		Tag:            tag,
		AmountPlatform: amount,
		Note:           note,
		BusinessDate:   bizDate,
		Source:         "hybrid",
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.metricsRepo.UpsertSiteUserMark(ctx, mark); err != nil {
		return SiteUserTopupMark{}, err
	}
	if latest, err := s.metricsRepo.GetSiteUserMark(ctx, userID, adminAccountID, platformUserID, mark.Ref); err == nil && latest != nil {
		return *latest, nil
	}
	return mark, nil
}

func siteUserTopupRef(platformRecordID string) string {
	return "plat:" + strings.TrimSpace(platformRecordID)
}

func siteUserFromAdmin(u upstream.Sub2APIAdminUser) SiteUserItem {
	item := SiteUserItem{
		ID:            u.ID,
		Email:         u.Email,
		Username:      u.Username,
		Role:          u.Role,
		Status:        u.Status,
		Notes:         u.Notes,
		Balance:       u.Balance,
		FrozenBalance: u.FrozenBalance,
	}
	if u.LastUsedAt != nil {
		ts := u.LastUsedAt.UTC().Format(time.RFC3339)
		item.LastUsedAt = &ts
	}
	if u.CreatedAt != nil {
		ts := u.CreatedAt.UTC().Format(time.RFC3339)
		item.CreatedAt = &ts
	}
	return item
}

// GetSiteUserUsage 拉取用户今日与历史用量，按分组拆分并计算占比。
func (s *MetricsService) GetSiteUserUsage(ctx context.Context, userID, platformUserID string) (SiteUserUsageResponse, error) {
	platformUserID = strings.TrimSpace(platformUserID)
	resp := SiteUserUsageResponse{
		Today:   SiteUserUsagePeriod{Label: "today", Groups: []SiteUserUsageGroup{}},
		History: SiteUserUsagePeriod{Label: "history", Groups: []SiteUserUsageGroup{}},
	}
	if platformUserID == "" {
		return resp, requestError("admin.siteUsers.errors.invalidUser")
	}
	session, adminAccountID, err := s.requireAdminSession(ctx, userID)
	if err != nil {
		return resp, err
	}
	rate := 1.0
	if filterCfg, ferr := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID); ferr == nil && filterCfg.SiteRechargeRate > 0 {
		rate = filterCfg.SiteRechargeRate
	}
	resp.SiteRechargeRate = rate
	reader, ok := s.siteUserReader()
	if !ok || session.Platform != upstream.PlatformSub2API {
		resp.MessageKey = "admin.siteUsers.errors.platformUnsupported"
		return resp, nil
	}

	// 资料
	if profile, err := reader.FetchSub2APIAdminUser(session, platformUserID); err == nil && profile.ID != "" {
		u := siteUserFromAdmin(profile)
		applySiteUserCny(&u, rate)
		if sums, sumErr := s.metricsRepo.MapTagSumsByPlatformUser(ctx, userID, adminAccountID); sumErr == nil {
			applyTagSumsToItem(&u, sums)
			applySiteUserCny(&u, rate)
		}
		resp.User = &u
	}

	today := upstream.BusinessToday()
	// 历史：自创建日（或 2020-01-01）至今日；与上游约定 start/end 同日表示该业务日
	historyStart := "2020-01-01"
	if resp.User != nil && resp.User.CreatedAt != nil {
		if t := parseRFC3339OrZero(resp.User.CreatedAt); !t.IsZero() {
			historyStart = t.In(upstream.BusinessLocation()).Format("2006-01-02")
		}
	}

	var wg sync.WaitGroup
	var todayGroups, historyGroups []upstream.Sub2APIUserGroupUsage
	var todayTotal, historyTotal float64
	var todayErr, historyErr, todaySumErr, historySumErr error

	wg.Add(4)
	go func() {
		defer wg.Done()
		todayGroups, todayErr = reader.FetchSub2APIAdminUserGroupUsage(session, platformUserID, today, today)
	}()
	go func() {
		defer wg.Done()
		historyGroups, historyErr = reader.FetchSub2APIAdminUserGroupUsage(session, platformUserID, historyStart, today)
	}()
	go func() {
		defer wg.Done()
		todayTotal, todaySumErr = reader.FetchSub2APIAdminUsageStatsFiltered(session, today, today, platformUserID)
	}()
	go func() {
		defer wg.Done()
		historyTotal, historySumErr = reader.FetchSub2APIAdminUsageStatsFiltered(session, historyStart, today, platformUserID)
	}()
	wg.Wait()

	if todayErr != nil && historyErr != nil {
		log.Printf("dashboard site-users: usage failed user_id=%s platform_user=%s today_err=%v history_err=%v",
			userID, platformUserID, todayErr, historyErr)
		resp.MessageKey = "admin.siteUsers.errors.usageUnavailable"
		return resp, nil
	}

	resp.Today = buildUsagePeriod("today", today, today, todayGroups, todayTotal, todaySumErr)
	resp.History = buildUsagePeriod("history", historyStart, today, historyGroups, historyTotal, historySumErr)
	if todayErr != nil {
		resp.Today.Groups = []SiteUserUsageGroup{}
	}
	if historyErr != nil {
		resp.History.Groups = []SiteUserUsageGroup{}
	}
	return resp, nil
}

func buildUsagePeriod(
	label, start, end string,
	groups []upstream.Sub2APIUserGroupUsage,
	statsTotal float64,
	statsErr error,
) SiteUserUsagePeriod {
	period := SiteUserUsagePeriod{
		Label:     label,
		StartDate: start,
		EndDate:   end,
		Groups:    make([]SiteUserUsageGroup, 0, len(groups)),
	}
	var sum float64
	for _, g := range groups {
		if g.ActualCost <= 0 && g.Cost <= 0 && g.Requests == 0 && g.TotalTokens == 0 {
			continue
		}
		cost := g.ActualCost
		if cost == 0 {
			cost = g.Cost
		}
		sum += cost
		period.Groups = append(period.Groups, SiteUserUsageGroup{
			GroupID:     g.GroupID,
			GroupName:   g.GroupName,
			ActualCost:  cost,
			Requests:    g.Requests,
			TotalTokens: g.TotalTokens,
		})
	}
	// 优先用 stats 接口总额；若无则用分组求和
	if statsErr == nil && statsTotal > 0 {
		period.TotalActualCost = statsTotal
	} else {
		period.TotalActualCost = sum
	}
	denom := period.TotalActualCost
	if denom <= 0 {
		denom = sum
	}
	for i := range period.Groups {
		if denom > 0 {
			period.Groups[i].Percent = period.Groups[i].ActualCost / denom * 100
		}
	}
	return period
}

func (s *MetricsService) fetchAllUserBalanceHistory(
	reader sub2APIUserReader,
	session upstream.Session,
	platformUserID string,
) ([]upstream.Sub2APIBalanceHistoryItem, *float64, error) {
	const pageSize = 100
	const maxPages = 50
	var all []upstream.Sub2APIBalanceHistoryItem
	var lifetime *float64
	for page := 1; page <= maxPages; page++ {
		hist, err := reader.FetchSub2APIAdminUserBalanceHistory(session, platformUserID, page, pageSize, "")
		if err != nil {
			if page == 1 {
				return nil, nil, err
			}
			break
		}
		if hist.TotalRecharged != nil {
			lifetime = hist.TotalRecharged
		}
		if len(hist.Items) == 0 {
			break
		}
		all = append(all, hist.Items...)
		if hist.Total > 0 && len(all) >= hist.Total {
			break
		}
		if len(hist.Items) < pageSize {
			break
		}
	}
	return all, lifetime, nil
}

func (s *MetricsService) autoMarkUserHistory(
	ctx context.Context,
	userID, adminAccountID, platformUserID string,
	items []upstream.Sub2APIBalanceHistoryItem,
) {
	if s.metricsRepo == nil {
		return
	}
	marks, err := s.metricsRepo.MapSiteUserMarksByRef(ctx, userID, adminAccountID, platformUserID)
	if err != nil {
		return
	}
	now := time.Now()
	for _, item := range items {
		// 正负均可；负向仅当 SuggestHistoryTag 有强信号才自动写入（裸扣款不猜）
		if item.Amount == nil || *item.Amount == 0 {
			continue
		}
		if upstream.IsNonBalanceCreditHistoryType(item.Type, item.Note) {
			continue
		}
		platformID := strings.TrimSpace(item.ID)
		if platformID == "" {
			continue
		}
		ref := siteUserTopupRef(platformID)
		if _, exists := marks[ref]; exists {
			continue
		}
		tag := upstream.SuggestHistoryTag(item.Type, item.Note, *item.Amount)
		if tag == "" {
			continue
		}
		id, err := newSiteUserMarkID()
		if err != nil {
			continue
		}
		bizDate := ""
		if item.CreatedAt != nil {
			bizDate = item.CreatedAt.In(upstream.BusinessLocation()).Format("2006-01-02")
		}
		mark := SiteUserTopupMark{
			ID:             id,
			UserID:         userID,
			AdminAccountID: adminAccountID,
			PlatformUserID: platformUserID,
			Ref:            ref,
			Tag:            tag,
			AmountPlatform: *item.Amount, // 保留符号：负向为退款/收回
			Note:           item.Note,
			BusinessDate:   bizDate,
			Source:         "auto",
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := s.metricsRepo.UpsertSiteUserMark(ctx, mark); err != nil {
			log.Printf("dashboard site-users: auto mark failed platform_user=%s ref=%s err=%v", platformUserID, ref, err)
			continue
		}
		marks[ref] = mark
	}
}

// ensureTopupMarksForUsers 列表返回前并发拉取各用户入账流水并自动打标。
// 保证 MarkedRecharge/Gift/Rebate 与打开明细时一致，前端不会先看到 0。
func (s *MetricsService) ensureTopupMarksForUsers(
	ctx context.Context,
	reader sub2APIUserReader,
	session upstream.Session,
	userID, adminAccountID string,
	platformUserIDs []string,
) {
	if s == nil || s.metricsRepo == nil || reader == nil || len(platformUserIDs) == 0 {
		return
	}
	// 去重
	seen := make(map[string]struct{}, len(platformUserIDs))
	ids := make([]string, 0, len(platformUserIDs))
	for _, id := range platformUserIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}

	const workers = 6
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, platformUserID := range ids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(pid string) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			all, _, err := s.fetchAllUserBalanceHistory(reader, session, pid)
			if err != nil {
				log.Printf("dashboard site-users: list auto-sync history failed platform_user=%s err=%v", pid, err)
				return
			}
			s.autoMarkUserHistory(ctx, userID, adminAccountID, pid, all)
		}(platformUserID)
	}
	wg.Wait()
}

// mergeGiftAmounts 合并手动赠送额度与流水标记（gift+rebate）。
// 标记优先作为自动来源；同一用户取 max(manual, marked)，避免漏扣也避免简单相加双计。
func mergeGiftAmounts(manual map[string]float64, marked map[string]float64) map[string]float64 {
	out := make(map[string]float64)
	for k, v := range manual {
		if v > 0 {
			out[k] = v
		}
	}
	for k, v := range marked {
		if v <= 0 {
			continue
		}
		if cur, ok := out[k]; !ok || v > cur {
			out[k] = v
		}
	}
	return out
}
