package upstream

import "time"

// BusinessTimezone 是成本、消费和仪表盘日切统一使用的业务时区。
// 容器默认 UTC 时，若按本地午夜切日，会把中国用户的 00:00-08:00 算错到前一天或漏计。
const BusinessTimezone = "Asia/Shanghai"

// BusinessLocation 返回业务时区；加载失败时回退到固定 UTC+8，避免日切退回容器本地时区。
func BusinessLocation() *time.Location {
	loc, err := time.LoadLocation(BusinessTimezone)
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// BusinessDate 返回 now 在业务时区下的日历日（2006-01-02）。
func BusinessDate(now time.Time) string {
	return now.In(BusinessLocation()).Format("2006-01-02")
}

// BusinessToday 返回当前业务日字符串。
func BusinessToday() string {
	return BusinessDate(time.Now())
}

// BusinessDayStart 返回业务时区下“今天 00:00:00”的 Unix 秒。
func BusinessDayStart() int64 {
	return businessDayBounds(time.Now()).start.Unix()
}

// BusinessDayEnd 返回业务时区下“今天 23:59:59”的 Unix 秒。
func BusinessDayEnd() int64 {
	return businessDayBounds(time.Now()).end.Unix()
}

// BusinessDayRange 把业务日历日 [startDate, endDate]（含首尾）转为 Unix 秒区间。
// 日期必须是 2006-01-02；解析失败返回错误，避免静默落到 UTC。
func BusinessDayRange(startDate, endDate string) (startTS int64, endTS int64, err error) {
	loc := BusinessLocation()
	start, err := time.ParseInLocation("2006-01-02", startDate, loc)
	if err != nil {
		return 0, 0, err
	}
	end, err := time.ParseInLocation("2006-01-02", endDate, loc)
	if err != nil {
		return 0, 0, err
	}
	startTS = start.Unix()
	// new-api 的 end_timestamp 按闭区间秒处理，取当天最后一秒。
	endTS = end.Add(24*time.Hour - time.Second).Unix()
	return startTS, endTS, nil
}

type dayBounds struct {
	start time.Time
	end   time.Time
}

func businessDayBounds(now time.Time) dayBounds {
	loc := BusinessLocation()
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	end := start.Add(24*time.Hour - time.Second)
	return dayBounds{start: start, end: end}
}
