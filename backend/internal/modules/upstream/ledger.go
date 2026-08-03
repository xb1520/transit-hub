package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 账本 kind / source / status 常量。
const (
	LedgerKindConsume           = "consume"            // 遗留：不再写入，列表过滤
	LedgerKindTopupBalance      = "topup_balance"      // 充值（计入今日进货）
	LedgerKindTopupSubscription = "topup_subscription" // 订阅实付（计入今日进货）
	LedgerKindTopupManual       = "topup_manual"       // 遗留手填
	LedgerKindAdjust            = "adjust"
	LedgerKindGift              = "gift"   // 赠送（不计进货成本）
	LedgerKindRebate            = "rebate" // 返利（不计进货成本）

	// 标记标签（API 入参）
	LedgerTagRecharge = "recharge"
	LedgerTagGift     = "gift"
	LedgerTagRebate   = "rebate"

	LedgerSourceAuto   = "auto"
	LedgerSourceManual = "manual"
	LedgerSourceHybrid = "hybrid"

	LedgerStatusConfirmed = "confirmed"
	LedgerStatusPending   = "pending"
	LedgerStatusVoided    = "voided"
)

// 自动检测时忽略小于此阈值的浮点抖动（上游原始单位）。
const ledgerAutoEpsilon = 0.0001

// LedgerRecord 上游进货/成本流水。
type LedgerRecord struct {
	ID                   string    `json:"id"`
	UserID               string    `json:"-"`
	AdminAccountID       string    `json:"-"`
	SiteID               string    `json:"siteId"`
	Kind                 string    `json:"kind"`
	AmountCost           float64   `json:"amountCost"`
	AmountPlatform       *float64  `json:"amountPlatform,omitempty"`
	Source               string    `json:"source"`
	Status               string    `json:"status"`
	BusinessDate         string    `json:"businessDate"` // 2006-01-02
	Ref                  string    `json:"ref"`
	Note                 string    `json:"note"`
	OperatorUserID       string    `json:"operatorUserId"`
	RechargeRateSnapshot *float64  `json:"rechargeRateSnapshot,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

// CreateLedgerInput 遗留手填入参（API 已下线，保留类型兼容）。
type CreateLedgerInput struct {
	Kind           string   `json:"kind"`
	AmountCost     float64  `json:"amountCost"`
	AmountPlatform *float64 `json:"amountPlatform,omitempty"`
	Note           string   `json:"note"`
	BusinessDate   string   `json:"businessDate,omitempty"`
	Status         string   `json:"status,omitempty"`
}

// MarkRechargeInput 对平台充值流水打标签。
type MarkRechargeInput struct {
	PlatformRecordID string   `json:"platformRecordId"`
	Tag              string   `json:"tag"` // recharge | gift | rebate
	AmountPlatform   *float64 `json:"amountPlatform,omitempty"`
	Note             string   `json:"note,omitempty"`
	CreatedAt        string   `json:"createdAt,omitempty"` // RFC3339 或日期，用于业务日
}

// RechargeCandidate 平台充值候选 + 本地标记。
type RechargeCandidate struct {
	PlatformID      string  `json:"platformId"`
	PlatformType    string  `json:"platformType,omitempty"`
	AmountPlatform  float64 `json:"amountPlatform"`
	AmountCost      float64 `json:"amountCost"`
	Note            string  `json:"note,omitempty"`
	CreatedAt       *string `json:"createdAt,omitempty"`
	SuggestedTag    string  `json:"suggestedTag,omitempty"` // recharge | gift | rebate
	Tag             string  `json:"tag,omitempty"`          // 已标记
	LedgerID        string  `json:"ledgerId,omitempty"`
	CountsAsInbound bool    `json:"countsAsInbound"`
	BusinessDate    string  `json:"businessDate,omitempty"`
}

// RechargeCandidatesResponse 站点充值记录列表（后端全量拉取后按 page 切片返回）。
type RechargeCandidatesResponse struct {
	Items        []RechargeCandidate `json:"items"`
	Available    bool                `json:"available"` // 平台是否提供流水接口
	RechargeRate float64             `json:"rechargeRate"`
	MessageKey   string              `json:"messageKey,omitempty"`
	Platform     string              `json:"platform,omitempty"`
	Page         int                 `json:"page"`
	PageSize     int                 `json:"pageSize"`
	Total        int                 `json:"total"` // 全量条数（过滤后）

	// 口径对照（均为成本口径 = 上游金额 × 充值倍率）
	// PlatformLifetimeCost：卡片「历史充值」同源（metrics.historyRecharge × rate），含赠送/返利等累计入账
	// DetailListCost：本次拉到的平台明细合计（未区分标签）
	// MarkedRechargeCost / MarkedGiftCost / MarkedRebateCost：按标签汇总
	PlatformLifetimeCost *float64 `json:"platformLifetimeCost,omitempty"`
	DetailListCost       float64  `json:"detailListCost"`
	MarkedRechargeCost   float64  `json:"markedRechargeCost"`
	MarkedGiftCost       float64  `json:"markedGiftCost"`
	MarkedRebateCost     float64  `json:"markedRebateCost"`
	// GapLifetimeVsMarked = PlatformLifetimeCost - MarkedRechargeCost（有 lifetime 时）
	GapLifetimeVsMarked *float64 `json:"gapLifetimeVsMarked,omitempty"`
}

// InboundBreakdownItem 仪表盘「今日进货」下钻：按上游站点汇总。
type InboundBreakdownItem struct {
	SiteID       string  `json:"siteId"`
	SiteName     string  `json:"siteName"`
	Platform     string  `json:"platform"`
	AmountCost   float64 `json:"amountCost"`
	EntryCount   int     `json:"entryCount"`
	RechargeRate float64 `json:"rechargeRate"`
}

// InboundBreakdownResponse 今日进货下钻响应。
type InboundBreakdownResponse struct {
	Date  string                 `json:"date"`
	Total float64                `json:"total"`
	Sites []InboundBreakdownItem `json:"sites"`
}

// LedgerRepository 持久化 upstream_ledger。
type LedgerRepository struct {
	db *pgxpool.Pool
}

func NewLedgerRepository(db *pgxpool.Pool) *LedgerRepository {
	return &LedgerRepository{db: db}
}

func (r *LedgerRepository) EnsureSchema(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	_, err := r.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS upstream_ledger (
			id text PRIMARY KEY,
			user_id text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			site_id text NOT NULL,
			kind text NOT NULL,
			amount_cost double precision NOT NULL,
			amount_platform double precision,
			source text NOT NULL DEFAULT 'manual',
			status text NOT NULL DEFAULT 'confirmed',
			business_date date NOT NULL,
			ref text NOT NULL DEFAULT '',
			note text NOT NULL DEFAULT '',
			operator_user_id text NOT NULL DEFAULT '',
			recharge_rate_snapshot double precision,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_upstream_ledger_workspace_date
			ON upstream_ledger (user_id, admin_account_id, business_date DESC, status);
		CREATE INDEX IF NOT EXISTS idx_upstream_ledger_site_date
			ON upstream_ledger (site_id, business_date DESC);
		-- 自动/标记流水按 ref 去重（不含 kind，便于充值↔赠送改标签）
		DROP INDEX IF EXISTS idx_upstream_ledger_auto_ref;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_ledger_auto_ref
			ON upstream_ledger (site_id, ref)
			WHERE source = 'auto' AND ref <> '';
	`)
	return err
}

func (r *LedgerRepository) Insert(ctx context.Context, record LedgerRecord) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO upstream_ledger (
			id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::date,$11,$12,$13,$14,$15,$16)
	`, record.ID, record.UserID, record.AdminAccountID, record.SiteID, record.Kind, record.AmountCost,
		record.AmountPlatform, record.Source, record.Status, record.BusinessDate, record.Ref, record.Note,
		record.OperatorUserID, record.RechargeRateSnapshot, record.CreatedAt, record.UpdatedAt)
	return err
}

// UpsertAutoByRef 按 (site, kind, ref) 幂等写入自动流水；已存在则更新金额与状态。
func (r *LedgerRepository) UpsertAutoByRef(ctx context.Context, record LedgerRecord) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO upstream_ledger (
			id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::date,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (site_id, ref) WHERE source = 'auto' AND ref <> ''
		DO UPDATE SET
			kind = EXCLUDED.kind,
			amount_cost = EXCLUDED.amount_cost,
			amount_platform = EXCLUDED.amount_platform,
			-- 已作废的自动流水不再复活
			status = CASE
				WHEN upstream_ledger.status = 'voided' THEN upstream_ledger.status
				ELSE EXCLUDED.status
			END,
			note = EXCLUDED.note,
			recharge_rate_snapshot = EXCLUDED.recharge_rate_snapshot,
			business_date = EXCLUDED.business_date,
			updated_at = EXCLUDED.updated_at
		WHERE upstream_ledger.status <> 'voided'
	`, record.ID, record.UserID, record.AdminAccountID, record.SiteID, record.Kind, record.AmountCost,
		record.AmountPlatform, record.Source, record.Status, record.BusinessDate, record.Ref, record.Note,
		record.OperatorUserID, record.RechargeRateSnapshot, record.CreatedAt, record.UpdatedAt)
	return err
}

func (r *LedgerRepository) ListBySite(ctx context.Context, userID, adminAccountID, siteID string, limit int) ([]LedgerRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date::text, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
		FROM upstream_ledger
		WHERE user_id = $1 AND admin_account_id = $2 AND site_id = $3
			AND kind <> $5
		ORDER BY business_date DESC, created_at DESC
		LIMIT $4
	`, userID, adminAccountID, siteID, limit, LedgerKindConsume)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLedgerRows(rows)
}

// MapByRef 返回站点下非 voided 流水的 ref → 记录映射（用于平台流水打标合并）。
func (r *LedgerRepository) MapByRef(ctx context.Context, userID, adminAccountID, siteID string) (map[string]LedgerRecord, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date::text, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
		FROM upstream_ledger
		WHERE user_id = $1 AND admin_account_id = $2 AND site_id = $3
			AND status <> $4 AND ref <> '' AND kind <> $5
	`, userID, adminAccountID, siteID, LedgerStatusVoided, LedgerKindConsume)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanLedgerRows(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]LedgerRecord, len(items))
	for _, item := range items {
		out[item.Ref] = item
	}
	return out, nil
}

// SumInboundConfirmedBySites 按站点汇总业务日已确认「进货」（仅充值/订阅/手填/调整，不含赠送/返利/消耗）。
func (r *LedgerRepository) SumInboundConfirmedBySites(ctx context.Context, userID, adminAccountID, businessDate string) (map[string]struct {
	Total float64
	Count int
}, error) {
	rows, err := r.db.Query(ctx, `
		SELECT site_id, COALESCE(SUM(amount_cost), 0), COUNT(*)
		FROM upstream_ledger
		WHERE user_id = $1 AND admin_account_id = $2 AND business_date = $3::date
			AND status = $4
			AND kind IN ($5, $6, $7, $8)
		GROUP BY site_id
	`, userID, adminAccountID, businessDate, LedgerStatusConfirmed,
		LedgerKindTopupBalance, LedgerKindTopupSubscription, LedgerKindTopupManual, LedgerKindAdjust)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct {
		Total float64
		Count int
	})
	for rows.Next() {
		var siteID string
		var total float64
		var count int
		if err := rows.Scan(&siteID, &total, &count); err != nil {
			return nil, err
		}
		out[siteID] = struct {
			Total float64
			Count int
		}{Total: total, Count: count}
	}
	return out, rows.Err()
}

func (r *LedgerRepository) ListByWorkspaceDate(
	ctx context.Context,
	userID, adminAccountID, businessDate string,
	kinds []string,
	statuses []string,
	limit int,
) ([]LedgerRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if len(kinds) == 0 {
		kinds = []string{
			LedgerKindTopupBalance, LedgerKindTopupSubscription, LedgerKindTopupManual, LedgerKindAdjust,
		}
	}
	if len(statuses) == 0 {
		statuses = []string{LedgerStatusConfirmed}
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date::text, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
		FROM upstream_ledger
		WHERE user_id = $1 AND admin_account_id = $2 AND business_date = $3::date
			AND kind = ANY($4) AND status = ANY($5)
		ORDER BY created_at DESC
		LIMIT $6
	`, userID, adminAccountID, businessDate, kinds, statuses, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLedgerRows(rows)
}

func (r *LedgerRepository) SumTopupsConfirmed(ctx context.Context, userID, adminAccountID, businessDate string) (float64, error) {
	var total float64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cost), 0)
		FROM upstream_ledger
		WHERE user_id = $1 AND admin_account_id = $2 AND business_date = $3::date
			AND status = $4
			AND kind IN ($5, $6, $7, $8)
	`, userID, adminAccountID, businessDate, LedgerStatusConfirmed,
		LedgerKindTopupBalance, LedgerKindTopupSubscription, LedgerKindTopupManual, LedgerKindAdjust,
	).Scan(&total)
	return total, err
}

func (r *LedgerRepository) SumTopupsConfirmedBySite(ctx context.Context, userID, adminAccountID, siteID, businessDate string) (float64, error) {
	var total float64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_cost), 0)
		FROM upstream_ledger
		WHERE user_id = $1 AND admin_account_id = $2 AND site_id = $3 AND business_date = $4::date
			AND status = $5
			AND kind IN ($6, $7, $8, $9)
	`, userID, adminAccountID, siteID, businessDate, LedgerStatusConfirmed,
		LedgerKindTopupBalance, LedgerKindTopupSubscription, LedgerKindTopupManual, LedgerKindAdjust,
	).Scan(&total)
	return total, err
}

func (r *LedgerRepository) GetByID(ctx context.Context, userID, adminAccountID, siteID, recordID string) (*LedgerRecord, error) {
	row := r.db.QueryRow(ctx, `
		SELECT id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date::text, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
		FROM upstream_ledger
		WHERE id = $1 AND user_id = $2 AND admin_account_id = $3 AND site_id = $4
	`, recordID, userID, adminAccountID, siteID)
	item, err := scanLedgerRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newRequestError(ErrorNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *LedgerRepository) Confirm(ctx context.Context, userID, adminAccountID, siteID, recordID string) (*LedgerRecord, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE upstream_ledger
		SET status = $1, updated_at = now()
		WHERE id = $2 AND user_id = $3 AND admin_account_id = $4 AND site_id = $5 AND status = $6
		RETURNING id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date::text, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
	`, LedgerStatusConfirmed, recordID, userID, adminAccountID, siteID, LedgerStatusPending)
	item, err := scanLedgerRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newRequestError(ErrorNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func (r *LedgerRepository) Void(ctx context.Context, userID, adminAccountID, siteID, recordID string) (*LedgerRecord, error) {
	row := r.db.QueryRow(ctx, `
		UPDATE upstream_ledger
		SET status = $1, updated_at = now()
		WHERE id = $2 AND user_id = $3 AND admin_account_id = $4 AND site_id = $5 AND status IN ($6, $7)
		RETURNING id, user_id, admin_account_id, site_id, kind, amount_cost, amount_platform,
			source, status, business_date::text, ref, note, operator_user_id, recharge_rate_snapshot,
			created_at, updated_at
	`, LedgerStatusVoided, recordID, userID, adminAccountID, siteID, LedgerStatusConfirmed, LedgerStatusPending)
	item, err := scanLedgerRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newRequestError(ErrorNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

type ledgerScanner interface {
	Scan(dest ...any) error
}

func scanLedgerRow(row ledgerScanner) (LedgerRecord, error) {
	var item LedgerRecord
	err := row.Scan(
		&item.ID, &item.UserID, &item.AdminAccountID, &item.SiteID, &item.Kind, &item.AmountCost, &item.AmountPlatform,
		&item.Source, &item.Status, &item.BusinessDate, &item.Ref, &item.Note, &item.OperatorUserID, &item.RechargeRateSnapshot,
		&item.CreatedAt, &item.UpdatedAt,
	)
	return item, err
}

func scanLedgerRows(rows pgx.Rows) ([]LedgerRecord, error) {
	items := make([]LedgerRecord, 0)
	for rows.Next() {
		item, err := scanLedgerRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func newLedgerID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ldg_" + hex.EncodeToString(buf), nil
}

// ─── Service 方法 ───────────────────────────────────────────

func (s *Service) SetLedgerRepository(repo *LedgerRepository) {
	s.ledgerRepo = repo
}

func (s *Service) ensureLedgerRepo() error {
	if s.ledgerRepo == nil {
		return errors.New("upstream: ledger repository not configured")
	}
	return nil
}

// ListLedger 列出站点进货标记流水（不含消耗）。
func (s *Service) ListLedger(ctx context.Context, userID, siteID string, limit int) ([]LedgerRecord, error) {
	if err := s.ensureLedgerRepo(); err != nil {
		return nil, err
	}
	_, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return nil, err
	}
	return s.ledgerRepo.ListBySite(ctx, userID, aid, siteID, limit)
}

// ListRechargeCandidates 全量拉取平台入账流水并合并本地标记，再按 page/pageSize 切片返回。
// page 从 1 起；pageSize 默认 20，最大 100。
func (s *Service) ListRechargeCandidates(ctx context.Context, userID, siteID string, page, pageSize int) (RechargeCandidatesResponse, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	site, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return RechargeCandidatesResponse{}, err
	}
	rate := site.RechargeRate
	if rate <= 0 {
		rate = 0
	}
	resp := RechargeCandidatesResponse{
		Items:        []RechargeCandidate{},
		RechargeRate: site.RechargeRate,
		Platform:     string(site.Platform),
		Page:         page,
		PageSize:     pageSize,
	}
	if site.Session == nil || !site.Session.IsAuthenticated() {
		resp.MessageKey = "admin.upstream.ledger.needSession"
		return resp, nil
	}

	// 全量拉取（内部按平台分页）
	history, err := s.platformService.FetchSiteBalanceHistory(*site.Session, 1, 0)
	if err != nil {
		log.Printf("upstream ledger: fetch platform history failed site_id=%s platform=%s err=%v", siteID, site.Platform, err)
		resp.MessageKey = "admin.upstream.ledger.historyUnavailable"
		return resp, nil
	}
	if history.Platform != "" {
		resp.Platform = string(history.Platform)
	}
	resp.Available = true

	// 打开即自动打标全量未标记流水（预授信不自动进货）
	if rate > 0 && !site.Settings.IsCreditLine() {
		s.autoMarkHistoryItems(ctx, userID, aid, siteID, rate, history.Items, false)
	}

	marks := map[string]LedgerRecord{}
	if s.ledgerRepo != nil {
		if m, mapErr := s.ledgerRepo.MapByRef(ctx, userID, aid, siteID); mapErr == nil {
			marks = m
		}
	}

	all := make([]RechargeCandidate, 0, len(history.Items))
	for _, item := range history.Items {
		if item.Amount == nil || *item.Amount <= 0 || !isFinite(*item.Amount) {
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
		ref := platformLedgerRef(platformID)
		costRate := rate
		if costRate <= 0 {
			costRate = 1
		}
		cand := RechargeCandidate{
			PlatformID:     platformID,
			PlatformType:   item.Type,
			AmountPlatform: *item.Amount,
			AmountCost:     *item.Amount * costRate,
			Note:           item.Note,
			SuggestedTag:   suggestRechargeTag(item),
		}
		if item.CreatedAt != nil {
			ts := item.CreatedAt.Format(time.RFC3339)
			cand.CreatedAt = &ts
			cand.BusinessDate = item.CreatedAt.In(BusinessLocation()).Format("2006-01-02")
		}
		if mark, ok := marks[ref]; ok {
			cand.Tag = kindToTag(mark.Kind)
			cand.LedgerID = mark.ID
			cand.CountsAsInbound = mark.Status == LedgerStatusConfirmed && kindCountsAsInbound(mark.Kind)
			cand.AmountCost = mark.AmountCost
			if mark.BusinessDate != "" {
				cand.BusinessDate = mark.BusinessDate
			}
		}
		all = append(all, cand)
	}

	// 口径对照汇总（全量，与当前页无关）
	costRate := rate
	if costRate <= 0 {
		costRate = 1
	}
	if site.Metrics.HistoryRecharge.Value != nil && isFinite(*site.Metrics.HistoryRecharge.Value) {
		v := *site.Metrics.HistoryRecharge.Value * costRate
		resp.PlatformLifetimeCost = &v
	}
	for _, c := range all {
		// 明细列表金额：优先用平台原始 × 倍率（与未改标时一致）
		detailCost := c.AmountPlatform * costRate
		if detailCost <= 0 {
			detailCost = c.AmountCost
		}
		resp.DetailListCost += detailCost
		switch c.Tag {
		case LedgerTagRecharge:
			resp.MarkedRechargeCost += c.AmountCost
		case LedgerTagGift:
			resp.MarkedGiftCost += c.AmountCost
		case LedgerTagRebate:
			resp.MarkedRebateCost += c.AmountCost
		}
	}
	if resp.PlatformLifetimeCost != nil {
		gap := *resp.PlatformLifetimeCost - resp.MarkedRechargeCost
		resp.GapLifetimeVsMarked = &gap
	}

	resp.Total = len(all)
	start := (page - 1) * pageSize
	if start >= len(all) {
		resp.Items = []RechargeCandidate{}
		return resp, nil
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	resp.Items = all[start:end]
	return resp, nil
}

// CreateSubscriptionTopupInput 手填订阅实付进货。
// BusinessDate 应为订阅开通日或续费日（业务时区日历日），计入该日「今日进货」。
type CreateSubscriptionTopupInput struct {
	SubscriptionID string   `json:"subscriptionId"`
	GroupName      string   `json:"groupName"`
	AmountCost     float64  `json:"amountCost"`
	AmountPlatform *float64 `json:"amountPlatform,omitempty"`
	// BusinessDate 开通/续费日，格式 2006-01-02；也可传 RFC3339，会按业务时区取日。
	BusinessDate string `json:"businessDate"`
	Note         string `json:"note"`
}

// CreateSubscriptionTopup 登记订阅实付进货（topup_subscription）。
// 幂等键：sub:{subscriptionId}:{businessDate}，同订阅同开通/续费日重复提交会更新金额。
func (s *Service) CreateSubscriptionTopup(ctx context.Context, userID, siteID string, input CreateSubscriptionTopupInput) (LedgerRecord, error) {
	if err := s.ensureLedgerRepo(); err != nil {
		return LedgerRecord{}, err
	}
	if input.AmountCost <= 0 || !isFinite(input.AmountCost) {
		return LedgerRecord{}, invalidBodyError("amountCost")
	}
	subID := strings.TrimSpace(input.SubscriptionID)
	if subID == "" {
		return LedgerRecord{}, invalidBodyError("subscriptionId")
	}
	bizDate, ok := parseBusinessDateInput(input.BusinessDate)
	if !ok {
		return LedgerRecord{}, invalidBodyError("businessDate")
	}
	site, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return LedgerRecord{}, err
	}
	rate := site.RechargeRate
	if rate <= 0 {
		rate = 1
	}
	now := time.Now()
	id, err := newLedgerID()
	if err != nil {
		return LedgerRecord{}, err
	}
	groupName := strings.TrimSpace(input.GroupName)
	note := strings.TrimSpace(input.Note)
	if note == "" {
		if groupName != "" {
			note = fmt.Sprintf("subscription topup %s (%s) @ %s", groupName, subID, bizDate)
		} else {
			note = fmt.Sprintf("subscription topup %s @ %s", subID, bizDate)
		}
	}
	ref := subscriptionLedgerRef(subID, bizDate)
	r := rate
	record := LedgerRecord{
		ID:                   id,
		UserID:               userID,
		AdminAccountID:       aid,
		SiteID:               siteID,
		Kind:                 LedgerKindTopupSubscription,
		AmountCost:           input.AmountCost,
		AmountPlatform:       input.AmountPlatform,
		Source:               LedgerSourceAuto, // 走 ref 幂等 upsert
		Status:               LedgerStatusConfirmed,
		BusinessDate:         bizDate,
		Ref:                  ref,
		Note:                 note,
		OperatorUserID:       userID,
		RechargeRateSnapshot: &r,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := s.ledgerRepo.UpsertAutoByRef(ctx, record); err != nil {
		return LedgerRecord{}, err
	}
	marks, err := s.ledgerRepo.MapByRef(ctx, userID, aid, siteID)
	if err != nil {
		return record, nil
	}
	if latest, ok := marks[ref]; ok {
		return latest, nil
	}
	return record, nil
}

func subscriptionLedgerRef(subscriptionID, businessDate string) string {
	return "sub:" + strings.TrimSpace(subscriptionID) + ":" + strings.TrimSpace(businessDate)
}

// parseBusinessDateInput 解析进货业务日：支持 2006-01-02 / RFC3339 / RFC3339Nano。
func parseBusinessDateInput(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if t, err := time.ParseInLocation("2006-01-02", raw, BusinessLocation()); err == nil {
		return t.Format("2006-01-02"), true
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.In(BusinessLocation()).Format("2006-01-02"), true
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.In(BusinessLocation()).Format("2006-01-02"), true
	}
	// 常见无时区时间：2026-03-01T00:00:00
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", raw, BusinessLocation()); err == nil {
		return t.Format("2006-01-02"), true
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, BusinessLocation()); err == nil {
		return t.Format("2006-01-02"), true
	}
	return "", false
}

// MarkRecharge 将平台流水标记为充值/赠送/返利。
func (s *Service) MarkRecharge(ctx context.Context, userID, siteID string, input MarkRechargeInput) (LedgerRecord, error) {
	if err := s.ensureLedgerRepo(); err != nil {
		return LedgerRecord{}, err
	}
	platformID := strings.TrimSpace(input.PlatformRecordID)
	if platformID == "" {
		return LedgerRecord{}, invalidBodyError("platformRecordId")
	}
	kind, ok := tagToKind(strings.TrimSpace(input.Tag))
	if !ok {
		return LedgerRecord{}, invalidBodyError("tag")
	}
	site, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return LedgerRecord{}, err
	}
	rate := site.RechargeRate
	if rate <= 0 {
		rate = 1
	}
	var platformAmt float64
	if input.AmountPlatform != nil && isFinite(*input.AmountPlatform) && *input.AmountPlatform > 0 {
		platformAmt = *input.AmountPlatform
	} else {
		return LedgerRecord{}, invalidBodyError("amountPlatform")
	}
	bizDate := BusinessToday()
	if ts := strings.TrimSpace(input.CreatedAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			bizDate = t.In(BusinessLocation()).Format("2006-01-02")
		} else if t, err := time.Parse("2006-01-02", ts); err == nil {
			bizDate = t.Format("2006-01-02")
		}
	}
	ref := platformLedgerRef(platformID)
	now := time.Now()
	id, err := newLedgerID()
	if err != nil {
		return LedgerRecord{}, err
	}
	p := platformAmt
	r := rate
	note := strings.TrimSpace(input.Note)
	if note == "" {
		note = fmt.Sprintf("mark:%s platform_id=%s", input.Tag, platformID)
	}
	record := LedgerRecord{
		ID:                   id,
		UserID:               userID,
		AdminAccountID:       aid,
		SiteID:               siteID,
		Kind:                 kind,
		AmountCost:           platformAmt * rate,
		AmountPlatform:       &p,
		Source:               LedgerSourceHybrid,
		Status:               LedgerStatusConfirmed,
		BusinessDate:         bizDate,
		Ref:                  ref,
		Note:                 note,
		OperatorUserID:       userID,
		RechargeRateSnapshot: &r,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	// 用 auto 源 + ref 幂等 upsert，便于改标签
	record.Source = LedgerSourceAuto
	if err := s.ledgerRepo.UpsertAutoByRef(ctx, record); err != nil {
		return LedgerRecord{}, err
	}
	// 读回最新
	marks, err := s.ledgerRepo.MapByRef(ctx, userID, aid, siteID)
	if err != nil {
		return record, nil
	}
	if latest, ok := marks[ref]; ok {
		return latest, nil
	}
	return record, nil
}

// InboundBreakdownToday 按上游站点汇总今日已确认进货。
func (s *Service) InboundBreakdownToday(ctx context.Context, userID string) (InboundBreakdownResponse, error) {
	date := BusinessToday()
	resp := InboundBreakdownResponse{Date: date, Sites: []InboundBreakdownItem{}}
	if s.ledgerRepo == nil {
		return resp, nil
	}
	aid, err := s.requireCurrentAdminAccountID(ctx, userID)
	if err != nil {
		return resp, err
	}
	sums, err := s.ledgerRepo.SumInboundConfirmedBySites(ctx, userID, aid, date)
	if err != nil {
		return resp, err
	}
	sites := s.List(ctx, userID)
	byID := make(map[string]Response, len(sites))
	for _, site := range sites {
		byID[site.ID] = site
	}
	for siteID, sum := range sums {
		item := InboundBreakdownItem{
			SiteID:     siteID,
			AmountCost: sum.Total,
			EntryCount: sum.Count,
		}
		if site, ok := byID[siteID]; ok {
			item.SiteName = site.Name
			item.Platform = string(site.Platform)
			item.RechargeRate = site.RechargeRate
		} else {
			item.SiteName = siteID
		}
		resp.Sites = append(resp.Sites, item)
		resp.Total += sum.Total
	}
	// 金额降序
	for i := 0; i < len(resp.Sites); i++ {
		for j := i + 1; j < len(resp.Sites); j++ {
			if resp.Sites[j].AmountCost > resp.Sites[i].AmountCost {
				resp.Sites[i], resp.Sites[j] = resp.Sites[j], resp.Sites[i]
			}
		}
	}
	return resp, nil
}

func platformLedgerRef(platformID string) string {
	return "plat:" + platformID
}

func tagToKind(tag string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case LedgerTagRecharge, "topup", "topup_balance":
		return LedgerKindTopupBalance, true
	case LedgerTagGift:
		return LedgerKindGift, true
	case LedgerTagRebate:
		return LedgerKindRebate, true
	default:
		return "", false
	}
}

func kindToTag(kind string) string {
	switch kind {
	case LedgerKindTopupBalance, LedgerKindTopupManual, LedgerKindTopupSubscription:
		return LedgerTagRecharge
	case LedgerKindGift:
		return LedgerTagGift
	case LedgerKindRebate:
		return LedgerTagRebate
	default:
		return ""
	}
}

func kindCountsAsInbound(kind string) bool {
	switch kind {
	case LedgerKindTopupBalance, LedgerKindTopupSubscription, LedgerKindTopupManual, LedgerKindAdjust:
		return true
	default:
		return false
	}
}

// IsNonBalanceCreditHistoryType 是否为非余额入账（并发/RPM 等额度），不应进余额流水与自动打标。
func IsNonBalanceCreditHistoryType(itemType, note string) bool {
	typ := strings.ToLower(strings.TrimSpace(itemType))
	noteL := strings.ToLower(strings.TrimSpace(note))
	blob := typ + " " + noteL
	switch typ {
	case "concurrency", "admin_concurrency", "rpm", "admin_rpm", "admin_concurrency_quota":
		return true
	}
	if strings.Contains(blob, "admin_concurrency") ||
		(strings.Contains(blob, "concurrency") && !strings.Contains(blob, "balance")) {
		return true
	}
	return false
}

// SuggestHistoryTag 按流水 type/note 建议标签（充值 / 赠送 / 返利）。
// 供上游进货账本与站点用户余额流水共用。非余额类型返回空。
//
// 负向金额（扣款）：绝不默认成赠送收回——仅在文案有强信号时建议类别
// （充值退款→recharge、赠送收回→gift、返利收回→rebate），否则返回空留给人工。
// 汇总时同一 tag 下金额可正可负，代数和即为净额。
func SuggestHistoryTag(itemType, note string, amount float64) string {
	if amount == 0 || !isFinite(amount) {
		return ""
	}
	if IsNonBalanceCreditHistoryType(itemType, note) {
		return ""
	}
	typ := strings.ToLower(strings.TrimSpace(itemType))
	noteL := strings.ToLower(strings.TrimSpace(note))
	blob := typ + " " + noteL

	// 负向：只认明确退款/收回语义，管理员裸扣款不猜
	if amount < 0 {
		return suggestDebitHistoryTag(typ, noteL, blob)
	}

	// 1) 返利：sub2api affiliate 入账 + 文案
	switch typ {
	case "affiliate_balance", "affiliate", "rebate", "invite_rebate":
		return LedgerTagRebate
	}
	if strings.Contains(blob, "affiliate") ||
		strings.Contains(blob, "invite_rebate") ||
		strings.Contains(noteL, "返利") ||
		(strings.Contains(blob, "rebate") && !strings.Contains(blob, "recharge")) {
		return LedgerTagRebate
	}

	// 2) 明确赠送（少见，留给手改为主；仅强信号自动 gift）
	if typ == "gift" || strings.Contains(noteL, "赠送") || strings.Contains(noteL, "赠金") || strings.Contains(noteL, "gift") {
		return LedgerTagGift
	}

	// 3) 其余正向入账一律充值：用户支付、管理员加款、兑换码、订阅实付等
	return LedgerTagRecharge
}

// suggestDebitHistoryTag 负向余额流水的标签建议（保守，默认可空）。
func suggestDebitHistoryTag(typ, noteL, blob string) string {
	// 充值退款 / 退费
	if strings.Contains(noteL, "退款") ||
		strings.Contains(noteL, "退费") ||
		strings.Contains(noteL, "充值退") ||
		strings.Contains(blob, "refund") ||
		strings.Contains(blob, "chargeback") ||
		typ == "refund" || typ == "recharge_refund" {
		return LedgerTagRecharge
	}
	// 赠送收回（必须有赠送语义，不能仅凭「扣除」）
	if strings.Contains(noteL, "收回赠送") ||
		strings.Contains(noteL, "撤销赠送") ||
		strings.Contains(noteL, "扣回赠") ||
		strings.Contains(noteL, "赠送收回") ||
		strings.Contains(noteL, "赠金收回") ||
		strings.Contains(blob, "gift_clawback") ||
		strings.Contains(blob, "clawback gift") ||
		(strings.Contains(noteL, "赠送") && (strings.Contains(noteL, "收回") || strings.Contains(noteL, "撤销") || strings.Contains(noteL, "扣回"))) ||
		(strings.Contains(blob, "gift") && (strings.Contains(blob, "clawback") || strings.Contains(blob, "revoke") || strings.Contains(blob, "reclaim"))) {
		return LedgerTagGift
	}
	// 返利收回
	if strings.Contains(noteL, "收回返利") ||
		strings.Contains(noteL, "撤销返利") ||
		strings.Contains(noteL, "返利收回") ||
		strings.Contains(noteL, "扣回返利") ||
		(strings.Contains(noteL, "返利") && (strings.Contains(noteL, "收回") || strings.Contains(noteL, "撤销") || strings.Contains(noteL, "扣回"))) ||
		((strings.Contains(blob, "affiliate") || strings.Contains(blob, "rebate")) &&
			(strings.Contains(blob, "clawback") || strings.Contains(blob, "revoke") || strings.Contains(blob, "reclaim"))) {
		return LedgerTagRebate
	}
	return ""
}

// suggestRechargeTag 按 sub2api 流水类型自动建议标签。
func suggestRechargeTag(item Sub2APIBalanceHistoryItem) string {
	if item.Amount == nil {
		return ""
	}
	return SuggestHistoryTag(item.Type, item.Note, *item.Amount)
}

func tagToLedgerKind(tag string) string {
	kind, ok := tagToKind(tag)
	if !ok {
		return LedgerKindTopupBalance
	}
	return kind
}

// TodayInbound 汇总当前工作区业务日已确认进货（成本口径）。
func (s *Service) TodayInbound(ctx context.Context, userID string) (float64, error) {
	if s.ledgerRepo == nil {
		return 0, nil
	}
	aid, err := s.requireCurrentAdminAccountID(ctx, userID)
	if err != nil {
		return 0, err
	}
	return s.ledgerRepo.SumTopupsConfirmed(ctx, userID, aid, BusinessToday())
}

// InboundOnDate 汇总指定工作区业务日已确认进货（供午夜快照）。
func (s *Service) InboundOnDate(ctx context.Context, userID, adminAccountID, date string) (float64, error) {
	if s.ledgerRepo == nil {
		return 0, nil
	}
	return s.ledgerRepo.SumTopupsConfirmed(ctx, userID, adminAccountID, date)
}

// ProcessLedgerAfterSync 同步后按类型自动打标并写入账本：
//   - 返利 → rebate（不计今日进货）
//   - 支付/管理员加款/兑换/订阅等 → recharge（计今日进货）
//   - 明确赠送 → gift（不计进货；极少自动）
//
// 仅处理「今日」业务日，避免历史全量刷进今日；已有标记（含用户手改）由 Upsert 的 voided 保护与 kind 更新策略处理。
// 预授信站跳过。
func (s *Service) ProcessLedgerAfterSync(
	ctx context.Context,
	userID, adminAccountID, siteID string,
	oldMetrics, newMetrics Metrics,
) {
	if s.ledgerRepo == nil {
		return
	}
	site, err := s.cache.Get(ctx, siteID)
	if err != nil || site == nil || site.UserID != userID {
		return
	}
	if site.Settings.IsCreditLine() {
		return
	}
	rate := site.RechargeRate
	if rate <= 0 || site.Session == nil {
		return
	}
	history, err := s.platformService.FetchSiteBalanceHistory(*site.Session, 1, 50)
	if err != nil || len(history.Items) == 0 {
		return
	}
	s.autoMarkHistoryItems(ctx, userID, adminAccountID, siteID, rate, history.Items, true)
	_ = oldMetrics
	_ = newMetrics
}

// autoMarkHistoryItems 将平台流水自动写入账本。onlyToday=true 时只处理业务今日。
// 已有同 ref 标记（含用户手改赠送）一律跳过，不覆盖。
func (s *Service) autoMarkHistoryItems(
	ctx context.Context,
	userID, adminAccountID, siteID string,
	rate float64,
	items []Sub2APIBalanceHistoryItem,
	onlyToday bool,
) {
	if s.ledgerRepo == nil || rate <= 0 {
		return
	}
	existing, err := s.ledgerRepo.MapByRef(ctx, userID, adminAccountID, siteID)
	if err != nil {
		existing = map[string]LedgerRecord{}
	}
	bizToday := BusinessToday()
	now := time.Now()
	for _, item := range items {
		tag := suggestRechargeTag(item)
		if tag == "" {
			continue
		}
		platformID := strings.TrimSpace(item.ID)
		if platformID == "" {
			continue
		}
		ref := platformLedgerRef(platformID)
		if _, exists := existing[ref]; exists {
			continue // 保留手改与既有标记
		}
		bizDate := bizToday
		if item.CreatedAt != nil {
			bizDate = item.CreatedAt.In(BusinessLocation()).Format("2006-01-02")
		}
		if onlyToday && bizDate != bizToday {
			continue
		}
		id, idErr := newLedgerID()
		if idErr != nil {
			continue
		}
		p := *item.Amount
		r := rate
		kind := tagToLedgerKind(tag)
		note := strings.TrimSpace(item.Note)
		if note == "" {
			note = fmt.Sprintf("auto:%s type=%s", tag, item.Type)
		} else {
			note = fmt.Sprintf("auto:%s · %s", tag, note)
		}
		rec := LedgerRecord{
			ID:                   id,
			UserID:               userID,
			AdminAccountID:       adminAccountID,
			SiteID:               siteID,
			Kind:                 kind,
			AmountCost:           p * rate,
			AmountPlatform:       &p,
			Source:               LedgerSourceAuto,
			Status:               LedgerStatusConfirmed,
			BusinessDate:         bizDate,
			Ref:                  ref,
			Note:                 note,
			OperatorUserID:       "",
			RechargeRateSnapshot: &r,
			CreatedAt:            now,
			UpdatedAt:            now,
		}
		if err := s.ledgerRepo.UpsertAutoByRef(ctx, rec); err != nil {
			log.Printf("upstream ledger: auto mark failed site_id=%s ref=%s tag=%s err=%v", siteID, rec.Ref, tag, err)
			continue
		}
		existing[ref] = rec
	}
}

func hasAnyMetricValue(m Metrics) bool {
	return m.Balance.Value != nil || m.TodayConsume.Value != nil || m.HistoryRecharge.Value != nil || m.LifetimeConsume.Value != nil
}

func metricValueOr(m MetricValue, fallback float64) float64 {
	if m.Value == nil || !isFinite(*m.Value) {
		return fallback
	}
	return *m.Value
}

// almostEqual 保留给测试/后续阈值判断。
func almostEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
