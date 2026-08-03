package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SettlementRepository 持久化上游手动结算流水。
type SettlementRepository struct {
	db *pgxpool.Pool
}

func NewSettlementRepository(db *pgxpool.Pool) *SettlementRepository {
	return &SettlementRepository{db: db}
}

func (r *SettlementRepository) EnsureSchema(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	_, err := r.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS upstream_settlement_records (
			id text PRIMARY KEY,
			user_id text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			site_id text NOT NULL,
			amount double precision NOT NULL,
			note text NOT NULL DEFAULT '',
			settled_at timestamptz NOT NULL,
			status text NOT NULL DEFAULT 'active',
			operator_user_id text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_upstream_settlement_site
			ON upstream_settlement_records (user_id, admin_account_id, site_id, settled_at DESC);
		CREATE INDEX IF NOT EXISTS idx_upstream_settlement_status
			ON upstream_settlement_records (site_id, status);
	`)
	return err
}

func (r *SettlementRepository) ListBySite(ctx context.Context, userID, adminAccountID, siteID string, limit int) ([]SettlementRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, site_id, amount, note, settled_at, status, operator_user_id, created_at, updated_at
		FROM upstream_settlement_records
		WHERE user_id = $1 AND admin_account_id = $2 AND site_id = $3
		ORDER BY settled_at DESC, created_at DESC
		LIMIT $4
	`, userID, adminAccountID, siteID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]SettlementRecord, 0)
	for rows.Next() {
		var item SettlementRecord
		if err := rows.Scan(
			&item.ID, &item.UserID, &item.AdminAccountID, &item.SiteID, &item.Amount, &item.Note,
			&item.SettledAt, &item.Status, &item.OperatorUserID, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *SettlementRepository) SumActiveBySite(ctx context.Context, userID, adminAccountID, siteID string) (float64, error) {
	var total float64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM upstream_settlement_records
		WHERE user_id = $1 AND admin_account_id = $2 AND site_id = $3 AND status = $4
	`, userID, adminAccountID, siteID, SettlementStatusActive).Scan(&total)
	return total, err
}

func (r *SettlementRepository) SumActiveBySites(ctx context.Context, userID, adminAccountID string, siteIDs []string) (map[string]float64, error) {
	result := make(map[string]float64, len(siteIDs))
	if len(siteIDs) == 0 {
		return result, nil
	}
	rows, err := r.db.Query(ctx, `
		SELECT site_id, COALESCE(SUM(amount), 0)
		FROM upstream_settlement_records
		WHERE user_id = $1 AND admin_account_id = $2 AND status = $3 AND site_id = ANY($4)
		GROUP BY site_id
	`, userID, adminAccountID, SettlementStatusActive, siteIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var siteID string
		var total float64
		if err := rows.Scan(&siteID, &total); err != nil {
			return nil, err
		}
		result[siteID] = total
	}
	return result, rows.Err()
}

func (r *SettlementRepository) Insert(ctx context.Context, record SettlementRecord) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO upstream_settlement_records (
			id, user_id, admin_account_id, site_id, amount, note, settled_at, status, operator_user_id, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, record.ID, record.UserID, record.AdminAccountID, record.SiteID, record.Amount, record.Note,
		record.SettledAt, record.Status, record.OperatorUserID, record.CreatedAt, record.UpdatedAt)
	return err
}

func (r *SettlementRepository) Void(ctx context.Context, userID, adminAccountID, siteID, recordID string) (*SettlementRecord, error) {
	var item SettlementRecord
	err := r.db.QueryRow(ctx, `
		UPDATE upstream_settlement_records
		SET status = $1, updated_at = now()
		WHERE id = $2 AND user_id = $3 AND admin_account_id = $4 AND site_id = $5 AND status = $6
		RETURNING id, user_id, admin_account_id, site_id, amount, note, settled_at, status, operator_user_id, created_at, updated_at
	`, SettlementStatusVoided, recordID, userID, adminAccountID, siteID, SettlementStatusActive).Scan(
		&item.ID, &item.UserID, &item.AdminAccountID, &item.SiteID, &item.Amount, &item.Note,
		&item.SettledAt, &item.Status, &item.OperatorUserID, &item.CreatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, newRequestError(ErrorNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// Delete 物理删除结算记录（含已作废），用于清理测试数据。
func (r *SettlementRepository) Delete(ctx context.Context, userID, adminAccountID, siteID, recordID string) error {
	tag, err := r.db.Exec(ctx, `
		DELETE FROM upstream_settlement_records
		WHERE id = $1 AND user_id = $2 AND admin_account_id = $3 AND site_id = $4
	`, recordID, userID, adminAccountID, siteID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return newRequestError(ErrorNotFound, "")
	}
	return nil
}

func newSettlementID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "stl_" + hex.EncodeToString(buf), nil
}

// CreateSettlementInput 登记结算请求。
type CreateSettlementInput struct {
	Amount    float64    `json:"amount"`
	Note      string     `json:"note"`
	SettledAt *time.Time `json:"settledAt"`
}

// SettlementService 方法挂在 Service 上，依赖可选的 settlementRepo。
func (s *Service) SetSettlementRepository(repo *SettlementRepository) {
	s.settlementRepo = repo
}

func (s *Service) ensureSettlementRepo() error {
	if s.settlementRepo == nil {
		return errors.New("upstream: settlement repository not configured")
	}
	return nil
}

// ListSettlements 列出站点结算流水。
func (s *Service) ListSettlements(ctx context.Context, userID, siteID string, limit int) ([]SettlementRecord, error) {
	if err := s.ensureSettlementRepo(); err != nil {
		return nil, err
	}
	site, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return nil, err
	}
	_ = site
	return s.settlementRepo.ListBySite(ctx, userID, aid, siteID, limit)
}

// CreateSettlement 手动登记一笔结算。
func (s *Service) CreateSettlement(ctx context.Context, userID, siteID string, input CreateSettlementInput) (SettlementRecord, error) {
	if err := s.ensureSettlementRepo(); err != nil {
		return SettlementRecord{}, err
	}
	if input.Amount <= 0 || !isFinite(input.Amount) {
		return SettlementRecord{}, invalidBodyError("amount")
	}
	site, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return SettlementRecord{}, err
	}
	_ = site
	now := time.Now()
	settledAt := now
	if input.SettledAt != nil && !input.SettledAt.IsZero() {
		settledAt = *input.SettledAt
	}
	id, err := newSettlementID()
	if err != nil {
		return SettlementRecord{}, err
	}
	record := SettlementRecord{
		ID:             id,
		UserID:         userID,
		AdminAccountID: aid,
		SiteID:         siteID,
		Amount:         input.Amount,
		Note:           strings.TrimSpace(input.Note),
		SettledAt:      settledAt,
		Status:         SettlementStatusActive,
		OperatorUserID: userID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.settlementRepo.Insert(ctx, record); err != nil {
		return SettlementRecord{}, err
	}
	return record, nil
}

// VoidSettlement 作废一笔结算（冲正，保留痕迹）。
func (s *Service) VoidSettlement(ctx context.Context, userID, siteID, recordID string) (SettlementRecord, error) {
	if err := s.ensureSettlementRepo(); err != nil {
		return SettlementRecord{}, err
	}
	_, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return SettlementRecord{}, err
	}
	item, err := s.settlementRepo.Void(ctx, userID, aid, siteID, recordID)
	if err != nil {
		return SettlementRecord{}, err
	}
	return *item, nil
}

// DeleteSettlement 物理删除结算记录（含作废记录）。
func (s *Service) DeleteSettlement(ctx context.Context, userID, siteID, recordID string) error {
	if err := s.ensureSettlementRepo(); err != nil {
		return err
	}
	_, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return err
	}
	return s.settlementRepo.Delete(ctx, userID, aid, siteID, recordID)
}

// SettlementSummaryForSite 计算站点结算汇总。
func (s *Service) SettlementSummaryForSite(ctx context.Context, userID, siteID string) (SettlementSummary, error) {
	site, aid, err := s.requireOwnedSite(ctx, userID, siteID)
	if err != nil {
		return SettlementSummary{}, err
	}
	return s.buildSettlementSummary(ctx, userID, aid, site, nil)
}

func (s *Service) buildSettlementSummary(
	ctx context.Context,
	userID, adminAccountID string,
	site *Site,
	settledBySite map[string]float64,
) (SettlementSummary, error) {
	settings := site.Settings
	mode := NormalizeSettlementMode(settings.SettlementMode)
	summary := SettlementSummary{
		Mode:               mode,
		CreditLimit:        settings.CreditLimit,
		SettlementCurrency: strings.TrimSpace(settings.SettlementCurrency),
	}
	if summary.SettlementCurrency == "" {
		summary.SettlementCurrency = "CNY"
	}
	if site.Metrics.Balance.Value != nil {
		v := *site.Metrics.Balance.Value
		summary.PlatformBalance = &v
	}

	// 累计消耗（成本）= 平台 lifetime × 充值倍率
	rate := site.RechargeRate
	if rate <= 0 {
		rate = 0
	}
	if site.Metrics.LifetimeConsume.Value != nil && rate > 0 {
		summary.ConsumedCost = *site.Metrics.LifetimeConsume.Value * rate
	}

	if s.settlementRepo != nil {
		if settledBySite != nil {
			summary.SettledCost = settledBySite[site.ID]
		} else {
			total, err := s.settlementRepo.SumActiveBySite(ctx, userID, adminAccountID, site.ID)
			if err != nil {
				return summary, err
			}
			summary.SettledCost = total
		}
	}
	outstanding := summary.ConsumedCost - summary.SettledCost
	if outstanding < 0 {
		outstanding = 0
	}
	summary.Outstanding = outstanding

	if mode == SettlementModeCreditLine && settings.CreditLimit != nil {
		remaining := *settings.CreditLimit - summary.ConsumedCost
		if remaining < 0 {
			remaining = 0
		}
		summary.CreditRemaining = &remaining
	}
	return summary, nil
}

func (s *Service) requireOwnedSite(ctx context.Context, userID, siteID string) (*Site, string, error) {
	site, err := s.cache.Get(ctx, siteID)
	if err != nil {
		return nil, "", err
	}
	if site == nil || site.UserID != userID {
		return nil, "", newRequestError(ErrorNotFound, "")
	}
	aid, err := s.requireCurrentAdminAccountID(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	if site.AdminAccountID != aid {
		return nil, "", newRequestError(ErrorNotFound, "")
	}
	return site, aid, nil
}
