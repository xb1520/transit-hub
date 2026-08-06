package connection_health

import (
	"context"
	"log"
	"strings"

	"transithub/backend/internal/modules/upstream"
)

const (
	RemoteActionSkippedTargetConflict          = "skipped_target_conflict"
	RemoteActionSkippedTargetInitiallyDisabled = "skipped_target_initially_disabled"
)

// reconcileTargetRemoteAction 把同一账号当前仍启用的全部模型状态聚合成一次上游动作。
// 模型仍独立记录健康，但账号/渠道是共享资源，不能让后执行的健康模型覆盖先前故障模型的停用决定。
func (s *Service) reconcileTargetRemoteAction(
	ctx context.Context,
	userID string,
	adminAccountID string,
	session upstream.Session,
	target AdminProbeTarget,
	specs []probeModelSpec,
) (string, error) {
	// 模型限制：只要策略开启自动降级且支持探活即可写入上游 models（不强制开远端启停）。
	// 账号启停：仍要求 AutoRemoteActionEnabled。
	modelLimitModels := make(map[string]struct{})
	statusModels := make(map[string]struct{})
	for _, spec := range specs {
		if !spec.policy.Enabled {
			continue
		}
		if policySupportsProbing(spec.policy) && spec.policy.AutoDegradeEnabled {
			modelLimitModels[spec.modelName] = struct{}{}
		}
		if policyRemoteActionEnabled(spec.policy) {
			statusModels[spec.modelName] = struct{}{}
		}
	}
	if len(modelLimitModels) == 0 && len(statusModels) == 0 {
		return "", nil
	}

	allStates, err := s.repo.ListStatesByConnection(ctx, target.TargetID)
	if err != nil {
		return "", err
	}
	// 聚合用：模型限制看 modelLimitModels；启停看 statusModels。两者并集用于读状态。
	unionModels := make(map[string]struct{}, len(modelLimitModels)+len(statusModels))
	for name := range modelLimitModels {
		unionModels[name] = struct{}{}
	}
	for name := range statusModels {
		unionModels[name] = struct{}{}
	}
	states := make([]ConnectionHealthState, 0, len(unionModels))
	statusStates := make([]ConnectionHealthState, 0, len(statusModels))
	modelLimitStates := make([]ConnectionHealthState, 0, len(modelLimitModels))
	for _, state := range allStates {
		if _, ok := unionModels[state.ModelName]; ok {
			states = append(states, state)
		}
		if _, ok := statusModels[state.ModelName]; ok {
			statusStates = append(statusStates, state)
		}
		if _, ok := modelLimitModels[state.ModelName]; ok {
			modelLimitStates = append(modelLimitStates, state)
		}
	}
	// 无任何本地状态时仍可能需要恢复历史 status=inactive（旧降级路径），不能直接 return。
	if len(states) == 0 && len(statusModels) == 0 && len(modelLimitModels) == 0 {
		return "", nil
	}
	statesComplete := len(statusStates) == len(statusModels) && len(statusModels) > 0
	if len(statusModels) == 0 {
		// 仅模型限制路径：不要求 status 完整。
		statesComplete = len(modelLimitStates) == len(modelLimitModels)
	}

	stored, err := s.repo.GetTargetActionState(ctx, userID, adminAccountID, target.TargetID)
	if err != nil {
		return "", err
	}
	// 启停决策只看 statusModels 对应状态；模型限制单独用 modelLimitStates。
	statusAggStates := statusStates
	if len(statusModels) == 0 {
		statusAggStates = nil
	}
	allHealthy, blocked, minWeight := aggregateTargetStates(statusAggStates)
	hasHealthyEffective := hasHealthyEffectiveModel(statusAggStates)
	if len(statusModels) == 0 {
		allHealthy = true
		blocked = false
		hasHealthyEffective = false
	}
	// fullyHealthy：所有受控模型都有状态且有效模型全健康（用于清理快照）。
	// 不完整状态时仍可根据 hasHealthyEffective 恢复账号启停，避免「有健康模型却永久停用」。
	// 无状态行时：不视为 fullyHealthy，但可走「历史 inactive 恢复」分支。
	fullyHealthy := allHealthy && (len(statusModels) == 0 || statesComplete) && len(statusAggStates) > 0
	// 模型限制摘除即使不阻塞账号启停，也需要 reconcile（可与启停路径独立）。
	needsModelLimits := target.Platform == string(upstream.PlatformSub2API) && len(modelLimitModels) > 0 &&
		(hasModelLimitExclusion(modelLimitStates) || (stored != nil && (strings.TrimSpace(stored.OriginalModels) != "" || strings.TrimSpace(stored.LastAppliedModels) != "")))
	// 账号启停：阻塞 / 恢复中 / 已有快照 / 当前摘流但应恢复调度。
	// Sub2API 用「调度开关」表示摘流，逻辑状态仍编码为 active/inactive。
	currentStatusPreview := effectiveLogicalStatus(target)
	trafficDisabled := !logicalStatusEnabled(target.Platform, currentStatusPreview)
	// 历史 status=inactive 恢复条件（满足任一即可）：
	// 1) 有健康有效模型；2) 本地有接管快照；3) 事件/状态里有系统降级痕迹；
	// 4) 未判定 blocked（含尚无探活状态）——避免旧 inactive 在无人探活时永久卡住。
	needsLegacyInactiveRestore := trafficDisabled && len(statusModels) > 0 && !blocked &&
		(hasHealthyEffective ||
			stored != nil ||
			legacyTargetWasManaged(statusAggStates) ||
			legacyTargetWasManaged(allStates) ||
			len(statusAggStates) == 0)
	needsStatusAction := len(statusModels) > 0 && (blocked || hasRecoveringState(statusAggStates) ||
		(stored != nil && (stored.OriginalStatus != "" || stored.LastAppliedStatus != "")) ||
		(trafficDisabled && hasHealthyEffective) ||
		needsLegacyInactiveRestore)
	if stored != nil && (strings.TrimSpace(stored.OriginalModels) != "" || strings.TrimSpace(stored.LastAppliedModels) != "") {
		// 已建立模型限制快照时也算「已接管」，避免丢失快照。
		needsStatusAction = needsStatusAction || len(statusModels) > 0
	}
	if !statesComplete && !blocked && !needsModelLimits && !needsStatusAction {
		return "", nil
	}
	if stored == nil && !needsStatusAction && !needsModelLimits {
		return "", nil
	}
	// 已接管但状态不完整：仅在 blocked / 模型限制 / 需要恢复启停时继续。
	if stored != nil && !statesComplete && !blocked && !needsModelLimits && !needsStatusAction {
		return "", nil
	}

	currentStatus := effectiveLogicalStatus(target)
	currentWeight := normalizedTargetWeight(target)

	// —— 仅模型限制、尚未接管启停：用轻量快照，绝不改账号 active/inactive ——
	if stored == nil && needsModelLimits && !needsStatusAction {
		stored = &TargetActionState{
			UserID: userID, AdminAccountID: adminAccountID, TargetID: target.TargetID,
			OriginalStatus: currentStatus, OriginalWeight: cloneIntPointer(currentWeight),
			LastAppliedStatus: currentStatus, LastAppliedWeight: cloneIntPointer(currentWeight),
		}
		if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
			return "", err
		}
		return s.reconcileTargetModelLimits(ctx, session, target, modelLimitStates, stored)
	}

	if stored == nil {
		originalStatus := currentStatus
		originalWeight := cloneIntPointer(currentWeight)
		// 账号当前摘流/停用且无历史快照时：
		// 1) 事件里有系统摘流痕迹，或
		// 2) 已有健康有效模型，或
		// 3) 需要历史 inactive 恢复（needsLegacyInactiveRestore）
		// → 按默认启用建立快照并继续恢复，避免永久钉死。
		// 明确 blocked 且无系统痕迹 → 视为用户原本停用，不擅自启用。
		if !logicalStatusEnabled(target.Platform, currentStatus) {
			legacyManaged := legacyTargetWasManaged(statusAggStates) || legacyTargetWasManaged(allStates)
			if legacyManaged || (hasHealthyEffective && !blocked) || needsLegacyInactiveRestore {
				originalStatus, originalWeight = legacyOriginalTargetState(target.Platform)
				log.Printf("[connection-health] adopt disabled account for restore target_id=%s account_id=%s healthy=%v legacyManaged=%v emptyStates=%v",
					target.TargetID, target.AccountID, hasHealthyEffective, legacyManaged, len(statusAggStates) == 0)
			} else if needsModelLimits {
				// 仅模型限制：不改启停。
				stored = &TargetActionState{
					UserID: userID, AdminAccountID: adminAccountID, TargetID: target.TargetID,
					OriginalStatus: currentStatus, OriginalWeight: cloneIntPointer(currentWeight),
					LastAppliedStatus: currentStatus, LastAppliedWeight: cloneIntPointer(currentWeight),
				}
				if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
					return "", err
				}
				return s.reconcileTargetModelLimits(ctx, session, target, modelLimitStates, stored)
			} else {
				return RemoteActionSkippedTargetInitiallyDisabled, nil
			}
		}
		stored = &TargetActionState{
			UserID: userID, AdminAccountID: adminAccountID, TargetID: target.TargetID,
			OriginalStatus: originalStatus, OriginalWeight: cloneIntPointer(originalWeight),
			LastAppliedStatus: currentStatus, LastAppliedWeight: cloneIntPointer(currentWeight),
		}
		if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
			return "", err
		}
	} else if len(statusModels) > 0 && targetActionCheckpointConflicted(target, stored, currentStatus, currentWeight) {
		// 当前摘流 + 有健康有效模型：优先视为「写入失败/进程中断/error 残留」，不要标死 conflict。
		// 只有账号已可调度却与 lastApplied 不一致时，才当作用户手动改动并停止覆盖。
		if !logicalStatusEnabled(target.Platform, currentStatus) && hasHealthyEffective && !blocked {
			log.Printf("[connection-health] ignore status mismatch for restore target_id=%s current=%s lastApplied=%s pending=%s",
				target.TargetID, currentStatus, stored.LastAppliedStatus, stored.PendingStatus)
			stored.Conflict = false
			stored.PendingStatus = ""
			stored.PendingWeight = nil
		} else {
			stored.Conflict = true
			stored.PendingStatus = ""
			stored.PendingWeight = nil
			stored.PendingModels = ""
			if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
				return "", err
			}
			log.Printf("[connection-health] skip target conflict target_id=%s current=%s lastApplied=%s",
				target.TargetID, currentStatus, stored.LastAppliedStatus)
			return RemoteActionSkippedTargetConflict, nil
		}
	}
	if stored.Conflict {
		// 摘流且有健康模型：清掉陈旧 conflict，允许恢复。
		if !logicalStatusEnabled(target.Platform, currentStatus) && hasHealthyEffective && !blocked {
			log.Printf("[connection-health] clear conflict for disabled healthy target_id=%s", target.TargetID)
			stored.Conflict = false
		} else if targetStateEqual(target, currentStatus, currentWeight, stored.LastAppliedStatus, stored.LastAppliedWeight) {
			log.Printf("[connection-health] clear stale target conflict target_id=%s current=%s lastApplied=%s",
				target.TargetID, currentStatus, stored.LastAppliedStatus)
			stored.Conflict = false
		} else {
			log.Printf("[connection-health] skip stored conflict target_id=%s current=%s lastApplied=%s",
				target.TargetID, currentStatus, stored.LastAppliedStatus)
			return RemoteActionSkippedTargetConflict, nil
		}
	}

	// 先处理 sub2api 模型限制摘除/恢复；动作标签可能与账号启停叠加。
	var modelsAction string
	var modelsErr error
	if needsModelLimits {
		modelsAction, modelsErr = s.reconcileTargetModelLimits(ctx, session, target, modelLimitStates, stored)
		if modelsErr != nil {
			log.Printf("[connection-health] reconcile model limits failed target_id=%s action=%s err=%v", target.TargetID, modelsAction, modelsErr)
			if modelsAction == "" {
				modelsAction = RemoteActionSub2APIModelsUpdateFailed
			}
		}
	}

	// 无远端启停权限时，只做模型限制。
	if len(statusModels) == 0 {
		return modelsAction, modelsErr
	}

	desiredStatus, desiredWeight := desiredTargetState(target.Platform, fullyHealthy, blocked, hasHealthyEffective, minWeight, *stored)
	statusEqual := targetStateEqual(target, currentStatus, currentWeight, desiredStatus, desiredWeight)
	if statusEqual {
		stored.LastAppliedStatus = desiredStatus
		stored.LastAppliedWeight = cloneIntPointer(desiredWeight)
		stored.PendingStatus = ""
		stored.PendingWeight = nil
		// 全部健康且模型限制也已恢复到原始列表时，才能删除接管快照。
		if fullyHealthy && !hasManagedModelLimits(stored) {
			if modelsAction != "" {
				_ = s.repo.DeleteTargetActionState(ctx, userID, adminAccountID, target.TargetID)
				return modelsAction, modelsErr
			}
			return "", s.repo.DeleteTargetActionState(ctx, userID, adminAccountID, target.TargetID)
		}
		if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
			return modelsAction, err
		}
		return modelsAction, modelsErr
	}

	// Persist the intended value before touching the upstream. A later database failure can
	// then be recognized as a completed system write instead of a manual conflict.
	stored.PendingStatus = desiredStatus
	stored.PendingWeight = cloneIntPointer(desiredWeight)
	if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
		return modelsAction, err
	}
	log.Printf("[connection-health] apply account status target_id=%s account_id=%s current=%s desired=%s fullyHealthy=%v blocked=%v hasHealthy=%v original=%s lastApplied=%s",
		target.TargetID, target.AccountID, currentStatus, desiredStatus, fullyHealthy, blocked, hasHealthyEffective, stored.OriginalStatus, stored.LastAppliedStatus)
	action, actionErr := s.dispatcher.ApplyTargetState(ctx, session, target, desiredWeight, desiredStatus)
	if actionErr != nil {
		log.Printf("[connection-health] aggregate target action failed target_id=%s action=%s err=%v", target.TargetID, action, actionErr)
		return joinRemoteActions(modelsAction, action), actionErr
	}
	stored.LastAppliedStatus = desiredStatus
	stored.LastAppliedWeight = cloneIntPointer(desiredWeight)
	stored.PendingStatus = ""
	stored.PendingWeight = nil
	if fullyHealthy && !hasManagedModelLimits(stored) {
		return joinRemoteActions(modelsAction, action), s.repo.DeleteTargetActionState(ctx, userID, adminAccountID, target.TargetID)
	}
	if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
		return joinRemoteActions(modelsAction, action), err
	}
	return joinRemoteActions(modelsAction, action), nil
}

// restoreUnmanagedTargetActions 恢复已经失去有效自动动作策略的目标。用户解绑分组、禁用策略、
// 删除最后一个模型或把目标加入排除列表后，都不能把此前由系统暂停的账号永久留在上游。
func (s *Service) restoreUnmanagedTargetActions(
	ctx context.Context,
	policies []Policy,
	targetAssignments []PolicyAssignment,
	groupAssignments []GroupPolicyAssignment,
	exclusions []GroupTargetExclusion,
	states []TargetActionState,
	inventoryCache adminInventoryCache,
) {
	if len(states) == 0 {
		return
	}
	targetPolicies := assignedEnabledPoliciesByTarget(policies, targetAssignments)
	groupPolicies := assignedEnabledPoliciesByGroup(policies, groupAssignments)
	excluded := groupTargetExclusionIndex(exclusions)
	for _, stored := range states {
		inventory, err := s.loadAdminInventory(ctx, stored.UserID, stored.AdminAccountID, inventoryCache)
		if err != nil {
			log.Printf("[connection-health] restore unmanaged target inventory failed target_id=%s err=%v", stored.TargetID, err)
			continue
		}
		inventoryComplete := true
		for _, groupInventory := range inventory.groups {
			if groupInventory.err != nil {
				inventoryComplete = false
				break
			}
		}
		if !inventoryComplete {
			// 任一分组成员读取失败时无法证明目标已经失去全部管理关系，保持当前状态更安全。
			continue
		}
		var target AdminProbeTarget
		found := false
		effectivePolicies := append([]Policy(nil), targetPolicies[stored.UserID+"|"+stored.AdminAccountID][stored.TargetID]...)
		for _, groupInventory := range inventory.groups {
			if groupInventory.err != nil {
				continue
			}
			for _, account := range groupInventory.accounts {
				targetID := buildTargetID(string(inventory.session.Platform), stored.AdminAccountID, account.ID)
				if targetID != stored.TargetID {
					continue
				}
				if !found {
					target = AdminProbeTarget{
						TargetID: targetID, Platform: string(inventory.session.Platform),
						AdminGroupID: groupInventory.group.ID, AdminGroupName: groupInventory.group.Name,
						AccountID: account.ID, AccountName: account.Name, AccountStatus: account.Status,
						AccountSchedulable: account.Schedulable, AccountWeight: cloneIntPointer(account.Weight),
						ProviderFamily: account.Platform, Models: splitModelList(account.Models),
					}
					found = true
				}
				workspaceKey := stored.UserID + "|" + stored.AdminAccountID
				if !excluded[workspaceKey][groupInventory.group.ID][targetID] {
					effectivePolicies = mergePoliciesByID(effectivePolicies, groupPolicies[workspaceKey][groupInventory.group.ID])
				}
			}
		}
		if hasRemoteActionModel(candidateModelSpecs(target.Models, effectivePolicies)) {
			continue
		}
		targetVisible := found
		if !found {
			parsed, ok := parseTargetID(stored.TargetID)
			if !ok || parsed.adminAccountID != stored.AdminAccountID || parsed.platform != string(inventory.session.Platform) {
				continue
			}
			// The account can remain upstream after being removed from every group. We no longer
			// have a list snapshot for conflict detection, but restoring the captured original
			// value is safer than leaving a system-disabled account stuck forever.
			// 无列表快照时：LastApplied 逻辑 inactive 表示关调度（或历史 status 停用）。
			// 恢复时 applySub2APITrafficControl 会开调度，并在 AccountStatus 非 active 时写回 active。
			schedulable := stored.LastAppliedStatus == "active" || stored.LastAppliedStatus == "1"
			target = AdminProbeTarget{
				TargetID: stored.TargetID, Platform: parsed.platform, AccountID: parsed.accountID,
				AccountStatus: stored.LastAppliedStatus, AccountSchedulable: &schedulable,
				AccountWeight: cloneIntPointer(stored.LastAppliedWeight),
			}
		}
		currentStatus := effectiveLogicalStatus(target)
		currentWeight := normalizedTargetWeight(target)
		if stored.Conflict || (targetVisible && targetActionCheckpointConflicted(target, &stored, currentStatus, currentWeight)) {
			stored.Conflict = true
			stored.PendingStatus = ""
			stored.PendingWeight = nil
			if err := s.repo.UpsertTargetActionState(ctx, stored); err != nil {
				log.Printf("[connection-health] store unmanaged target conflict failed target_id=%s err=%v", stored.TargetID, err)
			}
			continue
		}
		if targetVisible && targetStateEqual(target, currentStatus, currentWeight, stored.OriginalStatus, stored.OriginalWeight) {
			if err := s.repo.DeleteTargetActionState(ctx, stored.UserID, stored.AdminAccountID, stored.TargetID); err != nil {
				log.Printf("[connection-health] clear restored target action state failed target_id=%s err=%v", stored.TargetID, err)
			}
			continue
		}
		stored.PendingStatus = stored.OriginalStatus
		stored.PendingWeight = cloneIntPointer(stored.OriginalWeight)
		if strings.TrimSpace(stored.OriginalModels) != "" {
			stored.PendingModels = stored.OriginalModels
		}
		if err := s.repo.UpsertTargetActionState(ctx, stored); err != nil {
			log.Printf("[connection-health] store unmanaged target restore intent failed target_id=%s err=%v", stored.TargetID, err)
			continue
		}
		action, actionErr := s.dispatcher.ApplyTargetState(ctx, inventory.session, target, stored.OriginalWeight, stored.OriginalStatus)
		if actionErr != nil {
			log.Printf("[connection-health] restore unmanaged target failed target_id=%s action=%s err=%v", stored.TargetID, action, actionErr)
			continue
		}
		if strings.TrimSpace(stored.OriginalModels) != "" {
			modelsAction, modelsErr := s.dispatcher.ApplyTargetModels(ctx, inventory.session, target, stored.OriginalModels)
			if modelsErr != nil {
				log.Printf("[connection-health] restore unmanaged target models failed target_id=%s action=%s err=%v", stored.TargetID, modelsAction, modelsErr)
			} else {
				action = joinRemoteActions(action, modelsAction)
			}
		}
		s.recordTargetEvent(ctx, stored.UserID, stored.AdminAccountID, target, "", "*", "policy_unmanaged_restore", "", "", nil, "", "", action)
		if err := s.repo.DeleteTargetActionState(ctx, stored.UserID, stored.AdminAccountID, stored.TargetID); err != nil {
			log.Printf("[connection-health] clear unmanaged target action state failed target_id=%s err=%v", stored.TargetID, err)
		}
	}
}

func hasRemoteActionModel(specs []probeModelSpec) bool {
	for _, spec := range specs {
		if spec.policy.Enabled && policyRemoteActionEnabled(spec.policy) {
			return true
		}
	}
	return false
}

func legacyTargetWasManaged(states []ConnectionHealthState) bool {
	for _, state := range states {
		switch state.LastRemoteAction {
		case RemoteActionSub2APIStatusInactive, RemoteActionSub2APISchedulableOff, "newapi_channel_disabled":
			return true
		}
	}
	return false
}

func legacyOriginalTargetState(platform string) (string, *int) {
	if platform == string(upstream.PlatformNewAPI) {
		weight := 100
		return "1", &weight
	}
	return "active", nil
}

func aggregateTargetStates(states []ConnectionHealthState) (allHealthy bool, blocked bool, minWeight int) {
	// 账号级健康/阻塞只看「未走模型限制摘除」的模型。
	// 问题模型（暂停/观察/禁用/权重0）只摘模型，不得因单个坏模型把整账号 allHealthy/blocked 打坏，
	// 否则同账号其它健康模型也无法继续调度。
	// 仅当「没有任何剩余可调度有效模型」（全被摘除）时才 blocked，关整账号调度。
	allHealthy = true
	minWeight = 100
	effectiveCount := 0
	exclusionCount := 0
	for _, state := range states {
		if isModelLimitExclusionState(state) {
			exclusionCount++
			continue
		}
		effectiveCount++
		if state.State != StateHealthy {
			allHealthy = false
		}
		if state.CurrentWeight < minWeight {
			minWeight = state.CurrentWeight
		}
	}
	if effectiveCount == 0 {
		// 没有任何有效模型：若全是摘除模型则阻塞整账号；若完全无状态则视为健康空集。
		allHealthy = exclusionCount == 0
		if exclusionCount > 0 {
			blocked = true
		}
	}
	return allHealthy, blocked, minWeight
}

// isModelLimitExclusionState 判定模型是否应暂时从 sub2api「模型限制」中摘除（只动该模型，不关整账号）。
// 产品语义：
//   - suspended：探活暂停
//   - observing：恢复观察（尚未完全健康，先摘掉避免观察期误调度）
//   - disabled：模型级禁用（UI 即便暂无入口，语义也是单模型）
//   - 非 healthy 且 currentWeight <= 0：本地健康权重为 0，不应再被调度
// healthy 永不因 weight 字段缺省/为 0 被误摘（旧状态行可能未写 weight）。
// 整账号关调度仅在 aggregateTargetStates 判定「无剩余有效模型」时发生。
func isModelLimitExclusionState(state ConnectionHealthState) bool {
	switch state.State {
	case StateHealthy:
		return false
	case StateSuspended, StateObserving, StateDisabled:
		return true
	default:
		// degraded / recovering / 其它：权重归零才摘除
		return state.CurrentWeight <= 0
	}
}

func hasModelLimitExclusion(states []ConnectionHealthState) bool {
	for _, state := range states {
		if isModelLimitExclusionState(state) {
			return true
		}
	}
	return false
}

func hasManagedModelLimits(stored *TargetActionState) bool {
	if stored == nil {
		return false
	}
	if strings.TrimSpace(stored.PendingModels) != "" {
		return true
	}
	original := normalizeModelListString(stored.OriginalModels)
	applied := normalizeModelListString(stored.LastAppliedModels)
	// 原本不限制：只要写过非空白名单，就仍在管理中。
	if original == "" {
		return applied != ""
	}
	return applied != original
}

// reconcileTargetModelLimits 根据探活状态计算期望的 sub2api models 字段并写入上游。
//
// 基线语义：
//   - OriginalModels 非空：账号原本配置了模型限制，恢复时写回该列表；
//   - OriginalModels 为空：账号原本不限制模型；摘除时写入「已知模型 − 暂停模型」正向白名单，
//     全部恢复后写回空字符串以恢复「不限制」。
func (s *Service) reconcileTargetModelLimits(
	ctx context.Context,
	session upstream.Session,
	target AdminProbeTarget,
	states []ConnectionHealthState,
	stored *TargetActionState,
) (string, error) {
	if stored == nil || target.Platform != string(upstream.PlatformSub2API) {
		return "", nil
	}

	// 首次接管模型限制：用当前上游列表作为「有限制」基线；空列表表示原本不限制。
	if strings.TrimSpace(stored.OriginalModels) == "" && strings.TrimSpace(stored.LastAppliedModels) == "" {
		if !hasModelLimitExclusion(states) {
			return "", nil
		}
		current := joinModelList(target.Models)
		// 仅当上游确实配置了模型限制时才固化 OriginalModels；空 = 不限制。
		if current != "" {
			stored.OriginalModels = current
			stored.LastAppliedModels = current
		}
		// 不限制时 OriginalModels 保持空，LastApplied 待首次写入白名单后再填。
	}

	desired := desiredModelLimits(stored.OriginalModels, states, target.Models)
	currentModels := joinModelList(target.Models)

	// 冲突检测：上游当前 models 既不等于上次系统写入，也不等于 pending，视为人工修改。
	if targetModelLimitsConflicted(stored, currentModels) {
		stored.Conflict = true
		stored.PendingModels = ""
		if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
			return "", err
		}
		return RemoteActionSkippedTargetConflict, nil
	}

	if modelListsEqual(currentModels, desired) {
		stored.LastAppliedModels = desired
		stored.PendingModels = ""
		return "", s.repo.UpsertTargetActionState(ctx, *stored)
	}

	stored.PendingModels = desired
	if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
		return "", err
	}
	action, actionErr := s.dispatcher.ApplyTargetModels(ctx, session, target, desired)
	if actionErr != nil {
		log.Printf("[connection-health] apply model limits failed target_id=%s account_id=%s desired=%q action=%s err=%v",
			target.TargetID, target.AccountID, desired, action, actionErr)
		return action, actionErr
	}
	log.Printf("[connection-health] applied model limits target_id=%s account_id=%s models=%q original=%q",
		target.TargetID, target.AccountID, desired, stored.OriginalModels)
	stored.LastAppliedModels = desired
	stored.PendingModels = ""
	if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
		return action, err
	}
	return action, nil
}

func targetModelLimitsConflicted(stored *TargetActionState, currentModels string) bool {
	current := normalizeModelListString(currentModels)
	if strings.TrimSpace(stored.PendingModels) != "" {
		if current == normalizeModelListString(stored.PendingModels) {
			// 上游已是 pending 值：视为上次写成功但未确认。
			stored.LastAppliedModels = stored.PendingModels
			stored.PendingModels = ""
			return false
		}
	}
	last := normalizeModelListString(stored.LastAppliedModels)
	if last == "" {
		// 首次管理：当前值应等于 original，否则不强制冲突（允许刚接管）。
		return false
	}
	return current != last && current != normalizeModelListString(stored.PendingModels)
}

// desiredModelLimits 计算应写入上游的模型限制。
// originalModels 非空：从原始限制中去掉暂停模型。
// originalModels 为空：账号原本不限制；无暂停时返回空；有暂停时返回「已知模型 − 暂停」。
func desiredModelLimits(originalModels string, states []ConnectionHealthState, liveModels []string) string {
	excluded := make(map[string]struct{})
	for _, state := range states {
		if isModelLimitExclusionState(state) {
			name := strings.TrimSpace(state.ModelName)
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
	}

	original := splitModelList(originalModels)
	if len(original) == 0 {
		// 不限制：无暂停则保持空；有暂停则建立正向白名单。
		if len(excluded) == 0 {
			return ""
		}
		known := make([]string, 0, len(states)+len(liveModels))
		seen := make(map[string]struct{})
		for _, state := range states {
			name := strings.TrimSpace(state.ModelName)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			known = append(known, name)
		}
		for _, name := range liveModels {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			known = append(known, name)
		}
		kept := make([]string, 0, len(known))
		for _, name := range known {
			if _, drop := excluded[name]; drop {
				continue
			}
			kept = append(kept, name)
		}
		return joinModelList(kept)
	}

	kept := make([]string, 0, len(original))
	for _, model := range original {
		name := strings.TrimSpace(model)
		if name == "" {
			continue
		}
		if _, drop := excluded[name]; drop {
			continue
		}
		kept = append(kept, name)
	}
	return joinModelList(kept)
}

func modelListsEqual(a, b string) bool {
	return normalizeModelListString(a) == normalizeModelListString(b)
}

func joinModelList(models []string) string {
	parts := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		parts = append(parts, name)
	}
	return strings.Join(parts, ",")
}

// normalizeModelListString 用于比较：去空、去重、排序无关的集合相等。
func normalizeModelListString(models string) string {
	parts := splitModelList(models)
	if len(parts) == 0 {
		return ""
	}
	// 稳定比较：排序后 join
	sorted := append([]string(nil), parts...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return strings.Join(sorted, ",")
}

func joinRemoteActions(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, ",")
}

// expandTargetModelsForProbe 若已接管模型限制，用 OriginalModels 作为探活候选来源，
// 否则被摘除的模型会从账号列表消失，永远无法再被探活恢复。
func expandTargetModelsForProbe(target AdminProbeTarget, stored *TargetActionState) AdminProbeTarget {
	if stored == nil {
		return target
	}
	original := splitModelList(stored.OriginalModels)
	if len(original) == 0 {
		return target
	}
	target.Models = original
	return target
}

func hasRecoveringState(states []ConnectionHealthState) bool {
	for _, state := range states {
		if state.State == StateRecovering {
			return true
		}
	}
	return false
}

func desiredTargetState(platform string, fullyHealthy bool, blocked bool, hasHealthyEffective bool, minWeight int, stored TargetActionState) (string, *int) {
	if blocked {
		if platform == string(upstream.PlatformNewAPI) {
			weight := 0
			return "2", &weight
		}
		// Sub2API：逻辑 inactive 表示关调度（ApplyTargetState 会写成 schedulable=false）。
		return "inactive", nil
	}
	// 未阻塞：只要存在健康的有效模型（或全部有效模型健康），账号应保持/恢复可调度。
	// 这样可修复「系统曾关调度 / 误写 inactive」导致的永久摘流。
	// 用户手动改状态会走 conflict 路径，不会进入这里覆盖。
	if fullyHealthy || hasHealthyEffective {
		if logicalStatusEnabled(platform, stored.OriginalStatus) {
			return stored.OriginalStatus, cloneIntPointer(stored.OriginalWeight)
		}
		return legacyOriginalTargetState(platform)
	}
	if platform == string(upstream.PlatformNewAPI) {
		weight := scaledTargetWeight(stored.OriginalWeight, minWeight)
		return "1", &weight
	}
	return "active", nil
}

// effectiveLogicalStatus 把上游真实字段折叠为逻辑启停（active/inactive）。
// Sub2API：status 非 active → inactive；status active 但 schedulable=false → inactive（关调度摘流）。
func effectiveLogicalStatus(target AdminProbeTarget) string {
	if target.Platform == string(upstream.PlatformNewAPI) {
		return normalizeTargetStatus(target.Platform, target.AccountStatus)
	}
	if normalizeTargetStatus(string(upstream.PlatformSub2API), target.AccountStatus) != "active" {
		return "inactive"
	}
	if target.AccountSchedulable != nil && !*target.AccountSchedulable {
		return "inactive"
	}
	return "active"
}

func logicalStatusEnabled(platform, logicalStatus string) bool {
	if platform == string(upstream.PlatformNewAPI) {
		return logicalStatus == "1" || logicalStatus == "active" || logicalStatus == "enabled"
	}
	return logicalStatus == "active"
}

func hasHealthyEffectiveModel(states []ConnectionHealthState) bool {
	for _, state := range states {
		if isModelLimitExclusionState(state) {
			continue
		}
		if state.State == StateHealthy {
			return true
		}
	}
	return false
}

// scaledTargetWeight converts the state machine's 0-100 recovery percentage into the
// channel's real weight. Writing the percentage directly could increase traffic for a
// channel whose original weight was below the current recovery percentage.
func scaledTargetWeight(originalWeight *int, percentage int) int {
	base := 100
	if originalWeight != nil {
		base = maxInt(0, *originalWeight)
	}
	percentage = maxInt(0, minInt(100, percentage))
	if base == 0 || percentage == 0 {
		return 0
	}
	// Round up so a positive original weight receives at least one unit during recovery.
	return (base*percentage + 99) / 100
}

func normalizeTargetStatus(platform string, status string) string {
	normalized := strings.ToLower(strings.TrimSpace(status))
	if platform == string(upstream.PlatformNewAPI) {
		// New API 只有状态 1 表示启用；2（手动禁用）以及未来/其它非 1 状态都按禁用保护。
		// 空值仅用于兼容旧上游和测试夹具未返回 status 的情况。
		if normalized == "" || normalized == "1" || normalized == "active" || normalized == "enabled" {
			return "1"
		}
		return "2"
	}
	// Sub2API 账号 status 仅允许 active / inactive / error（error 在管理端也显示为停用）。
	if normalized == "inactive" || normalized == "disabled" || normalized == "2" ||
		normalized == "error" || normalized == "stopped" || normalized == "ban" || normalized == "banned" {
		return "inactive"
	}
	// 显式 active / enabled / 1 / 空（部分列表缺省）视为启用。
	if normalized == "" || normalized == "active" || normalized == "enabled" || normalized == "1" || normalized == "normal" {
		return "active"
	}
	// 未知枚举按停用处理，避免误判为已恢复而跳过写入。
	return "inactive"
}

func targetStatusEnabled(platform string, status string) bool {
	if platform == string(upstream.PlatformNewAPI) {
		return status == "1"
	}
	return normalizeTargetStatus(platform, status) == "active"
}

func normalizedTargetWeight(target AdminProbeTarget) *int {
	if target.Platform != string(upstream.PlatformNewAPI) {
		return nil
	}
	if target.AccountWeight != nil {
		return cloneIntPointer(target.AccountWeight)
	}
	weight := 100
	return &weight
}

func targetActionConflicted(target AdminProbeTarget, stored TargetActionState, currentStatus string, currentWeight *int) bool {
	if currentStatus != normalizeTargetStatus(target.Platform, stored.LastAppliedStatus) {
		return true
	}
	// 老版本/部分上游列表可能不返回 weight；缺失时只比较状态，不能凭空制造人工冲突。
	if target.Platform == string(upstream.PlatformNewAPI) && target.AccountWeight != nil {
		return !equalIntPointers(currentWeight, stored.LastAppliedWeight)
	}
	return false
}

// targetActionCheckpointConflicted reconciles the two-phase action checkpoint. A current
// value matching Pending means the previous upstream write succeeded but its final database
// acknowledgement did not. A value matching neither Pending nor LastApplied is a real manual
// conflict and must not be overwritten.
func targetActionCheckpointConflicted(target AdminProbeTarget, stored *TargetActionState, currentStatus string, currentWeight *int) bool {
	if stored.PendingStatus == "" {
		return targetActionConflicted(target, *stored, currentStatus, currentWeight)
	}
	if targetStateEqual(target, currentStatus, currentWeight, stored.PendingStatus, stored.PendingWeight) {
		stored.LastAppliedStatus = stored.PendingStatus
		stored.LastAppliedWeight = cloneIntPointer(stored.PendingWeight)
		stored.PendingStatus = ""
		stored.PendingWeight = nil
		return false
	}
	return targetActionConflicted(target, *stored, currentStatus, currentWeight)
}

func targetStateEqual(target AdminProbeTarget, currentStatus string, currentWeight *int, desiredStatus string, desiredWeight *int) bool {
	if currentStatus != normalizeTargetStatus(target.Platform, desiredStatus) {
		return false
	}
	if target.Platform == string(upstream.PlatformNewAPI) && target.AccountWeight != nil {
		return equalIntPointers(currentWeight, desiredWeight)
	}
	return target.Platform != string(upstream.PlatformNewAPI) || target.AccountWeight == nil
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func equalIntPointers(left *int, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
