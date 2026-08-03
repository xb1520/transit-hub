package connection_health

import (
	"context"
	"log"
	"sort"
	"strings"

	"transithub/backend/internal/modules/upstream"
)

// TargetPriorityActioner 是倍率排序策略对 upstream 模块的唯一写依赖。真实实现根据 session
// 平台更新 New API channel 或 Sub2API account 的 priority，并由 upstream 模块保证字段级写入安全。
type TargetPriorityActioner interface {
	UpdateAdminTargetPriority(session upstream.Session, targetID string, priority int) error
}

type priorityTargetInventory struct {
	target          AdminProbeTarget
	account         upstream.AdminGroupAccountInfo
	policies        []Policy
	multipliers     []float64
	currentPriority int
}

// syncMultiplierPriorities 在每轮探活前同步上游优先级。普通倍率策略「健康优先、倍率次之」：
// 可调度模型（healthy+degraded）按平均延迟分四档（<3s/<10s/<30s/≥30s），同档再比成本倍率；
// 观察中/恢复中/暂停仍单独更差；已从白名单摘除的模型不参与。
// 仅倍率策略则完全忽略探活状态。它故意与 job 生成分开，确保未到探活时间的目标也能更新顺序。
func (s *Service) syncMultiplierPriorities(
	ctx context.Context,
	policies []Policy,
	targetAssignments []PolicyAssignment,
	groupAssignments []GroupPolicyAssignment,
	exclusions []GroupTargetExclusion,
	allSyncStates []PrioritySyncState,
) {
	s.syncMultiplierPrioritiesWithCache(ctx, policies, targetAssignments, groupAssignments, exclusions, allSyncStates, make(adminInventoryCache))
}

func (s *Service) syncMultiplierPrioritiesWithCache(
	ctx context.Context,
	policies []Policy,
	targetAssignments []PolicyAssignment,
	groupAssignments []GroupPolicyAssignment,
	exclusions []GroupTargetExclusion,
	allSyncStates []PrioritySyncState,
	inventoryCache adminInventoryCache,
) {
	if s.priorityActions == nil || s.platformGroups == nil {
		return
	}

	assignedTargets := assignedEnabledPoliciesByTarget(policies, targetAssignments)
	assignedGroups := assignedEnabledPoliciesByGroup(policies, groupAssignments)
	excluded := groupTargetExclusionIndex(exclusions)
	statesByWorkspace := make(map[string][]PrioritySyncState)
	workspaceIdentity := make(map[string][2]string)
	for _, state := range allSyncStates {
		key := state.UserID + "|" + state.AdminAccountID
		statesByWorkspace[key] = append(statesByWorkspace[key], state)
		workspaceIdentity[key] = [2]string{state.UserID, state.AdminAccountID}
	}
	for _, policy := range policies {
		key := policy.UserID + "|" + policy.AdminAccountID
		workspaceIdentity[key] = [2]string{policy.UserID, policy.AdminAccountID}
	}
	for _, assignment := range targetAssignments {
		key := assignment.UserID + "|" + assignment.AdminAccountID
		workspaceIdentity[key] = [2]string{assignment.UserID, assignment.AdminAccountID}
	}
	for _, assignment := range groupAssignments {
		key := assignment.UserID + "|" + assignment.AdminAccountID
		workspaceIdentity[key] = [2]string{assignment.UserID, assignment.AdminAccountID}
	}

	for workspaceKey, identity := range workspaceIdentity {
		userID, adminAccountID := identity[0], identity[1]
		inventorySnapshot, err := s.loadAdminInventory(ctx, userID, adminAccountID, inventoryCache)
		if err != nil {
			log.Printf("[connection-health] priority sync load admin inventory failed user_id=%s admin_account_id=%s err=%v", userID, adminAccountID, err)
			continue
		}
		session := inventorySnapshot.session
		inventory, inventoryComplete, err := s.priorityInventoryForSnapshot(
			inventorySnapshot, adminAccountID, assignedTargets[workspaceKey], assignedGroups[workspaceKey], excluded[workspaceKey],
		)
		if err != nil {
			log.Printf("[connection-health] priority sync inventory failed user_id=%s admin_account_id=%s err=%v", userID, adminAccountID, err)
			continue
		}
		states, err := s.repo.ListStatesByWorkspace(ctx, userID, adminAccountID)
		if err != nil {
			log.Printf("[connection-health] priority sync list health states failed user_id=%s admin_account_id=%s err=%v", userID, adminAccountID, err)
			continue
		}
		s.syncWorkspacePriorities(ctx, session, userID, adminAccountID, inventory, inventoryComplete, states, statesByWorkspace[workspaceKey])
	}
}

func (s *Service) priorityInventoryForSnapshot(
	snapshot *adminWorkspaceInventory,
	adminAccountID string,
	targetPolicies map[string][]Policy,
	groupPolicies map[string][]Policy,
	excludedByGroup map[string]map[string]bool,
) (map[string]*priorityTargetInventory, bool, error) {
	session := snapshot.session
	platform := string(session.Platform)
	inventory := make(map[string]*priorityTargetInventory)
	inventoryComplete := true
	for _, groupInventory := range snapshot.groups {
		group := groupInventory.group
		if groupInventory.err != nil {
			// 单个分组失败不阻断其它分组排序；目标如果只存在于失败分组，本轮保持原值。
			inventoryComplete = false
			log.Printf("[connection-health] priority sync group accounts failed group_id=%s err=%v", group.ID, groupInventory.err)
			continue
		}
		for _, account := range groupInventory.accounts {
			targetID := buildTargetID(platform, adminAccountID, account.ID)
			item := inventory[targetID]
			if item == nil {
				item = &priorityTargetInventory{
					target: AdminProbeTarget{
						TargetID: targetID, Platform: platform, AdminGroupID: group.ID, AdminGroupName: group.Name,
						AccountID: account.ID, AccountName: account.Name, AccountStatus: account.Status, AccountWeight: cloneIntPointer(account.Weight),
						ProviderFamily: account.Platform, Models: splitModelList(account.Models),
					},
					account: account,
				}
				if account.Priority != nil {
					item.currentPriority = *account.Priority
				}
				inventory[targetID] = item
			}
			inherited := groupPolicies[group.ID]
			excluded := excludedByGroup[group.ID][targetID]
			if excluded {
				inherited = nil
			}
			// 倍率只来自目标实际参与策略继承的分组。先前在排除判断前收集倍率，会让已排除
			// 或无倍率策略的其它成员分组错误地压低当前目标优先级。
			explicitMultiplier := hasMultiplierPriorityPolicy(targetPolicies[targetID])
			inheritedMultiplier := !excluded && hasMultiplierPriorityPolicy(inherited)
			if group.Multiplier != nil && (explicitMultiplier || inheritedMultiplier) {
				item.multipliers = append(item.multipliers, *group.Multiplier)
			}
			item.policies = mergePoliciesByID(item.policies, targetPolicies[targetID], inherited)
		}
	}
	return inventory, inventoryComplete, nil
}

func (s *Service) syncWorkspacePriorities(
	ctx context.Context,
	session upstream.Session,
	userID string,
	adminAccountID string,
	inventory map[string]*priorityTargetInventory,
	inventoryComplete bool,
	healthStates []ConnectionHealthState,
	syncStates []PrioritySyncState,
) {
	statesByTarget := make(map[string][]ConnectionHealthState)
	for _, state := range healthStates {
		if _, isTarget := parseTargetID(state.ConnectionID); isTarget {
			statesByTarget[state.ConnectionID] = append(statesByTarget[state.ConnectionID], state)
		}
	}

	// 上游 API Key 真实成本倍率（分组倍率 × 站点充值倍率）优先于 admin 售卖分组倍率。
	// 同一售卖分组里常有 0.1x / 0.2x 等多条上游；若只按 admin 分组倍率排序，健康目标会全部落到同一 priority。
	upstreamKeyGroups := s.upstreamKeyGroupsByAdminAccount(ctx, userID, adminAccountID, string(session.Platform))

	managed := make(map[string]*priorityTargetInventory)
	missingMultiplier := make(map[string]struct{})
	distinctMultipliers := make([]float64, 0)
	seenMultipliers := make(map[float64]struct{})
	for targetID, item := range inventory {
		if !hasMultiplierPriorityPolicy(item.policies) {
			continue
		}
		multiplier, ok := resolvePriorityMultiplier(item, upstreamKeyGroups)
		if !ok {
			// 分组没有返回倍率、且也无法解析上游 Key 倍率时进入等待态：既不猜测 1x，
			// 也不把已接管目标恢复成旧优先级。保留同步快照后，倍率恢复可见时下一轮继续。
			missingMultiplier[targetID] = struct{}{}
			continue
		}
		item.multipliers = []float64{multiplier}
		managed[targetID] = item
		if _, exists := seenMultipliers[multiplier]; !exists {
			seenMultipliers[multiplier] = struct{}{}
			distinctMultipliers = append(distinctMultipliers, multiplier)
		}
	}
	sort.Float64s(distinctMultipliers)
	multiplierRank := make(map[float64]int, len(distinctMultipliers))
	for rank, multiplier := range distinctMultipliers {
		multiplierRank[multiplier] = rank
	}

	storedByTarget := make(map[string]PrioritySyncState, len(syncStates))
	for _, state := range syncStates {
		storedByTarget[state.TargetID] = state
	}

	for targetID, item := range managed {
		multiplier := item.multipliers[0]
		activeModels := make(map[string]struct{})
		if !hasMultiplierOnlyPolicy(item.policies) {
			for _, spec := range candidateModelSpecs(item.target.Models, item.policies) {
				// 关闭自动降级后模型状态不会继续推进，因此不能让历史 suspended/degraded
				// 状态永久影响倍率排序。倍率本身继续生效，但健康层级回到未配置档。
				if spec.policy.AutoDegradeEnabled {
					activeModels[spec.modelName] = struct{}{}
				}
			}
		}
		activeStates := make([]ConnectionHealthState, 0, len(activeModels))
		for _, state := range statesByTarget[targetID] {
			if _, active := activeModels[state.ModelName]; active {
				activeStates = append(activeStates, state)
			}
		}
		desired := desiredManagedPriorityForPlatformWithExpected(
			session.Platform, activeStates, multiplierRank[multiplier], len(activeModels),
		)
		stored, exists := storedByTarget[targetID]
		if !exists {
			stored = PrioritySyncState{
				UserID: userID, AdminAccountID: adminAccountID, TargetID: targetID,
				OriginalPriority: item.currentPriority, LastAppliedPriority: item.currentPriority,
			}
		}
		if stored.Conflict {
			continue
		}
		if stored.PendingPriority != nil && item.currentPriority == *stored.PendingPriority {
			stored.LastAppliedPriority = *stored.PendingPriority
			stored.PendingPriority = nil
		}
		if exists && item.currentPriority != stored.LastAppliedPriority && stored.PendingPriority == nil {
			current := item.currentPriority
			stored.Conflict = true
			stored.LastConflictPriority = &current
			stored.EffectiveMultiplier = multiplier
			if err := s.repo.UpsertPrioritySyncState(ctx, stored); err != nil {
				log.Printf("[connection-health] priority conflict state save failed target_id=%s err=%v", targetID, err)
			}
			continue
		}
		if exists && stored.PendingPriority != nil && item.currentPriority != stored.LastAppliedPriority {
			current := item.currentPriority
			stored.Conflict = true
			stored.PendingPriority = nil
			stored.LastConflictPriority = &current
			stored.EffectiveMultiplier = multiplier
			if err := s.repo.UpsertPrioritySyncState(ctx, stored); err != nil {
				log.Printf("[connection-health] priority pending conflict state save failed target_id=%s err=%v", targetID, err)
			}
			continue
		}
		if item.currentPriority != desired {
			pending := desired
			stored.PendingPriority = &pending
			stored.EffectiveMultiplier = multiplier
			if err := s.repo.UpsertPrioritySyncState(ctx, stored); err != nil {
				log.Printf("[connection-health] priority sync intent save failed target_id=%s err=%v", targetID, err)
				continue
			}
			if err := s.priorityActions.UpdateAdminTargetPriority(session, item.target.AccountID, desired); err != nil {
				log.Printf("[connection-health] priority sync update failed target_id=%s err=%v", targetID, err)
				continue
			}
		}
		stored.LastAppliedPriority = desired
		stored.PendingPriority = nil
		stored.EffectiveMultiplier = multiplier
		stored.Conflict = false
		stored.LastConflictPriority = nil
		if err := s.repo.UpsertPrioritySyncState(ctx, stored); err != nil {
			log.Printf("[connection-health] priority sync state save failed target_id=%s err=%v", targetID, err)
		}
	}

	// 不再被任何倍率策略覆盖的目标恢复接管前优先级。若管理员已经人工改过，则保留人工值。
	for targetID, stored := range storedByTarget {
		if _, stillManaged := managed[targetID]; stillManaged {
			continue
		}
		if _, waitingForMultiplier := missingMultiplier[targetID]; waitingForMultiplier {
			continue
		}
		item := inventory[targetID]
		if item == nil {
			if !inventoryComplete {
				// 分组读取失败时无法证明目标已经消失，保留当前优先级和同步快照，
				// 等下一次完整扫描再决定是否恢复。
				continue
			}
			if stored.Conflict {
				// 已确认目标不再受策略管理，但人工修改过的值不能被原始快照覆盖。
				if err := s.repo.DeletePrioritySyncState(ctx, userID, adminAccountID, targetID); err != nil {
					log.Printf("[connection-health] missing conflicted target priority state delete failed target_id=%s err=%v", targetID, err)
				}
				continue
			}
			parsed, ok := parseTargetID(targetID)
			if !ok || parsed.adminAccountID != adminAccountID || parsed.platform != string(session.Platform) {
				continue
			}
			pending := stored.OriginalPriority
			stored.PendingPriority = &pending
			if err := s.repo.UpsertPrioritySyncState(ctx, stored); err != nil {
				log.Printf("[connection-health] missing target priority restore intent save failed target_id=%s err=%v", targetID, err)
				continue
			}
			if err := s.priorityActions.UpdateAdminTargetPriority(session, parsed.accountID, stored.OriginalPriority); err != nil {
				log.Printf("[connection-health] missing target priority restore failed target_id=%s err=%v", targetID, err)
				continue
			}
			if err := s.repo.DeletePrioritySyncState(ctx, userID, adminAccountID, targetID); err != nil {
				log.Printf("[connection-health] missing target priority state delete failed target_id=%s err=%v", targetID, err)
			}
			continue
		}
		if stored.PendingPriority != nil && item.currentPriority == *stored.PendingPriority {
			stored.LastAppliedPriority = *stored.PendingPriority
			stored.PendingPriority = nil
		}
		if !stored.Conflict && item.currentPriority == stored.LastAppliedPriority && item.currentPriority != stored.OriginalPriority {
			pending := stored.OriginalPriority
			stored.PendingPriority = &pending
			if err := s.repo.UpsertPrioritySyncState(ctx, stored); err != nil {
				log.Printf("[connection-health] priority restore intent save failed target_id=%s err=%v", targetID, err)
				continue
			}
			if err := s.priorityActions.UpdateAdminTargetPriority(session, item.target.AccountID, stored.OriginalPriority); err != nil {
				log.Printf("[connection-health] priority restore failed target_id=%s err=%v", targetID, err)
				continue
			}
		}
		if err := s.repo.DeletePrioritySyncState(ctx, userID, adminAccountID, targetID); err != nil {
			log.Printf("[connection-health] priority sync state delete failed target_id=%s err=%v", targetID, err)
		}
	}
}

// 健康延迟分档（仅在可调度模型全部 healthy 时生效；已从上游白名单摘除的模型不在 states 中）。
// 平均延迟阈值：<3s / <10s / <30s / ≥30s（无延迟数据按最差档）。
const (
	latencyTierFastMS   = 3000
	latencyTierMediumMS = 10000
	latencyTierSlowMS   = 30000

	// Sub2API：数值越小越优先。各档互不重叠，每档最多 999 个倍率名次。
	sub2APIHealthyLatency0Base = 1     // avg < 3s
	sub2APIHealthyLatency1Base = 1000  // avg < 10s
	sub2APIHealthyLatency2Base = 2000  // avg < 30s
	sub2APIHealthyLatency3Base = 3000  // avg ≥ 30s 或无延迟
	sub2APIRecoveringBase      = 10000
	sub2APIDegradedBase        = 20000
	sub2APIUnconfiguredBase    = 30000
	sub2APISuspendedPriority   = 100000
	sub2APIBandWidth           = 1000

	// NewAPI：数值越大越优先。健康按延迟再分四档，再叠倍率分。
	newAPIHealthyLatency0Base = 70000
	newAPIHealthyLatency1Base = 60000
	newAPIHealthyLatency2Base = 50000
	newAPIHealthyLatency3Base = 40000
	newAPIRecoveringBase      = 30000
	newAPIDegradedBase        = 20000
	newAPIUnconfiguredBase    = 10000
	newAPISuspendedPriority   = 1
)

// desiredManagedPriorityForPlatform 按平台真实语义计算优先级：NewAPI 沿用「分数越高越优先」；
// Sub2API 使用紧凑的小数值状态分段，数值越小越优先。
func desiredManagedPriorityForPlatform(platform upstream.Platform, states []ConnectionHealthState, multiplierRank int) int {
	if platform == upstream.PlatformSub2API {
		return desiredSub2APIManagedPriority(states, multiplierRank, len(states))
	}
	return desiredManagedPriority(states, multiplierRank)
}

func desiredManagedPriorityForPlatformWithExpected(platform upstream.Platform, states []ConnectionHealthState, multiplierRank int, expectedModels int) int {
	if platform == upstream.PlatformSub2API {
		return desiredSub2APIManagedPriority(states, multiplierRank, expectedModels)
	}
	score := desiredManagedPriority(states, multiplierRank)
	if len(states) < expectedModels && score != newAPISuspendedPriority {
		// 缺探活状态视为待配置，不是健康。已知 suspended/disabled 仍保持最差档。
		priceScore := maxInt(0, 999-multiplierRank)
		score = newAPIUnconfiguredBase + priceScore
	}
	return score
}

// desiredSub2APIManagedPriority 使用 Sub2API「数值越小越优先」：
//
//	延迟档(healthy+degraded) > 恢复中 > 观察中 > 待配置 > 暂停/禁用。
//
// 「降级」是软失败后的权重下滑态，仍可调度，因此并入平均延迟分档，不再单独压到降级档。
// 「观察中」是暂停后的恢复观察（权重 0），「恢复中」是逐步抬权重，仍单独低于延迟档。
// states 仅应包含仍可调度的模型（上游白名单内）；已摘除的 suspended 不得传入。
// 同一档内 multiplierRank 越小 priority 越小；rank 超出档宽时在档末并列。
func desiredSub2APIManagedPriority(states []ConnectionHealthState, multiplierRank int, expectedModels int) int {
	for _, state := range states {
		if state.State == StateDisabled || state.State == StateSuspended {
			return sub2APISuspendedPriority
		}
	}
	if len(states) < expectedModels {
		return sub2APIPriorityWithinBand(sub2APIUnconfiguredBase, sub2APIUnconfiguredBase+sub2APIBandWidth, multiplierRank)
	}

	hasObserving := false
	hasRecovering := false
	for _, state := range states {
		switch state.State {
		case StateObserving:
			hasObserving = true
		case StateRecovering:
			hasRecovering = true
		}
	}
	// 观察中：暂停后权重仍为 0，不能按延迟冒充健康可调度。
	if hasObserving {
		return sub2APIPriorityWithinBand(sub2APIDegradedBase, sub2APIDegradedBase+sub2APIBandWidth, multiplierRank)
	}
	if hasRecovering {
		return sub2APIPriorityWithinBand(sub2APIRecoveringBase, sub2APIRecoveringBase+sub2APIBandWidth, multiplierRank)
	}

	// 仅倍率策略（无探活状态）不套延迟分档，直接按倍率排在最快健康档。
	if len(states) == 0 {
		return sub2APIPriorityWithinBand(sub2APIHealthyLatency0Base, sub2APIHealthyLatency0Base+sub2APIBandWidth, multiplierRank)
	}

	// healthy + degraded：按可调度模型平均延迟分档，再按倍率。
	base := sub2APIHealthyBaseForLatencyTier(latencyTierRank(states))
	return sub2APIPriorityWithinBand(base, base+sub2APIBandWidth, multiplierRank)
}

func sub2APIHealthyBaseForLatencyTier(tier int) int {
	switch tier {
	case 0:
		return sub2APIHealthyLatency0Base
	case 1:
		return sub2APIHealthyLatency1Base
	case 2:
		return sub2APIHealthyLatency2Base
	default:
		return sub2APIHealthyLatency3Base
	}
}

func sub2APIPriorityWithinBand(base int, nextBase int, multiplierRank int) int {
	offset := maxInt(0, multiplierRank)
	return base + minInt(offset, nextBase-base-1)
}

// latencyTierRank 返回 0(最快) .. 3(最慢/无数据)。
// 对 states 中有 LastLatencyMs 的模型求平均；全部缺失时落到最差档，避免无延迟数据伪装成最快。
func latencyTierRank(states []ConnectionHealthState) int {
	sum := 0
	count := 0
	for _, state := range states {
		if state.LastLatencyMs == nil || *state.LastLatencyMs < 0 {
			continue
		}
		sum += *state.LastLatencyMs
		count++
	}
	if count == 0 {
		return 3
	}
	avg := sum / count
	switch {
	case avg < latencyTierFastMS:
		return 0
	case avg < latencyTierMediumMS:
		return 1
	case avg < latencyTierSlowMS:
		return 2
	default:
		return 3
	}
}

func hasMultiplierPriorityPolicy(policies []Policy) bool {
	for _, policy := range policies {
		if policy.Enabled && normalizePriorityMode(policy.PriorityMode) == PriorityModeMultiplier {
			return true
		}
	}
	return false
}

// resolvePriorityMultiplier 决定倍率排序使用的成本倍率：
//  1. 优先使用 real_connections 绑定的上游 API Key 成本倍率（分组倍率 × 站点充值倍率）；
//  2. 无法可靠解析时回退到 admin 分组倍率中的最低值（目标跨多分组时取 min）。
//
// 返回 ok=false 表示本轮没有可用倍率，调用方应保持等待态而不是猜测 1x。
func resolvePriorityMultiplier(item *priorityTargetInventory, upstreamKeyGroups map[string]upstreamKeyGroupInfo) (float64, bool) {
	if item == nil {
		return 0, false
	}
	if info, ok := upstreamKeyGroups[strings.TrimSpace(item.target.AccountID)]; ok && info.multiplier != nil {
		return *info.multiplier, true
	}
	if len(item.multipliers) == 0 {
		return 0, false
	}
	return minFloat(item.multipliers), true
}

// hasMultiplierOnlyPolicy 让明确的仅倍率策略成为同一目标的优先级依据。即使目标还叠加了
// 一条负责记录健康状态的探活策略，健康状态也不会重新参与 priority 排名。
func hasMultiplierOnlyPolicy(policies []Policy) bool {
	for _, policy := range policies {
		if policy.Enabled && normalizeStrategyMode(policy.StrategyMode) == StrategyModeMultiplierOnly {
			return true
		}
	}
	return false
}

func minFloat(values []float64) float64 {
	minValue := values[0]
	for _, value := range values[1:] {
		if value < minValue {
			minValue = value
		}
	}
	return minValue
}

// desiredManagedPriority 计算 NewAPI「分数越高越优先」的路由分数：
// 延迟档(healthy+degraded) > 恢复中 > 观察中 > 待配置 > 暂停/禁用。
// 「降级」并入平均延迟分档；观察中/恢复中仍单独低于延迟档。
// states 仅应包含仍可调度的模型；已摘除模型不得传入。
// 同一档内，上游成本倍率排名越靠前（倍率越低）分数越大。
func desiredManagedPriority(states []ConnectionHealthState, multiplierRank int) int {
	priceScore := 999 - multiplierRank
	if priceScore < 0 {
		priceScore = 0
	}
	if len(states) == 0 {
		// 仅倍率 / 无探活状态：落在待配置档，仍按倍率区分。
		return newAPIUnconfiguredBase + priceScore
	}

	weight := 100
	hasObserving := false
	hasRecovering := false
	for _, state := range states {
		if state.CurrentWeight < weight {
			weight = state.CurrentWeight
		}
		switch state.State {
		case StateDisabled, StateSuspended:
			return newAPISuspendedPriority
		case StateObserving:
			hasObserving = true
		case StateRecovering:
			hasRecovering = true
		}
	}
	if hasObserving {
		return newAPIDegradedBase + maxInt(0, minInt(100, weight))*10 + priceScore
	}
	if hasRecovering {
		return newAPIRecoveringBase + maxInt(0, minInt(100, weight))*50 + priceScore
	}

	// healthy + degraded 走延迟分档。
	base := newAPIHealthyBaseForLatencyTier(latencyTierRank(states))
	return base + priceScore
}

func newAPIHealthyBaseForLatencyTier(tier int) int {
	switch tier {
	case 0:
		return newAPIHealthyLatency0Base
	case 1:
		return newAPIHealthyLatency1Base
	case 2:
		return newAPIHealthyLatency2Base
	default:
		return newAPIHealthyLatency3Base
	}
}
