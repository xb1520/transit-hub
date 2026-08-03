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
	if len(states) == 0 {
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
	if len(statusModels) == 0 {
		allHealthy = true
		blocked = false
	} else {
		allHealthy = allHealthy && statesComplete
	}
	// 模型限制摘除即使不阻塞账号启停，也需要 reconcile（可与启停路径独立）。
	needsModelLimits := target.Platform == string(upstream.PlatformSub2API) && len(modelLimitModels) > 0 &&
		(hasModelLimitExclusion(modelLimitStates) || (stored != nil && (strings.TrimSpace(stored.OriginalModels) != "" || strings.TrimSpace(stored.LastAppliedModels) != "")))
	// 账号启停：普通 degraded 只记健康；只有接管中 / 阻塞 / 恢复中才改上游启停。
	needsStatusAction := len(statusModels) > 0 && (blocked || hasRecoveringState(statusAggStates) || (stored != nil && (stored.OriginalStatus != "" || stored.LastAppliedStatus != "")))
	if stored != nil && (strings.TrimSpace(stored.OriginalModels) != "" || strings.TrimSpace(stored.LastAppliedModels) != "") {
		// 已建立模型限制快照时也算「已接管」，避免丢失快照。
		needsStatusAction = needsStatusAction || len(statusModels) > 0
	}
	if !statesComplete && !blocked && !needsModelLimits {
		return "", nil
	}
	if stored == nil && !needsStatusAction && !needsModelLimits {
		return "", nil
	}
	// 已接管但状态不完整：仅在 blocked 或模型限制需要时继续。
	if stored != nil && !statesComplete && !blocked && !needsModelLimits {
		return "", nil
	}

	currentStatus := normalizeTargetStatus(target.Platform, target.AccountStatus)
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
		// 用户原本就在上游暂停的账号不属于自动恢复对象，探活可以继续，但绝不替用户启用。
		if !targetStatusEnabled(target.Platform, currentStatus) {
			if !legacyTargetWasManaged(statusAggStates) {
				// 仍可尝试模型限制（账号本来就停用，改 models 无害）。
				if needsModelLimits {
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
				return RemoteActionSkippedTargetInitiallyDisabled, nil
			}
			// 升级前已由健康模块停用的目标没有动作快照。仅在历史 remote_action 能明确证明
			// 是系统执行的情况下，按旧默认 active/100 建立一次兼容快照。
			originalStatus, originalWeight = legacyOriginalTargetState(target.Platform)
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
		stored.Conflict = true
		stored.PendingStatus = ""
		stored.PendingWeight = nil
		stored.PendingModels = ""
		if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
			return "", err
		}
		return RemoteActionSkippedTargetConflict, nil
	}
	if stored.Conflict {
		return RemoteActionSkippedTargetConflict, nil
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

	// 仅模型限制场景且当前不需要改启停：保持原启停，不强制 active。
	if !blocked && !hasRecoveringState(statusAggStates) && !allHealthy {
		// 没有账号级阻塞时不要把用户手动 inactive 的账号改回 active。
		if !targetStatusEnabled(target.Platform, currentStatus) &&
			normalizeTargetStatus(target.Platform, stored.OriginalStatus) == currentStatus {
			if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
				return modelsAction, err
			}
			return modelsAction, modelsErr
		}
	}

	desiredStatus, desiredWeight := desiredTargetState(target.Platform, allHealthy, blocked, minWeight, *stored)
	statusEqual := targetStateEqual(target, currentStatus, currentWeight, desiredStatus, desiredWeight)
	if statusEqual {
		stored.LastAppliedStatus = desiredStatus
		stored.LastAppliedWeight = cloneIntPointer(desiredWeight)
		stored.PendingStatus = ""
		stored.PendingWeight = nil
		// 全部健康且模型限制也已恢复到原始列表时，才能删除接管快照。
		if allHealthy && !hasManagedModelLimits(stored) {
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
	action, actionErr := s.dispatcher.ApplyTargetState(ctx, session, target, desiredWeight, desiredStatus)
	if actionErr != nil {
		log.Printf("[connection-health] aggregate target action failed target_id=%s action=%s err=%v", target.TargetID, action, actionErr)
		return joinRemoteActions(modelsAction, action), actionErr
	}
	stored.LastAppliedStatus = desiredStatus
	stored.LastAppliedWeight = cloneIntPointer(desiredWeight)
	stored.PendingStatus = ""
	stored.PendingWeight = nil
	if allHealthy && !hasManagedModelLimits(stored) {
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
						AccountWeight: cloneIntPointer(account.Weight), ProviderFamily: account.Platform,
						Models: splitModelList(account.Models),
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
			target = AdminProbeTarget{
				TargetID: stored.TargetID, Platform: parsed.platform, AccountID: parsed.accountID,
				AccountStatus: stored.LastAppliedStatus, AccountWeight: cloneIntPointer(stored.LastAppliedWeight),
			}
		}
		currentStatus := normalizeTargetStatus(target.Platform, target.AccountStatus)
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
		case RemoteActionSub2APIStatusInactive, "newapi_channel_disabled":
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
	// 已 suspended 的模型会从白名单摘除，不得再把 allHealthy 打成 false，
	// 否则同账号其它健康模型也无法把账号从 inactive 恢复。
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
		// observing / disabled / 权重 0 仍阻塞账号（这些不靠模型白名单摘除解决）。
		if state.State == StateObserving || state.State == StateDisabled || state.CurrentWeight <= 0 {
			blocked = true
		}
	}
	if effectiveCount == 0 {
		// 没有任何有效模型：若全是摘除暂停则阻塞；若完全无状态则视为健康空集。
		allHealthy = exclusionCount == 0
		if exclusionCount > 0 {
			blocked = true
		}
	}
	return allHealthy, blocked, minWeight
}

// isModelLimitExclusionState 判定模型是否应暂时从 sub2api「模型限制」中摘除。
// 产品语义：凡进入「探活暂停」(suspended) 的模型都不应再被调度，直到探活恢复；
// 不区分 model_not_found / server_error / invalid_response / network_fluctuation 等具体原因。
func isModelLimitExclusionState(state ConnectionHealthState) bool {
	return state.State == StateSuspended
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

func desiredTargetState(platform string, allHealthy bool, blocked bool, minWeight int, stored TargetActionState) (string, *int) {
	if allHealthy {
		// 有效模型已全部健康：恢复接管前的启停。若快照缺失，默认启用。
		orig := strings.TrimSpace(stored.OriginalStatus)
		if orig == "" {
			return legacyOriginalTargetState(platform)
		}
		return stored.OriginalStatus, cloneIntPointer(stored.OriginalWeight)
	}
	if platform == string(upstream.PlatformNewAPI) {
		if blocked {
			weight := 0
			return "2", &weight
		}
		weight := scaledTargetWeight(stored.OriginalWeight, minWeight)
		return "1", &weight
	}
	if blocked {
		return "inactive", nil
	}
	// 未全健康但也未阻塞（例如仅有模型白名单摘除）：保持账号可调度，让健康模型继续服务。
	return "active", nil
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
	// Sub2API 管理端「停用」可能对应 inactive / error / disabled 等枚举。
	if normalized == "inactive" || normalized == "disabled" || normalized == "2" ||
		normalized == "error" || normalized == "stopped" || normalized == "ban" || normalized == "banned" {
		return "inactive"
	}
	return "active"
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
