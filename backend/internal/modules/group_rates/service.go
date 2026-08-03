package group_rates

import (
	"context"
	"strings"
	"time"
)

type Service struct {
	repository *Repository
}

func NewService(repository *Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) EnsureSchema(ctx context.Context) error {
	return s.repository.EnsureSchema(ctx)
}

func (s *Service) SaveSiteSnapshot(ctx context.Context, userID string, adminAccountID string, siteID string, siteName string, sitePlatform string, groups []SnapshotGroup) error {
	now := time.Now()
	ownerID := strings.TrimSpace(userID)
	workspaceID := strings.TrimSpace(adminAccountID)
	if ownerID == "" || workspaceID == "" {
		return nil
	}
	trimmedSiteID := strings.TrimSpace(siteID)
	trimmedSiteName := strings.TrimSpace(siteName)
	platform := strings.TrimSpace(sitePlatform)

	// 获取该站点每个分组的最新快照，用于判断倍率是否变化以及检测已消失分组。
	existing, err := s.repository.LatestGroupKeysForSite(ctx, ownerID, workspaceID, trimmedSiteID)
	if err != nil {
		return err
	}

	plan, err := planSiteSnapshot(ownerID, workspaceID, trimmedSiteID, trimmedSiteName, platform, groups, existing, now)
	if err != nil {
		return err
	}

	if err := s.repository.MarkDeleted(ctx, plan.toDelete); err != nil {
		return err
	}
	if err := s.repository.TouchSnapshots(ctx, plan.toTouch, trimmedSiteName, now); err != nil {
		return err
	}
	return s.repository.InsertSnapshots(ctx, plan.toInsert)
}

// snapshotPlan is the set of DB writes produced by one site sync.
type snapshotPlan struct {
	toInsert []snapshotRecord
	toTouch  []string
	toDelete []string
}

// groupPresenceKey returns the stable identity used for snapshot upsert / soft-delete.
// Prefer non-empty group_id; fall back to group_name (legacy rows).
func groupPresenceKey(groupID, groupName string) string {
	if id := strings.TrimSpace(groupID); id != "" {
		return id
	}
	return strings.TrimSpace(groupName)
}

// planSiteSnapshot decides insert / touch / soft-delete for one site sync.
//
// Presence (soft-delete) is based on every named group still returned by upstream,
// even when Multiplier is nil or site platform is empty. Those groups cannot get a
// new rate row (multiplier is required), but must not be marked deleted just because
// the rate payload is missing — that was wiping auto / incomplete groups from the
// default "all" tab on every sync.
//
// Only groups with a numeric multiplier and a non-empty site platform are inserted
// or used for rate-change detection.
func planSiteSnapshot(
	ownerID string,
	workspaceID string,
	siteID string,
	siteName string,
	sitePlatform string,
	groups []SnapshotGroup,
	existing []latestGroupKey,
	now time.Time,
) (snapshotPlan, error) {
	type validatedGroup struct {
		key        string
		groupID    string
		groupName  string
		platform   string
		groupType  string
		multiplier float64
	}

	// presentKeys: still visible upstream (name non-empty). Used only for soft-delete.
	presentKeys := make(map[string]struct{}, len(groups))
	var validated []validatedGroup
	for _, group := range groups {
		name := strings.TrimSpace(group.Name)
		if name == "" {
			continue
		}
		groupID := strings.TrimSpace(group.ID)
		key := groupPresenceKey(groupID, name)
		presentKeys[key] = struct{}{}

		// Rate rows require a numeric multiplier and a known site platform.
		if group.Multiplier == nil || sitePlatform == "" {
			continue
		}
		groupType := ""
		if group.Platform != nil {
			if gp := strings.TrimSpace(*group.Platform); gp != "" {
				groupType = gp
			}
		}
		validated = append(validated, validatedGroup{
			key:        key,
			groupID:    groupID,
			groupName:  name,
			platform:   sitePlatform,
			groupType:  groupType,
			multiplier: *group.Multiplier,
		})
	}

	existingMap := make(map[string]latestGroupKey, len(existing))
	for _, e := range existing {
		existingMap[groupPresenceKey(e.GroupID, e.GroupName)] = e
	}

	// Split into "rate unchanged → touch last_seen" vs "rate changed / new → insert".
	// Only real multiplier changes create a new snapshot row so LEAD() always
	// compares against the previous distinct rate, not every sync cycle.
	var plan snapshotPlan
	validatedKeys := make(map[string]struct{}, len(validated))
	for _, g := range validated {
		validatedKeys[g.key] = struct{}{}
		if prev, ok := existingMap[g.key]; ok && prev.Multiplier == g.multiplier && !prev.Deleted {
			plan.toTouch = append(plan.toTouch, prev.ID)
			continue
		}
		id, err := newSnapshotID()
		if err != nil {
			return snapshotPlan{}, err
		}
		plan.toInsert = append(plan.toInsert, snapshotRecord{
			ID:             id,
			UserID:         ownerID,
			AdminAccountID: workspaceID,
			SiteID:         siteID,
			SiteName:       siteName,
			GroupID:        g.groupID,
			GroupName:      g.groupName,
			Platform:       g.platform,
			Type:           g.groupType,
			Multiplier:     g.multiplier,
			CreatedAt:      now,
		})
	}

	// Present but not rate-insertable (nil multiplier / empty platform): keep the
	// existing active row alive by refreshing last_seen instead of soft-deleting.
	for key := range presentKeys {
		if _, ok := validatedKeys[key]; ok {
			continue
		}
		if prev, ok := existingMap[key]; ok && !prev.Deleted {
			plan.toTouch = append(plan.toTouch, prev.ID)
		}
	}

	// Soft-delete only groups that truly vanished from the upstream payload.
	for _, e := range existing {
		key := groupPresenceKey(e.GroupID, e.GroupName)
		if _, found := presentKeys[key]; !found && !e.Deleted {
			plan.toDelete = append(plan.toDelete, e.ID)
		}
	}
	return plan, nil
}

func (s *Service) List(ctx context.Context, userID string, adminAccountID string, query ListQuery) (ListResult, error) {
	query = normalizeListQuery(query)
	records, err := s.repository.List(ctx, strings.TrimSpace(userID), strings.TrimSpace(adminAccountID), query)
	if err != nil {
		return ListResult{}, err
	}

	rows := make([]RateRow, 0, len(records.Items))
	for _, record := range records.Items {
		delta, deltaPercent := change(record.Multiplier, record.PreviousMultiplier)
		rows = append(rows, RateRow{
			SiteID:             record.SiteID,
			SiteName:           record.SiteName,
			GroupID:            record.GroupID,
			GroupName:          record.GroupName,
			Platform:           record.Platform,
			Type:               record.Type,
			Mapped:             record.Mapped,
			Connected:          record.Mapped,
			PricingMapped:      record.PricingMapped,
			Deleted:            record.Deleted,
			UpstreamMultiplier: record.Multiplier,
			RechargeRate:       record.RechargeRate,
			CurrentMultiplier:  record.Multiplier * record.RechargeRate,
			Delta:              delta,
			DeltaPercent:       deltaPercent,
			UpdatedAt:          record.LastSeenAt,
		})
	}
	return ListResult{
		Items:        rows,
		Total:        records.Total,
		Page:         query.Page,
		PageSize:     query.PageSize,
		TotalPages:   totalPages(records.Total, query.PageSize),
		Types:        records.Types,
		Platforms:    records.Platforms,
		StatusCounts: records.StatusCounts,
	}, nil
}

func (s *Service) UpdateType(ctx context.Context, userID string, adminAccountID string, ref GroupRef, groupType string) error {
	return s.repository.UpdateType(ctx, strings.TrimSpace(userID), strings.TrimSpace(adminAccountID), normalizeGroupRef(ref), strings.TrimSpace(groupType))
}

// ListGroupNames 按 type/search/platform 从最新分组快照中筛出匹配的分组名集合，
// 供 group_rate_campaigns 模块（group_rate_campaigns.GroupTypeLookup）解析
// "按分组类型"/"当前筛选结果" 两种选择模式使用。
func (s *Service) ListGroupNames(ctx context.Context, userID string, adminAccountID string, search string, groupType string, platform string) ([]string, error) {
	return s.repository.ListDistinctGroupNames(ctx, strings.TrimSpace(userID), strings.TrimSpace(adminAccountID), search, groupType, platform)
}

func normalizeGroupRef(ref GroupRef) GroupRef {
	return GroupRef{SiteID: strings.TrimSpace(ref.SiteID), GroupName: strings.TrimSpace(ref.GroupName)}
}

func (s *Service) History(ctx context.Context, userID string, adminAccountID string, siteID string, groupName string, platform string) ([]HistoryRow, error) {
	records, err := s.repository.History(ctx, strings.TrimSpace(userID), strings.TrimSpace(adminAccountID), strings.TrimSpace(siteID), strings.TrimSpace(groupName), strings.TrimSpace(platform))
	if err != nil {
		return nil, err
	}

	rows := make([]HistoryRow, 0, len(records))
	for _, record := range records {
		delta, deltaPercent := change(record.Multiplier, record.PreviousMultiplier)
		rows = append(rows, HistoryRow{
			ID:                record.ID,
			SiteID:            record.SiteID,
			SiteName:          record.SiteName,
			GroupID:           record.GroupID,
			GroupName:         record.GroupName,
			Platform:          record.Platform,
			Type:              record.Type,
			Multiplier:        record.Multiplier,
			CurrentMultiplier: record.Multiplier * record.RechargeRate,
			Deleted:           record.Deleted,
			Delta:             delta,
			DeltaPercent:      deltaPercent,
			CreatedAt:         record.CreatedAt,
			UpdatedAt:         record.LastSeenAt,
		})
	}
	return rows, nil
}

func normalizeListQuery(query ListQuery) ListQuery {
	query.Search = strings.TrimSpace(query.Search)
	query.Type = strings.TrimSpace(query.Type)
	query.Platform = strings.TrimSpace(query.Platform)
	query.Status = strings.TrimSpace(query.Status)
	query.Sort = strings.TrimSpace(query.Sort)
	switch query.Status {
	case "":
		// Empty is the legacy contract: return active and deleted rows together.
		// New clients always send an explicit status and receive accurate totals.
		query.Status = "legacy"
	case "mapped", "unmapped", "deleted":
	case "all":
	default:
		query.Status = "legacy"
	}
	switch query.Sort {
	case "multiplierAsc", "multiplierDesc", "siteNameAsc", "groupNameAsc":
	default:
		query.Sort = "default"
	}
	if query.Page < 1 {
		query.Page = 1
	}
	if query.PageSize < 1 {
		query.PageSize = 10
	}
	if query.PageSize > 100 {
		query.PageSize = 100
	}
	return query
}

func totalPages(total int, pageSize int) int {
	if total == 0 || pageSize <= 0 {
		return 0
	}
	pages := total / pageSize
	if total%pageSize != 0 {
		pages++
	}
	return pages
}

func change(current float64, previous *float64) (*float64, *float64) {
	if previous == nil || *previous == 0 {
		return nil, nil
	}
	delta := current - *previous
	deltaPercent := delta / *previous * 100
	return &delta, &deltaPercent
}
