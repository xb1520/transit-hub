package connection_health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"transithub/backend/internal/modules/my_sites"
	"transithub/backend/internal/modules/upstream"
)

func TestProbeOnce_StopsRealProbingAfterDailyBudgetExhausted(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()

	repo := newFakeRepository()
	sites := fakeSiteLookup{site: &upstream.Site{ID: "site-1", BaseURL: server.URL, Platform: upstream.PlatformNewAPI}}
	svc := &Service{repo: repo, sites: sites, dispatcher: noopRemoteActionRunner{}, probeRunner: NewRealProbeRunner()}

	conn := my_sites.RealConnection{ID: "conn-1", UpstreamSiteID: "site-1", UpstreamKey: "key-1", UserID: "user1", WorkspaceAdminAccountID: "ws1"}
	policy := Policy{ID: "policy-1", UserID: "user1", AdminAccountID: "ws1", DailyProbeBudget: 1, RecoveryStepPercent: 25, FailureThreshold: 3, SuccessThreshold: 2, CooldownSeconds: 300, ObservationSeconds: 300, AutoDegradeEnabled: true}
	target := ModelTarget{ModelName: "gpt-4o-mini", ProviderFamily: ProviderOpenAI, MaxProbeTokens: 1}

	if _, err := svc.probeOnce(context.Background(), conn, policy, target); err != nil {
		t.Fatalf("unexpected error on first probe: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 real request after first probe, got %d", hits)
	}

	if _, err := svc.probeOnce(context.Background(), conn, policy, target); err != nil {
		t.Fatalf("unexpected error on second probe: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected daily budget to block the second real probe request, got %d hits", hits)
	}

	secondPolicy := policy
	secondPolicy.ID = "policy-2"
	if _, err := svc.probeOnce(context.Background(), conn, secondPolicy, target); err != nil {
		t.Fatalf("unexpected error for independent policy budget: %v", err)
	}
	if hits != 2 {
		t.Fatalf("one policy must not consume another policy's budget, got %d hits", hits)
	}
}

func TestComputeProbeCost_UsesUsageTokensAndGroupRatio(t *testing.T) {
	// 500 tokens / 500000 * 1.3 group = 0.0013 USD；recharge 7 → 0.0091 CNY
	cost := computeProbeCost(ProbeOutcome{TotalTokens: 500}, 1, 1.3, 7)
	if cost.USD < 0.00129 || cost.USD > 0.00131 {
		t.Fatalf("usd = %v, want ~0.0013", cost.USD)
	}
	if cost.CNY < 0.0090 || cost.CNY > 0.0092 {
		t.Fatalf("cny = %v, want ~0.0091", cost.CNY)
	}
}

func TestComputeProbeCost_PrefersActualCost(t *testing.T) {
	cost := computeProbeCost(ProbeOutcome{TotalTokens: 500, ActualCostUSD: 0.05}, 1, 2, 7)
	if cost.USD < 0.049 || cost.USD > 0.051 {
		t.Fatalf("usd = %v, want actual 0.05", cost.USD)
	}
	if cost.CNY < 0.34 || cost.CNY > 0.36 {
		t.Fatalf("cny = %v, want ~0.35", cost.CNY)
	}
}

func TestComputeProbeCost_FallsBackToMaxTokens(t *testing.T) {
	// tokens = 4+16 = 20 → 20/500000 * 1 = 0.00004 USD
	cost := computeProbeCost(ProbeOutcome{}, 4, 1, 1)
	if cost.USD < 0.000039 || cost.USD > 0.000041 {
		t.Fatalf("usd = %v, want ~0.00004", cost.USD)
	}
	if cost.CNY != cost.USD {
		t.Fatalf("cny = %v, want equal usd when rate=1", cost.CNY)
	}
}

func TestParseProbeUsageTokens(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"actual_cost":0.012}}`)
	p, c, total, actual := parseProbeUsage(body)
	if p != 10 || c != 2 || total != 12 {
		t.Fatalf("got prompt=%d completion=%d total=%d", p, c, total)
	}
	if actual < 0.011 || actual > 0.013 {
		t.Fatalf("actual cost = %v, want ~0.012", actual)
	}
}
