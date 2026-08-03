package upstream

import "testing"

func TestSumFilteredBalances_Dual口径(t *testing.T) {
	users := []any{
		map[string]any{"id": "1", "role": "user", "balance": 100.0},
		map[string]any{"id": "2", "role": "user", "balance": 50.0},
		map[string]any{"id": "3", "role": "admin", "balance": 999.0},
		map[string]any{"id": "4", "role": "user", "balance": 20.0},
	}
	got := sumFilteredBalances(users, BalanceFilter{
		ExcludeAdmin:    true,
		ExcludeUserIDs:  []string{"4"},
		UserGiftAmounts: map[string]float64{"1": 30},
	})
	// cost: 100+50 = 150 (admin and user4 excluded)
	if got.CostBalance != 150 || got.Balance != 150 {
		t.Fatalf("cost = %+v, want 150", got)
	}
	// revenue: (100-30)+50 = 120
	if got.RevenueBalance != 120 {
		t.Fatalf("revenue = %v, want 120", got.RevenueBalance)
	}
}

func TestSiteSettings_CountsAsPrepaidReserve(t *testing.T) {
	prepaid := SiteSettings{SettlementMode: SettlementModePrepaidWallet}
	credit := SiteSettings{SettlementMode: SettlementModeCreditLine}
	if !prepaid.CountsAsPrepaidReserve() {
		t.Fatal("prepaid should count as reserve")
	}
	if credit.CountsAsPrepaidReserve() {
		t.Fatal("credit line must not inflate prepaid reserve")
	}
}

func TestMinPositiveRemaining_SubscriptionTodayMax(t *testing.T) {
	// 日剩余 298.9，周 243.2，月 243.2 → 今日最多 243.2
	daily := 298.93
	weekly := 243.24
	monthly := 243.24
	got := minPositiveRemaining(&daily, &weekly, &monthly)
	if got == nil || *got != weekly {
		t.Fatalf("today max = %v, want %v", got, weekly)
	}
	if minPositiveRemaining(nil, nil) != nil {
		t.Fatal("all nil should return nil")
	}
}
