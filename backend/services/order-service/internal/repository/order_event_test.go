package repository

import (
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
)

// 幂等键的字面格式在这里钉死。它与 account-service 的 model.BaseGrantKey / BonusGrantKey
// 必须逐字一致（那边收到什么就用什么，从不重拼），所以这条断言是**跨服务契约**的守卫，
// 不是「顺手测一下拼接」。改这个格式必须同时改那边，两边的用例会一起红。
func TestFortuneCardGrantKeys(t *testing.T) {
	const orderID = "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"
	if got := baseGrantKey(orderID); got != "order:"+orderID+":base" {
		t.Fatalf("base key = %q", got)
	}
	if got := bonusGrantKey(orderID, "campaign-1"); got != "order:"+orderID+":bonus:campaign-1" {
		t.Fatalf("bonus key = %q", got)
	}
}

func TestFortuneCardGrants(t *testing.T) {
	const orderID = "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"

	cases := []struct {
		name     string
		expected int
		snapshot string
		want     []dto.OrderFortuneGrant
	}{
		{
			// 不送福卡的订单：没有发放项，不是「发 0 张」。事件的 fortuneCards 为空，
			// 消费方据此 ack，不会记一条 0 的流水。
			name:     "no fortune cards promised",
			expected: 0,
			want:     nil,
		},
		{
			// 快照缺失：退回一条 base，金额取承诺总数。用户拿到的张数不打折。
			name:     "missing snapshot falls back to a single base grant",
			expected: 2,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBase, Amount: 2, EntryKey: "order:" + orderID + ":base"},
			},
		},
		{
			name:     "malformed snapshot falls back",
			expected: 2,
			snapshot: `{"base":`,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBase, Amount: 2, EntryKey: "order:" + orderID + ":base"},
			},
		},
		{
			// 拆得动的正常形状：一条基础 + 一条加赠，加赠带上活动 id 与名字。
			// 名字在这里就渲染不了——账户域会给它套上「订单完成赠送（幸运杯套）」。
			name:     "base plus a named bonus",
			expected: 2,
			snapshot: `{"base":1,"bonus":{"id":"campaign-1","name":"幸运杯套","reward":1}}`,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBase, Amount: 1, EntryKey: "order:" + orderID + ":base"},
				{Kind: dto.FortuneGrantKindBonus, CampaignID: "campaign-1", CampaignName: "幸运杯套", Amount: 1, EntryKey: "order:" + orderID + ":bonus:campaign-1"},
			},
		},
		{
			// 快照算错了（1 + 1 ≠ 3）：不猜哪一边对，整条退回 base。承诺数写在 orders
			// 自己的列上，和它对上才算数。
			name:     "snapshot that does not add up falls back",
			expected: 3,
			snapshot: `{"base":1,"bonus":{"id":"campaign-1","name":"幸运杯套","reward":1}}`,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBase, Amount: 3, EntryKey: "order:" + orderID + ":base"},
			},
		},
		{
			// 加赠没有 id：拼不出幂等键，也就没法「只发一次」，所以当成没有这一条。
			// 但 base 与承诺数仍然对得上，账还是对的。
			name:     "a bonus without an id is not a grant",
			expected: 1,
			snapshot: `{"base":1,"bonus":{"name":"幸运杯套","reward":1}}`,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBase, Amount: 1, EntryKey: "order:" + orderID + ":base"},
			},
		},
		{
			// 只有加赠的订单（base 为 0）：不应凭空多出一条 0 张的基础发放。
			name:     "a bonus-only order does not emit a zero base line",
			expected: 1,
			snapshot: `{"base":0,"bonus":{"id":"campaign-2","name":"杯套","reward":1}}`,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBonus, CampaignID: "campaign-2", CampaignName: "杯套", Amount: 1, EntryKey: "order:" + orderID + ":bonus:campaign-2"},
			},
		},
		{
			// 负数：快照说什么都不认，退回到承诺数。
			name:     "negative amounts fall back",
			expected: 1,
			snapshot: `{"base":-1,"bonus":{"id":"campaign-1","reward":2}}`,
			want: []dto.OrderFortuneGrant{
				{Kind: dto.FortuneGrantKindBase, Amount: 1, EntryKey: "order:" + orderID + ":base"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot []byte
			if tc.snapshot != "" {
				snapshot = []byte(tc.snapshot)
			}
			got := fortuneCardGrants(orderID, tc.expected, snapshot)
			if len(got) != len(tc.want) {
				t.Fatalf("want %d grants, got %d: %+v", len(tc.want), len(got), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("grant %d: want %+v, got %+v", i, tc.want[i], got[i])
				}
			}
		})
	}
}

// 退款要冻哪几张，是「按退款范围冻」这条口径的全部实现。scope 说到的是哪一类行，
// 发放流水的幂等键正好按同一个维度分段，所以映射是一对一的——这条用例就是那张表。
//
// 单列一组 degraded 的情形：快照拆不动时整单全冻，不管 scope 说什么。两个方向错了的
// 代价不对称，多冻可逆、少冻不可逆。
func TestFortuneCardFreezeKeys(t *testing.T) {
	const orderID = "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"
	baseKey := "order:" + orderID + ":base"
	bonusKey := "order:" + orderID + ":bonus:campaign-1"
	split := `{"base":1,"bonus":{"id":"campaign-1","name":"幸运杯套","reward":1}}`

	cases := []struct {
		name     string
		expected int
		snapshot string
		scope    string
		want     []string
	}{
		{
			// 这一单没承诺过福卡：没有要冻的东西。空是常态，不是异常。
			name: "no cards promised freezes nothing", expected: 0, scope: model.AfterSaleScopeAll,
		},
		{
			name: "a whole order freezes both grants", expected: 2, snapshot: split,
			scope: model.AfterSaleScopeAll, want: []string{baseKey, bonusKey},
		},
		{
			// 退饮品行：基础赠送是跟着饮品来的，加赠是跟着加购来的。
			name: "a drink-only refund freezes the base grant", expected: 2, snapshot: split,
			scope: model.AfterSaleScopeDrink, want: []string{baseKey},
		},
		{
			name: "an addon-only refund freezes the bonus grant", expected: 2, snapshot: split,
			scope: model.AfterSaleScopeAddon, want: []string{bonusKey},
		},
		{
			// 会员套餐不产生福卡：快照里没有它的分量，退它不动任何卡。
			name: "a membership refund freezes nothing", expected: 2, snapshot: split,
			scope: model.AfterSaleScopeMembership,
		},
		{
			// 认不出来的 scope 少冻而不是全冻：让新增 scope 的作者显式地在这里加一条，
			// 而不是让一个打错字的 scope 悄悄锁住用户的卡。
			name: "an unknown scope freezes nothing", expected: 2, snapshot: split,
			scope: "gift-card",
		},
		{
			// 只有加赠的订单：base 为 0，那条发放根本不存在，键也就不该出现——
			// 冻一个永远发不出来的键，冻结行会永远停在 0 张。
			name: "a bonus-only order has no base key to freeze", expected: 1,
			snapshot: `{"base":0,"bonus":{"id":"campaign-1","name":"杯套","reward":1}}`,
			scope:    model.AfterSaleScopeDrink,
		},
		{
			// 快照缺失（degraded）：承诺数全记在 base 那条上，退加购行也冻它——
			// 库里分不出这张卡是哪一行送的，宁可多冻。
			name:     "a degraded split freezes everything even for an addon refund",
			expected: 2, scope: model.AfterSaleScopeAddon, want: []string{baseKey},
		},
		{
			// degraded 时 scope=membership 也冻。这个组合看着别扭（会员套餐不产生福卡），
			// 但 degraded 的含义正是「这一单的福卡分不出是哪一行送的」，而这一单确实承诺了
			// 福卡、确实有退款在跑。冻着，等申请结束再放回去。
			name:     "a degraded split freezes everything even for a membership refund",
			expected: 2, scope: model.AfterSaleScopeMembership, want: []string{baseKey},
		},
		{
			// 快照算错了（1 + 1 ≠ 3）也是 degraded，走同一条路。
			name: "a snapshot that does not add up freezes everything", expected: 3,
			snapshot: split, scope: model.AfterSaleScopeDrink, want: []string{baseKey},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var snapshot []byte
			if tc.snapshot != "" {
				snapshot = []byte(tc.snapshot)
			}
			got := fortuneCardFreezeKeys(orderID, tc.expected, snapshot, tc.scope)
			if len(got) != len(tc.want) {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("key %d: want %q, got %q", i, tc.want[i], got[i])
				}
			}
		})
	}
}

// 每一条发放都必须能拆出正数张数：账户侧的 amount <= 0 会让整条消息进死信，而事件进死信
// 意味着这一单一张福卡都不发。这条不变式在每一种输入下都要成立，所以单独扫一遍。
func TestFortuneCardGrantsAlwaysEmitPositiveAmounts(t *testing.T) {
	const orderID = "0c7f2c8e-6a1a-4d0e-9b8f-1f2a3b4c5d6e"
	snapshots := []string{
		"", "null", `{}`, `{"base":2}`, `{"base":1,"bonus":{"id":"c","name":"n","reward":1}}`,
		`{"base":0,"bonus":{"id":"c","reward":2}}`, `{"base":3,"bonus":{"id":"c","reward":1}}`,
		`{"base":2,"bonus":null}`, `{"base":"two"}`,
	}
	for _, expected := range []int{0, 1, 2, 5} {
		for _, snapshot := range snapshots {
			var raw []byte
			if snapshot != "" {
				raw = []byte(snapshot)
			}
			grants := fortuneCardGrants(orderID, expected, raw)
			var total int64
			for _, grant := range grants {
				if grant.Amount <= 0 {
					t.Fatalf("expected=%d snapshot=%q produced a non-positive grant: %+v", expected, snapshot, grant)
				}
				if grant.EntryKey == "" {
					t.Fatalf("expected=%d snapshot=%q produced a grant with no entry key: %+v", expected, snapshot, grant)
				}
				total += grant.Amount
			}
			// 承诺几张就发几张，每一种快照下都成立——这正是兜底存在的意义。
			if total != int64(expected) {
				t.Fatalf("expected=%d snapshot=%q granted %d", expected, snapshot, total)
			}
		}
	}
}
