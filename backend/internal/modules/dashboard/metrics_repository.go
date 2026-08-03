package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MetricsRepository 负责 dashboard_daily_stats 表的持久化操作。
// 与 Redis 的 SessionStore 独立，专门用于存储每日统计快照。
type MetricsRepository struct {
	db *pgxpool.Pool
}

func NewMetricsRepository(db *pgxpool.Pool) *MetricsRepository {
	return &MetricsRepository{db: db}
}

// EnsureSchema 在服务启动时创建 dashboard_daily_stats 和 dashboard_balance_filter 表及索引，
// 并将旧数据迁移到 workspace 维度。
//
// dashboard_daily_stats: (user_id, admin_account_id, date) 唯一索引保证每工作区每天至多一行。
// dashboard_balance_filter: (user_id, admin_account_id) 唯一索引保证每工作区至多一行配置。
//
// 迁移策略：
//  1. 新增 admin_account_id 列（DEFAULT ” 兼容旧行）。
//  2. 删除旧的单维度唯一索引/约束，创建新的多维度唯一索引。
//  3. 旧数据 admin_account_id=” 保留原样，由 admin_accounts 统一归属迁移负责补值。
func (r *MetricsRepository) EnsureSchema(ctx context.Context) error {
	_, err := r.db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS dashboard_daily_stats (
			id               text PRIMARY KEY,
			user_id          text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			date             date NOT NULL,
			today_profit     double precision NOT NULL DEFAULT 0,
			site_balance     double precision NOT NULL DEFAULT 0,
			today_purchase   double precision NOT NULL DEFAULT 0,
			today_inbound    double precision NOT NULL DEFAULT 0,
			net_profit       double precision NOT NULL DEFAULT 0,
			upstream_balance double precision NOT NULL DEFAULT 0,
			created_at       timestamptz NOT NULL DEFAULT now()
		);

		-- 阶段 D：今日进货与今日成本分列（today_purchase 语义保留为成本）
		DO $$ BEGIN
			ALTER TABLE dashboard_daily_stats ADD COLUMN today_inbound double precision NOT NULL DEFAULT 0;
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;

		-- 新增 admin_account_id 列（旧表迁移，IF NOT EXISTS 语义通过 DO NOTHING 实现）。
		DO $$ BEGIN
			ALTER TABLE dashboard_daily_stats ADD COLUMN admin_account_id text NOT NULL DEFAULT '';
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;

		-- 删除旧的 (user_id, date) 唯一索引，避免与新索引冲突。
		DROP INDEX IF EXISTS idx_dashboard_daily_stats_user_date;

		-- 创建新的 (user_id, admin_account_id, date) 唯一索引。
		CREATE UNIQUE INDEX IF NOT EXISTS idx_dashboard_daily_stats_user_account_date
			ON dashboard_daily_stats (user_id, admin_account_id, date);
		CREATE INDEX IF NOT EXISTS idx_dashboard_daily_stats_user_date_desc
			ON dashboard_daily_stats (user_id, admin_account_id, date DESC);

		CREATE TABLE IF NOT EXISTS dashboard_balance_filter (
			user_id          text NOT NULL,
			admin_account_id text NOT NULL DEFAULT '',
			exclude_admin    boolean NOT NULL DEFAULT true,
			exclude_balances jsonb NOT NULL DEFAULT '[]',
			updated_at       timestamptz NOT NULL DEFAULT now()
		);

		-- 新增 admin_account_id 列（旧表迁移）。
		DO $$ BEGIN
			ALTER TABLE dashboard_balance_filter ADD COLUMN admin_account_id text NOT NULL DEFAULT '';
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;

		-- 删除旧的 user_id 主键约束，改为复合唯一索引。
		-- 旧表可能用 user_id 做主键或唯一约束，需要先去除。
		ALTER TABLE dashboard_balance_filter DROP CONSTRAINT IF EXISTS dashboard_balance_filter_pkey;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_dashboard_balance_filter_user_account
			ON dashboard_balance_filter (user_id, admin_account_id);

		-- MVP：整户排除 + 用户赠送不计营收额度（只影响营收侧）。
		DO $$ BEGIN
			ALTER TABLE dashboard_balance_filter ADD COLUMN exclude_user_ids jsonb NOT NULL DEFAULT '[]'::jsonb;
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;
		DO $$ BEGIN
			ALTER TABLE dashboard_balance_filter ADD COLUMN user_gift_amounts jsonb NOT NULL DEFAULT '{}'::jsonb;
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;
		-- 工作区站点充值倍率：用户余额平台单位 → 成本/CNY
		DO $$ BEGIN
			ALTER TABLE dashboard_balance_filter ADD COLUMN site_recharge_rate double precision NOT NULL DEFAULT 1;
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$;
	`)
	if err != nil {
		return err
	}
	return r.EnsureSiteUserMarksSchema(ctx)
}

// Upsert 插入或更新指定用户指定工作区指定日期的快照行。
// 冲突时用最新的指标值覆盖旧值，保证一天内多次调用始终保留最新数据。
func (r *MetricsRepository) Upsert(ctx context.Context, snapshot DailySnapshot) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO dashboard_daily_stats (id, user_id, admin_account_id, date, today_profit, site_balance, today_purchase, today_inbound, net_profit, upstream_balance, created_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		WHERE EXISTS (SELECT 1 FROM admin_accounts WHERE user_id = $2 AND id = $3)
		ON CONFLICT (user_id, admin_account_id, date) DO UPDATE SET
			today_profit     = EXCLUDED.today_profit,
			site_balance     = EXCLUDED.site_balance,
			today_purchase   = EXCLUDED.today_purchase,
			today_inbound    = EXCLUDED.today_inbound,
			net_profit       = EXCLUDED.net_profit,
			upstream_balance = EXCLUDED.upstream_balance,
			created_at       = EXCLUDED.created_at
		WHERE EXISTS (SELECT 1 FROM admin_accounts WHERE user_id = EXCLUDED.user_id AND id = EXCLUDED.admin_account_id)
	`, snapshot.ID, snapshot.UserID, snapshot.AdminAccountID, snapshot.Date, snapshot.TodayProfit, snapshot.SiteBalance,
		snapshot.TodayPurchase, snapshot.TodayInbound, snapshot.NetProfit, snapshot.UpstreamBalance, snapshot.CreatedAt)
	return err
}

// ListRange 查询指定用户指定工作区最近 days 天的快照记录，按日期升序返回。
// 不包含当天（当天的数据由 LiveMetrics 实时提供），仅返回已保存的历史日期。
func (r *MetricsRepository) ListRange(ctx context.Context, userID, adminAccountID string, days int) ([]DailySnapshot, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, admin_account_id, date, today_profit, site_balance, today_purchase,
			COALESCE(today_inbound, 0), net_profit, upstream_balance, created_at
		FROM dashboard_daily_stats
		WHERE user_id = $1 AND admin_account_id = $2 AND date >= (CURRENT_DATE - $3::int) AND date < CURRENT_DATE
		ORDER BY date ASC
	`, userID, adminAccountID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	snapshots := make([]DailySnapshot, 0)
	for rows.Next() {
		var s DailySnapshot
		if err := rows.Scan(&s.ID, &s.UserID, &s.AdminAccountID, &s.Date, &s.TodayProfit, &s.SiteBalance,
			&s.TodayPurchase, &s.TodayInbound, &s.NetProfit, &s.UpstreamBalance, &s.CreatedAt); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}

// Exists 检查指定用户指定工作区指定日期是否已有快照行。
func (r *MetricsRepository) Exists(ctx context.Context, userID, adminAccountID string, date time.Time) (bool, error) {
	var count int
	err := r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM dashboard_daily_stats WHERE user_id = $1 AND admin_account_id = $2 AND date = $3
	`, userID, adminAccountID, date).Scan(&count)
	return count > 0, err
}

// GetBalanceFilter 读取指定用户指定工作区的余额筛选配置。
// 若用户尚未配置，返回默认值（排除 admin、倍率 1）。
func (r *MetricsRepository) GetBalanceFilter(ctx context.Context, userID, adminAccountID string) (BalanceFilterConfig, error) {
	config := BalanceFilterConfig{
		UserID:           userID,
		AdminAccountID:   adminAccountID,
		ExcludeAdmin:     true,
		ExcludeBalances:  []float64{},
		ExcludeUserIDs:   []string{},
		UserGiftAmounts:  map[string]float64{},
		SiteRechargeRate: 1,
	}
	var balancesJSON, userIDsJSON, giftsJSON []byte
	var rate float64
	err := r.db.QueryRow(ctx, `
		SELECT exclude_admin, exclude_balances,
			COALESCE(exclude_user_ids, '[]'::jsonb),
			COALESCE(user_gift_amounts, '{}'::jsonb),
			COALESCE(site_recharge_rate, 1)
		FROM dashboard_balance_filter WHERE user_id = $1 AND admin_account_id = $2
	`, userID, adminAccountID).Scan(&config.ExcludeAdmin, &balancesJSON, &userIDsJSON, &giftsJSON, &rate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return config, nil
		}
		// 旧库尚未迁移 site_recharge_rate 时回退
		err = r.db.QueryRow(ctx, `
			SELECT exclude_admin, exclude_balances,
				COALESCE(exclude_user_ids, '[]'::jsonb),
				COALESCE(user_gift_amounts, '{}'::jsonb)
			FROM dashboard_balance_filter WHERE user_id = $1 AND admin_account_id = $2
		`, userID, adminAccountID).Scan(&config.ExcludeAdmin, &balancesJSON, &userIDsJSON, &giftsJSON)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return config, nil
			}
			// 再回退只读基础字段
			err = r.db.QueryRow(ctx, `
				SELECT exclude_admin, exclude_balances FROM dashboard_balance_filter WHERE user_id = $1 AND admin_account_id = $2
			`, userID, adminAccountID).Scan(&config.ExcludeAdmin, &balancesJSON)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return config, nil
				}
				return config, err
			}
		}
		rate = 1
	}
	if rate > 0 {
		config.SiteRechargeRate = rate
	}
	if len(balancesJSON) > 0 {
		if err := json.Unmarshal(balancesJSON, &config.ExcludeBalances); err != nil {
			return config, err
		}
	}
	if len(userIDsJSON) > 0 {
		_ = json.Unmarshal(userIDsJSON, &config.ExcludeUserIDs)
	}
	if len(giftsJSON) > 0 {
		_ = json.Unmarshal(giftsJSON, &config.UserGiftAmounts)
	}
	if config.ExcludeUserIDs == nil {
		config.ExcludeUserIDs = []string{}
	}
	if config.UserGiftAmounts == nil {
		config.UserGiftAmounts = map[string]float64{}
	}
	if config.SiteRechargeRate <= 0 {
		config.SiteRechargeRate = 1
	}
	return config, nil
}

// SaveBalanceFilter 保存或更新指定用户指定工作区的余额筛选配置。
// 使用 upsert 确保幂等写入，用户首次配置和后续修改都走同一路径。
func (r *MetricsRepository) SaveBalanceFilter(ctx context.Context, config BalanceFilterConfig) error {
	if config.ExcludeBalances == nil {
		config.ExcludeBalances = []float64{}
	}
	if config.ExcludeUserIDs == nil {
		config.ExcludeUserIDs = []string{}
	}
	if config.UserGiftAmounts == nil {
		config.UserGiftAmounts = map[string]float64{}
	}
	if config.SiteRechargeRate <= 0 {
		config.SiteRechargeRate = 1
	}
	balancesJSON, err := json.Marshal(config.ExcludeBalances)
	if err != nil {
		return err
	}
	userIDsJSON, err := json.Marshal(config.ExcludeUserIDs)
	if err != nil {
		return err
	}
	giftsJSON, err := json.Marshal(config.UserGiftAmounts)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO dashboard_balance_filter (
			user_id, admin_account_id, exclude_admin, exclude_balances, exclude_user_ids, user_gift_amounts, site_recharge_rate, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (user_id, admin_account_id) DO UPDATE SET
			exclude_admin       = EXCLUDED.exclude_admin,
			exclude_balances    = EXCLUDED.exclude_balances,
			exclude_user_ids    = EXCLUDED.exclude_user_ids,
			user_gift_amounts   = EXCLUDED.user_gift_amounts,
			site_recharge_rate  = EXCLUDED.site_recharge_rate,
			updated_at          = now()
	`, config.UserID, config.AdminAccountID, config.ExcludeAdmin, balancesJSON, userIDsJSON, giftsJSON, config.SiteRechargeRate)
	return err
}
