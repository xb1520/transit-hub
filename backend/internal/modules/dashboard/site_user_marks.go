package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 站点用户充值流水标签（与上游进货账本语义对齐）。
const (
	SiteUserTagRecharge = "recharge"
	SiteUserTagGift     = "gift"
	SiteUserTagRebate   = "rebate"
)

// SiteUserTopupMark 本地对平台用户入账流水的标记。
type SiteUserTopupMark struct {
	ID             string    `json:"id"`
	UserID         string    `json:"-"`
	AdminAccountID string    `json:"-"`
	PlatformUserID string    `json:"platformUserId"`
	Ref            string    `json:"ref"`
	Tag            string    `json:"tag"`
	AmountPlatform float64   `json:"amountPlatform"`
	Note           string    `json:"note"`
	BusinessDate   string    `json:"businessDate,omitempty"`
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// EnsureSiteUserMarksSchema 创建站点用户流水标记表。
func (r *MetricsRepository) EnsureSiteUserMarksSchema(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	_, err := r.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS site_user_topup_marks (
			id text PRIMARY KEY,
			user_id text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			platform_user_id text NOT NULL,
			ref text NOT NULL,
			tag text NOT NULL,
			amount_platform double precision NOT NULL DEFAULT 0,
			note text NOT NULL DEFAULT '',
			business_date date,
			source text NOT NULL DEFAULT 'auto',
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_site_user_topup_marks_ref
			ON site_user_topup_marks (user_id, admin_account_id, platform_user_id, ref);
		CREATE INDEX IF NOT EXISTS idx_site_user_topup_marks_workspace_user
			ON site_user_topup_marks (user_id, admin_account_id, platform_user_id);
		CREATE INDEX IF NOT EXISTS idx_site_user_topup_marks_workspace_tag
			ON site_user_topup_marks (user_id, admin_account_id, tag);
	`)
	return err
}

func newSiteUserMarkID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sum_" + hex.EncodeToString(buf), nil
}

// MapSiteUserMarksByRef 返回某平台用户下 ref → 标记。
func (r *MetricsRepository) MapSiteUserMarksByRef(ctx context.Context, userID, adminAccountID, platformUserID string) (map[string]SiteUserTopupMark, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, platform_user_id, ref, tag, amount_platform, note,
			COALESCE(business_date::text, ''), source, created_at, updated_at
		FROM site_user_topup_marks
		WHERE user_id = $1 AND admin_account_id = $2 AND platform_user_id = $3
	`, userID, adminAccountID, platformUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]SiteUserTopupMark)
	for rows.Next() {
		var m SiteUserTopupMark
		if err := rows.Scan(
			&m.ID, &m.UserID, &m.AdminAccountID, &m.PlatformUserID, &m.Ref, &m.Tag, &m.AmountPlatform, &m.Note,
			&m.BusinessDate, &m.Source, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out[m.Ref] = m
	}
	return out, rows.Err()
}

// UpsertSiteUserMark 按 (workspace, platform_user, ref) 幂等写入标记。
func (r *MetricsRepository) UpsertSiteUserMark(ctx context.Context, mark SiteUserTopupMark) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO site_user_topup_marks (
			id, user_id, admin_account_id, platform_user_id, ref, tag, amount_platform, note,
			business_date, source, created_at, updated_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,
			NULLIF($9, '')::date, $10, $11, $12
		)
		ON CONFLICT (user_id, admin_account_id, platform_user_id, ref)
		DO UPDATE SET
			tag = EXCLUDED.tag,
			amount_platform = EXCLUDED.amount_platform,
			note = EXCLUDED.note,
			business_date = EXCLUDED.business_date,
			source = EXCLUDED.source,
			updated_at = EXCLUDED.updated_at
	`, mark.ID, mark.UserID, mark.AdminAccountID, mark.PlatformUserID, mark.Ref, mark.Tag, mark.AmountPlatform, mark.Note,
		mark.BusinessDate, mark.Source, mark.CreatedAt, mark.UpdatedAt)
	return err
}

// SumNonRevenueByPlatformUser 汇总 gift+rebate 标记金额（平台原始单位），用于营收侧扣除。
func (r *MetricsRepository) SumNonRevenueByPlatformUser(ctx context.Context, userID, adminAccountID string) (map[string]float64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT platform_user_id, COALESCE(SUM(amount_platform), 0)
		FROM site_user_topup_marks
		WHERE user_id = $1 AND admin_account_id = $2 AND tag IN ($3, $4)
		GROUP BY platform_user_id
	`, userID, adminAccountID, SiteUserTagGift, SiteUserTagRebate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var pid string
		var total float64
		if err := rows.Scan(&pid, &total); err != nil {
			return nil, err
		}
		out[pid] = total
	}
	return out, rows.Err()
}

// UserTagSums 单用户三类流水标记合计（平台原始单位）。
type UserTagSums struct {
	Gift     float64
	Rebate   float64
	Recharge float64
}

// SumTagsByPlatformUser 返回某用户 gift/rebate/recharge 标记合计。
func (r *MetricsRepository) SumTagsByPlatformUser(ctx context.Context, userID, adminAccountID, platformUserID string) (gift, rebate, recharge float64, err error) {
	rows, err := r.db.Query(ctx, `
		SELECT tag, COALESCE(SUM(amount_platform), 0)
		FROM site_user_topup_marks
		WHERE user_id = $1 AND admin_account_id = $2 AND platform_user_id = $3
		GROUP BY tag
	`, userID, adminAccountID, platformUserID)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		var total float64
		if err := rows.Scan(&tag, &total); err != nil {
			return 0, 0, 0, err
		}
		switch tag {
		case SiteUserTagGift:
			gift = total
		case SiteUserTagRebate:
			rebate = total
		case SiteUserTagRecharge:
			recharge = total
		}
	}
	return gift, rebate, recharge, rows.Err()
}

// MapTagSumsByPlatformUser 工作区下各用户 gift/rebate/recharge 标记合计（平台单位）。
func (r *MetricsRepository) MapTagSumsByPlatformUser(ctx context.Context, userID, adminAccountID string) (map[string]UserTagSums, error) {
	rows, err := r.db.Query(ctx, `
		SELECT platform_user_id, tag, COALESCE(SUM(amount_platform), 0)
		FROM site_user_topup_marks
		WHERE user_id = $1 AND admin_account_id = $2
		GROUP BY platform_user_id, tag
	`, userID, adminAccountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]UserTagSums)
	for rows.Next() {
		var pid, tag string
		var total float64
		if err := rows.Scan(&pid, &tag, &total); err != nil {
			return nil, err
		}
		cur := out[pid]
		switch tag {
		case SiteUserTagGift:
			cur.Gift = total
		case SiteUserTagRebate:
			cur.Rebate = total
		case SiteUserTagRecharge:
			cur.Recharge = total
		}
		out[pid] = cur
	}
	return out, rows.Err()
}

// SumTagsByWorkspace 全站 gift/rebate/recharge 标记合计（平台单位）。
// excludeUserIDs 非空时排除这些 platform_user_id。
func (r *MetricsRepository) SumTagsByWorkspace(ctx context.Context, userID, adminAccountID string, excludeUserIDs []string) (gift, rebate, recharge float64, err error) {
	if excludeUserIDs == nil {
		excludeUserIDs = []string{}
	}
	rows, err := r.db.Query(ctx, `
		SELECT tag, COALESCE(SUM(amount_platform), 0)
		FROM site_user_topup_marks
		WHERE user_id = $1 AND admin_account_id = $2
			AND (COALESCE(array_length($3::text[], 1), 0) = 0 OR platform_user_id <> ALL($3::text[]))
		GROUP BY tag
	`, userID, adminAccountID, excludeUserIDs)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		var total float64
		if err := rows.Scan(&tag, &total); err != nil {
			return 0, 0, 0, err
		}
		switch tag {
		case SiteUserTagGift:
			gift = total
		case SiteUserTagRebate:
			rebate = total
		case SiteUserTagRecharge:
			recharge = total
		}
	}
	return gift, rebate, recharge, rows.Err()
}

// GetSiteUserMark 读取单条（可选）。
func (r *MetricsRepository) GetSiteUserMark(ctx context.Context, userID, adminAccountID, platformUserID, ref string) (*SiteUserTopupMark, error) {
	var m SiteUserTopupMark
	err := r.db.QueryRow(ctx, `
		SELECT id, user_id, admin_account_id, platform_user_id, ref, tag, amount_platform, note,
			COALESCE(business_date::text, ''), source, created_at, updated_at
		FROM site_user_topup_marks
		WHERE user_id = $1 AND admin_account_id = $2 AND platform_user_id = $3 AND ref = $4
	`, userID, adminAccountID, platformUserID, ref).Scan(
		&m.ID, &m.UserID, &m.AdminAccountID, &m.PlatformUserID, &m.Ref, &m.Tag, &m.AmountPlatform, &m.Note,
		&m.BusinessDate, &m.Source, &m.CreatedAt, &m.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func normalizeSiteUserTag(tag string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case SiteUserTagRecharge, "topup", "paid":
		return SiteUserTagRecharge, true
	case SiteUserTagGift:
		return SiteUserTagGift, true
	case SiteUserTagRebate:
		return SiteUserTagRebate, true
	default:
		return "", false
	}
}
