package connection_health

import (
	"context"
	"testing"

	"transithub/backend/internal/modules/upstream"
)

func TestDesiredModelLimits_RemovesExcludedModels(t *testing.T) {
	original := "gpt-4o,gpt-5.6-luna,gpt-5.5"
	states := []ConnectionHealthState{
		{ModelName: "gpt-4o", State: StateHealthy},
		{ModelName: "gpt-5.6-luna", State: StateSuspended, LastErrorKey: string(ResultModelNotFound)},
		{ModelName: "gpt-5.5", State: StateSuspended, LastErrorKey: string(ResultServerError)},
	}
	got := desiredModelLimits(original, states, nil)
	want := "gpt-4o"
	if normalizeModelListString(got) != normalizeModelListString(want) {
		t.Fatalf("desiredModelLimits = %q, want %q", got, want)
	}
}

func TestDesiredModelLimits_AnySuspendedIsExcluded(t *testing.T) {
	original := "a,b,c"
	states := []ConnectionHealthState{
		{ModelName: "a", State: StateHealthy, CurrentWeight: 100},
		// 恢复观察：只摘模型，不保留在白名单
		{ModelName: "b", State: StateObserving, CurrentWeight: 0},
		// 探活暂停（无论原因）都应摘除
		{ModelName: "c", State: StateSuspended, LastErrorKey: string(ResultNetworkFluctuation), CurrentWeight: 0},
	}
	got := desiredModelLimits(original, states, nil)
	want := "a"
	if normalizeModelListString(got) != normalizeModelListString(want) {
		t.Fatalf("observing/suspended models must be excluded, got %q want %q", got, want)
	}
}

func TestDesiredModelLimits_ExcludesDisabledAndZeroWeight(t *testing.T) {
	original := "ok,disabled-one,zero-weight,recovering"
	states := []ConnectionHealthState{
		{ModelName: "ok", State: StateHealthy, CurrentWeight: 100},
		{ModelName: "disabled-one", State: StateDisabled, CurrentWeight: 0},
		{ModelName: "zero-weight", State: StateDegraded, CurrentWeight: 0},
		{ModelName: "recovering", State: StateRecovering, CurrentWeight: 25},
	}
	got := desiredModelLimits(original, states, nil)
	want := "ok,recovering"
	if normalizeModelListString(got) != normalizeModelListString(want) {
		t.Fatalf("desiredModelLimits = %q, want %q", got, want)
	}
}

func TestDesiredModelLimits_UnrestrictedBuildsAllowlist(t *testing.T) {
	// 原本不限制：有暂停时写入白名单；无暂停时保持空
	states := []ConnectionHealthState{
		{ModelName: "ok", State: StateHealthy},
		{ModelName: "bad", State: StateSuspended, LastErrorKey: string(ResultInvalidResponse)},
	}
	got := desiredModelLimits("", states, []string{"ok", "bad", "extra"})
	if normalizeModelListString(got) != normalizeModelListString("ok,extra") {
		t.Fatalf("unrestricted allowlist = %q", got)
	}
	if desiredModelLimits("", []ConnectionHealthState{{ModelName: "ok", State: StateHealthy}}, nil) != "" {
		t.Fatal("unrestricted with no suspension must stay empty")
	}
}

func TestAggregateTargetStates_PartialModelExclusionDoesNotBlock(t *testing.T) {
	states := []ConnectionHealthState{
		{ModelName: "ok", State: StateHealthy, CurrentWeight: 100},
		{ModelName: "bad", State: StateSuspended, CurrentWeight: 0, LastErrorKey: string(ResultModelNotFound)},
		{ModelName: "watching", State: StateObserving, CurrentWeight: 0},
		{ModelName: "disabled", State: StateDisabled, CurrentWeight: 0},
		{ModelName: "zero", State: StateDegraded, CurrentWeight: 0},
	}
	allHealthy, blocked, minWeight := aggregateTargetStates(states)
	// 问题模型只摘除：有效模型全部健康时应可保持/恢复账号调度
	if !allHealthy {
		t.Fatal("expected allHealthy when only excluded models are unhealthy")
	}
	if blocked {
		t.Fatal("partial model exclusion must not block whole account")
	}
	if minWeight != 100 {
		t.Fatalf("minWeight should ignore excluded models, got %d", minWeight)
	}
}

func TestAggregateTargetStates_AllExcludedBlocks(t *testing.T) {
	states := []ConnectionHealthState{
		{ModelName: "a", State: StateSuspended, CurrentWeight: 0, LastErrorKey: string(ResultModelNotFound)},
		{ModelName: "b", State: StateObserving, CurrentWeight: 0},
		{ModelName: "c", State: StateDisabled, CurrentWeight: 0},
	}
	_, blocked, _ := aggregateTargetStates(states)
	if !blocked {
		t.Fatal("all models excluded must still block account")
	}
}

func TestDesiredTargetState_RestoresActiveWhenHealthyPeers(t *testing.T) {
	stored := TargetActionState{OriginalStatus: "inactive", LastAppliedStatus: "inactive"}
	// 有健康有效模型且未阻塞：即使 Original 误记为 inactive，也应恢复 active
	status, _ := desiredTargetState(string(upstream.PlatformSub2API), true, false, true, 100, stored)
	if status != "active" {
		t.Fatalf("desired status = %q, want active", status)
	}
	// Original 为 active 时优先恢复 Original
	stored.OriginalStatus = "active"
	status, _ = desiredTargetState(string(upstream.PlatformSub2API), true, false, true, 100, stored)
	if status != "active" {
		t.Fatalf("desired status = %q, want active from original", status)
	}
	// 阻塞时仍 inactive
	status, _ = desiredTargetState(string(upstream.PlatformSub2API), false, true, false, 0, stored)
	if status != "inactive" {
		t.Fatalf("blocked desired = %q, want inactive", status)
	}
}

func TestReconcileTargetModelLimits_RemovesAndRestores(t *testing.T) {
	platform := &fakePlatformActioner{}
	repo := newFakeRepository()
	svc := &Service{
		repo:       repo,
		dispatcher: newRemoteActionDispatcher(nil, nil, platform),
	}
	target := AdminProbeTarget{
		TargetID:  "sub2api:ws1:10",
		Platform:  string(upstream.PlatformSub2API),
		AccountID: "10",
		Models:    []string{"gpt-4o", "gpt-5.6-luna"},
	}
	session := upstream.Session{Platform: upstream.PlatformSub2API}
	stored := &TargetActionState{
		UserID: "user1", AdminAccountID: "ws1", TargetID: target.TargetID,
		OriginalStatus: "active", LastAppliedStatus: "active",
	}

	// 1) 摘除不存在模型
	states := []ConnectionHealthState{
		{ModelName: "gpt-4o", State: StateHealthy, CurrentWeight: 100},
		{ModelName: "gpt-5.6-luna", State: StateSuspended, CurrentWeight: 0, LastErrorKey: string(ResultModelNotFound)},
	}
	action, err := svc.reconcileTargetModelLimits(context.Background(), session, target, states, stored)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != RemoteActionSub2APIModelsUpdated {
		t.Fatalf("action = %q", action)
	}
	if len(platform.sub2APIModelCalls) != 1 || platform.sub2APIModelCalls[0].models != "gpt-4o" {
		t.Fatalf("expected models=gpt-4o write, got %+v", platform.sub2APIModelCalls)
	}
	if stored.OriginalModels != "gpt-4o,gpt-5.6-luna" {
		t.Fatalf("original models = %q", stored.OriginalModels)
	}
	if stored.LastAppliedModels != "gpt-4o" {
		t.Fatalf("last applied = %q", stored.LastAppliedModels)
	}

	// 2) 观察中仍保持摘除（不因进入 observing 就写回）
	target.Models = []string{"gpt-4o"}
	platform.sub2APIModelCalls = nil
	states[1] = ConnectionHealthState{ModelName: "gpt-5.6-luna", State: StateObserving, CurrentWeight: 0}
	action, err = svc.reconcileTargetModelLimits(context.Background(), session, target, states, stored)
	if err != nil {
		t.Fatalf("observing step error: %v", err)
	}
	if action != "" || len(platform.sub2APIModelCalls) != 0 {
		t.Fatalf("observing must keep model excluded, action=%q calls=%+v", action, platform.sub2APIModelCalls)
	}

	// 3) 恢复：探活到 healthy 后才写回完整模型列表
	states[1] = ConnectionHealthState{ModelName: "gpt-5.6-luna", State: StateHealthy, CurrentWeight: 100}
	action, err = svc.reconcileTargetModelLimits(context.Background(), session, target, states, stored)
	if err != nil {
		t.Fatalf("restore error: %v", err)
	}
	if action != RemoteActionSub2APIModelsUpdated {
		t.Fatalf("restore action = %q", action)
	}
	if len(platform.sub2APIModelCalls) != 1 || normalizeModelListString(platform.sub2APIModelCalls[0].models) != normalizeModelListString("gpt-4o,gpt-5.6-luna") {
		t.Fatalf("expected full models restore, got %+v", platform.sub2APIModelCalls)
	}
}

func TestExpandTargetModelsForProbe_UsesOriginal(t *testing.T) {
	target := AdminProbeTarget{Models: []string{"gpt-4o"}}
	stored := &TargetActionState{OriginalModels: "gpt-4o,gpt-5.6-luna"}
	expanded := expandTargetModelsForProbe(target, stored)
	if normalizeModelListString(joinModelList(expanded.Models)) != normalizeModelListString("gpt-4o,gpt-5.6-luna") {
		t.Fatalf("expanded models = %v", expanded.Models)
	}
}
