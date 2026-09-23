// Package draw 是开奖算法本身：一个纯函数包，不认识数据库、不认识 HTTP、不认识时间。
//
// 它之所以单独成一个包而不是放在 service 或 repository 里：**两边都要用它**。种子与名单
// 必须在仓储持有期次行锁的那段事务里算出来（参与集合在锁内读、结果在同一个事务里落地），
// 而算法本身是 service 层拥有的业务规则。把算法放进任何一边都会让另一边反向依赖，
// 所以它独立出来，两边都只依赖它。
//
// 这层拆分也带来一个好处：可复现性——「记录的种子 + 记录的参与集合 = 记录的中奖名单」
// ——可以用一根没有任何基础设施的测试直接钉住（algorithm_test.go）。
package draw

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"github.com/panda-dev/panda-v2/backend/services/lottery-service/internal/model"
)

// 这个文件是开奖的**纯函数**部分：给定种子与参与集合，名单唯一确定，与插入顺序、
// 扫描顺序、节点数、重跑次数都无关。
//
// 它独立成文件不是为了分层好看，是为了让「可复现」这件事能被一根测试直接钉住：
// algorithm_test.go 拿一份硬编码的参与集合与种子，断言名单逐字相等。真要让第三方复核
// 一次开奖，靠的就是「记录的种子 + 记录的参与集合 = 记录的中奖名单」这条等式，而这条
// 等式只有在算法是纯函数时才成立。
//
// **这不是可证明公平**：seed 是派生值而不是先承诺后揭示的随机数，所以有本库读权限的
// 运维在「最后一人参与到扫描之间」那个窗口里能预测结果。commit–reveal 不在本轮范围，
// 这句话在 model/draw.go 与 migrations/lottery 里都写着，不要在任何地方暗示相反。

// SeedInput 是派生种子需要的全部事实。
//
// 每一个字段都来自**已落库的参与记录或期次行**，没有一个是「此刻」或「运气」：这正是
// 开奖之后任何人都能重算的原因。FirstParticipationID / LastParticipationID 是参与集合
// 按 id 升序的头尾两个——取头尾而不是取全部，是因为种子要短、要能抄进 lottery_draws.seed
// 并被无歧义地重算；参与集合本身在 lottery_participations 里查得到。
type SeedInput struct {
	RoundID              string
	FirstParticipationID string
	LastParticipationID  string
	ParticipantCount     int
	Trigger              string
}

// DeriveSeed 算出这一期的种子，十六进制小写 64 字符。
//
// 拼接不加分隔符是安全的：round_id 与参与 id 都是定长 36 字符的 UUID，count 是十进制
// 整数，trigger 取自一个两个取值的闭集（threshold / manual），而拼接的总长度
// 由参与数决定——没有哪两组的拼接结果会碰巧相等。
//
// 参与集合为空时**不该调用它**：零人参与不写开奖记录，直接作废（见 worker/draw.go），
// 所以这里不对空集合做特殊处理，但调用方必须先判空——头尾两个空串拼出来的种子会是一个
// 看起来合法、实际谁都能重放的值。
func DeriveSeed(in SeedInput) string {
	sum := sha256.Sum256([]byte(in.RoundID +
		in.FirstParticipationID +
		in.LastParticipationID +
		strconv.Itoa(in.ParticipantCount) +
		in.Trigger))
	return hex.EncodeToString(sum[:])
}

// Winner 是一条中奖的参与，附带它在奖品清单上的位置。
type Winner struct {
	ParticipationID string
	// PrizeIndex 是在传进来的 prizes 切片里的下标。开奖要给每条中奖记录发一个 prize_id，
	// 而「哪条参与拿哪个奖」正是这个算法决定的东西。
	PrizeIndex int
}

// Prize 是分配名额用的奖池视图：一项奖带自己的 quantity。
//
// 用切片而不是 []*model.CampaignPrize 是为了让算法不依赖数据库行——它的输入只有
// 「有几种奖、各多少份」，别的一概无关。
//
// 现在一个活动只有一个奖品，所以传进来的切片恒为一个元素、Quantity 恒为 1——**但这里
// 没有按这个前提改写**：它是纯函数、有多奖品分配的单测，而在上层收到约束比在这一层写死
// 一个「反正只有一个」要好（真要一期发多份时，动的只是 campaignParams 里那个 quantity）。
type Prize struct {
	ID       string
	Quantity int
}

// SelectWinners 抽出这一期的中奖名单。
//
// 规则是**固定中奖名额随机抽**（用户拍板）：不是按个人累计进度，后者是方案 §3.1 明确
// 舍弃的。「随机」在这里的唯一含义是「对每条参与算 sha256(seed || 参与 id)，按这个分数
// 升序取前 N 条」——分数与参与人的任何属性都无关，只与种子和它自己的 id 有关。
//
// 同分按 id 升序兜底。sha256 撞值在实践上不可能，但「升序取前 N」必须是一个**全序**，
// 否则重算时同一个集合可能排出两个名单，可复现性就成了概率问题。
//
// 名额可能多于参与人数（线下活动常见：30 个名额 12 个人参与）：这时全员中奖，返回的长度
// 就是参与数。**不补人、不流局**——开奖记录里的 winner_count 存的是实际抽出的人数。
func SelectWinners(seed string, participationIDs []string, prizes []Prize) []Winner {
	type scored struct {
		id    string
		score string
	}
	ranked := make([]scored, 0, len(participationIDs))
	for _, id := range participationIDs {
		sum := sha256.Sum256([]byte(seed + id))
		ranked = append(ranked, scored{id: id, score: hex.EncodeToString(sum[:])})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		return ranked[i].id < ranked[j].id
	})

	// 名额按切片顺序分配：第一项拿满自己的 quantity 份，再轮到下一项。切片顺序在过去就是
	// sort_order，现在只剩一个奖品、它恒排第一。名额与奖池对不上时以参与集合为准（见上），
	// 所以这里只算一个总数。
	slots := 0
	for _, prize := range prizes {
		if prize.Quantity <= 0 {
			continue
		}
		slots += prize.Quantity
	}
	if slots > len(ranked) {
		slots = len(ranked)
	}

	// 按档分配：prizes 由调用方按 sort_order 排好序传进来，第一档先拿满自己的 quantity
	// 份，再轮到下一档。顺序是调用方的责任——「哪一档先给」是业务规则，不是这个函数
	// 能猜的。排名靠前的参与拿到靠前的档，所以「头奖给谁」完全由上面的分数序决定。
	winners := make([]Winner, 0, slots)
	next := 0
	for index, prize := range prizes {
		if next >= slots {
			break
		}
		take := min(prize.Quantity, slots-next)
		for range take {
			winners = append(winners, Winner{ParticipationID: ranked[next].id, PrizeIndex: index})
			next++
		}
	}
	return winners
}

// SeedAlgorithmFor 返回这次开奖记录的算法标识。
//
// 做成函数而不是直接引用常量，是为了给「算法将来可换」留一个明确的落点：换算法时这里
// 返回新的标识，SelectWinners 同步改，两者必须同批改——已开出的奖仍按它当时的算法复盘。
func SeedAlgorithmFor() string { return model.SeedAlgorithm }
