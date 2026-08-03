package upstream

import "testing"

func TestHasAnyMetricValue(t *testing.T) {
	empty := Metrics{}
	if hasAnyMetricValue(empty) {
		t.Fatal("empty metrics should be false")
	}
	bal := 1.0
	withBal := Metrics{Balance: MetricValue{Value: &bal}}
	if !hasAnyMetricValue(withBal) {
		t.Fatal("balance set should be true")
	}
}

func TestMetricValueOr(t *testing.T) {
	if metricValueOr(MetricValue{}, 3) != 3 {
		t.Fatal("nil should fallback")
	}
	v := 2.5
	if metricValueOr(MetricValue{Value: &v}, 0) != 2.5 {
		t.Fatal("should return value")
	}
}

func TestProcessLedgerAfterSync_SkipWhenNoLedger(t *testing.T) {
	// 无 ledgerRepo 时不应 panic
	s := &Service{}
	s.ProcessLedgerAfterSync(nil, "u", "a", "s", Metrics{}, Metrics{})
}

func TestAlmostEqual(t *testing.T) {
	if !almostEqual(1.0, 1.0+1e-10, 1e-6) {
		t.Fatal("expected almost equal")
	}
	if almostEqual(1.0, 2.0, 0.01) {
		t.Fatal("expected not equal")
	}
}

func TestParseBusinessDateInput(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2026-03-01", "2026-03-01", true},
		{"2026-03-01T16:00:00Z", "2026-03-02", true}, // UTC 16:00 → 次日 Asia/Shanghai
		{"2026-03-01T00:00:00+08:00", "2026-03-01", true},
		{"", "", false},
		{"not-a-date", "", false},
	}
	for _, tc := range cases {
		got, ok := parseBusinessDateInput(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("in=%q: got (%q,%v) want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSubscriptionLedgerRef(t *testing.T) {
	got := subscriptionLedgerRef("42", "2026-03-01")
	if got != "sub:42:2026-03-01" {
		t.Fatalf("got %q", got)
	}
}

func TestSuggestRechargeTag_Sub2APITypes(t *testing.T) {
	amt := 10.0
	cases := []struct {
		typ, note, want string
	}{
		{"affiliate_balance", "invite", LedgerTagRebate},
		{"affiliate", "", LedgerTagRebate},
		{"balance", "redeem code", LedgerTagRecharge},
		{"admin_balance", "admin add", LedgerTagRecharge},
		{"payment", "alipay", LedgerTagRecharge},
		{"balance", "order", LedgerTagRecharge}, // 支付 order_type=balance
		{"subscription", "plan", LedgerTagRecharge},
		{"", "返利到账", LedgerTagRebate},
		{"admin_balance", "赠送活动", LedgerTagGift},
		{"gift", "promo", LedgerTagGift},
		{"admin_concurrency", "", ""},
		{"concurrency", "quota", ""},
	}
	for _, tc := range cases {
		got := suggestRechargeTag(Sub2APIBalanceHistoryItem{Type: tc.typ, Note: tc.note, Amount: &amt})
		if got != tc.want {
			t.Fatalf("type=%q note=%q: got %q want %q", tc.typ, tc.note, got, tc.want)
		}
	}
	if suggestRechargeTag(Sub2APIBalanceHistoryItem{Type: "balance"}) != "" {
		t.Fatal("nil amount should not tag")
	}
}

func TestSuggestHistoryTag_DebitDoesNotGuess(t *testing.T) {
	neg := -99999009.0
	// 裸管理员扣除：不自动归类（可能是充值退款也可能是赠送收回）
	if got := SuggestHistoryTag("admin_balance", "管理员调整", neg); got != "" {
		t.Fatalf("bare admin debit should not auto-tag, got %q", got)
	}
	if got := SuggestHistoryTag("admin_balance", "", neg); got != "" {
		t.Fatalf("empty note debit should not auto-tag, got %q", got)
	}
	// 强信号才建议
	if got := SuggestHistoryTag("admin_balance", "充值退款", neg); got != LedgerTagRecharge {
		t.Fatalf("refund note: got %q want recharge", got)
	}
	if got := SuggestHistoryTag("admin_balance", "收回赠送", neg); got != LedgerTagGift {
		t.Fatalf("gift clawback: got %q want gift", got)
	}
	if got := SuggestHistoryTag("admin_balance", "撤销返利", neg); got != LedgerTagRebate {
		t.Fatalf("rebate clawback: got %q want rebate", got)
	}
	if got := SuggestHistoryTag("refund", "user refund", neg); got != LedgerTagRecharge {
		t.Fatalf("refund type: got %q want recharge", got)
	}
}

func TestIsNonBalanceCreditHistoryType(t *testing.T) {
	if !IsNonBalanceCreditHistoryType("admin_concurrency", "") {
		t.Fatal("admin_concurrency should be non-balance")
	}
	if IsNonBalanceCreditHistoryType("admin_balance", "") {
		t.Fatal("admin_balance is balance credit")
	}
}
