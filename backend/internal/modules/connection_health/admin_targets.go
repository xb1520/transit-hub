package connection_health

import (
	"context"
	"log"
	"strings"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// 本文件实现「独立 admin 账号/渠道探活」体系：分组健康不再依赖 real_connections，
// 而是把当前 admin workspace 下的 admin 分组、分组下账号/channel 本身作为探活目标。
// 后端在探活前 server-only 地临时解析 base_url + key + model，用现有 RealProbeRunner 发起探活。
//
// 存储复用：新目标的健康状态/事件复用 connection_health_states / connection_health_events 两张表，
// 其 connection_id 列存放稳定的 targetId（见 buildTargetID）。targetId 形如
// "newapi:<workspaceAdminAccountID>:<accountID>"，与 real_connections 的 UUID 不会碰撞，
// 旧连接维度的查询/路由完全不受影响。

// RemoteActionSkippedIndependentProbe 标记：策略未同时开启自动降级和自动远端动作时，即使
// 状态机判定需要远端动作也只记录这个标记，不真正调用上游。开启后，sub2api target 切换
// 账号状态，New API target 切换 channel status/weight，二者都通过 dispatcher 的平台中性接口执行。
const RemoteActionSkippedIndependentProbe = "skipped_independent_probe"

// 探活不可用原因 -> 前端可识别的 i18n 错误 key 映射。reason 取值来自 upstream.Reason* 常量。
const (
	ErrorCredentialUnavailable      = "admin.connectionHealth.errors.credentialUnavailable"
	ErrorSecureVerificationRequired = "admin.connectionHealth.errors.secureVerificationRequired"
	ErrorBaseURLUnavailable         = "admin.connectionHealth.errors.baseUrlUnavailable"
	ErrorModelUnavailable           = "admin.connectionHealth.errors.modelUnavailable"
	ErrorExportUnavailable          = "admin.connectionHealth.errors.exportUnavailable"
	ErrorCredentialsRedacted        = "admin.connectionHealth.errors.credentialsRedacted"
	ErrorProbeTargetNotFound        = "admin.connectionHealth.errors.targetNotFound"
)

// AdminProbeTarget 是平台中性的独立探活目标：一个 admin 分组下的账号(sub2api)/渠道(new-api)。
// 不再要求存在 real_connections。TargetID 稳定且可复算，是新状态/事件的核心键。
type AdminProbeTarget struct {
	TargetID               string   `json:"targetId"`
	Platform               string   `json:"platform"`
	AdminGroupID           string   `json:"adminGroupId"`
	AdminGroupName         string   `json:"adminGroupName"`
	AccountID              string   `json:"accountId"`
	AccountName            string   `json:"accountName"`
	AccountStatus          string   `json:"accountStatus"`
	// AccountSchedulable 是 Sub2API 的调度开关；nil 表示上游未返回，按 true 兼容。
	// 自动降级通过关闭调度摘流，不再写 status=inactive。
	AccountSchedulable     *bool    `json:"accountSchedulable,omitempty"`
	AccountWeight          *int     `json:"accountWeight,omitempty"`
	ProviderFamily         string   `json:"providerFamily"`
	Models                 []string `json:"models"`
	ProbeAvailable         bool     `json:"probeAvailable"`
	ProbeUnavailableReason string   `json:"probeUnavailableReason,omitempty"`
	// CostGroupRatio 是上游 API Key 分组原始倍率（不含站点充值倍率）；0/缺失按 1。
	// 用于真实探活费用：USD = tokens/quota_per_unit × groupRatio（无 actual_cost 时）。
	CostGroupRatio float64 `json:"-"`
	// CostRechargeRate 是上游站点充值倍率（CNY = USD × rate）；0/缺失按 1。
	CostRechargeRate float64 `json:"-"`
}

// probeModelSpec 是一个「目标 + 具体探活模型」的组合，携带该模型来自哪条策略的探活参数。
type probeModelSpec struct {
	modelName      string
	providerFamily string
	maxProbeTokens int
	probePrompt    string
	policy         Policy
	// Event group metadata records where this policy was inherited for this target.
	// Explicit target assignments intentionally resolve to an empty group.
	eventGroupResolved  bool
	eventAdminGroupID   string
	eventAdminGroupName string
}

// targetProbeResult 暂存单模型探活结果。一个账号的全部到期模型完成后，再统一决定一次上游
// 动作并写事件，避免多模型按执行顺序互相启停同一个账号。
type targetProbeResult struct {
	state           *ConnectionHealthState
	previousState   State
	outcome         ProbeOutcome
	latencyMs       int
	spec            probeModelSpec
	triggeredRemote bool
}

// buildTargetID 生成稳定的探活目标 ID：platform:workspaceAdminAccountID:accountID。
// 不使用随机 ID；同一账号在同一 workspace 下每次都算出同一个 targetId，便于状态/事件持续累计。
func buildTargetID(platform string, adminAccountID string, accountID string) string {
	return platform + ":" + adminAccountID + ":" + accountID
}

// parsedTargetID 解析 targetId 的三段结构，用于手动探活时校验目标归属当前 workspace，
// 避免用户用别的 workspace 的 targetId 越权探活。
type parsedTargetID struct {
	platform       string
	adminAccountID string
	accountID      string
}

func parseTargetID(targetID string) (parsedTargetID, bool) {
	parts := strings.SplitN(targetID, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return parsedTargetID{}, false
	}
	return parsedTargetID{platform: parts[0], adminAccountID: parts[1], accountID: parts[2]}, true
}

// candidateModelSpecs 计算一个目标当前可探活的候选模型：
//   - 策略池 = 当前 workspace 全部启用策略下的启用 modelTargets（按模型名去重，先出现的优先）。
//   - 目标自带模型列表（new-api channel.models 等）非空时，取「目标模型 ∩ 策略池」。
//   - 目标没有模型列表时，直接用策略池（策略里明确配置的模型）。
//
// 说明：独立探活维度下，admin 目标不再按 own group 精确匹配策略（own group 是「我的分组」，
// 与 admin 分组是不同概念），因此这里用 workspace 级策略池，保留现有策略 modelTargets 的配置语义。
func candidateModelSpecs(targetModels []string, policies []Policy) []probeModelSpec {
	pool := make([]probeModelSpec, 0)
	seen := make(map[string]int)
	for _, p := range policies {
		if !p.Enabled || !policySupportsProbing(p) {
			continue
		}
		for _, t := range p.ModelTargets {
			if !t.Enabled {
				continue
			}
			name := strings.TrimSpace(t.ModelName)
			if name == "" {
				continue
			}
			if index, dup := seen[name]; dup {
				// 同一模型被多条策略覆盖时使用稳定且偏安全的策略：关闭远端动作优先，
				// 然后选择更低失败阈值、更长观察期和更短探活间隔，最后按 ID 决胜。
				if preferProbePolicy(p, pool[index].policy) {
					pool[index] = probeModelSpec{
						modelName: name, providerFamily: t.ProviderFamily, maxProbeTokens: t.MaxProbeTokens,
						probePrompt: t.ProbePrompt, policy: p,
					}
				}
				continue
			}
			seen[name] = len(pool)
			pool = append(pool, probeModelSpec{
				modelName:      name,
				providerFamily: t.ProviderFamily,
				maxProbeTokens: t.MaxProbeTokens,
				probePrompt:    t.ProbePrompt,
				policy:         p,
			})
		}
	}

	if len(targetModels) == 0 {
		return pool
	}
	allowed := make(map[string]struct{}, len(targetModels))
	for _, m := range targetModels {
		allowed[strings.TrimSpace(m)] = struct{}{}
	}
	filtered := make([]probeModelSpec, 0, len(pool))
	for _, spec := range pool {
		if _, ok := allowed[spec.modelName]; ok {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

func preferProbePolicy(candidate Policy, current Policy) bool {
	if policyRemoteActionEnabled(candidate) != policyRemoteActionEnabled(current) {
		return !policyRemoteActionEnabled(candidate)
	}
	if failureThreshold(candidate) != failureThreshold(current) {
		return failureThreshold(candidate) < failureThreshold(current)
	}
	if observationWindow(candidate) != observationWindow(current) {
		return observationWindow(candidate) > observationWindow(current)
	}
	if candidate.ProbeIntervalSeconds != current.ProbeIntervalSeconds {
		return defaultInt(candidate.ProbeIntervalSeconds, 60) < defaultInt(current.ProbeIntervalSeconds, 60)
	}
	return candidate.ID < current.ID
}

// targetProbeAvailability 在「不获取密钥」的前提下静态判断目标是否可探活，用于主列表展示：
//   - 没有任何候选模型 -> model_unavailable。
//   - new-api channel 缺少 base_url -> base_url_unavailable（凭据要点之一，list 阶段即可知）。
//   - 其余情况乐观标记可探活；密钥/安全验证等只有真正探活时才知道，失败会在 modelHealth/手动
//     探活错误里体现，不在这里预取密钥（避免高频命中受保护的 key 接口触发限流/安全验证）。
func targetProbeAvailability(platform string, baseURL string, specCount int) (bool, string) {
	if specCount == 0 {
		return false, upstream.ReasonModelUnavailable
	}
	if platform == string(upstream.PlatformNewAPI) && strings.TrimSpace(baseURL) == "" {
		return false, upstream.ReasonBaseURLUnavailable
	}
	return true, ""
}

// targetManualProbeAvailability 只判断一次性手动探活是否具备静态前置条件。手动探活会在打开
// 弹窗后实时发现模型，因此不能再依赖策略 modelTargets；否则一个尚未创建策略的新 workspace
// 会把所有目标误标为不可探活，用户也就无法通过简化流程开始配置。
func targetManualProbeAvailability(platform string, baseURL string) (bool, string) {
	if platform == string(upstream.PlatformNewAPI) && strings.TrimSpace(baseURL) == "" {
		return false, upstream.ReasonBaseURLUnavailable
	}
	return true, ""
}

// reasonToErrorKey 把探活不可用 reason 映射为前端 i18n 错误 key。
func reasonToErrorKey(reason string) string {
	switch reason {
	case upstream.ReasonSecureVerificationRequired:
		return ErrorSecureVerificationRequired
	case upstream.ReasonBaseURLUnavailable:
		return ErrorBaseURLUnavailable
	case upstream.ReasonModelUnavailable:
		return ErrorModelUnavailable
	case upstream.ReasonExportUnavailable:
		return ErrorExportUnavailable
	case upstream.ReasonCredentialsRedacted:
		return ErrorCredentialsRedacted
	default:
		return ErrorCredentialUnavailable
	}
}

func isCredentialUnavailableReason(reason string) bool {
	switch reason {
	case upstream.ReasonCredentialUnavailable, upstream.ReasonSecureVerificationRequired,
		upstream.ReasonBaseURLUnavailable, upstream.ReasonExportUnavailable, upstream.ReasonCredentialsRedacted:
		return true
	default:
		return false
	}
}

// resolveManualTarget 是手动一次性动作（旧策略候选手动探活 / 新模型发现 / 新一次性探活）共用的
// target 解析入口：校验 targetId 归属当前 workspace + platform、重新解析目标账号（不信任前端传入
// 的任何细节），返回 canonical 后的 session/target/account/adminAccountID。
// 不解析凭据（凭据解析由调用方按需调用 ResolveProbeCredential），因为策略分配管理等轻量场景
// 不需要真的打上游拿明文 key。
func (s *Service) resolveManualTarget(ctx context.Context, userID string, targetID string) (upstream.Session, AdminProbeTarget, upstream.AdminGroupAccountInfo, string, error) {
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", err
	}
	if s.platformGroups == nil {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", requestError(ErrorUnknown)
	}

	// 校验目标归属当前 workspace：targetId 内嵌的 adminAccountID 必须等于当前 workspace，
	// 防止用别的 workspace 的 targetId 越权操作。
	parsed, ok := parseTargetID(targetID)
	if !ok || parsed.adminAccountID != adminAccountID {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", requestError(ErrorProbeTargetNotFound)
	}

	session, err := s.mySites.RequireSession(ctx, userID, adminAccountID)
	if err != nil {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", err
	}

	// 校验 targetId 的 platform 段与当前 workspace 的 session 平台一致。否则 platform 段被伪造
	// （如 session 是 new-api 却传 sub2api:ws1:100）时，findAdminTarget 会用 session 平台重建
	// 出 canonical targetId（newapi:ws1:100），导致请求 targetId 与状态/事件 key 不一致。
	if parsed.platform != string(session.Platform) {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", requestError(ErrorProbeTargetNotFound)
	}

	// 重新解析目标账号（不信任前端），拿到 base_url/models 等探活必需信息。
	target, account, found, accountsReadError, err := s.findAdminTarget(ctx, session, adminAccountID, parsed.accountID)
	if err != nil {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", err
	}
	if !found {
		// 目标未找到时，若过程中发生过账号列表读取错误，说明可能是上游临时故障而非目标不存在，
		// 返回账号列表读取错误（安全 i18n key，不含上游明文），避免掩盖真实上游故障。
		if accountsReadError {
			return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", requestError(ErrorAccountsFetch)
		}
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", requestError(ErrorProbeTargetNotFound)
	}
	// canonical 校验：重建出的 targetId 必须与请求完全一致，杜绝任何 targetId 段不一致的写入。
	if target.TargetID != targetID {
		return upstream.Session{}, AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, "", requestError(ErrorProbeTargetNotFound)
	}

	return session, target, account, adminAccountID, nil
}

// AdminGroupProbeAutomationResult 是分组级手动策略探活的汇总结果。
type AdminGroupProbeAutomationResult struct {
	AdminGroupID   string `json:"adminGroupId"`
	AdminGroupName string `json:"adminGroupName"`
	// ProbedTargets 成功跑完策略探活（含 0 模型结果）的目标数。
	ProbedTargets int `json:"probedTargets"`
	// SkippedTargets 未分配启用探活策略 / 无可探活模型 / 不可探活而跳过的目标数。
	SkippedTargets int `json:"skippedTargets"`
	// FailedTargets 探活过程中出错的目标数（凭据失败、上游错误等）。
	FailedTargets int `json:"failedTargets"`
	// TotalAccounts 分组内账号/渠道总数。
	TotalAccounts int `json:"totalAccounts"`
}

// ProbeAdminGroupAutomation 对指定 admin 分组内所有「已分配启用探活策略」的账号/渠道
// 立即执行一轮完整策略探活（写状态/事件，并按策略触发自动降级/远端动作）。
// 与调度器路径口径一致，但不考虑探活间隔冷却——用于运营手动触发整组探测。
func (s *Service) ProbeAdminGroupAutomation(ctx context.Context, userID string, adminGroupID string) (AdminGroupProbeAutomationResult, error) {
	adminGroupID = strings.TrimSpace(adminGroupID)
	if adminGroupID == "" {
		return AdminGroupProbeAutomationResult{}, requestError(ErrorNotFound)
	}
	adminAccountID, err := s.currentAdminAccountID(ctx, userID)
	if err != nil {
		return AdminGroupProbeAutomationResult{}, err
	}
	if s.platformGroups == nil || s.mySites == nil {
		return AdminGroupProbeAutomationResult{}, requestError(ErrorUnknown)
	}
	session, err := s.mySites.RequireSession(ctx, userID, adminAccountID)
	if err != nil {
		return AdminGroupProbeAutomationResult{}, err
	}
	groups, err := s.platformGroups.FetchAdminAllGroups(session)
	if err != nil {
		return AdminGroupProbeAutomationResult{}, err
	}
	var group *upstream.AdminGroupInfo
	for i := range groups {
		if groups[i].ID == adminGroupID {
			group = &groups[i]
			break
		}
	}
	if group == nil {
		return AdminGroupProbeAutomationResult{}, requestError(ErrorNotFound)
	}
	accounts, err := s.platformGroups.ListAdminGroupAccounts(session, *group)
	if err != nil {
		return AdminGroupProbeAutomationResult{}, requestError(ErrorAccountsFetch)
	}

	result := AdminGroupProbeAutomationResult{
		AdminGroupID:   group.ID,
		AdminGroupName: group.Name,
		TotalAccounts:  len(accounts),
	}
	for _, acc := range accounts {
		targetID := buildTargetID(string(session.Platform), adminAccountID, acc.ID)
		// 空 models = 探活策略候选池全部模型（与旧 ProbeTarget 语义一致）。
		if _, probeErr := s.ProbeTarget(ctx, userID, targetID, nil); probeErr != nil {
			key := probeErr.Error()
			// 无策略/无模型/不可探活等属于跳过，不记失败。
			switch key {
			case ErrorNoMatchingModels, ErrorModelUnavailable, ErrorProbeTargetNotFound,
				ErrorCredentialUnavailable, ErrorSecureVerificationRequired, ErrorBaseURLUnavailable,
				ErrorExportUnavailable, ErrorCredentialsRedacted:
				result.SkippedTargets++
				continue
			}
			log.Printf("[connection-health] group automation probe failed group_id=%s target_id=%s err=%v", adminGroupID, targetID, probeErr)
			result.FailedTargets++
			continue
		}
		result.ProbedTargets++
	}
	return result, nil
}

// ProbeTarget 手动探活一个独立目标：前端传 targetId + models（不再传 connectionId/base_url/key）。
// 后端按当前 user/admin workspace 重新解析目标与凭据，不信任前端传入的任何上游地址或密钥。
// 不可探活时返回结构化 requestError（credential_unavailable / secure_verification_required /
// base_url_unavailable / model_unavailable 等对应的 i18n key）。
//
// 注意：这是旧的「策略候选池」手动探活路径，会写入 connection_health_states/events。
// 新账号弹窗的一次性手动探活已改用 ManualProbeTarget（见 manual_probe.go），不写状态/事件。
// 本接口继续保留只为兼容可能存在的旧调用方。
func (s *Service) ProbeTarget(ctx context.Context, userID string, targetID string, models []string) ([]ModelHealth, error) {
	session, target, account, adminAccountID, err := s.resolveManualTarget(ctx, userID, targetID)
	if err != nil {
		return nil, err
	}
	s.attachProbeCostRates(ctx, userID, adminAccountID, string(session.Platform), &target)
	release, err := s.repo.AcquireTargetLease(ctx, targetID)
	if err != nil {
		return nil, err
	}
	defer release()

	policies, err := s.repo.ListPolicies(ctx, userID, adminAccountID)
	if err != nil {
		return nil, err
	}
	// 已摘除模型限制时仍用 OriginalModels / 本地状态补回候选，避免被摘模型无法再被手动探活恢复。
	if stored, storeErr := s.repo.GetTargetActionState(ctx, userID, adminAccountID, target.TargetID); storeErr == nil {
		known := []string(nil)
		if states, stateErr := s.repo.ListStatesByConnection(ctx, target.TargetID); stateErr == nil {
			known = modelNamesFromStates(states)
		}
		target = expandTargetModelsForProbe(target, stored, known...)
	}
	allSpecs := candidateModelSpecs(target.Models, policies)
	specs := allSpecs

	// 按请求的 models 过滤（语义与 ProbeConnection 一致）：显式指定但一个都没命中 -> 明确拒绝。
	requested := make([]string, 0, len(models))
	for _, m := range models {
		if trimmed := strings.TrimSpace(m); trimmed != "" {
			requested = append(requested, trimmed)
		}
	}
	if len(requested) > 0 {
		wanted := make(map[string]struct{}, len(requested))
		for _, m := range requested {
			wanted[m] = struct{}{}
		}
		filtered := make([]probeModelSpec, 0, len(specs))
		for _, spec := range specs {
			if _, ok := wanted[spec.modelName]; ok {
				filtered = append(filtered, spec)
			}
		}
		if len(filtered) == 0 {
			return nil, requestError(ErrorNoMatchingModels)
		}
		specs = filtered
	}

	if len(specs) == 0 {
		return nil, requestError(ErrorModelUnavailable)
	}

	// 解析凭据（server-only，明文只在内存短暂存在）。失败 -> 结构化不可探活错误。
	cred, err := s.platformGroups.ResolveProbeCredential(session, account)
	if err != nil {
		return nil, requestError(reasonToErrorKey(upstream.ProbeCredentialReason(err)))
	}

	results := make([]ModelHealth, 0, len(specs))
	probeResults := make([]targetProbeResult, 0, len(specs))
	for _, spec := range specs {
		result, probeErr := s.probeTargetOnce(ctx, userID, adminAccountID, target, cred, spec)
		if probeErr != nil {
			log.Printf("[connection-health] manual target probe failed target_id=%s model=%s err=%v", target.TargetID, spec.modelName, probeErr)
			continue
		}
		if result != nil {
			probeResults = append(probeResults, *result)
		}
	}
	s.finishTargetProbeBatch(ctx, userID, adminAccountID, session, target, allSpecs, probeResults)
	for _, result := range probeResults {
		results = append(results, toModelHealth(result.spec.modelName, *result.state))
	}
	return results, nil
}

// ManualRestoreTarget 管理员手动恢复独立探活目标：
//  1. 清除 TargetActionState.Conflict，避免自动路径永久 skip；
//  2. 将指定模型（空 = 当前已摘除/非 healthy 的受控模型）强制置为 healthy/100；
//  3. 立即 reconcile 写回 sub2api 模型白名单，并尝试恢复账号可调度。
//
// 这是运维兜底：不等观察窗/冷却，也不再被 status conflict 卡住。
func (s *Service) ManualRestoreTarget(ctx context.Context, userID string, targetID string, models []string) ([]ModelHealth, error) {
	session, target, _, adminAccountID, err := s.resolveManualTarget(ctx, userID, targetID)
	if err != nil {
		return nil, err
	}
	release, err := s.repo.AcquireTargetLease(ctx, targetID)
	if err != nil {
		return nil, err
	}
	defer release()

	policies, err := s.repo.ListPolicies(ctx, userID, adminAccountID)
	if err != nil {
		return nil, err
	}
	allStates, err := s.repo.ListStatesByConnection(ctx, target.TargetID)
	if err != nil {
		return nil, err
	}
	stored, err := s.repo.GetTargetActionState(ctx, userID, adminAccountID, target.TargetID)
	if err != nil {
		return nil, err
	}
	target = expandTargetModelsForProbe(target, stored, modelNamesFromStates(allStates)...)

	fallbackPolicy := Policy{Enabled: true, AutoDegradeEnabled: true, AutoRemoteActionEnabled: true}
	for _, p := range policies {
		if p.Enabled && policySupportsProbing(p) {
			fallbackPolicy = p
			break
		}
	}

	// specs 覆盖：策略候选 ∩ 目标模型 + 本地状态模型 + OriginalModels。
	specs := candidateModelSpecs(target.Models, policies)
	seenSpec := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		seenSpec[spec.modelName] = struct{}{}
	}
	addSpec := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seenSpec[name]; ok {
			return
		}
		seenSpec[name] = struct{}{}
		specs = append(specs, probeModelSpec{modelName: name, policy: fallbackPolicy})
	}
	for _, state := range allStates {
		addSpec(state.ModelName)
	}
	if stored != nil {
		for _, name := range splitModelList(stored.OriginalModels) {
			addSpec(name)
		}
	}

	requested := make(map[string]struct{})
	for _, m := range models {
		if name := strings.TrimSpace(m); name != "" {
			requested[name] = struct{}{}
			addSpec(name)
		}
	}
	filterRequested := len(requested) > 0

	stateByName := make(map[string]ConnectionHealthState, len(allStates))
	for _, state := range allStates {
		stateByName[state.ModelName] = state
	}

	restoreSet := make(map[string]struct{})
	mark := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if filterRequested {
			if _, ok := requested[name]; !ok {
				return
			}
		}
		state, ok := stateByName[name]
		if filterRequested || !ok || isModelLimitExclusionState(state) || state.State != StateHealthy || state.CurrentWeight < 100 {
			restoreSet[name] = struct{}{}
		}
	}
	for _, spec := range specs {
		mark(spec.modelName)
	}
	for name := range stateByName {
		mark(name)
	}
	if stored != nil {
		for _, name := range splitModelList(stored.OriginalModels) {
			mark(name)
		}
	}
	restoreNames := make([]string, 0, len(restoreSet))
	for name := range restoreSet {
		restoreNames = append(restoreNames, name)
	}
	restoreNames = splitModelList(joinModelList(restoreNames))
	if filterRequested && len(restoreNames) == 0 {
		return nil, requestError(ErrorNoMatchingModels)
	}

	// 清除 conflict；无快照时不伪造 OriginalModels（避免把已摘除后的短列表固化成基线）。
	if stored != nil {
		healTargetActionConflict(stored, target.Platform, effectiveLogicalStatus(target), normalizedTargetWeight(target))
		if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
			return nil, err
		}
	}

	now := time.Now()
	for _, name := range restoreNames {
		fromState := string(StateSuspended)
		var next ConnectionHealthState
		if current, ok := stateByName[name]; ok {
			fromState = string(current.State)
			next = current
		} else {
			next = defaultTargetState(userID, adminAccountID, target, name)
		}
		next.State = StateHealthy
		next.CurrentWeight = 100
		next.ConsecutiveFailures = 0
		next.ConsecutiveSuccesses = 0
		next.CooldownUntil = nil
		next.ObservingUntil = nil
		next.LastErrorKey = ""
		next.LastErrorDetail = ""
		next.LastSuccessAt = &now
		next.UserID = userID
		next.AdminAccountID = adminAccountID
		if err := s.repo.UpsertState(ctx, next); err != nil {
			return nil, err
		}
		stateByName[name] = next
		s.recordTargetEvent(ctx, userID, adminAccountID, target, "", name, "manual_restore", fromState, string(StateHealthy), nil, "", "", "", 0, 0)
	}

	// 确保 reconcile 至少覆盖 original 中的模型（含刚恢复的）。
	if stored != nil && strings.TrimSpace(stored.OriginalModels) != "" {
		for _, name := range splitModelList(stored.OriginalModels) {
			addSpec(name)
		}
	}
	if len(specs) == 0 {
		for _, name := range restoreNames {
			addSpec(name)
		}
	}

	var remoteAction string
	if len(specs) > 0 {
		action, actionErr := s.reconcileTargetRemoteAction(ctx, userID, adminAccountID, session, target, specs)
		remoteAction = action
		if actionErr != nil {
			log.Printf("[connection-health] manual restore reconcile failed target_id=%s action=%s err=%v", target.TargetID, action, actionErr)
			// 状态已写 healthy；把远端错误返回 UI。
			out := make([]ModelHealth, 0, len(restoreNames))
			for _, name := range restoreNames {
				if st, ok := stateByName[name]; ok {
					out = append(out, toModelHealth(name, st))
				}
			}
			return out, actionErr
		}
	}

	// 兜底：reconcile 后仍 managed 且 original 未写回时，直接 ApplyTargetModels(original)。
	if target.Platform == string(upstream.PlatformSub2API) && s.dispatcher != nil {
		if latest, getErr := s.repo.GetTargetActionState(ctx, userID, adminAccountID, target.TargetID); getErr == nil && latest != nil {
			stored = latest
		}
		if stored != nil && hasManagedModelLimits(stored) {
			desired := stored.OriginalModels
			if !modelListsEqual(stored.LastAppliedModels, desired) || !modelListsEqual(joinModelList(target.Models), desired) {
				action, actionErr := s.dispatcher.ApplyTargetModels(ctx, session, target, desired)
				if actionErr != nil {
					log.Printf("[connection-health] manual restore apply models failed target_id=%s err=%v", target.TargetID, actionErr)
					return nil, actionErr
				}
				remoteAction = joinRemoteActions(remoteAction, action)
				stored.LastAppliedModels = desired
				stored.PendingModels = ""
				stored.Conflict = false
				if err := s.repo.UpsertTargetActionState(ctx, *stored); err != nil {
					return nil, err
				}
			}
		}
	}

	if remoteAction != "" && len(restoreNames) > 0 {
		if st, ok := stateByName[restoreNames[len(restoreNames)-1]]; ok {
			st.LastRemoteAction = remoteAction
			_ = s.repo.UpsertState(ctx, st)
			stateByName[restoreNames[len(restoreNames)-1]] = st
		}
	}

	out := make([]ModelHealth, 0, len(restoreNames))
	for _, name := range restoreNames {
		if st, ok := stateByName[name]; ok {
			out = append(out, toModelHealth(name, st))
		}
	}
	if len(out) == 0 {
		for _, st := range stateByName {
			out = append(out, toModelHealth(st.ModelName, st))
		}
	}
	return out, nil
}

// findAdminTarget 在当前 workspace 的 admin 分组/账号里按 accountID 找到目标，并构造 AdminProbeTarget。
// 用于手动探活时按 targetId 重新解析目标（不信任前端传入的目标细节）。
// 返回的 accountsReadError 表示遍历过程中是否有分组的账号列表读取失败：单分组失败仍会继续查
// 其它分组，但若最终没找到目标，调用方可据此区分「目标不存在」与「上游读取失败」。
func (s *Service) findAdminTarget(ctx context.Context, session upstream.Session, adminAccountID string, accountID string) (target AdminProbeTarget, account upstream.AdminGroupAccountInfo, found bool, accountsReadError bool, err error) {
	groups, err := s.platformGroups.FetchAdminAllGroups(session)
	if err != nil {
		return AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, false, false, err
	}
	platform := string(session.Platform)
	for _, group := range groups {
		accounts, accErr := s.platformGroups.ListAdminGroupAccounts(session, group)
		if accErr != nil {
			// 单分组失败不影响在其它分组里继续找，但记录下发生过读取错误。
			accountsReadError = true
			continue
		}
		for _, acc := range accounts {
			if acc.ID != accountID {
				continue
			}
			resolved := AdminProbeTarget{
				TargetID:           buildTargetID(platform, adminAccountID, acc.ID),
				Platform:           platform,
				AdminGroupID:       group.ID,
				AdminGroupName:     group.Name,
				AccountID:          acc.ID,
				AccountName:        acc.Name,
				AccountStatus:      acc.Status,
				AccountSchedulable: acc.Schedulable,
				AccountWeight:      cloneIntPointer(acc.Weight),
				ProviderFamily:     acc.Platform,
				Models:             splitModelList(acc.Models),
			}
			return resolved, acc, true, accountsReadError, nil
		}
	}
	return AdminProbeTarget{}, upstream.AdminGroupAccountInfo{}, false, accountsReadError, nil
}

// probeTargetOnce 对一个 (target, model) 组合执行一次独立探活并落库状态。事件和账号级上游
// 动作由 finishTargetProbeBatch 在同一账号全部模型完成后统一处理。
// 与 probeOnce 的关键差异：状态/事件以 targetId 为键（存在 connection_id 列）。
// 远端动作规则：
//   - 自动降级或自动远端动作任一关闭时，即使状态机判定需要远端动作，也只记
//     RemoteActionSkippedIndependentProbe，绝不调用上游（与旧行为一致）。
//   - 两个开关都开启且 target.Platform 是 sub2api 时，真实调用
//     dispatcher：降级关调度（schedulable=false），恢复开调度；历史 inactive 会顺带写回 active。
//   - New API target 按 currentWeight 更新 channel weight/status，实现逐步恢复。
//
// session 来自调用方（ProbeTarget 的 resolveManualTarget / 调度器 job 的 RequireSession），
// 不信任前端传入的任何 platform/account 信息。
// 每日探活预算耗尽时跳过真实请求，只保留当前状态。
func (s *Service) probeTargetOnce(ctx context.Context, userID string, adminAccountID string, target AdminProbeTarget, cred upstream.ProbeCredential, spec probeModelSpec) (*targetProbeResult, error) {
	current, err := s.repo.GetState(ctx, target.TargetID, spec.modelName)
	if err != nil {
		return nil, err
	}
	if current == nil {
		defaultState := defaultTargetState(userID, adminAccountID, target, spec.modelName)
		current = &defaultState
	}

	dayStart := probeBudgetDayStart(time.Now())
	allowed, err := s.repo.TryConsumeProbeBudget(ctx, userID, adminAccountID, spec.policy.ID, dayStart, probeBudgetLimit(spec.policy), spec.policy.DailyProbeBudgetCost)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, nil
	}

	providerFamily := spec.providerFamily
	if providerFamily == "" {
		providerFamily = target.ProviderFamily
	}
	outcome := s.probeRunner.Probe(ctx, ProbeRequest{
		BaseURL: cred.BaseURL, UpstreamKey: cred.Key, ProviderFamily: providerFamily,
		ModelName: spec.modelName, MaxTokens: spec.maxProbeTokens, ProbePrompt: spec.probePrompt,
	})
	cost := computeProbeCost(outcome, spec.maxProbeTokens, target.CostGroupRatio, target.CostRechargeRate)
	outcome.CostUSD = cost.USD
	outcome.CostCNY = cost.CNY
	if addErr := s.repo.AddProbeBudgetCost(ctx, userID, adminAccountID, spec.policy.ID, dayStart, cost.CNY, cost.USD); addErr != nil {
		log.Printf("[connection-health] add probe budget cost failed policy_id=%s err=%v", spec.policy.ID, addErr)
	}

	now := time.Now()
	transitionOut := Transition(TransitionInput{
		Current: current.State, CurrentWeight: current.CurrentWeight, ConsecutiveFailures: current.ConsecutiveFailures,
		ConsecutiveSuccesses: current.ConsecutiveSuccesses, ObservingUntil: current.ObservingUntil, Now: now,
		Result: outcome.Result, Policy: spec.policy,
	})
	if !spec.policy.AutoDegradeEnabled {
		// 自动降级关闭：只记录探活结果，状态机不推进。
		transitionOut = TransitionOutput{
			NextState: current.State, Weight: current.CurrentWeight,
			ConsecutiveFailures: transitionOut.ConsecutiveFailures, ConsecutiveSuccesses: transitionOut.ConsecutiveSuccesses,
		}
	}

	next := *current
	next.State = transitionOut.NextState
	next.CurrentWeight = transitionOut.Weight
	next.ConsecutiveFailures = transitionOut.ConsecutiveFailures
	next.ConsecutiveSuccesses = transitionOut.ConsecutiveSuccesses
	next.CooldownUntil = transitionOut.CooldownUntil
	next.ObservingUntil = transitionOut.ObservingUntil
	next.LastProbeAt = &now
	latencyMs := outcome.LatencyMs
	next.LastLatencyMs = &latencyMs

	if outcome.Result == ResultOK {
		next.LastSuccessAt = &now
		next.LastErrorKey = ""
		next.LastErrorDetail = ""
	} else {
		next.LastFailureAt = &now
		next.LastErrorKey = string(outcome.Result)
		next.LastErrorDetail = outcome.Detail
	}

	if err := s.repo.UpsertState(ctx, next); err != nil {
		return nil, err
	}
	return &targetProbeResult{
		state: &next, previousState: current.State, outcome: outcome, latencyMs: latencyMs, spec: spec,
		triggeredRemote: transitionOut.TriggerRemoteDegrade || transitionOut.TriggerRemoteRestore,
	}, nil
}

func probeBudgetDayStart(now time.Time) time.Time {
	// 产品当前按中国自然日展示“每日预算”，使用固定 UTC+8 避免容器运行在 UTC 时于早上 8 点重置。
	location := time.FixedZone("UTC+8", 8*60*60)
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
}

func probeBudgetLimit(policy Policy) int {
	return defaultInt(policy.DailyProbeBudget, 1000)
}

// probeQuotaPerUnit 与 new-api 默认 quota_per_unit 一致：group_ratio=1 时 tokens → 平台 USD。
// 真实费用 = tokens / probeQuotaPerUnit × 上游分组倍率，再 × 站点充值倍率得到 CNY。
// 不再使用可配置「估算费率（USD/千 tokens）」。
const probeQuotaPerUnit = 500000.0

// resolveProbeTokens 取上游 usage tokens；无 usage 时用 max_tokens+16 兜底（短 prompt）。
func resolveProbeTokens(outcome ProbeOutcome, maxTokens int) int {
	tokens := outcome.TotalTokens
	if tokens <= 0 {
		tokens = outcome.PromptTokens + outcome.CompletionTokens
	}
	if tokens <= 0 {
		if maxTokens <= 0 {
			maxTokens = 1
		}
		tokens = maxTokens + 16
	}
	return tokens
}

// computeProbeCost 计算单次探活真实费用（双币种）。
// 优先使用上游响应 actual_cost/total_cost（平台 USD）；否则 tokens/quota × 分组倍率。
// CNY = USD × 站点充值倍率。各上游 USD 口径可能不同，预算与主展示以 CNY 为准。
func computeProbeCost(outcome ProbeOutcome, maxTokens int, groupRatio float64, rechargeRate float64) ProbeCost {
	if groupRatio <= 0 || !isFiniteFloat(groupRatio) {
		groupRatio = 1
	}
	if rechargeRate <= 0 || !isFiniteFloat(rechargeRate) {
		rechargeRate = 1
	}
	var usd float64
	if outcome.ActualCostUSD > 0 && isFiniteFloat(outcome.ActualCostUSD) {
		// 响应已带真实费用时仍乘分组倍率？多数网关 actual_cost 已含 group_ratio，不再二次乘。
		usd = outcome.ActualCostUSD
	} else {
		tokens := resolveProbeTokens(outcome, maxTokens)
		usd = float64(tokens) / probeQuotaPerUnit * groupRatio
	}
	if usd < 0 || !isFiniteFloat(usd) {
		usd = 0
	}
	cny := usd * rechargeRate
	if cny < 0 || !isFiniteFloat(cny) {
		cny = 0
	}
	return ProbeCost{USD: usd, CNY: cny}
}

// applyUpstreamKeyCostRates 把上游 API Key 分组倍率拆成 groupRatio + rechargeRate 写入 target。
// costMultiplier = group × recharge（与列表「上游 API Key 倍率」同口径）；拆分后用于双币种记账。
func applyUpstreamKeyCostRates(target *AdminProbeTarget, info upstreamKeyGroupInfo, rechargeRate float64) {
	if target == nil {
		return
	}
	rate := rechargeRate
	if rate <= 0 || !isFiniteFloat(rate) {
		rate = 1
	}
	target.CostRechargeRate = rate
	if info.multiplier == nil || *info.multiplier <= 0 || !isFiniteFloat(*info.multiplier) {
		target.CostGroupRatio = 1
		return
	}
	// multiplier 已是 group×recharge；还原原始分组倍率。
	target.CostGroupRatio = *info.multiplier / rate
	if target.CostGroupRatio <= 0 || !isFiniteFloat(target.CostGroupRatio) {
		target.CostGroupRatio = 1
	}
}

// attachProbeCostRates 为独立探活目标补齐上游分组倍率与站点充值倍率。
// 无法解析时按 1x 记账（仍基于真实 tokens / actual_cost，不再使用策略级估算费率）。
func (s *Service) attachProbeCostRates(ctx context.Context, userID string, adminAccountID string, platform string, target *AdminProbeTarget) {
	if target == nil {
		return
	}
	target.CostGroupRatio = 1
	target.CostRechargeRate = 1
	groups := s.upstreamKeyGroupsByAdminAccount(ctx, userID, adminAccountID, platform)
	info, ok := groups[strings.TrimSpace(target.AccountID)]
	if !ok {
		return
	}
	rate := 1.0
	if info.siteID != "" && s.sites != nil {
		if site, err := s.sites.GetSite(ctx, info.siteID); err == nil && site != nil && site.RechargeRate > 0 {
			rate = site.RechargeRate
		}
	}
	applyUpstreamKeyCostRates(target, info, rate)
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (s *Service) finishTargetProbeBatch(ctx context.Context, userID string, adminAccountID string, session upstream.Session, target AdminProbeTarget, specs []probeModelSpec, results []targetProbeResult) {
	if len(results) == 0 {
		return
	}
	remoteAction, actionErr := s.reconcileTargetRemoteAction(ctx, userID, adminAccountID, session, target, specs)
	if actionErr != nil {
		log.Printf("[connection-health] reconcile target action failed target_id=%s action=%s err=%v", target.TargetID, remoteAction, actionErr)
	}
	if remoteAction == "" {
		for _, result := range results {
			if result.triggeredRemote && !policyRemoteActionEnabled(result.spec.policy) {
				remoteAction = RemoteActionSkippedIndependentProbe
				break
			}
		}
	}
	if remoteAction != "" {
		actionIndex := len(results) - 1
		for index := len(results) - 1; index >= 0; index-- {
			if policyRemoteActionEnabled(results[index].spec.policy) {
				actionIndex = index
				break
			}
		}
		results[actionIndex].state.LastRemoteAction = remoteAction
		if err := s.repo.UpsertState(ctx, *results[actionIndex].state); err != nil {
			log.Printf("[connection-health] store target remote action failed target_id=%s err=%v", target.TargetID, err)
		}
		for index := range results {
			result := &results[index]
			eventTarget := targetForProbeSpec(target, result.spec)
			action := ""
			if index == actionIndex {
				action = remoteAction
			}
			s.recordTargetEvent(ctx, userID, adminAccountID, eventTarget, result.spec.policy.ID, result.spec.modelName,
				string(result.outcome.Result), string(result.previousState), string(result.state.State), &result.latencyMs,
				result.state.LastErrorKey, result.state.LastErrorDetail, action, result.outcome.CostUSD, result.outcome.CostCNY)
		}
		return
	}
	for index := range results {
		result := &results[index]
		eventTarget := targetForProbeSpec(target, result.spec)
		s.recordTargetEvent(ctx, userID, adminAccountID, eventTarget, result.spec.policy.ID, result.spec.modelName,
			string(result.outcome.Result), string(result.previousState), string(result.state.State), &result.latencyMs,
			result.state.LastErrorKey, result.state.LastErrorDetail, "", result.outcome.CostUSD, result.outcome.CostCNY)
	}
}

func targetForProbeSpec(target AdminProbeTarget, spec probeModelSpec) AdminProbeTarget {
	if !spec.eventGroupResolved {
		return target
	}
	target.AdminGroupID = spec.eventAdminGroupID
	target.AdminGroupName = spec.eventAdminGroupName
	return target
}

// defaultTargetState 构造一个目标模型的初始健康状态。connection_id 列存 targetId，
// upstream_site_id 允许为空字符串（NOT NULL 但可为空串），admin 分组信息落在 own_group_* /
// upstream_group_name 字段里，复用现有列语义。
func defaultTargetState(userID string, adminAccountID string, target AdminProbeTarget, modelName string) ConnectionHealthState {
	return ConnectionHealthState{
		ConnectionID:      target.TargetID,
		ModelName:         modelName,
		UserID:            userID,
		AdminAccountID:    adminAccountID,
		OwnGroupID:        target.AdminGroupID,
		OwnGroupName:      target.AdminGroupName,
		UpstreamSiteID:    "",
		UpstreamGroupID:   target.AdminGroupID,
		UpstreamGroupName: target.AdminGroupName,
		State:             StateHealthy,
		CurrentWeight:     100,
	}
}

// recordTargetEvent 写入一条独立探活事件（connection_id 列存 targetId）。error_detail 已在
// probe_runner 里脱敏，绝不含明文 key。
func (s *Service) recordTargetEvent(ctx context.Context, userID string, adminAccountID string, target AdminProbeTarget, policyID string, modelName string, result string, fromState string, toState string, latencyMs *int, errorKey string, errorDetail string, remoteAction string, costUSD float64, costCNY float64) {
	id, err := newID()
	if err != nil {
		log.Printf("[connection-health] generate target event id failed: %v", err)
		return
	}
	event := ConnectionHealthEvent{
		ID: id, ConnectionID: target.TargetID, ModelName: modelName, UserID: userID, AdminAccountID: adminAccountID,
		PolicyID: policyID, AdminGroupID: target.AdminGroupID,
		OwnGroupName: target.AdminGroupName, UpstreamSiteID: "", UpstreamGroupName: target.AdminGroupName, Result: result,
		FromState: fromState, ToState: toState, LatencyMs: latencyMs, ErrorKey: errorKey, ErrorDetail: errorDetail, RemoteAction: remoteAction,
		CostUSD: costUSD, CostCNY: costCNY,
	}
	if err := s.repo.InsertEvent(ctx, event); err != nil {
		log.Printf("[connection-health] insert target event failed target_id=%s err=%v", target.TargetID, err)
	}
}

// splitModelList 把逗号分隔的模型字符串拆成去空列表（连接层已有 splitModels 在 upstream 包，
// 此处提供 connection_health 包内等价实现，避免跨包耦合一个纯字符串工具）。
func splitModelList(models string) []string {
	if strings.TrimSpace(models) == "" {
		return nil
	}
	parts := strings.Split(models, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
