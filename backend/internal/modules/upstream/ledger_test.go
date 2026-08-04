package upstream

import (
	"testing"
	"time"
)

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

func TestBusinessDayWindow(t *testing.T) {
	start, end, err := businessDayWindow("2026-08-04")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if start.In(BusinessLocation()).Format("2006-01-02 15:04:05") != "2026-08-04 00:00:00" {
		t.Fatalf("start = %v", start)
	}
	if end.Sub(start) != 24*time.Hour {
		t.Fatalf("window length = %v, want 24h", end.Sub(start))
	}
	// 半开区间：当日 19:28 计入，次日 00:00 不计入
	mid := start.Add(19*time.Hour + 28*time.Minute)
	if !( !mid.Before(start) && mid.Before(end) ) {
		t.Fatalf("mid %v should be inside window [%v, %v)", mid, start, end)
	}
	if _, _, err := businessDayWindow("not-a-date"); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestIsImplausibleBusinessDate(t *testing.T) {
	if !isImplausibleBusinessDate("1970-01-01") {
		t.Fatal("epoch date should be implausible")
	}
	if !isImplausibleBusinessDate("1999-12-31") {
		t.Fatal("pre-2000 should be implausible")
	}
	if !isImplausibleBusinessDate("") {
		t.Fatal("empty should be implausible")
	}
	if isImplausibleBusinessDate("2026-08-04") {
		t.Fatal("normal business date should be plausible")
	}
}

func TestShouldRepairAutoBusinessDate(t *testing.T) {
	autoEpoch := LedgerRecord{Source: LedgerSourceAuto, Status: LedgerStatusConfirmed, BusinessDate: "1970-01-01"}
	if !shouldRepairAutoBusinessDate(autoEpoch, "2026-08-04") {
		t.Fatal("auto epoch mark should be repaired to real date")
	}
	// 已是合理日期：不反复改写（避免覆盖用户/系统已确认的业务日）
	autoOK := LedgerRecord{Source: LedgerSourceAuto, Status: LedgerStatusConfirmed, BusinessDate: "2026-08-03"}
	if shouldRepairAutoBusinessDate(autoOK, "2026-08-04") {
		t.Fatal("plausible existing date must not be overwritten")
	}
	// 非 auto 源不修
	manual := LedgerRecord{Source: LedgerSourceManual, Status: LedgerStatusConfirmed, BusinessDate: "1970-01-01"}
	if shouldRepairAutoBusinessDate(manual, "2026-08-04") {
		t.Fatal("manual marks must not be repaired")
	}
	// 新日期也不合理则不修
	if shouldRepairAutoBusinessDate(autoEpoch, "1970-01-01") {
		t.Fatal("should not repair to another implausible date")
	}
	// 相同日期不修
	if shouldRepairAutoBusinessDate(autoEpoch, "1970-01-01") {
		t.Fatal("same date should not repair")
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
