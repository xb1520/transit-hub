package connection_health

import (
	"context"
	"errors"
	"testing"

	"transithub/backend/internal/modules/my_sites"
	"transithub/backend/internal/modules/upstream"
)

type fakeSiteLookup struct {
	site *upstream.Site
	err  error
}

func (f fakeSiteLookup) GetSite(ctx context.Context, siteID string) (*upstream.Site, error) {
	return f.site, f.err
}

type fakeSessionProvider struct {
	session upstream.Session
	err     error
}

func (f fakeSessionProvider) RequireSession(ctx context.Context, userID string, adminAccountID string) (upstream.Session, error) {
	return f.session, f.err
}

type fakePlatformActioner struct {
	err        error
	panicValue any
	calls      []struct {
		channelID string
		weight    int
		status    int
	}
	// sub2APICalls 记录 status 写入（仅历史 inactive 恢复路径）。
	sub2APICalls []struct {
		accountID string
		status    string
	}
	// sub2APISchedulableCalls 记录调度开关写入（主降级/恢复路径）。
	sub2APISchedulableCalls []struct {
		accountID   string
		schedulable bool
	}
	sub2APIModelCalls []struct {
		accountID string
		models    string
	}
	sub2APIErr           error
	sub2APISchedulableErr error
	sub2APIModelsErr     error
}

func (f *fakePlatformActioner) UpdateNewAPIChannelWeightStatus(session upstream.Session, channelID string, weight int, status int) error {
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	f.calls = append(f.calls, struct {
		channelID string
		weight    int
		status    int
	}{channelID, weight, status})
	return f.err
}

func (f *fakePlatformActioner) UpdateSub2APIAdminAccountStatus(session upstream.Session, accountID string, status string) error {
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	f.sub2APICalls = append(f.sub2APICalls, struct {
		accountID string
		status    string
	}{accountID, status})
	return f.sub2APIErr
}

func (f *fakePlatformActioner) UpdateSub2APIAdminAccountSchedulable(session upstream.Session, accountID string, schedulable bool) error {
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	f.sub2APISchedulableCalls = append(f.sub2APISchedulableCalls, struct {
		accountID   string
		schedulable bool
	}{accountID, schedulable})
	return f.sub2APISchedulableErr
}

func (f *fakePlatformActioner) UpdateSub2APIAdminAccountModels(session upstream.Session, accountID string, models string) error {
	if f.panicValue != nil {
		panic(f.panicValue)
	}
	f.sub2APIModelCalls = append(f.sub2APIModelCalls, struct {
		accountID string
		models    string
	}{accountID, models})
	return f.sub2APIModelsErr
}

func TestActions_NewAPIDegradeSuccess(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformNewAPI}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformNewAPI}}
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "channel-42", UserID: "u1", WorkspaceAdminAccountID: "ws1"}
	action, err := dispatcher.Degrade(context.Background(), conn, ConnectionHealthState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != "newapi_channel_disabled" {
		t.Fatalf("unexpected remote action: %s", action)
	}
	if len(platform.calls) != 1 || platform.calls[0].weight != 0 || platform.calls[0].status != 2 {
		t.Fatalf("expected one call with weight=0 status=2, got %+v", platform.calls)
	}
}

func TestActions_NewAPIDegradeFailurePropagatesAsUnsupported(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformNewAPI}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformNewAPI}}
	platform := &fakePlatformActioner{err: errors.New("upstream 500")}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "channel-42"}
	action, err := dispatcher.Degrade(context.Background(), conn, ConnectionHealthState{})
	if err == nil {
		t.Fatalf("expected error to propagate")
	}
	if action != RemoteActionUnsupported {
		t.Fatalf("expected unsupported on failure, got %s", action)
	}
}

func TestActions_NewAPIDegradePanicRecovered(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformNewAPI}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformNewAPI}}
	platform := &fakePlatformActioner{panicValue: "boom"}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "channel-42"}

	action, err := dispatcher.Degrade(context.Background(), conn, ConnectionHealthState{})
	if err == nil {
		t.Fatalf("expected panic to surface as error, scheduler must not crash")
	}
	if action != RemoteActionUnsupported {
		t.Fatalf("expected unsupported after panic recovery, got %s", action)
	}
}

// TestActions_Sub2APIDegradeTurnsSchedulableOff 验证 sub2api 自动降级只关调度开关，
// 绝不写 status=inactive（管理端对 inactive 难以恢复）。
func TestActions_Sub2APIDegradeTurnsSchedulableOff(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformSub2API}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "sub-account-1", UpstreamKeyID: "key-1"}
	action, err := dispatcher.Degrade(context.Background(), conn, ConnectionHealthState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != RemoteActionSub2APISchedulableOff {
		t.Fatalf("expected sub2api_account_schedulable_off, got %s", action)
	}
	if len(platform.sub2APISchedulableCalls) != 1 || platform.sub2APISchedulableCalls[0].accountID != "sub-account-1" || platform.sub2APISchedulableCalls[0].schedulable {
		t.Fatalf("expected schedulable=false, got %+v", platform.sub2APISchedulableCalls)
	}
	if len(platform.sub2APICalls) != 0 {
		t.Fatalf("degrade must not write status, got %+v", platform.sub2APICalls)
	}
}

// TestActions_Sub2APIRestoreTurnsSchedulableOn 验证恢复会开调度，并顺带把历史 inactive status 拉回 active。
func TestActions_Sub2APIRestoreTurnsSchedulableOn(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformSub2API}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "sub-account-1"}
	action, err := dispatcher.Restore(context.Background(), conn, ConnectionHealthState{CurrentWeight: 25})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != RemoteActionSub2APISchedulableOn+","+RemoteActionSub2APIStatusActive {
		t.Fatalf("expected schedulable_on + status_active, got %s", action)
	}
	if len(platform.sub2APISchedulableCalls) != 1 || !platform.sub2APISchedulableCalls[0].schedulable {
		t.Fatalf("expected schedulable=true, got %+v", platform.sub2APISchedulableCalls)
	}
	if len(platform.sub2APICalls) != 1 || platform.sub2APICalls[0].status != "active" {
		t.Fatalf("expected status=active recovery write, got %+v", platform.sub2APICalls)
	}
}

// TestActions_Sub2APIDoesNotUseNewAPIWeightStatus 验证 sub2api 降级/恢复绝不调用
// new-api 专用的 UpdateNewAPIChannelWeightStatus（不把 priority 当 weight 处理）。
func TestActions_Sub2APIDoesNotUseNewAPIWeightStatus(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformSub2API}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "sub-account-1"}
	if _, err := dispatcher.Degrade(context.Background(), conn, ConnectionHealthState{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := dispatcher.Restore(context.Background(), conn, ConnectionHealthState{CurrentWeight: 50}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(platform.calls) != 0 {
		t.Fatalf("sub2api must never call the new-api channel weight/status update, got %d calls", len(platform.calls))
	}
}

// TestActions_Sub2APIRemoteFailureIsReturned 验证关调度失败时返回 schedulable_off_failed，不折叠成 unsupported。
func TestActions_Sub2APIRemoteFailureIsReturned(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformSub2API}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	platform := &fakePlatformActioner{sub2APISchedulableErr: errors.New("upstream 500")}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "sub-account-1"}
	action, err := dispatcher.Degrade(context.Background(), conn, ConnectionHealthState{})
	if err == nil {
		t.Fatalf("expected error to propagate")
	}
	if action != RemoteActionSub2APISchedulableOffFailed {
		t.Fatalf("expected sub2api_account_schedulable_off_failed, got %s", action)
	}
}

// TestActions_Sub2APIDegradeTargetTurnsSchedulableOff 验证 target 维度降级只关调度。
func TestActions_Sub2APIDegradeTargetTurnsSchedulableOff(t *testing.T) {
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, platform)

	session := upstream.Session{Platform: upstream.PlatformSub2API}
	target := AdminProbeTarget{TargetID: "sub2api:ws1:acc-1", Platform: string(upstream.PlatformSub2API), AccountID: "acc-1", AccountStatus: "active"}
	action, err := dispatcher.DegradeTarget(context.Background(), session, target, ConnectionHealthState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != RemoteActionSub2APISchedulableOff {
		t.Fatalf("expected sub2api_account_schedulable_off, got %s", action)
	}
	if len(platform.sub2APISchedulableCalls) != 1 || platform.sub2APISchedulableCalls[0].schedulable || platform.sub2APISchedulableCalls[0].accountID != "acc-1" {
		t.Fatalf("expected schedulable=false for acc-1, got %+v", platform.sub2APISchedulableCalls)
	}
	if len(platform.sub2APICalls) != 0 {
		t.Fatalf("must not write status on degrade, got %+v", platform.sub2APICalls)
	}
}

// TestActions_Sub2APIRestoreTargetTurnsSchedulableOn 验证 target 维度恢复开调度；status 已是 active 时不写 status。
func TestActions_Sub2APIRestoreTargetTurnsSchedulableOn(t *testing.T) {
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, platform)

	session := upstream.Session{Platform: upstream.PlatformSub2API}
	target := AdminProbeTarget{TargetID: "sub2api:ws1:acc-1", Platform: string(upstream.PlatformSub2API), AccountID: "acc-1", AccountStatus: "active"}
	action, err := dispatcher.RestoreTarget(context.Background(), session, target, ConnectionHealthState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != RemoteActionSub2APISchedulableOn {
		t.Fatalf("expected sub2api_account_schedulable_on, got %s", action)
	}
	if len(platform.sub2APISchedulableCalls) != 1 || !platform.sub2APISchedulableCalls[0].schedulable {
		t.Fatalf("expected schedulable=true, got %+v", platform.sub2APISchedulableCalls)
	}
	if len(platform.sub2APICalls) != 0 {
		t.Fatalf("active status must not re-write status, got %+v", platform.sub2APICalls)
	}
}

// TestActions_Sub2APIDegradeTargetFailureReturnsFailedAction 验证关调度失败返回 failed 标记。
func TestActions_Sub2APIDegradeTargetFailureReturnsFailedAction(t *testing.T) {
	platform := &fakePlatformActioner{sub2APISchedulableErr: errors.New("upstream 500")}
	dispatcher := newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, platform)

	session := upstream.Session{Platform: upstream.PlatformSub2API}
	target := AdminProbeTarget{TargetID: "sub2api:ws1:acc-1", Platform: string(upstream.PlatformSub2API), AccountID: "acc-1"}
	action, err := dispatcher.DegradeTarget(context.Background(), session, target, ConnectionHealthState{})
	if err == nil {
		t.Fatalf("expected error to propagate")
	}
	if action != RemoteActionSub2APISchedulableOffFailed {
		t.Fatalf("expected sub2api_account_schedulable_off_failed, got %s", action)
	}
}

// TestActions_Sub2APIRestoreTargetFailureReturnsFailedAction 验证开调度失败返回 failed 标记。
func TestActions_Sub2APIRestoreTargetFailureReturnsFailedAction(t *testing.T) {
	platform := &fakePlatformActioner{sub2APISchedulableErr: errors.New("upstream 500")}
	dispatcher := newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, platform)

	session := upstream.Session{Platform: upstream.PlatformSub2API}
	target := AdminProbeTarget{TargetID: "sub2api:ws1:acc-1", Platform: string(upstream.PlatformSub2API), AccountID: "acc-1", AccountStatus: "active"}
	action, err := dispatcher.RestoreTarget(context.Background(), session, target, ConnectionHealthState{})
	if err == nil {
		t.Fatalf("expected error to propagate")
	}
	if action != RemoteActionSub2APISchedulableOnFailed {
		t.Fatalf("expected sub2api_account_schedulable_on_failed, got %s", action)
	}
}

// TestActions_NewAPITargetRemoteActionDisablesChannel 验证独立 New API target 降级时直接更新
// channel weight/status，不依赖旧 RealConnection。
func TestActions_NewAPITargetRemoteActionDisablesChannel(t *testing.T) {
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, platform)

	session := upstream.Session{Platform: upstream.PlatformNewAPI}
	target := AdminProbeTarget{TargetID: "newapi:ws1:100", Platform: string(upstream.PlatformNewAPI), AccountID: "100"}
	action, err := dispatcher.DegradeTarget(context.Background(), session, target, ConnectionHealthState{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != "newapi_channel_disabled" {
		t.Fatalf("expected newapi channel disable action, got %s", action)
	}
	if len(platform.sub2APICalls) != 0 || len(platform.calls) != 1 || platform.calls[0].weight != 0 || platform.calls[0].status != 2 {
		t.Fatalf("expected one newapi disable call, sub2api=%+v newapi=%+v", platform.sub2APICalls, platform.calls)
	}
}

func TestActions_NewAPIRestoreUsesCurrentWeight(t *testing.T) {
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", Platform: upstream.PlatformNewAPI}}
	sessions := fakeSessionProvider{session: upstream.Session{Platform: upstream.PlatformNewAPI}}
	platform := &fakePlatformActioner{}
	dispatcher := newRemoteActionDispatcher(sites, sessions, platform)

	conn := my_sites.RealConnection{UpstreamSiteID: "site-1", AdminAccountID: "channel-42"}
	state := ConnectionHealthState{CurrentWeight: 25}
	action, err := dispatcher.Restore(context.Background(), conn, state)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != "newapi_channel_weight_25" {
		t.Fatalf("unexpected remote action: %s", action)
	}
	if len(platform.calls) != 1 || platform.calls[0].weight != 25 || platform.calls[0].status != 1 {
		t.Fatalf("expected weight=25 status=1, got %+v", platform.calls)
	}
}
