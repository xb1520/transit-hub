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
		{ModelName: "a", State: StateHealthy},
		{ModelName: "b", State: StateObserving, LastErrorKey: ""}, // 已探测成功进入观察，应保留
		// 探活暂停（无论原因）都应摘除
		{ModelName: "c", State: StateSuspended, LastErrorKey: string(ResultNetworkFluctuation)},
	}
	got := desiredModelLimits(original, states, nil)
	want := "a,b"
	if normalizeModelListString(got) != normalizeModelListString(want) {
		t.Fatalf("suspended models must be excluded, got %q want %q", got, want)
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
	}
	allHealthy, blocked, _ := aggregateTargetStates(states)
	if allHealthy {
		t.Fatal("expected not all healthy")
	}
	if blocked {
		t.Fatal("partial model_not_found exclusion must not block whole account")
	}
}

func TestAggregateTargetStates_AllExcludedBlocks(t *testing.T) {
	states := []ConnectionHealthState{
		{ModelName: "a", State: StateSuspended, CurrentWeight: 0, LastErrorKey: string(ResultModelNotFound)},
		{ModelName: "b", State: StateSuspended, CurrentWeight: 0, LastErrorKey: string(ResultServerError)},
	}
	_, blocked, _ := aggregateTargetStates(states)
	if !blocked {
		t.Fatal("all models excluded must still block account")
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

	// 2) 恢复：模拟上游当前已是上次写入，探活成功
	target.Models = []string{"gpt-4o"}
	platform.sub2APIModelCalls = nil
	states[1] = ConnectionHealthState{ModelName: "gpt-5.6-luna", State: StateObserving, CurrentWeight: 0}
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
