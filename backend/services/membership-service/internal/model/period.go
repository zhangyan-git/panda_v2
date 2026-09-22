package model

import "time"

// AddPeriod 按日历把 base 往后推 count 个 period（month / year）。
//
// 这是本包唯一一个不是「列与字段一一对应」的东西，写在这里是因为**它是库结构本身的语言**：
// migrations/membership/001 把时长存成 period + period_count 而不是 duration_days，理由就是
// 续期要按日历加；那么「按日历加」这个动作的定义，与那两列是同一件事的两半，分开迟早会漂。
//
// # 为什么不能用 time.AddDate
//
// AddDate 对「不存在的日期」是**向后溢出**的：1 月 31 日加一个月，它会先算成 2 月 31 日，
// 再规整成 3 月 3 日（2024 年闰年）。用户看到的是会员莫名其妙多算了三天，续几年就漂几天。
// 微信代扣是按「每月 31 号扣」签约的，扣款日与到期日就这么对不上了。
//
// 所以这里做的是**截断到目标月的最后一天**：1 月 31 日 + 1 个月 = 2 月 28 日（闰年 29 日），
// 12 月 31 日 + 1 个月 = 明年 1 月 31 日。这与微信、与老系统的行为一致。
//
// 时分秒原样保留、时区原样保留：会员在几点几分开的，到期就在几点几分。
// count 取 int32：它从 period_count 列来，而那一列是 INTEGER。让调用方在每一处写 int(...)
// 只会多出两个没人看得懂的类型转换，而它们谁也拦不住真正的越界（那由 CHECK (period_count > 0)
// 兜着）。
func AddPeriod(base time.Time, period string, count int32) time.Time {
	if count <= 0 {
		return base
	}
	n := int(count)
	switch period {
	case PeriodYear:
		return clampDay(base, base.Year()+n, int(base.Month()))
	default:
		// 未知取值（含空串）按 month 处理。**不 panic**：period 列上有 CHECK 挡着，走到这里
		// 只可能是老数据或迁移途中，而那时候让一次续费按最短周期生效，比让整个事件消费停住
		// 要好——后者会让所有买会员的人全都收不到权益。
		total := int(base.Month()) - 1 + n
		return clampDay(base, base.Year()+total/12, total%12+1)
	}
}

// clampDay 拼出「同一天的同一时刻」，日号超出目标月天数时取该月最后一天。
func clampDay(base time.Time, year, month int) time.Time {
	day := base.Day()
	if last := daysIn(year, time.Month(month)); day > last {
		day = last
	}
	return time.Date(year, time.Month(month), day,
		base.Hour(), base.Minute(), base.Second(), base.Nanosecond(), base.Location())
}

// daysIn 返回某年某月的天数。用「下个月的第 0 天」这个技巧算——它比手写闰年判断可靠，
// 而且是 time 包自己认可的口径。
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
