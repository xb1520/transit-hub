package upstream

import (
	"encoding/json"
	"math"
	"testing"
)

func TestParseSub2APIAccountsTodayStats_MapByID(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"50": map[string]any{"actual_cost": 116.9, "requests": 10},
			"36": map[string]any{"today_actual_cost": 25.5},
		},
	}
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	out := ParseSub2APIAccountsTodayStatsForTest(decoded)
	if math.Abs(out["50"].ActualCost-116.9) > 1e-9 {
		t.Fatalf("50 = %+v", out["50"])
	}
	if math.Abs(out["36"].ActualCost-25.5) > 1e-9 {
		t.Fatalf("36 = %+v", out["36"])
	}
}

func TestParseSub2APIAccountsTodayStats_NestedToday(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"today": map[string]any{"user_cost": 12.3, "account_cost": 9.0},
			"id":    50,
		},
	}
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	out := ParseSub2APIAccountsTodayStatsForTest(decoded)
	// 营收取 user_cost，不是 account_cost
	found := false
	for _, v := range out {
		if math.Abs(v.ActualCost-12.3) < 1e-9 {
			found = true
			if math.Abs(v.Cost-9.0) > 1e-9 {
				t.Fatalf("cost should be account_cost 9, got %+v", v)
			}
		}
	}
	if !found {
		t.Fatalf("expected nested user_cost revenue, got %+v", out)
	}
}

// TestParseSub2APIAccountsTodayStats_UserCostPreferred 营收=用户侧总消费，不是上游成本。
// 对齐管理站：总消费 $121.99 / 成本 $91.93。
func TestParseSub2APIAccountsTodayStats_UserCostPreferred(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"50": map[string]any{
				"actual_cost":  91.93, // 易与成本混淆的字段
				"account_cost": 91.93,
				"user_cost":    121.99,
			},
		},
	}
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	out := ParseSub2APIAccountsTodayStatsForTest(decoded)
	s := out["50"]
	if math.Abs(s.ActualCost-121.99) > 1e-9 {
		t.Fatalf("ActualCost (revenue) = %v, want 121.99 user_cost", s.ActualCost)
	}
	if math.Abs(s.Cost-91.93) > 1e-9 {
		t.Fatalf("Cost = %v, want 91.93 account_cost", s.Cost)
	}
}

// TestParseSub2APIAccountsTodayStats_ActualVsCostTakesLargerAsRevenue
// 仅有 actual_cost + cost 且 actual < cost 时：较大为营收（兼容字段颠倒）。
func TestParseSub2APIAccountsTodayStats_ActualVsCostTakesLargerAsRevenue(t *testing.T) {
	payload := map[string]any{
		"data": map[string]any{
			"actual_cost": 91.93,
			"cost":        121.99,
			"id":          "50",
		},
	}
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	out := ParseSub2APIAccountsTodayStatsForTest(decoded)
	s, ok := out["50"]
	if !ok {
		// may be under "_"
		for _, v := range out {
			s = v
			ok = true
			break
		}
	}
	if !ok {
		t.Fatal("empty parse")
	}
	if math.Abs(s.ActualCost-121.99) > 1e-9 {
		t.Fatalf("ActualCost = %v, want 121.99 (larger)", s.ActualCost)
	}
}
