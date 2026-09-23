package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组测的是**钱怎么分**：命中什么、算多少、算不出来时归谁，以及这三个维度有没有真的走到
// 命中那一步。它们全是纯逻辑（computeSettlement 不碰数据库），所以表驱动地测。

// account 造一个可用账户。ReceiverID / PartyName 由主体名派生成确定的形状，便于断言快照有没有
// 被原样带进接收方行。
func account(party string) *repository.SettlementAccount {
	return &repository.SettlementAccount{
		ID: "acc-" + party, PartyName: "主体 " + party, ReceiverType: "MERCHANT_ID",
		ReceiverID: "mch-" + party,
	}
}

// percentItem 造一个比例项：ratio 用「× 1000000 的整数」给，与仓储传上来的一致（0.45 → 450000）。
func percentItem(party string, ratioScaled int64) repository.SettlementRuleItem {
	return repository.SettlementRuleItem{
		PartyType: party, CalcType: model.SettlementCalcPercent,
		RatioScaled: ratioScaled, Account: account(party),
	}
}

// fixedItem 造一个固定额项。
func fixedItem(party string, amount int64) repository.SettlementRuleItem {
	return repository.SettlementRuleItem{
		PartyType: party, CalcType: model.SettlementCalcFixed,
		FixedAmount: amount, Account: account(party),
	}
}

// TestComputeSettlement 是分账算法的表驱动用例。
//
// 每一条都顺带核一遍恒等式 base = 平台 + Σ接收方——它是这张表上唯一**跨越两张表的**不变量
// （这条不变量跨两张表，CHECK 表达不了），而算错它的后果是结算时才发现的几分钱差额。
func TestComputeSettlement(t *testing.T) {
	cases := []struct {
		name string
		base int64
		mode string
		// items 为 nil 时用下面的快捷字段。绝大多数用例要的是「几个比例项」，写全结构体噪音大。
		items       []repository.SettlementRuleItem
		wantAmounts map[string]int64 // 主体 → 金额
		wantNotes   int
		// wantErr 非 nil 时这是一条**配置错**的用例：整个计划算不出来，这次发起要失败。
		// 与 wantAmounts 互斥（算法不可能既算出接收方又报错）。
		wantErr error
	}{
		{
			name: "四成五给门店，余下归平台",
			base: 7000, mode: model.SettlementAllocationNormal,
			items:       []repository.SettlementRuleItem{percentItem("member_store", 450000)},
			wantAmounts: map[string]int64{"member_store": 3150},
		},
		{
			name: "两位接收方各拿各的，平台拿差额",
			base: 10000, mode: model.SettlementAllocationNormal,
			items: []repository.SettlementRuleItem{
				percentItem("member_store", 300000), percentItem("agent", 200000),
			},
			wantAmounts: map[string]int64{"member_store": 3000, "agent": 2000},
		},
		{
			name: "比例算出来不足一分：不建行",
			base: 1, mode: model.SettlementAllocationNormal,
			items:       []repository.SettlementRuleItem{percentItem("member_store", 450000)},
			wantAmounts: map[string]int64{},
		},
		{
			name: "比例向下取整到分（0.333333 × 2500 = 833.3325 → 833）",
			base: 2500, mode: model.SettlementAllocationNormal,
			items:       []repository.SettlementRuleItem{percentItem("member_store", 333333)},
			wantAmounts: map[string]int64{"member_store": 833},
		},
		{
			// 这条是「比例项为什么用 floor」那个决定的回归：四舍五入时两项各自进位到 657，
			// 加起来 1314 > 1313，一笔**完全合法**的规则会把整笔钱算成「分出去的比收进来的多」。
			name: "奇数分基数上两个 50% 各让一分，合计不越过基数",
			base: 1313, mode: model.SettlementAllocationNormal,
			items: []repository.SettlementRuleItem{
				percentItem("member_store", 500000), percentItem("agent", 500000),
			},
			wantAmounts: map[string]int64{"member_store": 656, "agent": 656},
		},
		{
			// 同样的道理走到极端：三项各 33.33% 时逐项进位会把 1 分钱的三项算成 3 分。
			name: "一分钱基数上三个 33.33%",
			base: 1, mode: model.SettlementAllocationNormal,
			items: []repository.SettlementRuleItem{
				percentItem("member_store", 333300), percentItem("agent", 333300),
				percentItem("city_center", 333300),
			},
			wantAmounts: map[string]int64{},
		},
		{
			name: "固定额按原值给",
			base: 7000, mode: model.SettlementAllocationNormal,
			items:       []repository.SettlementRuleItem{fixedItem("city_center", 500)},
			wantAmounts: map[string]int64{"city_center": 500},
		},
		{
			name: "先扣固定额，比例按剩余部分算",
			base: 7000, mode: model.SettlementAllocationFixedThenRemaining,
			items: []repository.SettlementRuleItem{
				{PartyType: "platform", CalcType: model.SettlementCalcRemainder},
				fixedItem("city_center", 500), percentItem("member_store", 500000),
			},
			// 基数 7000 − 固定额 500 = 6500，门店 50% = 3250，其余归平台。
			wantAmounts: map[string]int64{"city_center": 500, "member_store": 3250},
		},
		{
			name: "平台项不产生接收方，金额靠差额倒挤",
			base: 1000, mode: model.SettlementAllocationNormal,
			items: []repository.SettlementRuleItem{
				{PartyType: "platform", CalcType: model.SettlementCalcRemainder},
				percentItem("member_store", 450000),
			},
			wantAmounts: map[string]int64{"member_store": 450},
		},
		{
			// 这两条**不再是**「整单归平台」：那会把一次配置故障变成一次安静的错误分账
			// （钱全留平台、报文里连分账键都没有、库里却处处自洽）。发起失败是唯一能让人
			// 知道规则配坏了的出口。
			name: "固定额吃穿基数：报错，这次发起失败",
			base: 300, mode: model.SettlementAllocationFixedThenRemaining,
			items:   []repository.SettlementRuleItem{fixedItem("city_center", 500)},
			wantErr: ErrSettlementBaseOverflow,
		},
		{
			name: "比例之和超过 1：报错，这次发起失败",
			base: 1000, mode: model.SettlementAllocationNormal,
			items: []repository.SettlementRuleItem{
				percentItem("member_store", 600000), percentItem("agent", 600000),
			},
			wantErr: ErrSettlementBaseOverflow,
		},
		{
			name: "基数不是正数：报错（负数乘上比例分母之后符号会翻）",
			base: -1, mode: model.SettlementAllocationNormal,
			items:   []repository.SettlementRuleItem{percentItem("member_store", 450000)},
			wantErr: ErrSettlementBaseUnusable,
		},
		{
			name: "账户不可用：那一份归平台，并且留下一条说明",
			base: 7000, mode: model.SettlementAllocationNormal,
			items: []repository.SettlementRuleItem{
				{PartyType: "member_store", CalcType: model.SettlementCalcPercent, RatioScaled: 450000},
				percentItem("agent", 100000),
			},
			wantAmounts: map[string]int64{"agent": 700},
			wantNotes:   1,
		},
		{
			name: "没有项：整单归平台",
			base: 7000, mode: model.SettlementAllocationNormal,
			items:       nil,
			wantAmounts: map[string]int64{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			receivers, platform, notes, err := computeSettlement(tc.base, tc.mode, tc.items)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
				}
				// 配置错时一个接收方都不该产生：半份计划发出去比不发更危险。
				if len(receivers) != 0 || platform != 0 || len(notes) != 0 {
					t.Errorf("报错时应当什么都不返回，实际 receivers=%d platform=%d notes=%v",
						len(receivers), platform, notes)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错：%v", err)
			}

			got := make(map[string]int64, len(receivers))
			var total int64
			for _, receiver := range receivers {
				got[receiver.PartyType] += receiver.Amount
				total += receiver.Amount
				if receiver.Amount <= 0 {
					t.Errorf("接收方 %s 的金额是 %d：settlement_receivers.amount > 0，不建 0 元的行", receiver.PartyType, receiver.Amount)
				}
				if receiver.AccountID == "" || receiver.ReceiverID == "" {
					t.Errorf("接收方 %s 缺账户或渠道号：回退时要按它指回渠道", receiver.PartyType)
				}
			}
			if len(got) != len(tc.wantAmounts) {
				t.Fatalf("接收方 = %v，期望 %v", got, tc.wantAmounts)
			}
			for party, want := range tc.wantAmounts {
				if got[party] != want {
					t.Errorf("接收方 %s = %d，期望 %d", party, got[party], want)
				}
			}
			if len(notes) != tc.wantNotes {
				t.Errorf("说明 = %v，期望 %d 条", notes, tc.wantNotes)
			}
			if platform+total != tc.base {
				t.Errorf("平台 %d + 接收方合计 %d = %d，基数 %d：恒等式不成立",
					platform, total, platform+total, tc.base)
			}
			if platform < 0 {
				t.Errorf("平台金额 = %d：settlement_tasks.platform_amount 的 CHECK 要求它非负", platform)
			}
		})
	}
}

// TestComputeSettlementKeepsTheRatioSnapshot 钉住落进接收方行的那个比例。
//
// 两种算法记的东西不一样，而这是**账**：比例项记当初生效的比例，固定额项记 0（金额在 amount
// 上）——settlement_receivers.ratio 的列注释这么分的。记错了不会让任何一笔钱算错，但会让「当初凭什么分这么多」在
// 数据上读不出来。
func TestComputeSettlementKeepsTheRatioSnapshot(t *testing.T) {
	receivers, _, _, err := computeSettlement(7000, model.SettlementAllocationNormal,
		[]repository.SettlementRuleItem{fixedItem("city_center", 500), percentItem("member_store", 450000)})
	if err != nil {
		t.Fatalf("意外报错：%v", err)
	}

	byParty := make(map[string]int64, len(receivers))
	for _, receiver := range receivers {
		byParty[receiver.PartyType] = receiver.RatioScaled
	}
	if byParty["member_store"] != 450000 {
		t.Errorf("比例项的快照比例 = %d，期望 450000", byParty["member_store"])
	}
	if byParty["city_center"] != 0 {
		t.Errorf("固定额项的快照比例 = %d，期望 0", byParty["city_center"])
	}
}

// TestCreatePaymentSendsTheSettlementDimensions 守住那三个维度真的走到了命中那一步。
//
// 它为什么是断言而不是「看一眼调用点」：三个维度里任何一个没送到，表现都是**安静地命不中
// 规则、整单归平台**——没有报错、没有异常，只是钱分错了地方。
func TestCreatePaymentSendsTheSettlementDimensions(t *testing.T) {
	repo := &fakeRepository{}
	svc := newTestService(t, repo, &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}, testMethodCode)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if len(repo.ruleQueries) != 1 {
		t.Fatalf("查规则的次数 = %d，期望 1", len(repo.ruleQueries))
	}
	got := repo.ruleQueries[0]
	if got.BizType != model.SettlementBizCoffee || got.StoreRef != testStoreID ||
		got.DeviceRef != testDeviceID || got.Provider != catalog.ChannelCodeUMS {
		t.Errorf("命中维度 = %+v，期望 bizType=%s store=%s device=%s channel=%s",
			got, model.SettlementBizCoffee, testStoreID, testDeviceID, catalog.ChannelCodeUMS)
	}
}

// TestCreatePaymentSettlesTheReceiversInTheSameTransaction：算出来的计划要交给建支付单那一段，
// 而不是停在 service 里。任务与支付单同生共死是这条链路的全部要点。
func TestCreatePaymentSettlesTheReceiversInTheSameTransaction(t *testing.T) {
	repo := &fakeRepository{rule: &repository.SettlementRule{
		ID: "rule-1", BizType: model.SettlementBizCoffee, ScopeType: model.SettlementScopeStore,
		ScopeRef: testStoreID, AllocationMode: model.SettlementAllocationNormal,
		Items: []repository.SettlementRuleItem{percentItem("member_store", 450000)},
	}}
	svc := newTestService(t, repo, &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}, testMethodCode)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if len(repo.beginParams) != 1 {
		t.Fatalf("建单次数 = %d，期望 1", len(repo.beginParams))
	}
	plan := repo.beginParams[0].Settlement
	if plan == nil {
		t.Fatal("建单时没有带分账计划")
	}
	if plan.RuleID != "rule-1" || plan.ScopeType != model.SettlementScopeStore || plan.ScopeRef != testStoreID {
		t.Errorf("命中快照 = %+v，期望 rule-1/store/%s", plan, testStoreID)
	}
	// 门店维度来自**请求**，不是规则的范围：走 global 那条规则时它照样要有值。
	if plan.StoreRef != testStoreID {
		t.Errorf("storeRef = %q，期望 %q", plan.StoreRef, testStoreID)
	}
	if len(plan.Receivers) != 1 || plan.Receivers[0].Amount != 891 || plan.PlatformAmount != 1089 {
		t.Errorf("计划 = %+v（平台 %d）：1980 的 45%% 是 891", plan.Receivers, plan.PlatformAmount)
	}
}

// TestCreatePaymentWithoutARuleSettlesEverythingToThePlatform：没配规则不是错误，是一个正常的
// 业务事实——任务照样建（整单归平台），与 settlement_tasks 的存照一致。
func TestCreatePaymentWithoutARuleSettlesEverythingToThePlatform(t *testing.T) {
	repo := &fakeRepository{}
	svc := newTestService(t, repo, &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}, testMethodCode)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	plan := repo.beginParams[0].Settlement
	if plan == nil {
		t.Fatal("没命中规则时也该建任务（整单归平台）")
	}
	if plan.RuleID != "" || len(plan.Receivers) != 0 {
		t.Errorf("计划 = %+v，期望没有规则、没有接收方", plan)
	}
	if plan.PlatformAmount != 1980 {
		t.Errorf("平台金额 = %d，期望整单 1980", plan.PlatformAmount)
	}
	if plan.ScopeType != "" {
		// 空串表示「这次没有规则命中」，与 global 那一档分得开。
		t.Errorf("scopeType = %q，没命中规则时它该是空串", plan.ScopeType)
	}
}

// TestCreatePaymentFailsWhenTheRuleCannotBeRead：查规则失败就让这次发起失败，**不做**老系统那种
// 「查失败就当没规则、全归平台」的兜底——那是把一次配置故障静默变成一次错误的分账。
func TestCreatePaymentFailsWhenTheRuleCannotBeRead(t *testing.T) {
	repo := &fakeRepository{ruleErr: errors.New("settlement rules are unreachable")}
	svc := newTestService(t, repo, &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}, testMethodCode)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("读规则失败，但这次发起继续了")
	}
	if len(repo.beginParams) != 0 {
		t.Fatalf("建单次数 = %d；读不到规则就不该建出支付单", len(repo.beginParams))
	}
}

// TestCreateAccountPaymentDoesNotSettle：账户出资（咖啡豆）不建分账任务。
//
// 渠道分账分的是**渠道里的钱**，豆支付没有渠道资金可动，整条路走不到渠道的分账接口。
func TestCreateAccountPaymentDoesNotSettle(t *testing.T) {
	repo := &fakeRepository{}
	svc := newBeanService(t, repo, &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}})

	if _, err := svc.CreatePayment(context.Background(), validBeanRequest()); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if len(repo.ruleQueries) != 0 {
		t.Errorf("账户出资查了 %d 次规则，期望 0 次", len(repo.ruleQueries))
	}
	if plan := repo.beginParams[0].Settlement; plan != nil {
		t.Errorf("账户出资带了分账计划 %+v，期望 nil", plan)
	}
}
