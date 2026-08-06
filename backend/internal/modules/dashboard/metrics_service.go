package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// UpstreamLister 抽象上游站点列表读取，由 upstream.Service 实现。
// 仪表盘只需要读取已同步的站点数据，不需要修改或触发同步。
// List 用于用户请求路径（自动使用当前工作区），
// ListForAccount 用于后台调度等需要显式指定工作区的内部流程。
type UpstreamLister interface {
	List(ctx context.Context, userID string) []upstream.Response
	ListForAccount(ctx context.Context, userID, adminAccountID string) []upstream.Response
	// KeyUsageToday 和 BalanceBreakdown 是「今日成本」「上游总余额」下钻弹窗的数据源，
	// 由 upstream.Service 实现（持有 session/cache，能校验站点归属和当前工作区）。
	KeyUsageToday(ctx context.Context, userID string) ([]upstream.KeyUsageTodayItem, error)
	BalanceBreakdown(ctx context.Context, userID string) ([]upstream.BalanceBreakdownItem, error)
	// PurchaseOnDate 按业务日回查上游实际成本（各站实际消费 × rechargeRate）。
	// 午夜快照回填昨天必须走这里，不能读已翻日的 TodayConsume 缓存。
	// 命名保留 Purchase 以兼容历史调用方；语义是「今日成本」不是「进货」。
	PurchaseOnDate(ctx context.Context, userID, adminAccountID, date string) (float64, error)
	// TodayInbound / InboundOnDate 汇总账本已确认进货（成本口径）。
	TodayInbound(ctx context.Context, userID string) (float64, error)
	InboundOnDate(ctx context.Context, userID, adminAccountID, date string) (float64, error)
	// InboundBreakdownToday 按上游站点汇总今日已确认进货（下钻弹窗）。
	InboundBreakdownToday(ctx context.Context, userID string) (upstream.InboundBreakdownResponse, error)
}

// PricingMappingSource 提供「自有分组 → 上游分组」只读映射，供上游分组利润估算使用。
// 由 my_sites 适配器注入；未注入时上游分组仅用同名自有倍率或全站利润率回退。
type PricingMappingSource interface {
	ListPricingTargetLinks(ctx context.Context, userID string) ([]PricingTargetLink, error)
}

// MetricsService 负责仪表盘指标的实时计算、历史快照存储与午夜调度。
// 与同包的 Service（admin 会话管理）职责分离，共享 SessionStore 和 PlatformClient。
type MetricsService struct {
	store            SessionStore
	platform         PlatformClient
	upstreams        UpstreamLister
	metricsRepo      *MetricsRepository
	accounts         AdminAccountService
	sessionSync      MySiteStateSync
	pricingMappings  PricingMappingSource
}

func (s *MetricsService) SetMySiteSync(sync MySiteStateSync) {
	s.sessionSync = sync
}

func (s *MetricsService) SetPricingMappingSource(source PricingMappingSource) {
	s.pricingMappings = source
}

func (s *MetricsService) freshAdminSession(ctx context.Context, userID string, adminAccountID string, record *AdminSession) (upstream.Session, error) {
	if s.sessionSync != nil {
		stored, exists, err := s.sessionSync.StoredSession(ctx, userID, adminAccountID)
		if err != nil {
			return upstream.Session{}, err
		}
		if !exists || sessionAppearsNewer(record.Session, stored) {
			if err := s.sessionSync.SyncAdminSession(ctx, userID, adminAccountID, record.Session, record.Identity); err != nil {
				return upstream.Session{}, err
			}
		}
		canonical, err := s.sessionSync.RequireSession(ctx, userID, adminAccountID)
		if err != nil {
			return upstream.Session{}, err
		}
		if !sessionEqual(canonical, record.Session) {
			record.Session = canonical
			record.LastRefreshedAt = nowMillis()
			if err := s.store.Save(ctx, userID, adminAccountID, *record); err != nil {
				return upstream.Session{}, err
			}
		}
		return canonical, nil
	}

	refreshed, err := s.platform.RefreshSession(record.Session)
	if err != nil {
		return upstream.Session{}, err
	}
	if !sessionEqual(refreshed, record.Session) {
		record.Session = refreshed
		record.LastRefreshedAt = nowMillis()
		if err := s.store.Save(ctx, userID, adminAccountID, *record); err != nil {
			return upstream.Session{}, err
		}
	}
	return refreshed, nil
}

func NewMetricsService(store SessionStore, platform PlatformClient, upstreams UpstreamLister, metricsRepo *MetricsRepository, accounts AdminAccountService) *MetricsService {
	return &MetricsService{store: store, platform: platform, upstreams: upstreams, metricsRepo: metricsRepo, accounts: accounts}
}

// LiveMetrics 实时计算五项核心指标并返回。
// 同时将当天的指标作为快照 upsert 到数据库，确保趋势图数据持续积累。
//
// 计算逻辑：
//   - todayProfit:     管理员站点今日总实际消费，通过 sub2api /api/v1/admin/usage/stats 获取
//   - siteBalance:     管理员站点所有非 admin 用户余额之和，通过 sub2api /api/v1/admin/users 分页求和
//   - todayCost / todayPurchase: 所有上游站点今日消费 × 站点倍率之和（复用缓存；语义=成本）
//   - todayInbound:    账本 confirmed 进货合计（与成本分列）
//   - upstreamBalance: 上游预存备付（不含预授信账面）
//   - netProfit:       todayProfit - todayCost
func (s *MetricsService) LiveMetrics(ctx context.Context, userID string) (MetricsResponse, error) {
	// 获取并校验 admin 会话（平台感知：sub2api 检查 AccessToken，new-api 检查 Cookie+UserID）。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return MetricsResponse{}, err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return MetricsResponse{}, err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return MetricsResponse{}, requestError(ErrorAdminOnly)
	}

	// 如有必要先刷新令牌（new-api 不使用 refresh token，RefreshSession 会直接返回原会话）。
	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return MetricsResponse{}, requestError(ErrorAdminOnly)
	}

	// 校验 admin 角色（平台中性）。
	if err := s.platform.VerifyAdmin(session); err != nil {
		return MetricsResponse{}, requestError(ErrorAdminOnly)
	}

	// 并行获取四项独立数据：今日盈利、站点余额、分组数量、上游指标。
	// 各 goroutine 出错只记日志、降级为零值，不阻塞整体返回。
	// 业务日统一使用 Asia/Shanghai，避免容器 UTC 把 00:00-08:00 的中国日切错位。
	today := upstream.BusinessToday()
	var (
		todayProfit                float64
		siteBalance                float64
		siteRevenueBalance         float64
		siteBalancePlatform        float64
		siteRevenueBalancePlatform float64
		siteRechargeRate           = 1.0
		siteGiftPlat               float64
		siteRebatePlat             float64
		siteRechargePlat           float64
		groupCount                 int
		todayCost                  float64
		todayInbound               float64
		upstreamPrepaid            float64
		upstreamCreditRef          float64
		wg                         sync.WaitGroup
	)

	// goroutine 1: 今日盈利额度（平台中性）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		profit, err := s.platform.FetchAdminUsageStats(session, today, today)
		if err != nil {
			log.Printf("dashboard metrics: fetch usage stats failed user_id=%s err=%v", userID, err)
			return
		}
		todayProfit = profit
	}()

	// goroutine 2: 站点用户余额双口径（成本侧含赠送；营收侧扣赠送/返利）。
	// 营收扣除 = max(手动赠送额度, 流水标记 gift+rebate 合计)。
	wg.Add(1)
	go func() {
		defer wg.Done()
		filterConfig, err := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
		if err != nil {
			log.Printf("dashboard metrics: load balance filter failed user_id=%s err=%v, using defaults", userID, err)
			filterConfig = BalanceFilterConfig{ExcludeAdmin: true, ExcludeBalances: []float64{}, ExcludeUserIDs: []string{}, UserGiftAmounts: map[string]float64{}}
		}
		giftAmounts := filterConfig.UserGiftAmounts
		if marked, markErr := s.metricsRepo.SumNonRevenueByPlatformUser(ctx, userID, adminAccountID); markErr == nil {
			giftAmounts = mergeGiftAmounts(filterConfig.UserGiftAmounts, marked)
		}
		balanceResult, err := s.platform.FetchAdminSiteBalanceFiltered(session, upstream.BalanceFilter{
			ExcludeAdmin:    filterConfig.ExcludeAdmin,
			ExcludeBalances: filterConfig.ExcludeBalances,
			ExcludeUserIDs:  filterConfig.ExcludeUserIDs,
			UserGiftAmounts: giftAmounts,
		})
		if err != nil {
			log.Printf("dashboard metrics: fetch site balance failed user_id=%s err=%v", userID, err)
			return
		}
		rate := filterConfig.SiteRechargeRate
		if rate <= 0 {
			rate = 1
		}
		siteRechargeRate = rate
		costPlat := balanceResult.CostBalance
		if costPlat == 0 && balanceResult.Balance != 0 {
			costPlat = balanceResult.Balance
		}
		siteBalancePlatform = costPlat
		siteRevenueBalancePlatform = balanceResult.RevenueBalance
		// 平台余额 × 工作区充值倍率 → 成本/CNY 口径（与上游备付一致）
		siteBalance = costPlat * rate
		siteRevenueBalance = balanceResult.RevenueBalance * rate

		// 全站流水标记：累计赠送 / 返利 / 充值，与兑付余额同一套排除（指定用户 + 可选 admin）
		excludeIDs := s.collectBalanceExcludedUserIDs(ctx, session, filterConfig)
		if gift, rebate, recharge, sumErr := s.metricsRepo.SumTagsByWorkspace(ctx, userID, adminAccountID, excludeIDs); sumErr == nil {
			siteGiftPlat = gift
			siteRebatePlat = rebate
			siteRechargePlat = recharge
		} else {
			log.Printf("dashboard metrics: sum user topup tags failed user_id=%s err=%v", userID, sumErr)
		}
	}()

	// goroutine 3: 管理员站点分组数量（平台中性）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		groups, err := s.platform.FetchAdminAllGroups(session)
		if err != nil {
			log.Printf("dashboard metrics: fetch admin groups failed user_id=%s err=%v", userID, err)
			return
		}
		groupCount = len(groups)
	}()

	// goroutine 4: 今日成本与上游备付（缓存）。
	// 预存站余额计入 prepaid；预授信站账面只进 creditReference，不进覆盖率分子。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, site := range s.upstreams.List(ctx, userID) {
			if site.RechargeRate <= 0 {
				continue
			}
			if site.Metrics.TodayConsume.Value != nil {
				todayCost += *site.Metrics.TodayConsume.Value * site.RechargeRate
			}
			if site.Metrics.Balance.Value == nil {
				continue
			}
			costBal := *site.Metrics.Balance.Value * site.RechargeRate
			if site.Settings.CountsAsPrepaidReserve() {
				upstreamPrepaid += costBal
			} else {
				upstreamCreditRef += costBal
			}
		}
	}()

	// goroutine 5: 今日进货（账本 confirmed topup）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		inbound, err := s.upstreams.TodayInbound(ctx, userID)
		if err != nil {
			log.Printf("dashboard metrics: today inbound failed user_id=%s err=%v", userID, err)
			return
		}
		todayInbound = inbound
	}()

	wg.Wait()

	netProfit := todayProfit - todayCost
	coverageApplicable := siteBalance > 0
	var coverageRatio *float64
	if coverageApplicable {
		ratio := (upstreamPrepaid / siteBalance) * 100
		coverageRatio = &ratio
	}

	result := MetricsResponse{
		TodayProfit:                todayProfit,
		SiteBalance:                siteBalance,
		TodayPurchase:              todayCost, // 兼容：todayPurchase 现语义=今日成本
		TodayCost:                  todayCost,
		TodayInbound:               todayInbound,
		NetProfit:                  netProfit,
		UpstreamBalance:            upstreamPrepaid,
		GroupCount:                 groupCount,
		SiteRevenueBalance:         siteRevenueBalance,
		UpstreamPrepaidBalance:     upstreamPrepaid,
		UpstreamCreditReference:    upstreamCreditRef,
		CoverageApplicable:         coverageApplicable,
		CoverageRatio:              coverageRatio,
		SiteRechargeRate:           siteRechargeRate,
		SiteBalancePlatform:        siteBalancePlatform,
		SiteRevenueBalancePlatform: siteRevenueBalancePlatform,
		SiteGiftTotalPlatform:      siteGiftPlat,
		SiteRebateTotalPlatform:    siteRebatePlat,
		SiteRechargeTotalPlatform:  siteRechargePlat,
		SiteGiftTotal:              siteGiftPlat * siteRechargeRate,
		SiteRebateTotal:            siteRebatePlat * siteRechargeRate,
		SiteRechargeTotal:          siteRechargePlat * siteRechargeRate,
	}

	// 将当天指标 upsert 到数据库，即使部分指标获取失败也保存已有数据，
	// 后续调用会用更完整的数据覆盖。
	s.upsertSnapshot(ctx, userID, adminAccountID, today, result)

	return result, nil
}

// Trends 查询历史趋势数据，返回最近 days 天的每日快照（不含当天）。
// 当天的数据由前端通过 LiveMetrics 获取后追加到序列末尾。
func (s *MetricsService) Trends(ctx context.Context, userID string, days int) (TrendResponse, error) {
	if days != 7 && days != 30 {
		days = 7
	}
	// 按当前工作区过滤趋势数据。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return TrendResponse{}, err
	}
	snapshots, err := s.metricsRepo.ListRange(ctx, userID, adminAccountID, days)
	if err != nil {
		return TrendResponse{}, err
	}
	points := make([]TrendPoint, 0, len(snapshots))
	for _, snap := range snapshots {
		points = append(points, TrendPoint{
			Date:            snap.Date.Format("2006-01-02"),
			TodayProfit:     snap.TodayProfit,
			SiteBalance:     snap.SiteBalance,
			TodayPurchase:   snap.TodayPurchase,
			TodayCost:       snap.TodayPurchase,
			TodayInbound:    snap.TodayInbound,
			NetProfit:       snap.NetProfit,
			UpstreamBalance: snap.UpstreamBalance,
		})
	}
	return TrendResponse{Points: points}, nil
}

// StartScheduler 启动午夜快照调度协程。
// 每天午夜（Asia/Shanghai 时区）为所有活跃 admin 用户保存当天的指标快照，
// 确保即使用户当天未访问仪表盘，趋势图也不会出现空缺。
func (s *MetricsService) StartScheduler(ctx context.Context) {
	go func() {
		loc, err := time.LoadLocation("Asia/Shanghai")
		if err != nil {
			log.Printf("dashboard scheduler: failed to load Asia/Shanghai timezone, using UTC: %v", err)
			loc = time.UTC
		}

		for {
			now := time.Now().In(loc)
			nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
			timer := time.NewTimer(time.Until(nextMidnight))

			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				s.snapshotAll(ctx)
			}
		}
	}()
}

// snapshotAll 遍历所有活跃 admin 用户，为昨天的业务日保存终态指标快照。
// 午夜（Asia/Shanghai）执行时：
//   - 盈利额度：按昨天日期查询 admin usage stats（平台中性）
//   - 进货额度：按昨天日期回查各上游站点实际消费，而不是读已翻日的 TodayConsume
//   - 余额：取当前值（余额不按天重置）
//
// 即使白天 LiveMetrics 已写入昨天快照，午夜仍会用历史查询覆盖为终态。
// 白天快照可能只是中午访问时的中间值；日切后 TodayConsume 也会归零，
// 对含订阅分组的上游尤其容易把前一天成本写错。
//
// 注意：此方法是后台调度路径，使用 ListForAccount / PurchaseOnDate 显式传入
// adminAccountID，不依赖当前工作区上下文。
func (s *MetricsService) snapshotAll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("dashboard scheduler panic recovered: %v", r)
		}
	}()

	refs, err := s.store.ActiveSessions(ctx)
	if err != nil {
		log.Printf("dashboard scheduler: list active users failed: %v", err)
		return
	}

	loc := upstream.BusinessLocation()
	yesterday := time.Now().In(loc).AddDate(0, 0, -1).Format("2006-01-02")

	for _, ref := range refs {
		userID := ref.UserID
		adminAccountID := ref.AdminAccountID

		record, err := s.store.Get(ctx, userID, adminAccountID)
		if err != nil || record == nil || !record.Session.IsAuthenticated() {
			continue
		}

		session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
		if err != nil {
			log.Printf("dashboard scheduler: refresh session failed user_id=%s err=%v", userID, err)
			continue
		}

		// 昨日盈利（平台中性，按业务日查询）。
		todayProfit, err := s.platform.FetchAdminUsageStats(session, yesterday, yesterday)
		if err != nil {
			log.Printf("dashboard scheduler: fetch usage stats failed user_id=%s err=%v", userID, err)
			todayProfit = 0
		}

		// 站点用户总余额（平台中性）。
		var siteBalance float64
		filterCfg, _ := s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
		if result, err := s.platform.FetchAdminSiteBalanceFiltered(session, upstream.BalanceFilter{
			ExcludeAdmin:    filterCfg.ExcludeAdmin,
			ExcludeBalances: filterCfg.ExcludeBalances,
		}); err == nil {
			siteBalance = result.Balance
		}

		// 昨日成本：按业务日回查上游实际消费，避免日切后 TodayConsume 归零或订阅站口径漂移。
		// 失败时绝不能回退到 TodayConsume——午夜后缓存已是新一天，写进去会污染昨天快照。
		todayCost, err := s.upstreams.PurchaseOnDate(ctx, userID, adminAccountID, yesterday)
		if err != nil {
			log.Printf("dashboard scheduler: fetch yesterday cost failed user_id=%s err=%v", userID, err)
			todayCost = 0
		}

		// 昨日进货：账本 confirmed topup（与成本分列）。
		todayInbound, err := s.upstreams.InboundOnDate(ctx, userID, adminAccountID, yesterday)
		if err != nil {
			log.Printf("dashboard scheduler: fetch yesterday inbound failed user_id=%s err=%v", userID, err)
			todayInbound = 0
		}

		var upstreamBalance float64
		for _, site := range s.upstreams.ListForAccount(ctx, userID, adminAccountID) {
			if site.RechargeRate <= 0 {
				continue
			}
			if site.Metrics.Balance.Value != nil {
				// 午夜快照上游余额：仅预存站计入（与 LiveMetrics 一致）
				if site.Settings.CountsAsPrepaidReserve() {
					upstreamBalance += *site.Metrics.Balance.Value * site.RechargeRate
				}
			}
		}

		result := MetricsResponse{
			TodayProfit:     todayProfit,
			SiteBalance:     siteBalance,
			TodayPurchase:   todayCost,
			TodayCost:       todayCost,
			TodayInbound:    todayInbound,
			NetProfit:       todayProfit - todayCost,
			UpstreamBalance: upstreamBalance,
		}
		s.upsertSnapshot(ctx, userID, adminAccountID, yesterday, result)
		log.Printf("dashboard scheduler: snapshot saved user_id=%s admin_account_id=%s date=%s cost=%.6f inbound=%.6f", userID, adminAccountID, yesterday, todayCost, todayInbound)
	}
}

// upsertSnapshot 将指标写入 dashboard_daily_stats 表。
// 冲突时更新已有行，保证同一天内多次调用始终保留最新数据。
func (s *MetricsService) upsertSnapshot(ctx context.Context, userID, adminAccountID, date string, metrics MetricsResponse) {
	parsedDate, err := time.Parse("2006-01-02", date)
	if err != nil {
		log.Printf("dashboard metrics: invalid date %s: %v", date, err)
		return
	}
	id, err := metricsRandomID()
	if err != nil {
		log.Printf("dashboard metrics: generate id failed: %v", err)
		return
	}
	cost := metrics.TodayCost
	if cost == 0 && metrics.TodayPurchase != 0 {
		cost = metrics.TodayPurchase
	}
	snapshot := DailySnapshot{
		ID:              id,
		UserID:          userID,
		AdminAccountID:  adminAccountID,
		Date:            parsedDate,
		TodayProfit:     metrics.TodayProfit,
		SiteBalance:     metrics.SiteBalance,
		TodayPurchase:   cost,
		TodayInbound:    metrics.TodayInbound,
		NetProfit:       metrics.NetProfit,
		UpstreamBalance: metrics.UpstreamBalance,
		CreatedAt:       time.Now(),
	}
	if err := s.metricsRepo.Upsert(ctx, snapshot); err != nil {
		log.Printf("dashboard metrics: upsert snapshot failed user_id=%s date=%s err=%v", userID, date, err)
	}
}

// AdminGroups 获取管理员站点的所有分组列表（平台中性）。
func (s *MetricsService) AdminGroups(ctx context.Context, userID string) (AdminGroupsResponse, error) {
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return AdminGroupsResponse{}, err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return AdminGroupsResponse{}, err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return AdminGroupsResponse{}, requestError(ErrorAdminOnly)
	}

	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return AdminGroupsResponse{}, requestError(ErrorAdminOnly)
	}

	groups, err := s.platform.FetchAdminGroups(session)
	if err != nil {
		return AdminGroupsResponse{}, err
	}

	items := make([]AdminGroupItem, 0, len(groups))
	for _, g := range groups {
		platform := ""
		if g.Platform != nil {
			platform = *g.Platform
		}
		items = append(items, AdminGroupItem{
			ID:         g.ID,
			Name:       g.Name,
			Platform:   platform,
			Multiplier: g.MultiplierDisplay,
		})
	}
	return AdminGroupsResponse{Count: len(items), Groups: items}, nil
}

// GroupUsageToday 获取当前工作区「我的站点」所有分组今日的使用额度（平台中性）。
// 数据只在弹窗打开时按需请求，不参与 LiveMetrics 的批量指标计算。
func (s *MetricsService) GroupUsageToday(ctx context.Context, userID string) (GroupUsageTodayResponse, error) {
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}
	record, err := s.store.Get(ctx, userID, adminAccountID)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}
	if record == nil || !record.Session.IsAuthenticated() {
		return GroupUsageTodayResponse{}, requestError(ErrorAdminOnly)
	}

	session, err := s.freshAdminSession(ctx, userID, adminAccountID, record)
	if err != nil {
		return GroupUsageTodayResponse{}, requestError(ErrorAdminOnly)
	}

	if err := s.platform.VerifyAdmin(session); err != nil {
		return GroupUsageTodayResponse{}, requestError(ErrorAdminOnly)
	}

	groups, err := s.platform.FetchAdminGroups(session)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}

	stats, err := s.platform.FetchAdminGroupDailyStats(session, groups)
	if err != nil {
		return GroupUsageTodayResponse{}, err
	}

	// 归一化：分组名去空格、空名跳过、重名分组合并求和；顺序按首次出现排列。
	order := make([]string, 0, len(stats))
	totals := make(map[string]float64, len(stats))
	for _, stat := range stats {
		name := strings.TrimSpace(stat.GroupName)
		if name == "" {
			continue
		}
		if _, exists := totals[name]; !exists {
			order = append(order, name)
		}
		totals[name] += stat.TodayActualCost
	}

	items := make([]GroupUsageTodayItem, 0, len(order))
	var total float64
	for _, name := range order {
		amount := totals[name]
		items = append(items, GroupUsageTodayItem{GroupName: name, TodayAmount: amount})
		total += amount
	}

	return GroupUsageTodayResponse{
		Date:   upstream.BusinessToday(),
		Total:  total,
		Groups: items,
	}, nil
}

// GroupProfitToday 返回今天有营收/有上游成本的各分组利润与利润率（仪表盘「今日净利润 / 今日利润率」下钻）。
//
// 口径：
//   - 营收：与 GroupUsageToday 相同（admin 站点分组今日实际消费）
//   - 自有分组成本：该分组关联的上游 key 实耗之和（调价映射「自有分组 → 上游分组」；
//     无映射时回退同名自有分组）。同一上游分到多个自有分组时均分，避免重复计入。
//     与分组健康列表「上游分组消耗」及本接口 upstreamGroups.cost 同源。
//   - 总成本：优先 key 用量合计；不可用时回退站点 TodayConsume × 充值倍率。
//   - 利润：营收 − 成本；只返回 revenue > 0 或 cost > 0 的自有分组。
func (s *MetricsService) GroupProfitToday(ctx context.Context, userID string) (GroupProfitTodayResponse, error) {
	usage, err := s.GroupUsageToday(ctx, userID)
	if err != nil {
		return GroupProfitTodayResponse{}, err
	}

	// 售卖倍率：与分组列表同源，失败时降级为 1x，不阻塞利润弹窗。
	multipliers := map[string]float64{}
	if adminAccountID, accErr := s.requireCurrentAdminAccount(ctx, userID); accErr == nil {
		if record, getErr := s.store.Get(ctx, userID, adminAccountID); getErr == nil && record != nil && record.Session.IsAuthenticated() {
			if session, sessErr := s.freshAdminSession(ctx, userID, adminAccountID, record); sessErr == nil {
				if groups, groupsErr := s.platform.FetchAdminGroups(session); groupsErr == nil {
					for _, g := range groups {
						name := strings.TrimSpace(g.Name)
						if name == "" || g.Multiplier == nil || !isPositiveFinite(*g.Multiplier) {
							continue
						}
						multipliers[name] = *g.Multiplier
					}
				} else {
					log.Printf("dashboard group profit: fetch admin groups failed user_id=%s err=%v", userID, groupsErr)
				}
			}
		}
	}

	// 已知自有分组名：今日有营收的 + 有售卖倍率的（用于映射回退与展示）。
	knownOwn := map[string]struct{}{}
	for name := range multipliers {
		if strings.TrimSpace(name) != "" {
			knownOwn[strings.TrimSpace(name)] = struct{}{}
		}
	}
	type workingGroup struct {
		name       string
		revenue    float64
		multiplier float64
		cost       float64
	}
	byName := map[string]*workingGroup{}
	var totalRevenue float64
	for _, item := range usage.Groups {
		revenue := item.TodayAmount
		if revenue < 0 || !isFinite(revenue) {
			continue
		}
		name := strings.TrimSpace(item.GroupName)
		if name == "" {
			continue
		}
		knownOwn[name] = struct{}{}
		mult := multipliers[name]
		if !isPositiveFinite(mult) {
			mult = 1
		}
		byName[name] = &workingGroup{name: name, revenue: revenue, multiplier: mult}
		if revenue > 0 {
			totalRevenue += revenue
		}
	}

	// 上游 key 实耗 + 调价映射：自有分组成本与 upstreamGroups 共用一次采集。
	keyCosts, reverseOwn, keyTotalCost, keyPartial, keyOK := s.collectUpstreamKeyCosts(ctx, userID)
	ownCostByName := allocateOwnGroupCostsFromUpstream(keyCosts, reverseOwn, knownOwn)
	for name, g := range byName {
		g.cost = ownCostByName[name]
	}
	// 有上游成本但今日无营收的自有分组也要展示（成本/利润可见）。
	for name, cost := range ownCostByName {
		if cost <= 0 {
			continue
		}
		if _, exists := byName[name]; exists {
			continue
		}
		if _, known := knownOwn[name]; !known {
			// 仅当映射明确指向该自有名时才会进入 ownCostByName；再补一条展示行。
			knownOwn[name] = struct{}{}
		}
		mult := multipliers[name]
		if !isPositiveFinite(mult) {
			mult = 1
		}
		byName[name] = &workingGroup{name: name, revenue: 0, multiplier: mult, cost: cost}
	}

	// 站点级总成本作 key 不可用时的回退；有 key 数据时以 key 合计为准，与上游分组合计对齐。
	var siteTotalCost float64
	if s.upstreams != nil {
		for _, site := range s.upstreams.List(ctx, userID) {
			if site.RechargeRate <= 0 {
				continue
			}
			if site.Metrics.TodayConsume.Value != nil {
				siteTotalCost += *site.Metrics.TodayConsume.Value * site.RechargeRate
			}
		}
	}
	totalCost := siteTotalCost
	if keyOK {
		totalCost = keyTotalCost
	}

	items := make([]GroupProfitTodayItem, 0, len(byName))
	for _, g := range byName {
		if g.revenue <= 0 && g.cost <= 0 {
			continue
		}
		profit := g.revenue - g.cost
		margin := 0.0
		if g.revenue > 0 {
			margin = profit / g.revenue
		}
		multCopy := g.multiplier
		items = append(items, GroupProfitTodayItem{
			GroupName:      g.name,
			Revenue:        g.revenue,
			Cost:           g.cost,
			Profit:         profit,
			ProfitMargin:   margin,
			SaleMultiplier: &multCopy,
		})
	}

	// 默认按利润从高到低，便于运营一眼看到贡献最大的分组。
	sort.Slice(items, func(i, j int) bool {
		if items[i].Profit != items[j].Profit {
			return items[i].Profit > items[j].Profit
		}
		return items[i].GroupName < items[j].GroupName
	})

	totalProfit := totalRevenue - totalCost
	totalMargin := 0.0
	if totalRevenue > 0 {
		totalMargin = totalProfit / totalRevenue
	}

	upstreamGroups, upstreamPartial := s.buildUpstreamGroupProfits(ctx, userID, multipliers, totalRevenue, totalCost, totalMargin, keyCosts, reverseOwn)
	if keyPartial {
		upstreamPartial = true
	}

	return GroupProfitTodayResponse{
		Date:            usage.Date,
		TotalRevenue:    totalRevenue,
		TotalCost:       totalCost,
		TotalProfit:     totalProfit,
		TotalMargin:     totalMargin,
		Groups:          items,
		UpstreamGroups:  upstreamGroups,
		UpstreamPartial: upstreamPartial,
	}, nil
}

// upstreamKeyCostRow 是 (站点, 上游分组) 维度的 key 实耗合计。
type upstreamKeyCostRow struct {
	siteID       string
	siteName     string
	platform     string
	groupName    string
	cost         float64
	rechargeRate float64
}

// collectUpstreamKeyCosts 采集今日上游 key 实耗，并构建「上游 → 自有分组」映射。
// ok=false 表示完全采不到 key 用量（自有成本需回退其它策略时用）。
func (s *MetricsService) collectUpstreamKeyCosts(ctx context.Context, userID string) (
	rows []upstreamKeyCostRow,
	reverseOwn map[string][]string,
	totalCost float64,
	partial bool,
	ok bool,
) {
	reverseOwn = map[string][]string{}
	if s.upstreams == nil {
		return nil, reverseOwn, 0, false, false
	}

	keyItems, err := s.upstreams.KeyUsageToday(ctx, userID)
	if err != nil {
		var collectionErr *upstream.KeyUsageCollectionError
		if !errors.As(err, &collectionErr) || collectionErr.TotalSites <= 0 || collectionErr.FailedSites >= collectionErr.TotalSites {
			log.Printf("dashboard group profit: upstream key usage unavailable user_id=%s err=%v", userID, err)
			return nil, reverseOwn, 0, false, false
		}
		partial = true
		log.Printf("dashboard group profit: partial upstream key usage user_id=%s failed=%d total=%d", userID, collectionErr.FailedSites, collectionErr.TotalSites)
	}

	order := make([]string, 0)
	byKey := make(map[string]*upstreamKeyCostRow)
	for _, item := range keyItems {
		if item.TodayAmount <= 0 || !isFinite(item.TodayAmount) {
			continue
		}
		groupName := strings.TrimSpace(item.GroupName)
		if groupName == "" {
			groupName = "Ungrouped"
		}
		siteID := strings.TrimSpace(item.SiteID)
		key := siteID + "\x00" + groupName
		row, exists := byKey[key]
		if !exists {
			row = &upstreamKeyCostRow{
				siteID:       siteID,
				siteName:     item.SiteName,
				platform:     string(item.Platform),
				groupName:    groupName,
				rechargeRate: item.RechargeRate,
			}
			byKey[key] = row
			order = append(order, key)
		}
		row.cost += item.TodayAmount
		if row.siteName == "" {
			row.siteName = item.SiteName
		}
		if row.platform == "" {
			row.platform = string(item.Platform)
		}
		if row.rechargeRate <= 0 && item.RechargeRate > 0 {
			row.rechargeRate = item.RechargeRate
		}
	}

	rows = make([]upstreamKeyCostRow, 0, len(order))
	for _, key := range order {
		row := byKey[key]
		if row == nil || row.cost <= 0 {
			continue
		}
		rows = append(rows, *row)
		totalCost += row.cost
	}

	if s.pricingMappings != nil {
		if links, linkErr := s.pricingMappings.ListPricingTargetLinks(ctx, userID); linkErr != nil {
			log.Printf("dashboard group profit: pricing mappings failed user_id=%s err=%v", userID, linkErr)
		} else {
			seenOwn := map[string]map[string]struct{}{}
			for _, link := range links {
				own := strings.TrimSpace(link.OwnGroup)
				siteID := strings.TrimSpace(link.SiteID)
				gName := strings.TrimSpace(link.GroupName)
				if own == "" || siteID == "" || gName == "" {
					continue
				}
				key := siteID + "\x00" + gName
				if seenOwn[key] == nil {
					seenOwn[key] = map[string]struct{}{}
				}
				if _, exists := seenOwn[key][own]; exists {
					continue
				}
				seenOwn[key][own] = struct{}{}
				reverseOwn[key] = append(reverseOwn[key], own)
			}
		}
	}

	return rows, reverseOwn, totalCost, partial, true
}

// allocateOwnGroupCostsFromUpstream 把上游 key 实耗归到自有分组。
// 优先调价映射；无映射时仅当存在同名「已知自有分组」才整笔计入。
// 一上游对多自有时均分，避免同一笔成本被重复加总。
func allocateOwnGroupCostsFromUpstream(
	rows []upstreamKeyCostRow,
	reverseOwn map[string][]string,
	knownOwn map[string]struct{},
) map[string]float64 {
	out := map[string]float64{}
	for _, row := range rows {
		if row.cost <= 0 {
			continue
		}
		key := row.siteID + "\x00" + row.groupName
		mapped := reverseOwn[key]
		if len(mapped) == 0 {
			// 无映射：仅同名且确为自有分组时回退，绝不把陌生上游组名当成自有组。
			name := strings.TrimSpace(row.groupName)
			if name == "" {
				continue
			}
			if _, ok := knownOwn[name]; !ok {
				continue
			}
			mapped = []string{name}
		}
		// 过滤映射到未知空名的项。
		targets := make([]string, 0, len(mapped))
		for _, own := range mapped {
			own = strings.TrimSpace(own)
			if own == "" {
				continue
			}
			targets = append(targets, own)
		}
		if len(targets) == 0 {
			continue
		}
		share := row.cost / float64(len(targets))
		for _, own := range targets {
			out[own] += share
		}
	}
	return out
}

// buildUpstreamGroupProfits 聚合今日有消耗的上游分组，并估算利润/利润率。
// 口径与调价映射预算毛利率一致：margin = (sale - costMult) / sale；
// revenue = cost * sale / costMult（cost 已是 key 实际成本）。缺倍率时回退全站利润率。
// keyCosts / reverseOwn 由调用方 collectUpstreamKeyCosts 提供，避免重复拉 key 用量。
func (s *MetricsService) buildUpstreamGroupProfits(
	ctx context.Context,
	userID string,
	saleByOwnGroup map[string]float64,
	totalRevenue, totalCost, totalMargin float64,
	keyCosts []upstreamKeyCostRow,
	reverseOwn map[string][]string,
) ([]UpstreamGroupProfitTodayItem, bool) {
	if s.upstreams == nil || len(keyCosts) == 0 {
		return []UpstreamGroupProfitTodayItem{}, false
	}
	if reverseOwn == nil {
		reverseOwn = map[string][]string{}
	}

	// 上游分组倍率：站点缓存 Metrics.Groups × rechargeRate → 有效成本倍率。
	costMultByKey := map[string]float64{}
	siteNameByID := map[string]string{}
	for _, site := range s.upstreams.List(ctx, userID) {
		siteNameByID[site.ID] = site.Name
		if site.RechargeRate <= 0 {
			continue
		}
		for _, g := range site.Metrics.Groups {
			name := strings.TrimSpace(g.Name)
			if name == "" || g.Multiplier == nil || !isPositiveFinite(*g.Multiplier) {
				continue
			}
			costMultByKey[site.ID+"\x00"+name] = *g.Multiplier * site.RechargeRate
		}
	}

	items := make([]UpstreamGroupProfitTodayItem, 0, len(keyCosts))
	for _, row := range keyCosts {
		if row.cost <= 0 {
			continue
		}
		siteName := row.siteName
		if siteName == "" {
			if name := siteNameByID[row.siteID]; name != "" {
				siteName = name
			} else {
				siteName = row.siteID
			}
		}
		key := row.siteID + "\x00" + row.groupName
		mapped := append([]string(nil), reverseOwn[key]...)
		var salePtr *float64
		if sale, ok := resolveUpstreamSaleMultiplier(row.groupName, mapped, saleByOwnGroup); ok {
			v := sale
			salePtr = &v
		}

		var costMultPtr *float64
		if costMult, ok := costMultByKey[key]; ok && isPositiveFinite(costMult) {
			v := costMult
			costMultPtr = &v
		}

		revenue, profit, margin := estimateUpstreamProfit(row.cost, salePtr, costMultPtr, totalRevenue, totalCost, totalMargin)
		items = append(items, UpstreamGroupProfitTodayItem{
			SiteID:          row.siteID,
			SiteName:        siteName,
			Platform:        row.platform,
			GroupName:       row.groupName,
			Cost:            row.cost,
			Revenue:         revenue,
			Profit:          profit,
			ProfitMargin:    margin,
			SaleMultiplier:  salePtr,
			CostMultiplier:  costMultPtr,
			MappedOwnGroups: mapped,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Profit != items[j].Profit {
			return items[i].Profit > items[j].Profit
		}
		if items[i].Cost != items[j].Cost {
			return items[i].Cost > items[j].Cost
		}
		if items[i].SiteName != items[j].SiteName {
			return items[i].SiteName < items[j].SiteName
		}
		return items[i].GroupName < items[j].GroupName
	})
	return items, false
}

// resolveUpstreamSaleMultiplier 优先用调价映射关联的自有分组倍率均值，其次同名自有分组。
func resolveUpstreamSaleMultiplier(upstreamGroup string, mappedOwn []string, saleByOwnGroup map[string]float64) (float64, bool) {
	var sum float64
	var n int
	for _, own := range mappedOwn {
		if m, ok := saleByOwnGroup[strings.TrimSpace(own)]; ok && isPositiveFinite(m) {
			sum += m
			n++
		}
	}
	if n > 0 {
		return sum / float64(n), true
	}
	if m, ok := saleByOwnGroup[strings.TrimSpace(upstreamGroup)]; ok && isPositiveFinite(m) {
		return m, true
	}
	return 0, false
}

// estimateUpstreamProfit 估算上游分组营收/利润/利润率。
// 优先：margin=(sale-costMult)/sale，revenue=cost*sale/costMult（与调价映射预算毛利率一致）。
// 回退：用全站利润率把该上游成本反推营收，使无映射分组仍可展示。
func estimateUpstreamProfit(
	cost float64,
	saleMult, costMult *float64,
	totalRevenue, totalCost, totalMargin float64,
) (revenue, profit, margin float64) {
	if saleMult != nil && costMult != nil && isPositiveFinite(*saleMult) && isPositiveFinite(*costMult) {
		margin = (*saleMult - *costMult) / *saleMult
		revenue = cost * (*saleMult) / (*costMult)
		profit = revenue - cost
		return revenue, profit, margin
	}
	if totalRevenue > 0 && isFinite(totalMargin) {
		margin = totalMargin
		// revenue - cost = margin * revenue  =>  revenue = cost / (1 - margin)
		if margin < 1 {
			denom := 1 - margin
			if denom > 1e-12 {
				revenue = cost / denom
				profit = revenue - cost
				return revenue, profit, margin
			}
		}
		// 利润率 ≥ 100% 的极端情况：按成本占比分摊全站利润。
		if totalCost > 0 {
			profit = (totalRevenue - totalCost) * (cost / totalCost)
			revenue = cost + profit
			if revenue > 0 {
				margin = profit / revenue
			}
			return revenue, profit, margin
		}
	}
	// 无任何参照：只展示成本，利润按 0 营收下的 -cost。
	return 0, -cost, 0
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func isPositiveFinite(v float64) bool {
	return isFinite(v) && v > 0
}

// UpstreamKeyUsageToday 获取当前工作区所有上游站点中，今天有消费的 key 明细（仪表盘「今日成本」下钻）。
// 数据只在弹窗打开时按需请求，不参与 LiveMetrics 的批量指标计算。
// 排序、总额与筛选逻辑全部由 upstream.Service.KeyUsageToday 保证与 todayPurchase 口径一致，
// 这里只负责排序展示和响应封装。
func (s *MetricsService) UpstreamKeyUsageToday(ctx context.Context, userID string) (UpstreamKeyUsageTodayResponse, error) {
	items, err := s.upstreams.KeyUsageToday(ctx, userID)
	failedSites := 0
	totalSites := 0
	if err != nil {
		var collectionErr *upstream.KeyUsageCollectionError
		if !errors.As(err, &collectionErr) || collectionErr.TotalSites <= 0 || collectionErr.FailedSites >= collectionErr.TotalSites {
			return UpstreamKeyUsageTodayResponse{}, requestError(ErrorUpstreamKeyUsageUnavailable)
		}
		failedSites = collectionErr.FailedSites
		totalSites = collectionErr.TotalSites
		log.Printf("dashboard key usage: partial upstream failure user_id=%s failed_sites=%d total_sites=%d", userID, failedSites, totalSites)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].TodayAmount > items[j].TodayAmount
	})

	responseItems := make([]UpstreamKeyUsageTodayItem, 0, len(items))
	var total float64
	for _, item := range items {
		responseItems = append(responseItems, UpstreamKeyUsageTodayItem{
			SiteID:       item.SiteID,
			SiteName:     item.SiteName,
			Platform:     string(item.Platform),
			KeyID:        item.KeyID,
			KeyName:      item.KeyName,
			GroupName:    item.GroupName,
			TodayAmount:  item.TodayAmount,
			RawAmount:    item.RawAmount,
			RechargeRate: item.RechargeRate,
		})
		total += item.TodayAmount
	}

	return UpstreamKeyUsageTodayResponse{
		Date:        upstream.BusinessToday(),
		Total:       total,
		Keys:        responseItems,
		FailedSites: failedSites,
		TotalSites:  totalSites,
	}, nil
}

// TodayInboundBreakdown 获取今日进货按上游站点汇总（仪表盘「今日进货」下钻）。
func (s *MetricsService) TodayInboundBreakdown(ctx context.Context, userID string) (TodayInboundBreakdownResponse, error) {
	raw, err := s.upstreams.InboundBreakdownToday(ctx, userID)
	if err != nil {
		return TodayInboundBreakdownResponse{}, err
	}
	items := make([]TodayInboundBreakdownItem, 0, len(raw.Sites))
	for _, site := range raw.Sites {
		items = append(items, TodayInboundBreakdownItem{
			SiteID:       site.SiteID,
			SiteName:     site.SiteName,
			Platform:     site.Platform,
			AmountCost:   site.AmountCost,
			EntryCount:   site.EntryCount,
			RechargeRate: site.RechargeRate,
		})
	}
	return TodayInboundBreakdownResponse{
		Date:  raw.Date,
		Total: raw.Total,
		Sites: items,
	}, nil
}

// UpstreamBalanceBreakdown 获取当前工作区所有上游站点的余额明细（仪表盘「上游总余额」下钻）。
// total 只合计预存类站点（prepaid），与 LiveMetrics.upstreamBalance / 覆盖率分子一致；
// 预授信站点单独标 credit_reference，不进 total。
func (s *MetricsService) UpstreamBalanceBreakdown(ctx context.Context, userID string) (UpstreamBalanceBreakdownResponse, error) {
	items, err := s.upstreams.BalanceBreakdown(ctx, userID)
	if err != nil {
		return UpstreamBalanceBreakdownResponse{}, err
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Balance == nil || items[j].Balance == nil {
			return items[i].Balance != nil
		}
		return *items[i].Balance > *items[j].Balance
	})

	responseItems := make([]UpstreamBalanceBreakdownItem, 0, len(items))
	var total float64
	for _, item := range items {
		responseItems = append(responseItems, UpstreamBalanceBreakdownItem{
			SiteID:         item.SiteID,
			SiteName:       item.SiteName,
			Platform:       string(item.Platform),
			Balance:        item.Balance,
			RawBalance:     item.RawBalance,
			RechargeRate:   item.RechargeRate,
			LastSyncedAt:   item.LastSyncedAt,
			Status:         string(item.Status),
			SettlementMode: item.SettlementMode,
			ReserveKind:    item.ReserveKind,
		})
		if item.Balance != nil && item.ReserveKind == "prepaid" {
			total += *item.Balance
		}
	}

	return UpstreamBalanceBreakdownResponse{
		Total: total,
		Sites: responseItems,
	}, nil
}

// collectBalanceExcludedUserIDs 与兑付余额同一套排除：配置的整户排除 +（可选）全部 admin 用户 ID。
func (s *MetricsService) collectBalanceExcludedUserIDs(ctx context.Context, session upstream.Session, filter BalanceFilterConfig) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(filter.ExcludeUserIDs)+8)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range filter.ExcludeUserIDs {
		add(id)
	}
	if !filter.ExcludeAdmin {
		return out
	}
	reader, ok := s.siteUserReader()
	if !ok || session.Platform != upstream.PlatformSub2API {
		return out
	}
	// 分页拉取 role=admin，并入排除名单（与余额汇总排除 admin 对齐）
	const pageSize = 100
	const maxPages = 30
	for page := 1; page <= maxPages; page++ {
		pageData, err := reader.FetchSub2APIAdminUsersPage(session, upstream.Sub2APIAdminUsersQuery{
			Page:      page,
			PageSize:  pageSize,
			Role:      "admin",
			SortBy:    "created_at",
			SortOrder: "desc",
		})
		if err != nil {
			log.Printf("dashboard metrics: list admin users for exclude failed err=%v", err)
			break
		}
		for _, u := range pageData.Items {
			add(u.ID)
		}
		if len(pageData.Items) < pageSize {
			break
		}
		if pageData.Total > 0 && page*pageSize >= pageData.Total {
			break
		}
	}
	return out
}

// GetBalanceFilter 读取当前用户当前工作区的余额筛选配置。
func (s *MetricsService) GetBalanceFilter(ctx context.Context, userID string) (BalanceFilterConfig, error) {
	// 按当前工作区隔离筛选配置。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return BalanceFilterConfig{}, err
	}
	return s.metricsRepo.GetBalanceFilter(ctx, userID, adminAccountID)
}

// SaveBalanceFilter 保存用户当前工作区的余额筛选配置。
func (s *MetricsService) SaveBalanceFilter(ctx context.Context, userID string, config BalanceFilterConfig) error {
	// 按当前工作区隔离筛选配置。
	adminAccountID, err := s.requireCurrentAdminAccount(ctx, userID)
	if err != nil {
		return err
	}
	config.UserID = userID
	config.AdminAccountID = adminAccountID
	return s.metricsRepo.SaveBalanceFilter(ctx, config)
}

func (s *MetricsService) requireCurrentAdminAccount(ctx context.Context, userID string) (string, error) {
	if s.accounts == nil {
		return "", requestError(ErrorAdminOnly)
	}
	return s.accounts.RequireCurrentID(ctx, userID)
}

func metricsRandomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
