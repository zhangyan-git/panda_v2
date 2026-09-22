package repository

import (
	"encoding/json"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 福卡幂等键的格式。
//
// 这三个生成函数与 account-service 的 model.BaseGrantKey / BonusGrantKey 是**同一条格式**
// 的两份实现，不是巧合：键由订单域拼（它才知道活动是哪一次），账户域收到什么就用什么、
// 从不重拼。两边漂移的后果不是编译错，而是同一单在重投时得到两个不同的键——
// 「重投不会发两次福卡」这条保证会当场断掉，而且只在重投时才现形。
//
// 之所以宁可重写一份也不共享：account-service 与 order-service 是两个模块，
// 共享这段逻辑要把它提到 contracts 里，为三个字符串字面量加一层跨服务依赖不值得。
// 代价是这条注释和 order_event_test.go 里那几条断言字面格式的用例——格式一改，
// 两边各自的测试都会红。
func baseGrantKey(orderID string) string { return "order:" + orderID + ":base" }

func bonusGrantKey(orderID, campaignID string) string {
	return "order:" + orderID + ":bonus:" + campaignID
}

// fortuneCardSnapshot 是 orders.fortune_card_snapshot 期望的形状。
//
// 形状只在 migrations/order/001 那一列的注释里写着（"基础赠送 + 各加购活动的加赠"），
// 没有任何写路径产生它——它是调用方给的 JSON。所以下面每一处解析都必须按「可能不是
// 这个形状」来写，而不是按「它就是」来写。
type fortuneCardSnapshot struct {
	Base  int64             `json:"base"`
	Bonus *fortuneCardBonus `json:"bonus"`
}

type fortuneCardBonus struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reward int64  `json:"reward"`
}

// fortuneCardGrants 把「这一单承诺几张」拆成要记进福卡流水的每一条。
// 第二个返回值表示这份拆解是不是**兜底**产物，见 fortuneCardSplit。
func fortuneCardGrants(orderID string, expected int, raw []byte) []dto.OrderFortuneGrant {
	grants, _ := fortuneCardSplit(orderID, expected, raw)
	return grants
}

// fortuneCardSplit 把「这一单承诺几张」拆成每一条发放，并说明这次拆解可不可信。
//
// 拆不动时**退回一条 base**（金额取承诺总数）：用户拿到的张数永远等于承诺的数，
// 退化只发生在流水的构成粒度上。这条兜底不是防御性编程，是必须有的行为——快照是调用方
// 给的，它会缺、会换形状、会算错，而「答应了 2 张就发 2 张」是订单自己的承诺，
// 不该因为上游没给好快照而缩水。退化成一条的好处是它仍然对得上账，只是看不出加赠来自
// 哪个活动；这比少发一张好，也比让事件进死信好。
//
// degraded 为真时，返回的是那一条退化的 base。它只有一件事要说清楚：**这一单的福卡
// 分不出是哪一行送的**。冻卡要按退款范围分层（退加购行只冻加赠那张），而兜底之后
// 两张卡在库里就是混在 base 里的，想分层也分不开——所以那边见了 degraded 就整单全冻。
func fortuneCardSplit(orderID string, expected int, raw []byte) (grants []dto.OrderFortuneGrant, degraded bool) {
	if expected <= 0 {
		return nil, false
	}
	fallback := []dto.OrderFortuneGrant{{
		Kind:     dto.FortuneGrantKindBase,
		Amount:   int64(expected),
		EntryKey: baseGrantKey(orderID),
	}}

	var snapshot fortuneCardSnapshot
	if len(raw) > 0 {
		// 解析失败就当快照不存在，走兜底。这里不报错：一张快照语法坏了不该让整单完不成。
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return fallback, true
		}
	}
	base := snapshot.Base
	var bonusReward int64
	hasBonus := snapshot.Bonus != nil && strings.TrimSpace(snapshot.Bonus.ID) != ""
	if hasBonus {
		bonusReward = snapshot.Bonus.Reward
	}
	// 唯一的判据是「拆出来的和等于承诺数」。逐字段去校验快照是查不过来的（形状会变），
	// 而承诺数写在 orders 自己那一列上——和它对上，这一单的账就是对的。
	if base < 0 || bonusReward < 0 || base+bonusReward != int64(expected) {
		return fallback, true
	}

	grants = make([]dto.OrderFortuneGrant, 0, 2)
	if base > 0 {
		grants = append(grants, dto.OrderFortuneGrant{
			Kind:     dto.FortuneGrantKindBase,
			Amount:   base,
			EntryKey: baseGrantKey(orderID),
		})
	}
	if hasBonus && bonusReward > 0 {
		grants = append(grants, dto.OrderFortuneGrant{
			Kind:         dto.FortuneGrantKindBonus,
			CampaignID:   strings.TrimSpace(snapshot.Bonus.ID),
			CampaignName: strings.TrimSpace(snapshot.Bonus.Name),
			Amount:       bonusReward,
			EntryKey:     bonusGrantKey(orderID, strings.TrimSpace(snapshot.Bonus.ID)),
		})
	}
	if len(grants) == 0 {
		// 和等于承诺数（> 0）却拆不出任何一条：只可能是 base 与 bonus 相互抵消，
		// 而两者都非负，所以这里到不了。留着是为了让「拆出的每一条都不大于 0」这种
		// 改动在这里停住，而不是变成一条空发放的事件。
		return fallback, true
	}
	return grants, false
}

// fortuneCardFreezeKeys 给出「这一张售后申请要冻住哪几笔发放」。
//
// 退款范围说到的是「哪一类行」，而发放流水的幂等键正好按同一个维度分段
// （base 是基础赠送，bonus:{campaignId} 是加购活动的加赠），所以映射是一对一的：
//
//	drink  → base    退饮品行，冻基础赠送那张
//	addon  → bonus   退加购行，冻那一次活动的加赠
//	all    → 全部    整单退，两张都冻
//	membership → 无  会员套餐不产生福卡，福卡快照里也没有它的分量
//
// 拆不动（degraded）时**整单全冻**，不管 scope 说什么。那时这一单的福卡在库里就是混在
// 一条 base 里的，退加购行与退饮品行没有区别——而两个方向错了的代价不对称：多冻一张是
// 可逆的（申请被驳回就解冻），少冻一张是不可逆的（卡在这期间被抽掉，钱退了这个洞补不回来）。
//
// 空的返回值是常态（这一单没承诺福卡），调用方据此不发冻结，不是异常。
func fortuneCardFreezeKeys(orderID string, expected int, raw []byte, scope string) []string {
	return fortuneCardFreezePlan(orderID, expected, raw, scope).EntryKeys
}

// FortuneCardFreezePlan 是一次退款申请要冻住的那几笔发放，以及它们一共承诺了几张。
//
// 两个值必须一起算出来：受理退款申请时要拿 Cards 去比「账户此刻还冻得上这么多吗」，而
// Cards 与 EntryKeys 出自同一次拆分——分两处算，退加购行时就会拿整单的张数去比一张卡。
type FortuneCardFreezePlan struct {
	EntryKeys []string
	Cards     int64
}

// fortuneCardFreezePlan 是上面那两件事的唯一来源（fortuneCardFreezeKeys 是它的薄壳，保给
// 「只要键」的调用点：下单事件那条路不关心张数）。
//
// degraded 时整单全冻，所以 Cards 也就是整单承诺的张数，与 EntryKeys 覆盖的范围一致——
// 这正是两个值必须一起返回的原因。
func fortuneCardFreezePlan(orderID string, expected int, raw []byte, scope string) FortuneCardFreezePlan {
	grants, degraded := fortuneCardSplit(orderID, expected, raw)
	if len(grants) == 0 {
		return FortuneCardFreezePlan{}
	}
	plan := FortuneCardFreezePlan{EntryKeys: make([]string, 0, len(grants))}
	for _, grant := range grants {
		if !degraded && !grantKindInScope(grant.Kind, scope) {
			continue
		}
		plan.EntryKeys = append(plan.EntryKeys, grant.EntryKey)
		plan.Cards += grant.Amount
	}
	return plan
}

// grantKindInScope 说一笔发放属不属于这次退款范围。
func grantKindInScope(kind, scope string) bool {
	switch scope {
	case model.AfterSaleScopeAll:
		return true
	case model.AfterSaleScopeDrink:
		return kind == dto.FortuneGrantKindBase
	case model.AfterSaleScopeAddon:
		return kind == dto.FortuneGrantKindBonus
	default:
		// membership（会员套餐不送福卡）以及将来新增的范围：不冻任何东西。
		// 认不出来就少冻，让新 scope 的作者显式地在这里加一条——默认全冻会让一个
		// 打错字的 scope 悄悄锁住用户的卡。
		return false
	}
}
