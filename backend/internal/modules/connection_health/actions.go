package connection_health

import (
	"context"
	"fmt"

	"transithub/backend/internal/modules/my_sites"
	"transithub/backend/internal/modules/upstream"
)

// RemoteActionRunner 是自动降级/恢复对上游平台的远端动作接口，按平台类型分派到具体实现。
// 所有实现都必须是 panic-safe 的：远端调用失败绝不能让调度器崩溃。
//
// Degrade/Restore 服务旧 real_connections 对接链路路径；DegradeTarget/RestoreTarget 服务当前
// 分组健康的独立探活 targetId 路径（见 admin_targets.go 的 probeTargetOnce），不依赖
// real_connections，避免为了调用远端动作而伪造一条 RealConnection。
type RemoteActionRunner interface {
	Degrade(ctx context.Context, conn my_sites.RealConnection, state ConnectionHealthState) (remoteAction string, err error)
	Restore(ctx context.Context, conn my_sites.RealConnection, state ConnectionHealthState) (remoteAction string, err error)
	DegradeTarget(ctx context.Context, session upstream.Session, target AdminProbeTarget, state ConnectionHealthState) (remoteAction string, err error)
	RestoreTarget(ctx context.Context, session upstream.Session, target AdminProbeTarget, state ConnectionHealthState) (remoteAction string, err error)
	ApplyTargetState(ctx context.Context, session upstream.Session, target AdminProbeTarget, weight *int, status string) (remoteAction string, err error)
	// ApplyTargetModels 写入 sub2api 账号模型限制；new-api 等不支持时返回 unsupported。
	ApplyTargetModels(ctx context.Context, session upstream.Session, target AdminProbeTarget, models string) (remoteAction string, err error)
}

// PlatformActioner 是 connection_health 对 upstream.PlatformService 远端降级能力的窄依赖，
// 避免直接依赖 PlatformService 的其余大量方法。
type PlatformActioner interface {
	UpdateNewAPIChannelWeightStatus(session upstream.Session, channelID string, weight int, status int) error
	// UpdateSub2APIAdminAccountStatus 切换 sub2api 转发账号 status（active/inactive）。
	// 仅用于从历史 inactive 恢复；新的自动降级请用 UpdateSub2APIAdminAccountSchedulable。
	UpdateSub2APIAdminAccountStatus(session upstream.Session, accountID string, status string) error
	// UpdateSub2APIAdminAccountSchedulable 切换 sub2api 转发账号调度开关。
	// 自动降级关调度、恢复开调度，避免写 status=inactive 导致管理端无法恢复。
	UpdateSub2APIAdminAccountSchedulable(session upstream.Session, accountID string, schedulable bool) error
	// UpdateSub2APIAdminAccountModels 更新 sub2api 转发账号的模型限制（逗号分隔 models 字段）。
	UpdateSub2APIAdminAccountModels(session upstream.Session, accountID string, models string) error
}

// SessionProvider 复用 my_sites 已登录并自动刷新的 admin 会话，不重复实现登录逻辑。
type SessionProvider interface {
	RequireSession(ctx context.Context, userID string, adminAccountID string) (upstream.Session, error)
}

// RemoteActionUnsupported 是没有已验证安全接口时的统一标记，绝不发明未经证实的远端请求。
const RemoteActionUnsupported = "unsupported"

// Sub2API 远端动作标记：
//   - 自动降级/恢复主路径改为调度开关（schedulable），不再写 status=inactive。
//   - status_active 仍保留：仅用于把历史误写成 inactive 的账号拉回 active，便于管理端继续操作。
//   - 不做 priority 权重映射——sub2api 的 priority 是调度优先级，不等同于 NewAPI 的 weight。
//
// *Failed 与 RemoteActionUnsupported 区分：unsupported=平台无能力；*Failed=已调用但上游失败。
const (
	// 兼容旧事件/快照中的 status 动作名；新写入优先用 schedulable_*。
	RemoteActionSub2APIStatusInactive       = "sub2api_account_status_inactive"
	RemoteActionSub2APIStatusActive         = "sub2api_account_status_active"
	RemoteActionSub2APIStatusInactiveFailed = "sub2api_account_status_inactive_failed"
	RemoteActionSub2APIStatusActiveFailed   = "sub2api_account_status_active_failed"
	RemoteActionSub2APISchedulableOff       = "sub2api_account_schedulable_off"
	RemoteActionSub2APISchedulableOn        = "sub2api_account_schedulable_on"
	RemoteActionSub2APISchedulableOffFailed = "sub2api_account_schedulable_off_failed"
	RemoteActionSub2APISchedulableOnFailed  = "sub2api_account_schedulable_on_failed"
	RemoteActionSub2APIModelsUpdated        = "sub2api_account_models_updated"
	RemoteActionSub2APIModelsUpdateFailed   = "sub2api_account_models_update_failed"
	RemoteActionNewAPIUpdateFailed          = "newapi_channel_update_failed"
)

// remoteActionDispatcher 按连接所在上游站点的平台类型（new-api / sub2api）分派远端动作。
// new-api 通过 admin channel 的 weight/status 实现降级/恢复；sub2api 通过 admin account 的
// status（active/inactive）实现降级/恢复，不涉及 priority。
type remoteActionDispatcher struct {
	sites    SiteLookup
	sessions SessionProvider
	platform PlatformActioner
}

func newRemoteActionDispatcher(sites SiteLookup, sessions SessionProvider, platform PlatformActioner) *remoteActionDispatcher {
	return &remoteActionDispatcher{sites: sites, sessions: sessions, platform: platform}
}

func (d *remoteActionDispatcher) Degrade(ctx context.Context, conn my_sites.RealConnection, state ConnectionHealthState) (remoteAction string, err error) {
	defer func() {
		if r := recover(); r != nil {
			remoteAction = RemoteActionUnsupported
			err = fmt.Errorf("remote degrade panic recovered: %v", r)
		}
	}()

	site, siteErr := d.sites.GetSite(ctx, conn.UpstreamSiteID)
	if siteErr != nil || site == nil {
		return RemoteActionUnsupported, siteErr
	}

	switch site.Platform {
	case upstream.PlatformNewAPI:
		return d.degradeNewAPI(ctx, conn)
	case upstream.PlatformSub2API:
		return d.degradeSub2API(ctx, conn)
	default:
		return RemoteActionUnsupported, nil
	}
}

func (d *remoteActionDispatcher) Restore(ctx context.Context, conn my_sites.RealConnection, state ConnectionHealthState) (remoteAction string, err error) {
	defer func() {
		if r := recover(); r != nil {
			remoteAction = RemoteActionUnsupported
			err = fmt.Errorf("remote restore panic recovered: %v", r)
		}
	}()

	site, siteErr := d.sites.GetSite(ctx, conn.UpstreamSiteID)
	if siteErr != nil || site == nil {
		return RemoteActionUnsupported, siteErr
	}

	switch site.Platform {
	case upstream.PlatformNewAPI:
		return d.restoreNewAPI(ctx, conn, state)
	case upstream.PlatformSub2API:
		return d.restoreSub2API(ctx, conn)
	default:
		return RemoteActionUnsupported, nil
	}
}

// DegradeTarget / RestoreTarget 服务当前分组健康的独立探活 targetId 路径：不依赖
// real_connections，直接用调用方已经持有的 session + AdminProbeTarget 发起远端动作。
func (d *remoteActionDispatcher) DegradeTarget(ctx context.Context, session upstream.Session, target AdminProbeTarget, state ConnectionHealthState) (remoteAction string, err error) {
	defer func() {
		if r := recover(); r != nil {
			remoteAction = RemoteActionUnsupported
			err = fmt.Errorf("remote degrade target panic recovered: %v", r)
		}
	}()
	if target.AccountID == "" {
		return RemoteActionUnsupported, nil
	}
	if target.Platform == string(upstream.PlatformNewAPI) {
		if err := d.platform.UpdateNewAPIChannelWeightStatus(session, target.AccountID, 0, 2); err != nil {
			return RemoteActionNewAPIUpdateFailed, err
		}
		return "newapi_channel_disabled", nil
	}
	if target.Platform != string(upstream.PlatformSub2API) {
		return RemoteActionUnsupported, nil
	}
	return d.applySub2APITrafficControl(session, target, "inactive")
}

func (d *remoteActionDispatcher) RestoreTarget(ctx context.Context, session upstream.Session, target AdminProbeTarget, state ConnectionHealthState) (remoteAction string, err error) {
	defer func() {
		if r := recover(); r != nil {
			remoteAction = RemoteActionUnsupported
			err = fmt.Errorf("remote restore target panic recovered: %v", r)
		}
	}()
	if target.AccountID == "" {
		return RemoteActionUnsupported, nil
	}
	if target.Platform == string(upstream.PlatformNewAPI) {
		weight := state.CurrentWeight
		status := 1
		if weight <= 0 {
			status = 2
		}
		if err := d.platform.UpdateNewAPIChannelWeightStatus(session, target.AccountID, weight, status); err != nil {
			return RemoteActionNewAPIUpdateFailed, err
		}
		return fmt.Sprintf("newapi_channel_weight_%d", weight), nil
	}
	if target.Platform != string(upstream.PlatformSub2API) {
		return RemoteActionUnsupported, nil
	}
	return d.applySub2APITrafficControl(session, target, "active")
}

// ApplyTargetState 写入账号级聚合决策。旧 real_connections 仍使用 Degrade/Restore；新的
// admin 分组健康链路通过这里精确恢复接管前保存的启停状态和权重。
func (d *remoteActionDispatcher) ApplyTargetState(ctx context.Context, session upstream.Session, target AdminProbeTarget, weight *int, status string) (remoteAction string, err error) {
	defer func() {
		if r := recover(); r != nil {
			remoteAction = RemoteActionUnsupported
			err = fmt.Errorf("apply target state panic recovered: %v", r)
		}
	}()
	if target.AccountID == "" {
		return RemoteActionUnsupported, nil
	}
	if target.Platform == string(upstream.PlatformNewAPI) {
		resolvedWeight := 100
		if weight != nil {
			resolvedWeight = *weight
		}
		resolvedStatus := 1
		if status == "2" || status == "disabled" || status == "inactive" {
			resolvedStatus = 2
		}
		if err := d.platform.UpdateNewAPIChannelWeightStatus(session, target.AccountID, resolvedWeight, resolvedStatus); err != nil {
			return RemoteActionNewAPIUpdateFailed, err
		}
		if resolvedStatus == 2 && resolvedWeight == 0 {
			return "newapi_channel_disabled", nil
		}
		return fmt.Sprintf("newapi_channel_weight_%d", resolvedWeight), nil
	}
	if target.Platform != string(upstream.PlatformSub2API) {
		return RemoteActionUnsupported, nil
	}
	resolvedStatus := "active"
	if status == "inactive" || status == "disabled" || status == "2" {
		resolvedStatus = "inactive"
	}
	return d.applySub2APITrafficControl(session, target, resolvedStatus)
}

// applySub2APITrafficControl 把逻辑启停映射到 Sub2API 调度开关：
//   - 降级（inactive）：只关 schedulable，绝不写 status=inactive
//   - 恢复（active）：开 schedulable；若账号仍是历史 inactive/error，再写 status=active 以便管理端可操作
func (d *remoteActionDispatcher) applySub2APITrafficControl(session upstream.Session, target AdminProbeTarget, desired string) (string, error) {
	if desired == "inactive" {
		if err := d.platform.UpdateSub2APIAdminAccountSchedulable(session, target.AccountID, false); err != nil {
			return RemoteActionSub2APISchedulableOffFailed, err
		}
		return RemoteActionSub2APISchedulableOff, nil
	}

	// 恢复：先开调度。
	if err := d.platform.UpdateSub2APIAdminAccountSchedulable(session, target.AccountID, true); err != nil {
		return RemoteActionSub2APISchedulableOnFailed, err
	}
	action := RemoteActionSub2APISchedulableOn
	// 历史路径可能写过 status=inactive；管理端对 inactive 难恢复，这里顺带拉回 active。
	if normalizeTargetStatus(string(upstream.PlatformSub2API), target.AccountStatus) != "active" {
		if err := d.platform.UpdateSub2APIAdminAccountStatus(session, target.AccountID, "active"); err != nil {
			return RemoteActionSub2APIStatusActiveFailed, err
		}
		action = RemoteActionSub2APISchedulableOn + "," + RemoteActionSub2APIStatusActive
	}
	return action, nil
}

// ApplyTargetModels 仅对 sub2api 账号写入 models 字段（模型限制）。其它平台返回 unsupported。
func (d *remoteActionDispatcher) ApplyTargetModels(ctx context.Context, session upstream.Session, target AdminProbeTarget, models string) (remoteAction string, err error) {
	defer func() {
		if r := recover(); r != nil {
			remoteAction = RemoteActionUnsupported
			err = fmt.Errorf("apply target models panic recovered: %v", r)
		}
	}()
	if target.AccountID == "" || target.Platform != string(upstream.PlatformSub2API) {
		return RemoteActionUnsupported, nil
	}
	if err := d.platform.UpdateSub2APIAdminAccountModels(session, target.AccountID, models); err != nil {
		return RemoteActionSub2APIModelsUpdateFailed, err
	}
	return RemoteActionSub2APIModelsUpdated, nil
}

func (d *remoteActionDispatcher) degradeNewAPI(ctx context.Context, conn my_sites.RealConnection) (string, error) {
	// new-api 场景下 RealConnection.AdminAccountID 存的是创建真实对接时回查得到的 channel ID
	// （见 my_sites.Service.RealConnect），不是转发子账号 ID。
	channelID := conn.AdminAccountID
	if channelID == "" {
		return RemoteActionUnsupported, nil
	}
	session, err := d.sessions.RequireSession(ctx, conn.UserID, conn.WorkspaceAdminAccountID)
	if err != nil {
		return RemoteActionUnsupported, err
	}
	if err := d.platform.UpdateNewAPIChannelWeightStatus(session, channelID, 0, 2); err != nil {
		return RemoteActionUnsupported, err
	}
	return "newapi_channel_disabled", nil
}

func (d *remoteActionDispatcher) restoreNewAPI(ctx context.Context, conn my_sites.RealConnection, state ConnectionHealthState) (string, error) {
	channelID := conn.AdminAccountID
	if channelID == "" {
		return RemoteActionUnsupported, nil
	}
	session, err := d.sessions.RequireSession(ctx, conn.UserID, conn.WorkspaceAdminAccountID)
	if err != nil {
		return RemoteActionUnsupported, err
	}
	weight := state.CurrentWeight
	status := 1
	if weight <= 0 {
		// 权重仍为 0 时不解除远端禁用，避免观察期误放流量。
		status = 2
	}
	if err := d.platform.UpdateNewAPIChannelWeightStatus(session, channelID, weight, status); err != nil {
		return RemoteActionUnsupported, err
	}
	return fmt.Sprintf("newapi_channel_weight_%d", weight), nil
}

// degradeSub2API / restoreSub2API 是旧 real_connections 对接链路路径下的 sub2api 远端动作：
// RealConnection.AdminAccountID 在 sub2api 场景下就是 sub2api admin account id。
// 与独立探活路径一致：降级关调度，恢复开调度（必要时顺带恢复 status=active）。
func (d *remoteActionDispatcher) degradeSub2API(ctx context.Context, conn my_sites.RealConnection) (string, error) {
	accountID := conn.AdminAccountID
	if accountID == "" {
		return RemoteActionUnsupported, nil
	}
	session, err := d.sessions.RequireSession(ctx, conn.UserID, conn.WorkspaceAdminAccountID)
	if err != nil {
		return RemoteActionUnsupported, err
	}
	target := AdminProbeTarget{AccountID: accountID, Platform: string(upstream.PlatformSub2API), AccountStatus: "active"}
	return d.applySub2APITrafficControl(session, target, "inactive")
}

func (d *remoteActionDispatcher) restoreSub2API(ctx context.Context, conn my_sites.RealConnection) (string, error) {
	accountID := conn.AdminAccountID
	if accountID == "" {
		return RemoteActionUnsupported, nil
	}
	session, err := d.sessions.RequireSession(ctx, conn.UserID, conn.WorkspaceAdminAccountID)
	if err != nil {
		return RemoteActionUnsupported, err
	}
	// 旧路径没有当前 status 快照：恢复时同时尝试 status=active（AccountStatus 置 inactive 触发）。
	target := AdminProbeTarget{AccountID: accountID, Platform: string(upstream.PlatformSub2API), AccountStatus: "inactive"}
	return d.applySub2APITrafficControl(session, target, "active")
}
