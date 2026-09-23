package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 本文件是「一期扣款的结论落到本域」这一步：建单 + 结算 + 收口，以及建单落不下去时的待办重试。
//
// # 两个入口，一条规则
//
//	事件入口   支付域推来的 charge_succeeded / charge_failed 经 MQ 进来（event.go 的
//	           handleChargeEvent 负责解码、校验、判这一期是成是败），再落到这里
//	待办入口   扣款成功但当时没能建单的那几笔，由 worker 定时重试（SettlePendingCharges）
//
// 两个入口**共用 settleCharge**：收口那一段（重复投递怎么答、订阅已经结束怎么答、哪几条要进死信）
// 只写一份。它们的差别只有一处，而且是有意的——**建单失败时怎么办**：事件入口把这一期落成待办
// （消息不能压在队列里等订单域恢复，见下），待办入口把它推后再试（它本身就是那个重试）。
//
// # 为什么「等一会儿再投」这件事要自己做
//
// 因为平台那条重试路是**毫秒级**的：republishRetry 立即重投、5 次之后 Reject 进
// panda.events.dlq，而那个队列今天既没有消费者也没有监控（见 migrations/membership 的文件
// 头）。所以「订单域重启的那三分钟里到的扣款成功事件」在今天会**全部消失**，而且两边都不报错：
// 会员没续、订单没建，渠道那边的钱是真的动了。
//
// 待办表补的就是这一格：一行 = 一笔收了钱而账没落成的扣款，worker 一分钟一轮地试到落成为止。
// 它**不会放弃**——试满几次就删行等于把这笔钱从账上抹掉，而它是一条对不上账的钱
// （见 membership_charge_settlements 的 last_error 那一列）。

// settleCharge 把一期的结论落进本域，并决定这一次投递该怎么收场（ack 还是进死信）。
//
// # 调用方必须先建单
//
// p.OrderID 是上游为这一期建的续费单，**它必须在调本方法之前就存在**（见 renewal_order.go 的
// 文件头与 event.go 里那段顺序说明）。本方法只管把那个既成事实写下来，不管它从哪来。
//
// # 什么算「做完了」
//
// 三种情况都算，都回 nil：真的写了（outcome.Changed）、这一笔已经结算过（撞幂等键）、这份协议
// 不属于本域。回错误会让平台重投一条「投一万次也是同一个答案」的消息，最后停在死信里——而那里
// 今天没有人看。所以这里能答的问题一律答完。
func (s *MembershipService) settleCharge(ctx context.Context, p repository.ChargeSettleParams) error {
	outcome, err := s.repository.SettleCharge(ctx, p)
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrDuplicateChange):
		// 同一笔渠道流水已经续过了。支付那边对同一次扣款可能发出两遍（通知重投、补发），而
		// **重投不是故障**——正是幂等想要的答案（与 handleOrderPaid 那条逐字相同）。
		return nil
	case errors.Is(err, repository.ErrSubscriptionNotFound):
		// 这份协议不属于本域。绝大多数情况下它压根不是本域的东西（协议是支付域的通用能力），
		// 所以留一条 warn 就够，不让整条队列堵在它上面。真出现「扣了钱却没有订阅」时，这条日志
		// 是唯一的线索。
		slog.WarnContext(ctx, "membership: charge result has no subscription",
			"agreementId", p.AgreementID, "bizPeriod", p.BizPeriod, "target", p.Target)
		return nil
	default:
		// 其中包括 ErrChargeEndedSubscription（一条解约了的订阅扣到了钱）：它要的是人来看，不是
		// 重试，返回错误让它停在死信里（见那条错误的说明）。
		return err
	}

	// 归属对不上：这一期说的签约人与订阅上的不是同一个人。
	//
	// **结算已经做完了**（上面那一步按协议 id 定位，改的是对的那一行），这里报的是另一件事：支付域
	// 记的签约人与本域记的不是同一个人。返回错误让它进死信，**不是为了重做**（重投还是同一条），
	// 而是为了让它停在一个人看得见的地方。
	if outcome.Subscription != nil && outcome.Subscription.UserID != p.UserID {
		return fmt.Errorf("%w: agreement %s belongs to user %s, the charge says %s",
			ErrInvalidEvent, p.AgreementID, outcome.Subscription.UserID, p.UserID)
	}
	if !outcome.Changed {
		// 什么都没写，两种原因、轻重不同，所以分开说：
		//
		//   - 订阅已经结束了（解约 / 走完）：这一期扣成没扣成对它都没有意义。钱没动过的那种不必
		//     惊动人；扣到了钱的那种已经在仓储里报错进了死信，走不到这里。
		//   - 这一笔已经结算过（同一笔渠道流水的重复投递）：**正常**，是幂等生效的样子。
		//
		// 对**待办重试**这条路，前一种也同样是「做完了」：一条待办对应的订阅如果已经结束，重试
		// 一万次也是这个答案，把它留在表里只会每小时 ERROR 一次。
		switch outcome.Subscription.Status {
		case model.SubscriptionStatusCancelled, model.SubscriptionStatusExpired:
			slog.WarnContext(ctx, "membership: charge result for a subscription that is no longer charging",
				"agreementId", p.AgreementID, "bizPeriod", p.BizPeriod, "target", p.Target,
				"subscriptionStatus", outcome.Subscription.Status)
		default:
			slog.InfoContext(ctx, "membership: charge result already settled; nothing to do",
				"agreementId", p.AgreementID, "bizPeriod", p.BizPeriod, "target", p.Target)
		}
		return nil
	}

	// 下面两条是运营要能看见的信号。成功那条走 info：一次正常的续费不该是告警；**停扣走 warn**——
	// 那是这一整条链上唯一一个「从此不再扣这个人的钱」的动作，而它今天不对外发事件。
	if outcome.Suspended {
		slog.WarnContext(ctx, "membership: auto charge suspended after consecutive failures",
			"agreementId", p.AgreementID, "bizPeriod", p.BizPeriod,
			"consecutiveFailedCount", outcome.Subscription.ConsecutiveFailedCount,
			"failureCode", p.FailureCode)
		return nil
	}
	slog.InfoContext(ctx, "membership: charge settled",
		"agreementId", p.AgreementID, "bizPeriod", p.BizPeriod, "target", p.Target,
		"amount", p.Amount, "providerTransactionId", p.ProviderTransactionID)
	return nil
}

// parkChargeSettlement 是「建单失败」那一格的处置：能靠重试解决的就落成待办，其余进死信。
//
// 事件入口调它。待办入口不调——待办入口本身就是那个重试，它只需要把行推后（见
// settlePendingCharge）。
func (s *MembershipService) parkChargeSettlement(ctx context.Context, p repository.ChargeSettleParams, orderErr error) error {
	if !orderFailureCanBeRetried(orderErr) {
		// 坏数据与装配缺失：重投一百次也是同一个答案，照旧返回错误让它停在死信里等人看。
		return orderErr
	}
	if err := s.repository.ParkChargeSettlement(ctx, p); err != nil {
		// 连待办都写不进去（本库也坏了）。两个错误都留着：前者解释了「这一期为什么没落成」，后者
		// 解释了「为什么没能自己兜住」——而这一条返回非 nil 会让平台重投，那是当时唯一的出路。
		return errors.Join(orderErr, fmt.Errorf("park charge settlement: %w", err))
	}
	// ERROR 而不是 warn：这一格意味着**钱已经收了而账没有落**。用户那边看不到任何异常（会员没续
	// 上），所以这条日志是它唯一的存在痕迹。恢复正常时 worker 会补上，并再记一条 info。
	slog.ErrorContext(ctx, "membership: a succeeded charge has no renewal order yet; parked for retry",
		"agreementId", p.AgreementID, "bizPeriod", p.BizPeriod, "amount", p.Amount,
		"providerTransactionId", p.ProviderTransactionID, "error", orderErr)
	// ack：这一行待办比那条消息耐久，而消息留在队列里只会变成一条没人看的死信。
	return nil
}

// orderFailureCanBeRetried 判一次建单失败**能不能靠重试解决**。
//
// 能重试的：订单域没答上来（连不上、超时、应答是坏的——client.ErrOrderUnavailable）、本库读快照
// 时抖了一下。这些都是「过一会儿就好了」，也正是待办表存在的理由。
//
// 不能重试的，逐条都在下面，它们要么重投不会变（我们自己的 bug 写出的事件），要么重投只会更糟
// （对面明确拒了）。这些照旧返回错误进死信——那里是人会来看的地方。
//
// **默认那一支是「能重试」**，这不是偷懒：判错的代价两侧不对称。把一条其实重试不了的判成能重试，
// 代价是一张吵闹的待办（每小时一条 ERROR，人来看一眼就能定性、删掉）；反过来（把能重试的判成不
// 能重试）的代价是**这笔钱从此没有人管**——它不会报错，只是不再被提起。这与「宁可留下痕迹」在
// 这一整个服务里是同一条规矩。
func orderFailureCanBeRetried(err error) bool {
	switch {
	case errors.Is(err, ErrInvalidEvent):
		// 事件体本身坏了（比如成功事件没带渠道流水号）。重投不会让那个字段出现。
		return false
	case errors.Is(err, ErrChannelUnavailable):
		// 这个进程压根没接订单域（装配缺失）。重投也不会把它装上——那是要改部署的事。
		return false
	case errors.Is(err, repository.ErrMembershipNotFound):
		// 订阅挂在一个不存在的会员上，是一行坏数据（membership_id 上有外键，正常写不出来）。
		return false
	case errors.Is(err, client.ErrRenewalOrderRejected),
		errors.Is(err, client.ErrRenewalOrderConflict),
		errors.Is(err, client.ErrOrderNotFound):
		// 订单域**答了**，答案是「这单我不建」：报文它不收、这个流水号挂在别种的单上、或者它说
		// 查无此单。三种都是结论，重投一遍只会拿到同一个结论。
		return false
	default:
		return true
	}
}

// SettlePendingCharges 重试一批「钱收了、账没落成」的待办，返回这一轮**落成了几条**。
//
// 返回的那个数只用来写日志、以及让 worker 判断这一轮是不是满载（与 ChargeDue 同义）；它不代表
// 这一期一共落成了多少。
//
// 没有待办时它什么都不做，代价是那条索引上的一次查找——所以它可以跑得比别的扫描勤（一分钟一轮），
// 让订单域恢复之后的那一分钟里积压就开始消化。
func (s *MembershipService) SettlePendingCharges(ctx context.Context, limit int) (int, error) {
	due, err := s.repository.ClaimDueChargeSettlements(ctx, limit)
	if err != nil {
		return 0, err
	}

	settled := 0
	for _, row := range due {
		// **一行失败不拦后面的**：这一批里剩下的行走的是同一个订单域，而失败的那一条可能是它自己
		// 的事（比如那一行数据坏了）。没成的那条已经被推后了（见 rescheduleSettlement），下一轮
		// 还会被捞到。
		if err := s.settlePendingCharge(ctx, row); err != nil {
			slog.ErrorContext(ctx, "membership: retry a parked charge settlement",
				"providerTransactionId", row.ProviderTransactionID, "agreementId", row.AgreementID,
				"bizPeriod", row.BizPeriod, "attempts", row.Attempts, "error", err)
			continue
		}
		settled++
	}
	return settled, nil
}

// settlePendingCharge 走完一条待办：建单 → 结算 → 删行。
//
// # 三步都幂等，所以「多做一遍」永远不会是灾难
//
// 建单的幂等键是渠道流水号（订单域那侧命中同一张单），结算的幂等键也是它（本库撞
// membership_changes.request_id 那条唯一索引）。这不是巧合——**待办能存在的前提就是这两步都能
// 被重复执行**，否则这张表本身就成了「可能扣两次钱」的东西。所以认领那条租约到期（worker 崩在
// 半路上）之后别人接着做，是安全的。
//
// # 任何一步失败都只推后，不删行
//
// 包括装配缺失（没接订单域）那种「重试不会变好」的：待办里存的是一笔真收了的钱，**删行等于把它
// 抹掉**，所以这条路上一律保留、一直试，让它在日志里吵下去，直到有人把根因修好。这与事件入口的
// 处置不同（那边有死信队列可以交出去），差别是有意的。
func (s *MembershipService) settlePendingCharge(ctx context.Context, row *model.ChargeSettlement) error {
	params := chargeSettleParamsOf(row)

	orderID, err := s.recordRenewalOrderFor(ctx, params.Target, params.AgreementID, params.ProviderTransactionID, params.Amount)
	if err != nil {
		return s.rescheduleSettlement(ctx, row, err)
	}
	params.OrderID = orderID

	if err := s.settleCharge(ctx, params); err != nil {
		return s.rescheduleSettlement(ctx, row, err)
	}
	if err := s.repository.DeleteChargeSettlement(ctx, row.ProviderTransactionID); err != nil {
		// 账已经落完了，只是这一行没删掉。回错误只为让上面那句日志说清「是删行失败」——下一轮会
		// 拿着同一笔流水把两步再做一遍（都幂等，见本方法开头），然后把它删掉。
		return err
	}
	// 这一条是运营看得见的收口：一个曾经「钱收了、账没落」的人现在对上了。
	slog.InfoContext(ctx, "membership: parked charge settled",
		"providerTransactionId", row.ProviderTransactionID, "agreementId", row.AgreementID,
		"bizPeriod", row.BizPeriod, "amount", row.Amount, "orderId", params.OrderID,
		"attempts", row.Attempts)
	return nil
}

// rescheduleSettlement 把一条待办推后再来，并记下这一次为什么没成。
//
// 推后的间隔按**已经试过几次**算（见 settlementBackoff）：一次订单域重启顶多让这一行等一分钟，
// 而一张永远修不好的待办最终会退到一个小时一次，不再占着日志。
//
// 推后失败（本库写不进去）时不动那一行：它的 next_attempt_at 还是认领时推过的租期（见
// ClaimDueChargeSettlements），到期自然会被再捞起来。这是认领即推后那条写法顺手带上的兜底——
// 所以这里**不需要**再加一次「推后失败就删行」之类的补偿。
func (s *MembershipService) rescheduleSettlement(ctx context.Context, row *model.ChargeSettlement, failure error) error {
	next := s.Now().Add(settlementBackoff(row.Attempts))
	if err := s.repository.RescheduleChargeSettlement(ctx, row.ProviderTransactionID, next, failure.Error()); err != nil {
		return errors.Join(failure, fmt.Errorf("reschedule charge settlement: %w", err))
	}
	return failure
}

// chargeSettleParamsOf 把一条待办翻成一次结算的入参。
//
// OrderID 留空：待办发生的时候它还不存在，由建单那一步补上（见 settlePendingCharge）。失败原因的
// 两列也留空：待办只可能是**成功**那一期（失败那一期压根不建单，见 recordRenewalOrderFor），而
// 成功那一期没有失败原因可言。
func chargeSettleParamsOf(row *model.ChargeSettlement) repository.ChargeSettleParams {
	return repository.ChargeSettleParams{
		AgreementID:           row.AgreementID,
		UserID:                row.UserID,
		Target:                row.Target,
		BizPeriod:             row.BizPeriod,
		Amount:                row.Amount,
		ProviderTransactionID: row.ProviderTransactionID,
		// 用**事件到达的那个时刻**，不是重试的时刻：它是续期的起点候选之一（这个人的会员已经过期
		// 时就从这一刻重新起算），用重试时刻会让「这一期从哪天开始」随一次订单域抖动而漂。
		OccurredAt: row.OccurredAt,
		TraceID:    row.TraceID,
	}
}

// settlementBackoff 是待办两次重试之间的间隔：2^(attempts-1) 秒，封顶一小时。
//
// attempts 是**认领时加过一**的那个数（见 ClaimDueChargeSettlements），所以第一次失败时它是 1。
// 前几档（1 秒到半分钟）全都短于 worker 的扫描周期，实际效果就是「下一轮再试」——订单域重启
// 两三分钟这种最常见的抖动，一分钟一轮地追着试最划算。从第七次（64 秒）起退避开始超过那个周期，
// 之后逐次翻倍，第十三次（2^12 秒）越过一小时被削平：一张永远修不好的待办会安静下来。
//
// **封顶而不是放弃**：放弃等于把这笔钱从账上抹掉（见 settlePendingCharge 的说明）。
func settlementBackoff(attempts int) time.Duration {
	// 上界只管一件事：别让移位溢出。取到 2^12 秒（约 68 分钟）刚好越过下面那个一小时的封顶，
	// 所以再往后削平的结果都一样，没有必要也不该让它继续翻。
	const maxShift = 13
	if attempts < 1 {
		attempts = 1
	}
	if attempts > maxShift {
		attempts = maxShift
	}
	backoff := time.Second << (attempts - 1)
	if backoff > settlementBackoffCap {
		return settlementBackoffCap
	}
	return backoff
}

// settlementBackoffCap 是退避的上界，见 settlementBackoff。
const settlementBackoffCap = time.Hour
