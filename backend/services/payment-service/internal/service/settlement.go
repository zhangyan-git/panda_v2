package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// settlementRatioScale 是比例的分母。settlement_rule_items.ratio 是 NUMERIC(20,6)，仓储把它
// 换算成「× 1000000 的整数」传上来（0.45 → 450000）。换算放在 SQL 里做，因为那里是精确的
// numeric 运算；到了 Go 这边就只剩整数乘除。
const settlementRatioScale = 1000000

// buildSettlementPlan 在发起支付这一刻把分账计划算出来（方案 B：任务在这里建，支付成功后只剩
// 推进状态）。
//
// 为什么在这一刻：请求里正好带着这笔钱的全部上下文——实付金额、门店、设备、业务分类都由订单
// 权威给出。等支付成功了再回头凑这些维度，要么跨服务反查，要么补一次契约，反而更绕。规则也因此
// 在**发起支付那一刻冻结**：运营之后改了比例，不影响已经建好的这一笔。
//
// 两处「不建任务」是同一件事：账户出资（咖啡豆）。渠道分账分的是**渠道里的钱**，豆支付没有渠道
// 资金可动，整条路走不到渠道的分账接口。返回 nil 就是这件事的写法。
//
// 查规则失败**直接让这次发起失败**，不做老系统那种「查失败就当没规则、全归平台」的兜底：那是把
// 一次配置故障静默变成一次错误的分账，而且规则就在同一个库，读不动它基本等于建单也建不动。
func (s *PaymentService) buildSettlementPlan(ctx context.Context, in CreateRequest, route paymentRoute) (*repository.SettlementPlan, error) {
	if route.Method.Action == provider.ActionAccount {
		return nil, nil
	}

	rule, err := s.repository.FindSettlementRule(ctx, repository.SettlementRuleQuery{
		BizType: in.BizType,
		// 渠道名而不是渠道行的 uuid：接收方账户按渠道登记（settlement_accounts.provider），
		// 而那个值今天就是这个名字（见 009 迁移）。
		Provider:  route.Provider(),
		DeviceRef: in.DeviceID,
		StoreRef:  in.StoreID,
		// brand / product 两档给不出来，见 SettlementRuleQuery 的注释：订单库里没有品牌，
		// 一笔支付也没有单一商品可指。规则表里配了这两档，今天命中不了。
	})
	if err != nil {
		return nil, fmt.Errorf("find settlement rule for store %q device %q: %w", in.StoreID, in.DeviceID, err)
	}

	// 门店/品牌是**订单侧推过来的**值引用，不是命中的那条规则的范围：没配规则、走的是 global
	// 那条规则时，它们照样有值——结算按门店主体归集时要的正是它。
	plan := &repository.SettlementPlan{StoreRef: in.StoreID}
	if rule == nil {
		// 没命中规则不是错误，是「这个门店没配规则」这个正常的业务事实。任务照样建，整单归平台
		// （008 的存照：门店没配规则时这条任务照样建）。
		plan.PlatformAmount = in.Amount
		return plan, nil
	}

	plan.RuleID = rule.ID
	plan.ScopeType = rule.ScopeType
	plan.ScopeRef = rule.ScopeRef
	receivers, platformAmount, notes, err := computeSettlement(in.Amount, rule.AllocationMode, rule.Items)
	if err != nil {
		// 这条规则与这笔钱凑不出一个能下发的计划。**让这次发起失败**，不退回「整单归平台」：
		// 静默归平台的表现与一次正常的分账支付一模一样，没有任何一处看得出规则配坏了。
		return nil, fmt.Errorf("compute settlement for rule %q store %q: %w", rule.ID, in.StoreID, err)
	}
	plan.Receivers = receivers
	plan.PlatformAmount = platformAmount
	for _, note := range notes {
		slog.WarnContext(ctx, "settlement item did not produce a receiver",
			"rule_id", rule.ID, "biz_type", rule.BizType, "scope_type", rule.ScopeType,
			"scope_ref", rule.ScopeRef, "payment_order_no", in.OrderNo, "reason", note)
	}
	return plan, nil
}

// computeSettlement 按一条规则的项算出各接收方的金额与平台自留。
//
// 纯函数：不碰数据库、不记日志、没有接收者，所以它可以表驱动地测——而这段算法正是「钱分给谁
// 多少」的全部所在。过程中的那几句「本该分出去、结果没分出去」由第三个返回值带出去，调用方
// 记日志。
//
// 算法照 008 与老系统：
//
//	percent   金额 = floor(percentBase × ratio)
//	fixed     金额 = fixed_amount
//	remainder 平台项，不产生接收方；平台金额**永远**是差额倒挤（base − Σ 接收方）——
//	          这是唯一能吸收固定额与取整尾差的算法，也是 008 那条 platform_amount >= 0 的来源
//
// fixed_then_remaining 模式下 percentBase = base − Σfixed（**所有**固定额项，包括账户不可用的
// 那些：那一份照样从基数里扣掉，只是它归平台而不是归门店——与老系统一致）。
//
// # 比例项为什么是向下取整而不是四舍五入
//
// 四舍五入是**逐项**做的，尾差没有地方去。两个各 50% 的项、基数 1313 分（13.13 元）会各自
// 进位到 657，加起来 1314 > 1313——一笔配置完全合法的支付算出「分出去的钱比收进来的多」，
// 然后被下面那条超额兜底（或改之前的「整单归平台」）整单吞掉。奇数分基数下这不是边角情况，
// 是常态。
//
// floor 则天然安全：Σratio ≤ 1 时 **Σfloor(base×ratio) ≤ floor(base×Σratio) ≤ base**，
// 无论有多少项、基数多奇怪，接收方一侧都不可能超过基数。少掉的那几分尾差全部留给平台额
// ——它本来就是差额倒挤出来的，吸收尾差正是它的职责。
//
// 四舍五入只属于**展示**（后台页面把分换成元时），不属于账。
//
// # 两种「算不平」是配置错，直接让这次发起失败
//
//	Σfixed > base                 配置把基数吃穿了
//	Σ接收方 > base                比例之和 > 1（绕过写入口直接写库的数据才会到这）
//
// 这两条**不再**退回「整单归平台」。那条兜底会把一次配置故障静默变成一次错误的分账：
// 钱全留在平台、接收方一行没有、报文里连分账键都不出现（`divisionFor` 见没有接收方就返回
// nil），而库里的任务自洽、恒等式成立、回调照样置 succeeded——**没有任何一处看得出错了**。
// 发起失败是唯一能让人知道「这条规则配坏了」的出口，与 buildSettlementPlan 那句「一次分账
// 错误比一次付不成更贵」是同一条判断。
//
// 剩下的「本该分出去、结果没分出去」只有两种，都不是配置错，所以仍然只记一条 warn 让别人
// 看得见：账户不可用（停用或不属于本渠道）、算法值不认识（绕过 CHECK 写进来的）。
//
// 「不足 1 分的接收方不建行」也是 008 定的（amount > 0 那条 CHECK）：行上的 0 会让
// 「Σ 明细 = base − 平台」要打折才成立，比不拿这几分钱麻烦得多。
func computeSettlement(base int64, allocationMode string, items []repository.SettlementRuleItem) ([]repository.SettlementReceiverLine, int64, []string, error) {
	// 基数是正数、且乘得动比例的分母：base × 1000000 溢出 int64 之后符号会翻，接收方金额
	// 变成负数、被下面那条 `amount <= 0` 静默丢掉——又一次「钱没了但哪儿都没报错」。付不到
	// 922 亿元这个量级，但一条不花成本的判断就能把它从「静默」变成「报错」。
	if base <= 0 || base > math.MaxInt64/settlementRatioScale {
		return nil, 0, nil, fmt.Errorf("%w: base=%d", ErrSettlementBaseUnusable, base)
	}

	percentBase := base
	if allocationMode == model.SettlementAllocationFixedThenRemaining {
		var fixedTotal int64
		for _, item := range items {
			if item.CalcType == model.SettlementCalcFixed {
				fixedTotal += item.FixedAmount
			}
		}
		if fixedTotal > base {
			return nil, 0, nil, fmt.Errorf("%w: 固定分账额合计 %d 分 > 基数 %d 分",
				ErrSettlementBaseOverflow, fixedTotal, base)
		}
		percentBase = base - fixedTotal
	}

	var (
		receivers []repository.SettlementReceiverLine
		total     int64
		notes     []string
	)
	for _, item := range items {
		var amount int64
		switch item.CalcType {
		case model.SettlementCalcPercent:
			amount = scaleFloor(percentBase, item.RatioScaled)
		case model.SettlementCalcFixed:
			amount = item.FixedAmount
		case model.SettlementCalcRemainder:
			// 平台项：金额靠差额倒挤，不产生接收方（008：平台永远没有接收方明细）。
			continue
		default:
			// 008 的 CHECK 保证只有三种 calc_type。真读到一个别的值，是配置被绕过约束写进来的：
			// 不猜它是什么意思，把它当成「这项不产生接收方」——钱归平台，账仍然是平的。
			notes = append(notes, fmt.Sprintf("规则项 %s 的算法 %q 不认识，这一项归平台",
				item.PartyType, item.CalcType))
			continue
		}
		if amount <= 0 {
			// 不足 1 分不分账（008）：算出来是 0 的接收方不建行，不留一行 0。
			continue
		}
		if item.Account == nil {
			// 项还在、账户不可用：停用了，或者挂在别的渠道上。这一份归平台——**要能看见**，
			// 所以它是一条 warn 而不是悄悄跳过。
			notes = append(notes, fmt.Sprintf("主体 %s 的账户不可用（停用或不属于本渠道），其应得的 %d 分归平台",
				item.PartyType, amount))
			continue
		}
		// MerchantRef / BrandRef / StoreRef 三个快照**故意不填**：账户上已经没有这三列了（016），
		// settlement_receivers 上那三列会一直是空串，全仓也没有一处渲染它们。列留着是等哪天要
		// 删的时候单独走一条迁移，不夹在这一刀里。
		receivers = append(receivers, repository.SettlementReceiverLine{
			AccountID:    item.Account.ID,
			PartyType:    item.PartyType,
			PartyName:    item.Account.PartyName,
			ReceiverType: item.Account.ReceiverType,
			ReceiverID:   item.Account.ReceiverID,
			// 固定额项的 ratio 记 0：它的金额在 amount 上，008 的列注释就是这么分的。
			RatioScaled: ratioForSnapshot(item),
			Amount:      amount,
		})
		total += amount
	}

	if total > base {
		// 分出去的钱比收进来的多，只可能是配置错了（同一档上几个比例之和不大于 1 是配置的
		// 责任，库里没有地方钉得住）。**不退回整单归平台**——见文件头「两种算不平是配置错」。
		return nil, 0, nil, fmt.Errorf("%w: 各接收方合计 %d 分 > 基数 %d 分",
			ErrSettlementBaseOverflow, total, base)
	}
	return receivers, base - total, notes, nil
}

// ratioForSnapshot 取落进 settlement_receivers.ratio 的那个数。
//
// 只有比例项有快照比例；固定额项记 0（金额在 amount 上）。分成两个函数而不是在调用处写一个
// 三元表达式，是因为这个口径写在 008 的列注释里，值得有一个能被引用的名字。
func ratioForSnapshot(item repository.SettlementRuleItem) int64 {
	if item.CalcType == model.SettlementCalcPercent {
		return item.RatioScaled
	}
	return 0
}

// scaleFloor 算 base × ratioScaled ÷ 1000000 并**向下取整**到分。
//
// 整数运算，不用浮点：金额一律是 BIGINT 分（008 的第一条），而 float 的尾差会一路走到分账
// 明细、再走到结算单上，对账时表现为几分钱的差。两个操作数都非负，Go 的整数除法就是 floor。
//
// **不是四舍五入**，理由见 computeSettlement 文件头「比例项为什么是向下取整」：逐项四舍五入
// 会让合法的 50/50 规则在奇数分基数上算出比基数还多的一笔钱。
func scaleFloor(base, ratioScaled int64) int64 {
	return base * ratioScaled / settlementRatioScale
}

// 分账计划算不出来时的错误。它们与写入口那批校验错误是两回事：那几个是「这份请求不成立」
// （400，用户改一下再提交），这三个是「这条**已经存下来的**规则与这笔钱凑不出一个能下发的
// 计划」——改请求没用，得回去改规则。所以它们让这次发起失败，并原样落到日志里。
var (
	// ErrSettlementBaseUnusable 基数是 0/负数，或者大到乘上比例分母会溢出 int64。
	ErrSettlementBaseUnusable = errors.New("settlement base amount is unusable")
	// ErrSettlementBaseOverflow 分出去的钱比收进来的多。见 computeSettlement 的说明。
	ErrSettlementBaseOverflow = errors.New("settlement amounts add up to more than the base amount")
)
