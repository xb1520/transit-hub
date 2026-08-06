package connection_health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"transithub/backend/internal/modules/upstream"
)

// newAdminTargetsRemoteActionService 构造一个用真实 remoteActionDispatcher（而不是
// noopRemoteActionRunner）驱动的 Service，供本文件测试断言 probeTargetOnce 触发的真实远端动作调用。
func newAdminTargetsRemoteActionService(reader PlatformGroupReader, mySites MySitesReader, repo *fakeRepository, platform *fakePlatformActioner) *Service {
	return &Service{
		repo:           repo,
		mySites:        mySites,
		accounts:       fakeAdminAccountResolver{id: "ws1"},
		dispatcher:     newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, platform),
		probeRunner:    NewRealProbeRunner(),
		platformGroups: reader,
	}
}

// sub2APIProbePolicy 返回一条启用策略：自动降级开启，自动远端动作按参数控制。
func sub2APIProbePolicy(autoRemoteAction bool) Policy {
	return Policy{
		ID: "policy-1", UserID: "user1", AdminAccountID: "ws1", Name: "p", Enabled: true, DailyProbeBudget: 1000,
		AutoDegradeEnabled: true, AutoRemoteActionEnabled: autoRemoteAction,
		FailureThreshold: 3, SuccessThreshold: 2, CooldownSeconds: 300, ObservationSeconds: 300, RecoveryStepPercent: 25,
		ModelTargets: []ModelTarget{{ID: "t1", PolicyID: "policy-1", ModelName: "gpt-4o", ProviderFamily: ProviderOpenAI, Enabled: true, MaxProbeTokens: 1}},
	}
}

// TestProbeTargetOnce_Sub2APIAutoRemoteDegradeUpdatesInactive 验证 AutoRemoteActionEnabled=true
// 时，sub2api target 探活遭遇硬失败会关调度开关（schedulable=false），绝不写 status=inactive。
func TestProbeTargetOnce_Sub2APIAutoRemoteDegradeUpdatesInactive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	repo := newFakeRepository()
	repo.policies = []Policy{sub2APIProbePolicy(true)}
	platform := &fakePlatformActioner{}
	mySites := fakeMySitesReader{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	reader := fakePlatformGroupReader{
		groups:        []upstream.AdminGroupInfo{{ID: "g1", Name: "vip"}},
		accountsByGrp: map[string][]upstream.AdminGroupAccountInfo{"g1": {{ID: "acc-1", Name: "acc", Models: "gpt-4o"}}},
		credByAccount: map[string]upstream.ProbeCredential{"acc-1": {BaseURL: server.URL, Key: "k"}},
	}
	svc := newAdminTargetsRemoteActionService(reader, mySites, repo, platform)

	targetID := "sub2api:ws1:acc-1"
	results, err := svc.ProbeTarget(context.Background(), "user1", targetID, []string{"gpt-4o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].State != StateSuspended {
		t.Fatalf("expected hard failure to suspend immediately, got %+v", results)
	}
	if len(platform.sub2APISchedulableCalls) != 1 || platform.sub2APISchedulableCalls[0].accountID != "acc-1" || platform.sub2APISchedulableCalls[0].schedulable {
		t.Fatalf("expected schedulable=false for acc-1, got %+v", platform.sub2APISchedulableCalls)
	}
	if len(platform.sub2APICalls) != 0 {
		t.Fatalf("degrade must not write status, got %+v", platform.sub2APICalls)
	}
	st := repo.states[targetID]["gpt-4o"]
	if !strings.Contains(st.LastRemoteAction, RemoteActionSub2APISchedulableOff) {
		t.Fatalf("expected state.LastRemoteAction to include %s, got %q", RemoteActionSub2APISchedulableOff, st.LastRemoteAction)
	}
	if len(repo.events) != 1 || !strings.Contains(repo.events[0].RemoteAction, RemoteActionSub2APISchedulableOff) {
		t.Fatalf("expected event.RemoteAction to include %s, got %+v", RemoteActionSub2APISchedulableOff, repo.events)
	}
}

// TestProbeTargetOnce_Sub2APIAutoRemoteRestoreUpdatesActive 验证从 observing 达到成功阈值时
// 会开调度，并因账号仍是历史 inactive 而顺带写 status=active。
func TestProbeTargetOnce_Sub2APIAutoRemoteRestoreUpdatesActive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	repo := newFakeRepository()
	repo.policies = []Policy{sub2APIProbePolicy(true)}
	targetID := "sub2api:ws1:acc-1"
	observingUntil := time.Now().Add(-1 * time.Minute)
	repo.states[targetID] = map[string]ConnectionHealthState{
		"gpt-4o": {
			ConnectionID: targetID, ModelName: "gpt-4o", UserID: "user1", AdminAccountID: "ws1",
			State: StateObserving, ConsecutiveSuccesses: 1, ObservingUntil: &observingUntil, CurrentWeight: 0,
			LastRemoteAction: RemoteActionSub2APISchedulableOff,
		},
	}
	platform := &fakePlatformActioner{}
	mySites := fakeMySitesReader{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	reader := fakePlatformGroupReader{
		groups:        []upstream.AdminGroupInfo{{ID: "g1", Name: "vip"}},
		accountsByGrp: map[string][]upstream.AdminGroupAccountInfo{"g1": {{ID: "acc-1", Name: "acc", Status: "inactive", Models: "gpt-4o"}}},
		credByAccount: map[string]upstream.ProbeCredential{"acc-1": {BaseURL: server.URL, Key: "k"}},
	}
	svc := newAdminTargetsRemoteActionService(reader, mySites, repo, platform)

	results, err := svc.ProbeTarget(context.Background(), "user1", targetID, []string{"gpt-4o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].State != StateRecovering {
		t.Fatalf("expected transition to recovering after success threshold, got %+v", results)
	}
	if len(platform.sub2APISchedulableCalls) != 1 || !platform.sub2APISchedulableCalls[0].schedulable {
		t.Fatalf("expected schedulable=true, got %+v", platform.sub2APISchedulableCalls)
	}
	if len(platform.sub2APICalls) != 1 || platform.sub2APICalls[0].status != "active" {
		t.Fatalf("expected status=active recovery write, got %+v", platform.sub2APICalls)
	}
	st := repo.states[targetID]["gpt-4o"]
	if !strings.Contains(st.LastRemoteAction, RemoteActionSub2APISchedulableOn) {
		t.Fatalf("expected state.LastRemoteAction to include %s, got %q", RemoteActionSub2APISchedulableOn, st.LastRemoteAction)
	}
}

// TestProbeTargetOnce_Sub2APIAutoRemoteDegradeFailureRecordsFailedAction 验证关调度失败时
// remoteAction 记录 schedulable_off_failed，绝不能回退成 unsupported。
func TestProbeTargetOnce_Sub2APIAutoRemoteDegradeFailureRecordsFailedAction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()

	repo := newFakeRepository()
	repo.policies = []Policy{sub2APIProbePolicy(true)}
	platform := &fakePlatformActioner{sub2APISchedulableErr: errors.New("upstream 500")}
	mySites := fakeMySitesReader{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	reader := fakePlatformGroupReader{
		groups:        []upstream.AdminGroupInfo{{ID: "g1", Name: "vip"}},
		accountsByGrp: map[string][]upstream.AdminGroupAccountInfo{"g1": {{ID: "acc-1", Name: "acc", Models: "gpt-4o"}}},
		credByAccount: map[string]upstream.ProbeCredential{"acc-1": {BaseURL: server.URL, Key: "k"}},
	}
	svc := newAdminTargetsRemoteActionService(reader, mySites, repo, platform)

	targetID := "sub2api:ws1:acc-1"
	results, err := svc.ProbeTarget(context.Background(), "user1", targetID, []string{"gpt-4o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].State != StateSuspended {
		t.Fatalf("expected hard failure to suspend, got %+v", results)
	}
	st := repo.states[targetID]["gpt-4o"]
	if !strings.Contains(st.LastRemoteAction, RemoteActionSub2APISchedulableOffFailed) {
		t.Fatalf("expected state.LastRemoteAction to include %s, got %q", RemoteActionSub2APISchedulableOffFailed, st.LastRemoteAction)
	}
	if len(repo.events) != 1 || !strings.Contains(repo.events[0].RemoteAction, RemoteActionSub2APISchedulableOffFailed) {
		t.Fatalf("expected event.RemoteAction to include %s, got %+v", RemoteActionSub2APISchedulableOffFailed, repo.events)
	}
}

// TestProbeTargetOnce_Sub2APIAutoRemoteRestoreFailureRecordsFailedAction 验证远端恢复调用失败
// 时，state/event 记录 sub2api_account_status_active_failed，不能回退成 unsupported。
func TestProbeTargetOnce_Sub2APIAutoRemoteRestoreFailureRecordsFailedAction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	repo := newFakeRepository()
	repo.policies = []Policy{sub2APIProbePolicy(true)}
	targetID := "sub2api:ws1:acc-1"
	observingUntil := time.Now().Add(-1 * time.Minute)
	repo.states[targetID] = map[string]ConnectionHealthState{
		"gpt-4o": {
			ConnectionID: targetID, ModelName: "gpt-4o", UserID: "user1", AdminAccountID: "ws1",
			State: StateObserving, ConsecutiveSuccesses: 1, ObservingUntil: &observingUntil, CurrentWeight: 0,
			LastRemoteAction: RemoteActionSub2APIStatusInactive,
		},
	}
	platform := &fakePlatformActioner{sub2APIErr: errors.New("upstream 500")}
	mySites := fakeMySitesReader{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	reader := fakePlatformGroupReader{
		groups:        []upstream.AdminGroupInfo{{ID: "g1", Name: "vip"}},
		accountsByGrp: map[string][]upstream.AdminGroupAccountInfo{"g1": {{ID: "acc-1", Name: "acc", Status: "inactive", Models: "gpt-4o"}}},
		credByAccount: map[string]upstream.ProbeCredential{"acc-1": {BaseURL: server.URL, Key: "k"}},
	}
	svc := newAdminTargetsRemoteActionService(reader, mySites, repo, platform)

	results, err := svc.ProbeTarget(context.Background(), "user1", targetID, []string{"gpt-4o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].State != StateRecovering {
		t.Fatalf("expected transition to recovering, got %+v", results)
	}
	st := repo.states[targetID]["gpt-4o"]
	if st.LastRemoteAction != RemoteActionSub2APIStatusActiveFailed {
		t.Fatalf("expected state.LastRemoteAction=%s, got %q", RemoteActionSub2APIStatusActiveFailed, st.LastRemoteAction)
	}
}

// TestProbeTargetOnce_Sub2APIRealPlatformServiceComboDegradeSucceeds 是覆盖「PlatformService
// 单测通过、dispatcher fake 单测通过，但真实组合路径失败」这类盲区的端到端测试：
// 用同一个 httptest.Server 同时模拟探活端点（返回 500 触发 healthy -> suspended）和 sub2api
// admin accounts 的字段级批量更新，dispatcher 的 PlatformActioner 用真实 *upstream.PlatformService
// （不是 fake），断言最终状态/事件里的 remoteAction 是 sub2api_account_status_inactive，
// 且请求体只把指定账号的 status 改成 inactive。
func TestProbeTargetOnce_Sub2APIRealPlatformServiceComboDegradeSucceeds(t *testing.T) {
	var bulkBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/accounts/bulk-update":
			if err := json.NewDecoder(r.Body).Decode(&bulkBody); err != nil {
				t.Fatalf("failed to decode bulk update body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	realPlatform := upstream.NewPlatformService(upstream.NewHTTPClient(server.Client()))
	repo := newFakeRepository()
	repo.policies = []Policy{sub2APIProbePolicy(true)}
	mySites := fakeMySitesReader{session: upstream.Session{Platform: upstream.PlatformSub2API, BaseURL: server.URL, AccessToken: "token-1", TokenType: "Bearer"}}
	reader := fakePlatformGroupReader{
		groups:        []upstream.AdminGroupInfo{{ID: "g1", Name: "vip"}},
		accountsByGrp: map[string][]upstream.AdminGroupAccountInfo{"g1": {{ID: "1515", Name: "acc", Models: "gpt-4o"}}},
		credByAccount: map[string]upstream.ProbeCredential{"1515": {BaseURL: server.URL, Key: "probe-key"}},
	}
	svc := &Service{
		repo: repo, mySites: mySites, accounts: fakeAdminAccountResolver{id: "ws1"},
		dispatcher:     newRemoteActionDispatcher(fakeSiteLookup{}, fakeSessionProvider{}, realPlatform),
		probeRunner:    NewRealProbeRunner(),
		platformGroups: reader,
	}

	targetID := "sub2api:ws1:1515"
	results, err := svc.ProbeTarget(context.Background(), "user1", targetID, []string{"gpt-4o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].State != StateSuspended {
		t.Fatalf("expected hard failure to suspend, got %+v", results)
	}

	st := repo.states[targetID]["gpt-4o"]
	// 单模型 server_error：先摘除模型限制，并在「全部受控模型均异常」时同时停用账号。
	// remoteAction 可能是 models_updated、status_inactive，或二者逗号拼接（顺序取决于 reconcile）。
	if st.LastRemoteAction == "" {
		t.Fatalf("expected remote action to be recorded, got empty")
	}
	if !strings.Contains(st.LastRemoteAction, RemoteActionSub2APIStatusInactive) &&
		!strings.Contains(st.LastRemoteAction, RemoteActionSub2APIModelsUpdated) {
		t.Fatalf("expected status inactive and/or models updated, got %q", st.LastRemoteAction)
	}
	if len(repo.events) != 1 {
		t.Fatalf("expected 1 event, got %+v", repo.events)
	}
	if bulkBody == nil {
		t.Fatalf("expected a real bulk update request to the sub2api admin accounts API")
	}
	accountIDs, ok := bulkBody["account_ids"].([]any)
	if !ok || len(accountIDs) != 1 || accountIDs[0] != float64(1515) {
		t.Fatalf("expected account_ids=[1515], got %+v", bulkBody["account_ids"])
	}
}

// TestProbeTargetOnce_Sub2APIRemoteActionDisabledStillUpdatesModelLimits 验证
// AutoRemoteActionEnabled=false 时：不改账号 status，但仍摘除暂停模型的模型限制。
func TestProbeTargetOnce_Sub2APIRemoteActionDisabledStillUpdatesModelLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	repo := newFakeRepository()
	repo.policies = []Policy{sub2APIProbePolicy(false)}
	platform := &fakePlatformActioner{}
	mySites := fakeMySitesReader{session: upstream.Session{Platform: upstream.PlatformSub2API}}
	reader := fakePlatformGroupReader{
		groups:        []upstream.AdminGroupInfo{{ID: "g1", Name: "vip"}},
		accountsByGrp: map[string][]upstream.AdminGroupAccountInfo{"g1": {{ID: "acc-1", Name: "acc", Models: "gpt-4o"}}},
		credByAccount: map[string]upstream.ProbeCredential{"acc-1": {BaseURL: server.URL, Key: "k"}},
	}
	svc := newAdminTargetsRemoteActionService(reader, mySites, repo, platform)

	targetID := "sub2api:ws1:acc-1"
	results, err := svc.ProbeTarget(context.Background(), "user1", targetID, []string{"gpt-4o"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].State != StateSuspended {
		t.Fatalf("expected hard failure to suspend, got %+v", results)
	}
	if len(platform.sub2APICalls) != 0 {
		t.Fatalf("expected no account status call when AutoRemoteActionEnabled=false, got %+v", platform.sub2APICalls)
	}
	if len(platform.sub2APIModelCalls) != 1 {
		t.Fatalf("expected model limits update even when remote status action disabled, got %+v", platform.sub2APIModelCalls)
	}
	st := repo.states[targetID]["gpt-4o"]
	if !strings.Contains(st.LastRemoteAction, RemoteActionSub2APIModelsUpdated) {
		t.Fatalf("expected LastRemoteAction to include models update, got %q", st.LastRemoteAction)
	}
}
