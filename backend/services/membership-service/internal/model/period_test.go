package model

import (
	"testing"
	"time"
)

// 这一组测试钉住 AddPeriod 的**日历语义**。
//
// 它是这个包里唯一一个不是「列与字段一一对应」的东西，也是最容易被人「顺手换成
// time.AddDate」的一处——那个换法在绝大多数日期上看不出区别，只在 29/30/31 号上多算几天，
// 而那几天正是用户会来投诉的几天（「我明明买的是 1 月 31 号到期」）。

// mustParse 读一个 RFC3339 时刻，读不动就直接失败。
//
// 用具体的时刻而不是 time.Date(...) 拼：测试里的日期差异只有肉眼能看出来（1 月 31 日 vs
// 3 月 3 日），写成数字串更容易核对。
func mustParse(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("测试写错了，%q 不是 RFC3339：%v", value, err)
	}
	return parsed
}

// TestAddPeriodClampsToTheEndOfTheMonth 是本文件的核心：日号超出目标月天数时截到最后一天。
//
// 每一条的右边如果用 time.AddDate 算，都会是别的答案——注释里写出了那个答案，因为它正是
// 「为什么不能用 AddDate」的现场证据。
func TestAddPeriodClampsToTheEndOfTheMonth(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		period string
		count  int32
		want   string
	}{
		// 1 月 31 日 + 1 个月：2 月没有 31 号，取 2 月 28 日。
		// AddDate 会算成 3 月 3 日（2025 年不是闰年，2 月 31 日规整成 3 月 3 日）。
		{"月末加一个月", "2025-01-31T10:00:00+08:00", PeriodMonth, 1, "2025-02-28T10:00:00+08:00"},
		// 闰年：同一天应该是 29 号。
		{"月末加一个月（闰年二月）", "2024-01-31T10:00:00+08:00", PeriodMonth, 1, "2024-02-29T10:00:00+08:00"},
		// 30 天的月份：5 月 31 日 + 1 个月 = 6 月 30 日（AddDate 给 7 月 1 日）。
		{"小月截断", "2025-05-31T08:30:00+08:00", PeriodMonth, 1, "2025-06-30T08:30:00+08:00"},
		// 12 月 31 日 + 1 个月要跨年，而且是完整的一个月（1 月有 31 天，不截断）。
		{"跨年不截断", "2025-12-31T23:59:59+08:00", PeriodMonth, 1, "2026-01-31T23:59:59+08:00"},
		// 加 12 个月 = 一年，日号照旧。
		{"加十二个月等于一年", "2025-03-15T00:00:00Z", PeriodMonth, 12, "2026-03-15T00:00:00Z"},
		// 2 月 29 日（闰年）+ 1 年 = 2 月 28 日（平年）。这一条尤其重要：微信代扣签约日就是
		// 2 月 29 日的人，一年后必须落在 28 日，不能溢出到 3 月 1 日。
		{"闰日加一年", "2024-02-29T12:00:00+08:00", PeriodYear, 1, "2025-02-28T12:00:00+08:00"},
		{"闰日加四年又回到闰日", "2024-02-29T12:00:00+08:00", PeriodYear, 4, "2028-02-29T12:00:00+08:00"},
		{"整年不截断", "2025-07-01T09:00:00Z", PeriodYear, 1, "2026-07-01T09:00:00Z"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AddPeriod(mustParse(t, tc.base), tc.period, tc.count)
			want := mustParse(t, tc.want)
			if !got.Equal(want) {
				t.Fatalf("AddPeriod(%s, %s, %d) = %s，期望 %s",
					tc.base, tc.period, tc.count, got.Format(time.RFC3339), tc.want)
			}
		})
	}
}

// TestAddPeriodKeepsTheClockAndTheZone 确认时分秒与时区原样保留。
//
// 它们在库上都是 TIMESTAMPTZ，落库时会被转成 UTC 存起来；这个测试盯的是**返回值**那一层：
// 如果实现里用 time.Date 时漏了 Location 参数，返回的值会悄悄变成 UTC——落库的值一样，
// 但所有拿它做本地时间显示的路径都会差八小时，而那种偏差在小程序上看起来只是「到期日不对」。
func TestAddPeriodKeepsTheClockAndTheZone(t *testing.T) {
	shanghai := time.FixedZone("CST", 8*3600)
	base := time.Date(2025, 1, 31, 7, 45, 30, 123456789, shanghai)

	got := AddPeriod(base, PeriodMonth, 1)

	if got.Location() != shanghai {
		t.Fatalf("时区变成了 %v，期望 %v", got.Location(), shanghai)
	}
	hour, minute, second := got.Clock()
	if hour != 7 || minute != 45 || second != 30 {
		t.Fatalf("时分秒变成了 %02d:%02d:%02d，期望 07:45:30", hour, minute, second)
	}
	if got.Nanosecond() != 123456789 {
		t.Fatalf("纳秒丢了：%d", got.Nanosecond())
	}
}

// TestAddPeriodRefusesNonPositiveCount 确认 count <= 0 时原样返回。
//
// 它挡的是一条**会在生产里出现**的路径：period_count 列上虽然有 CHECK (> 0)，但事件里的
// 那个值是订单行的快照，一个零值经过一次 JSON 往返就能到这儿。返回 base 的含义是「这次
// 续期不改变到期时间」——比往后推一个奇怪的天数要好，而它也不会悄悄多发或少发权益。
func TestAddPeriodRefusesNonPositiveCount(t *testing.T) {
	base := mustParse(t, "2025-06-15T00:00:00Z")

	for _, count := range []int32{0, -1} {
		if got := AddPeriod(base, PeriodMonth, count); !got.Equal(base) {
			t.Fatalf("count=%d 时返回了 %s，期望原样返回 %s",
				count, got.Format(time.RFC3339), base.Format(time.RFC3339))
		}
	}
}

// TestAddPeriodTreatsUnknownPeriodAsMonth 确认未知的 period 按 month 处理，而不是 panic。
//
// 见 AddPeriod 的说明：period 列上有 CHECK 挡着，走到这里只可能是老数据或迁移途中。让它按
// 最短周期生效，比让整个事件消费停住要好——后者会让所有买会员的人全都收不到权益。
func TestAddPeriodTreatsUnknownPeriodAsMonth(t *testing.T) {
	base := mustParse(t, "2025-01-31T00:00:00Z")

	got := AddPeriod(base, "", 1)
	want := mustParse(t, "2025-02-28T00:00:00Z")
	if !got.Equal(want) {
		t.Fatalf("空 period 得到 %s，期望按 month 处理得到 %s",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
