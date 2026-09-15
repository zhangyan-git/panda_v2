package model

import "time"

// Draw 对应 lottery_draws 表：一次开奖。这张表只允许追加（触发器挡改和挡删）。
//
// Seed 是**派生值而不是随机数**：sha256(round_id || 首个参与 id || 末个参与 id ||
// 参与数 || trigger)。参与 id 是 v4 UUID，所以开奖前谁也预测不了；开奖后任何人都能拿
// lottery_draws + lottery_participations 把中奖名单完整重算一遍——这正是审计要的性质。
//
// **这不是可证明公平**：有本库读权限的运维在「最后一人参与到扫描之间」那个窗口里能预测
// 结果，commit–reveal 不在本轮范围。能保证的只有「记录的种子 + 记录的参与集合 = 记录的
// 中奖名单」，外加这张表改不动。
type Draw struct {
	ID         string `db:"id"`
	RoundID    string `db:"round_id"`
	CampaignID string `db:"campaign_id"`
	// auto / manual。与 Trigger 由 CHECK 绑成同一件事：manual ⇔ trigger='manual'。
	Mode string `db:"mode"`
	// threshold / deadline / manual，见 Round 那边的常量。
	Trigger string `db:"trigger"`
	// 算中奖名单用的算法标识。换算法时改这个值——已开出的奖因此仍然可以按当时的算法
	// 复盘，而不是被新算法重新解释一遍。
	Algorithm string `db:"algorithm"`
	// 十六进制小写，长度 64（sha256）。见上面关于「派生而非随机」的说明。
	Seed string `db:"seed"`
	// 开奖那一刻的合格参与数（= 参与记录里 status='confirmed' 的行数），**不是**读
	// rounds.participant_count：那个数是滚动维护的，开奖记录要的是快照。
	ParticipantCount int32 `db:"participant_count"`
	// 实际抽出的人数 = min(winner_count, participant_count)。
	WinnerCount int32 `db:"winner_count"`
	// 自动开奖没有操作人也没有理由；人工干预两者都必填（CHECK 挡）。
	DrawnBy   *string   `db:"drawn_by"`
	Reason    string    `db:"reason"`
	CreatedAt time.Time `db:"created_at"`
}

// SeedAlgorithm 是 lottery_draws.algorithm 今天的唯一取值。
//
// 名字里带 sort 是因为结果完全由「按 sha256(seed || id) 升序取前 N」决定——插入顺序、
// 扫描顺序都不影响它。改算法 = 换这个字符串 + 换 LotteryWinners 的实现，两者必须同批改。
const SeedAlgorithm = "sha256-sort-v1"
