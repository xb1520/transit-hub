package upstream

import (
	"testing"
	"time"
)

func TestBusinessDayRange_UsesShanghaiNotUTC(t *testing.T) {
	startTS, endTS, err := BusinessDayRange("2026-08-02", "2026-08-02")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2026-08-02 00:00:00 CST = 2026-08-01 16:00:00 UTC
	wantStart := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC).Unix()
	// 2026-08-02 23:59:59 CST = 2026-08-02 15:59:59 UTC
	wantEnd := time.Date(2026, 8, 2, 15, 59, 59, 0, time.UTC).Unix()
	if startTS != wantStart || endTS != wantEnd {
		t.Fatalf("range = [%d, %d], want [%d, %d]", startTS, endTS, wantStart, wantEnd)
	}
}

func TestBusinessDate_CrossesUTCMidnight(t *testing.T) {
	// UTC 2026-08-01 20:00 = 上海 2026-08-02 04:00，业务日应为 8/2。
	utcEvening := time.Date(2026, 8, 1, 20, 0, 0, 0, time.UTC)
	if got := BusinessDate(utcEvening); got != "2026-08-02" {
		t.Fatalf("BusinessDate(%v) = %q, want 2026-08-02", utcEvening, got)
	}
	// UTC 2026-08-02 01:00 = 上海 2026-08-02 09:00，仍为 8/2。
	utcMorning := time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC)
	if got := BusinessDate(utcMorning); got != "2026-08-02" {
		t.Fatalf("BusinessDate(%v) = %q, want 2026-08-02", utcMorning, got)
	}
}

func TestBusinessDayStartEnd_MatchShanghaiCalendar(t *testing.T) {
	// 直接验证 bounds 函数对固定时刻的结果。
	now := time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC) // 上海 11:00
	bounds := businessDayBounds(now)
	if bounds.start.Unix() != time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("unexpected day start: %v", bounds.start.UTC())
	}
	if bounds.end.Unix() != time.Date(2026, 8, 2, 15, 59, 59, 0, time.UTC).Unix() {
		t.Fatalf("unexpected day end: %v", bounds.end.UTC())
	}
}
