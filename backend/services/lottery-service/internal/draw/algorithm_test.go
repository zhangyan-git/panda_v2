package draw

import (
	"fmt"
	"testing"
)

// 这一份测试钉住的是**可复现性**，不是「随机性」。
//
// 开奖记录里存着种子，参与记录里存着参与集合，所以第三方能拿这两样把中奖名单完整重算
// 一遍——这是 lottery_draws 这张只增表存在的全部意义。它成立的前提只有一条：SelectWinners
// 是纯函数，给定同样的输入永远给同样的输出。下面的 golden 用例就是这条等式的具体形态，
// **任何改动算法（换哈希、换排序、改分配顺序）都会让它红**，而那正是应有的反应：
// 已开出的奖必须仍按它当时的算法复盘，换算法要同批换 SeedAlgorithmFor 的返回值。
//
// 反过来说，这一份测试**不证明公平**：种子是派生值而不是先承诺后揭示的随机数。别把它
// 读成公平性证明。

// goldenIDs 是一组固定的参与 id。用字面量而不是随机生成：用例失败时要能一眼看出是哪一条，
// 而 golden 断言的全部意义就在于「这些字节就是答案」。
var goldenIDs = []string{
	"0b6f0b7a-1c2d-4e3f-8a9b-0c1d2e3f4a5b",
	"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
	"2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f",
	"3d4e5f6a-7b8c-4d9e-8f0a-2b3c4d5e6f7a",
	"4e5f6a7b-8c9d-4e0f-9a1b-3c4d5e6f7a8b",
	"5f6a7b8c-9d0e-4f1a-8b2c-4d5e6f7a8b9c",
	"6a7b8c9d-0e1f-4a2b-9c3d-5e6f7a8b9c0d",
	"7b8c9d0e-1f2a-4b3c-8d4e-6f7a8b9c0d1e",
}

const (
	goldenRoundID = "9f8e7d6c-5b4a-4392-8180-7f6e5d4c3b2a"
	goldenSeed    = "59a1547254ea646a20a08e705cf97f21e185e6b689d8b6b1eba3352eed1dcad6"
)

func goldenSeedInput() SeedInput {
	return SeedInput{
		RoundID:              goldenRoundID,
		FirstParticipationID: goldenIDs[0],
		LastParticipationID:  goldenIDs[len(goldenIDs)-1],
		ParticipantCount:     len(goldenIDs),
		Trigger:              "threshold",
	}
}

// TestDeriveSeedGolden 钉住种子的算法。
//
// 种子变了意味着同一个参与集合会开出两份不同的名单，而库里的开奖记录只存着其中的一份
// ——所以它是这套可复现性里最不能悄悄改的那个值。
func TestDeriveSeedGolden(t *testing.T) {
	got := DeriveSeed(goldenSeedInput())
	if got != goldenSeed {
		t.Fatalf("种子变了：得到 %s，期望 %s", got, goldenSeed)
	}
	if len(got) != 64 {
		t.Fatalf("种子应当是 64 个十六进制字符，得到 %d 个", len(got))
	}
}

// TestDeriveSeedChangesWithEveryField 确认五个字段**每一个**都参与运算。
//
// 漏掉任何一个字段都会让两类不同的开奖算出同一个种子——而症状是「两期开出了同一份名单」，
// 那看起来像运气，查起来要很久。
func TestDeriveSeedChangesWithEveryField(t *testing.T) {
	base := goldenSeedInput()
	want := DeriveSeed(base)

	cases := map[string]SeedInput{
		"roundId":              {RoundID: base.RoundID + "0", FirstParticipationID: base.FirstParticipationID, LastParticipationID: base.LastParticipationID, ParticipantCount: base.ParticipantCount, Trigger: base.Trigger},
		"firstParticipationId": {RoundID: base.RoundID, FirstParticipationID: base.FirstParticipationID + "0", LastParticipationID: base.LastParticipationID, ParticipantCount: base.ParticipantCount, Trigger: base.Trigger},
		"lastParticipationId":  {RoundID: base.RoundID, FirstParticipationID: base.FirstParticipationID, LastParticipationID: base.LastParticipationID + "0", ParticipantCount: base.ParticipantCount, Trigger: base.Trigger},
		"participantCount":     {RoundID: base.RoundID, FirstParticipationID: base.FirstParticipationID, LastParticipationID: base.LastParticipationID, ParticipantCount: base.ParticipantCount + 1, Trigger: base.Trigger},
		"trigger":              {RoundID: base.RoundID, FirstParticipationID: base.FirstParticipationID, LastParticipationID: base.LastParticipationID, ParticipantCount: base.ParticipantCount, Trigger: "manual"},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if DeriveSeed(input) == want {
				t.Fatalf("改 %s 之后种子没变——它没有参与运算", name)
			}
		})
	}
}

// TestSelectWinnersGoldenList 是这一份测试里最要紧的一条：**名单本身**。
//
// 它同时钉住三件事：分数怎么算、同分怎么排、名额按档怎么分配。任何一件改了都会红。
func TestSelectWinnersGoldenList(t *testing.T) {
	prizes := []Prize{{ID: "p1", Quantity: 1}, {ID: "p2", Quantity: 2}, {ID: "p3", Quantity: 3}}
	got := SelectWinners(goldenSeed, goldenIDs, prizes)

	want := []Winner{
		{ParticipationID: "7b8c9d0e-1f2a-4b3c-8d4e-6f7a8b9c0d1e", PrizeIndex: 0},
		{ParticipationID: "5f6a7b8c-9d0e-4f1a-8b2c-4d5e6f7a8b9c", PrizeIndex: 1},
		{ParticipationID: "0b6f0b7a-1c2d-4e3f-8a9b-0c1d2e3f4a5b", PrizeIndex: 1},
		{ParticipationID: "4e5f6a7b-8c9d-4e0f-9a1b-3c4d5e6f7a8b", PrizeIndex: 2},
		{ParticipationID: "3d4e5f6a-7b8c-4d9e-8f0a-2b3c4d5e6f7a", PrizeIndex: 2},
		{ParticipationID: "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f", PrizeIndex: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("中奖人数 %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 名是 %+v，期望 %+v", i+1, got[i], want[i])
		}
	}
}

// TestSelectWinnersIgnoresInputOrder 是可复现性的**可操作形态**：第三方重算时手上的参与
// 集合来自一次 SQL 查询，而那次查询的排序未必与我们开奖时用的 ORDER BY 一样。
//
// 名单与输入顺序无关，复核才有可能：否则「重算对不上」与「查询顺序不同」两种情况会混在
// 一起，谁也说不清是哪一种。
func TestSelectWinnersIgnoresInputOrder(t *testing.T) {
	prizes := []Prize{{ID: "p1", Quantity: 3}}
	want := SelectWinners(goldenSeed, goldenIDs, prizes)

	// 几种典型的乱序：整体反转、两两交换、按 id 降序。
	orders := map[string][]string{
		"reversed":      reversed(goldenIDs),
		"swap-halves":   swapHalves(goldenIDs),
		"rotate-by-one": append(append([]string{}, goldenIDs[1:]...), goldenIDs[0]),
	}
	for name, ids := range orders {
		t.Run(name, func(t *testing.T) {
			got := SelectWinners(goldenSeed, ids, prizes)
			if len(got) != len(want) {
				t.Fatalf("人数 %d，期望 %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("第 %d 名是 %+v，期望 %+v——名单依赖了输入顺序", i+1, got[i], want[i])
				}
			}
		})
	}
}

// TestSelectWinnersIsRepeatable 对同一份输入跑一百次。
//
// 听上去是废话，但这条测试挡的是 Go 里两种真实存在的写法：map 迭代顺序（用它做去重或
// 分组）、以及原地排序却复用了调用方的切片。两者都会让同一个集合在两次调用里排出两个
// 名单，而且**只在数据量大的时候偶尔出现**。
func TestSelectWinnersIsRepeatable(t *testing.T) {
	prizes := []Prize{{ID: "p1", Quantity: 2}, {ID: "p2", Quantity: 2}}
	first := fmt.Sprint(SelectWinners(goldenSeed, goldenIDs, prizes))
	for i := 0; i < 100; i++ {
		if got := fmt.Sprint(SelectWinners(goldenSeed, goldenIDs, prizes)); got != first {
			t.Fatalf("第 %d 次跑出了不同的名单：\n%s\n%s", i+2, first, got)
		}
	}
}

// TestSelectWinnersDoesNotMutateInput 确认算法不动调用方的切片。
//
// 仓储把参与集合按 id 升序读出来交给它，紧接着还要用同一份切片去写中奖记录。原地排序
// 会让写进库里的参与顺序与开奖时读到的顺序不一样——不难发现，但发现的人往往已经查了很久。
func TestSelectWinnersDoesNotMutateInput(t *testing.T) {
	ids := append([]string{}, goldenIDs...)
	before := fmt.Sprint(ids)
	SelectWinners(goldenSeed, ids, []Prize{{ID: "p1", Quantity: 4}})
	if fmt.Sprint(ids) != before {
		t.Fatalf("输入切片被改动了：\n改动前 %s\n改动后 %s", before, ids)
	}
}

// TestSelectWinnersAllWinWhenSlotsExceedParticipants 是线下活动的常见局面：30 个名额 8 个人
// 参与。这时全员中奖，返回的长度就是参与数——**不补人、不流局**。
//
// 开奖记录里的 winner_count 存的是实际抽出的人数，所以「名额没用完」这件事在记录里是
// 看得出来的，不需要另外一列去表达。
func TestSelectWinnersAllWinWhenSlotsExceedParticipants(t *testing.T) {
	prizes := []Prize{{ID: "p1", Quantity: 20}, {ID: "p2", Quantity: 10}}
	got := SelectWinners(goldenSeed, goldenIDs, prizes)
	if len(got) != len(goldenIDs) {
		t.Fatalf("全员中奖时应当有 %d 人，得到 %d 人", len(goldenIDs), len(got))
	}
	// 名额够时全部落在第一档：按 sort_order 依次分配，第一档没吃完轮不到第二档。
	for i, winner := range got {
		if winner.PrizeIndex != 0 {
			t.Fatalf("第 %d 名落在第 %d 档，期望第 0 档（第一档名额还没吃完）", i+1, winner.PrizeIndex)
		}
	}
}

// TestSelectWinnersAllocatesPrizesInOrder 钉住名额分配的次序：第一档先拿满自己的 quantity
// 份，再轮到下一档。
//
// 「哪一档先给」是业务规则（sort_order 由运营决定），而算法按传进来的顺序执行——所以这条
// 测试同时在说：**调用方必须按 sort_order 排好序传进来**，算法不会替你排。
func TestSelectWinnersAllocatesPrizesInOrder(t *testing.T) {
	prizes := []Prize{{ID: "first", Quantity: 2}, {ID: "second", Quantity: 3}, {ID: "third", Quantity: 99}}
	got := SelectWinners(goldenSeed, goldenIDs, prizes)
	if len(got) != len(goldenIDs) {
		t.Fatalf("全员中奖时应当有 %d 人，得到 %d 人", len(goldenIDs), len(got))
	}

	// 8 个人、2+3 之后还剩 3 个给第三档。
	wantIndexes := []int{0, 0, 1, 1, 1, 2, 2, 2}
	for i, want := range wantIndexes {
		if got[i].PrizeIndex != want {
			t.Fatalf("第 %d 名落在第 %d 档，期望第 %d 档", i+1, got[i].PrizeIndex, want)
		}
	}
}

// TestSelectWinnersSkipsNonPositiveQuantities 挡的是奖池里的 0 与负数。
//
// 数据库上有 CHECK (quantity > 0)，所以这两值进不来；但算法不该依赖一条它读不到的约束
// ——负数会让 slots 变小甚至变负，而 slots 负的后果是「一个奖都不发」却**不报错**。
func TestSelectWinnersSkipsNonPositiveQuantities(t *testing.T) {
	prizes := []Prize{{ID: "zero", Quantity: 0}, {ID: "real", Quantity: 2}, {ID: "negative", Quantity: -5}}
	got := SelectWinners(goldenSeed, goldenIDs, prizes)
	if len(got) != 2 {
		t.Fatalf("应当只发出 2 个奖，得到 %d 个", len(got))
	}
	for _, winner := range got {
		if winner.PrizeIndex != 1 {
			t.Fatalf("中奖落在第 %d 档，期望第 1 档（第 0 档 0 份、第 2 档是负数）", winner.PrizeIndex)
		}
	}
}

// TestSelectWinnersWithNoParticipants 确认空集合不 panic、不返回人。
//
// 零人参与的期次**不写开奖记录**，直接作废（见 worker/draw.go），所以正常路上算法不会
// 拿到空集合。但它仍然必须安全：一个 nil 切片从别处漏进来时，这里 panic 的后果是开奖
// worker 崩掉，而它崩掉意味着**所有**期次都不再开奖。
func TestSelectWinnersWithNoParticipants(t *testing.T) {
	for _, ids := range [][]string{nil, {}} {
		got := SelectWinners(goldenSeed, ids, []Prize{{ID: "p1", Quantity: 3}})
		if len(got) != 0 {
			t.Fatalf("没有参与时不该有人中奖，得到 %d 个", len(got))
		}
	}
}

// TestSelectWinnersWithNoPrizes 确认奖池为空时不发奖，也不 panic。
//
// 活动新建时要求至少一个奖品（service 校验），所以正常路上不会空。它挡的是另一半：
// 奖池在开奖前被清空了（今天的接口不允许，但结构上做得到），此时应当开出一份**空名单**
// 并如实记下 winner_count = 0，而不是给每个人发一个不存在的奖。
func TestSelectWinnersWithNoPrizes(t *testing.T) {
	if got := SelectWinners(goldenSeed, goldenIDs, nil); len(got) != 0 {
		t.Fatalf("奖池为空时不该有人中奖，得到 %d 个", len(got))
	}
}

// TestSelectWinnersNeverInventsOrRepeatsAParticipant 确认名单是参与集合的一个子集，
// 且没有重复。
//
// 听起来同样像废话，但它是「一期里一条参与只能中一次」这条不变量的算法侧依据——数据
// 库上那条 lottery_wins_participation_unique 是兜底，而一次抽两个人出来的 bug 会在
// 插入第二条时才被唯一索引拦下，那时整次开奖事务已经要回滚了。
func TestSelectWinnersNeverInventsOrRepeatsAParticipant(t *testing.T) {
	prizes := []Prize{{ID: "p1", Quantity: 4}, {ID: "p2", Quantity: 4}}
	known := map[string]bool{}
	for _, id := range goldenIDs {
		known[id] = true
	}
	got := SelectWinners(goldenSeed, goldenIDs, prizes)
	if len(got) != len(goldenIDs) {
		t.Fatalf("8 个名额 8 个人，应当全员中奖，得到 %d 个", len(got))
	}
	seen := map[string]bool{}
	for _, winner := range got {
		if !known[winner.ParticipationID] {
			t.Fatalf("名单里出现了不在参与集合里的 id：%s", winner.ParticipationID)
		}
		if seen[winner.ParticipationID] {
			t.Fatalf("同一个参与被抽中了两次：%s", winner.ParticipationID)
		}
		seen[winner.ParticipationID] = true
	}
}

// TestSelectWinnersDependsOnTheSeed 确认换一个种子会换一份名单。
//
// 二十个不同的期次 id 推出二十个种子，如果第一名永远是同一条参与，说明分数里根本没用到
// 种子——那时「种子」就成了一个好看但没用的字段，而开奖也就退化成「按参与 id 排序取前 N」
// （先参与的人永远中奖）。
func TestSelectWinnersDependsOnTheSeed(t *testing.T) {
	prizes := []Prize{{ID: "p1", Quantity: 1}}
	firsts := map[string]bool{}
	for i := 0; i < 20; i++ {
		seed := DeriveSeed(SeedInput{
			RoundID:              fmt.Sprintf("%s-%02d", goldenRoundID, i),
			FirstParticipationID: goldenIDs[0],
			LastParticipationID:  goldenIDs[len(goldenIDs)-1],
			ParticipantCount:     len(goldenIDs),
			Trigger:              "threshold",
		})
		got := SelectWinners(seed, goldenIDs, prizes)
		if len(got) != 1 {
			t.Fatalf("1 个名额应当抽出 1 个人，得到 %d 个", len(got))
		}
		firsts[got[0].ParticipationID] = true
	}
	// 八个候选人里只出现一个，等同于「种子没参与运算」。给一点余量：允许出现两三个，
	// 但那已经说明分布可疑了——八个里出两个的概率极低（1/8 的二十次独立抽样）。
	if len(firsts) < 3 {
		t.Fatalf("二十个不同种子只抽出了 %d 个不同的第一名，种子没有真正参与运算", len(firsts))
	}
}

// TestSeedAlgorithmFor 确认标识符就是模型里那一个。换算法要同批改两处，这条断言让「只改了
// 一处」在编译之后仍然会被测出来。
func TestSeedAlgorithmFor(t *testing.T) {
	if got := SeedAlgorithmFor(); got != "sha256-sort-v1" {
		t.Fatalf("算法标识是 %q，期望 \"sha256-sort-v1\"（改了算法就要同批改这里与 SelectWinners）", got)
	}
}

// —— 测试用的小工具 ——

func reversed(ids []string) []string {
	out := make([]string, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		out = append(out, ids[i])
	}
	return out
}

func swapHalves(ids []string) []string {
	half := len(ids) / 2
	out := append([]string{}, ids[half:]...)
	return append(out, ids[:half]...)
}
