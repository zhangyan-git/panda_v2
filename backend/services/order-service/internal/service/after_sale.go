package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// maxAfterSaleImages 是凭证图片的条数上限。
//
// 不是业务规则，是防呆：真需要十几张图才说得清的事故，客服会另有渠道要材料。设上限是为了
// 让「客户端 bug 塞了一万条 URL」在进库前停住，而不是变成一条几十 KB 的 JSONB。
const maxAfterSaleImages = 9

// ApplyAfterSaleInput 是一次用户发起的退款申请。
//
// 没有金额字段（见 dto.ApplyAfterSaleRequest）：退多少由服务端在事务里算。
type ApplyAfterSaleInput struct {
	OrderID        string
	UserID         string
	IdempotencyKey string
	Scope          string
	OrderLineID    *string
	Reason         string
	Images         []string
	TraceID        string
	ActorID        *string
}

// ApplyAfterSale 受理一次退款申请。
//
// 这一层只管**请求的形状**：范围与行是否自洽、理由有没有、图片是不是 URL。金额、状态、
// 互斥（同一行是不是已经有一张在跑的售后单）全都由仓储在锁内判——那些判据要看账上的
// 事实，在这里判等于在锁外判，并发下就是错的。所以这里不读订单、不读售后单。
func (s *OrderService) ApplyAfterSale(ctx context.Context, in ApplyAfterSaleInput) (*repository.AfterSaleRow, bool, error) {
	in.OrderID = strings.TrimSpace(in.OrderID)
	in.Scope = strings.TrimSpace(in.Scope)
	in.Reason = strings.TrimSpace(in.Reason)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)

	if in.OrderID == "" {
		// 不给 id 的申请没有可指的对象。回「不存在」而不是「参数缺失」：与 CancelOrder
		// 一致，路径参数的空值对调用方来说就是一个查不到的订单。
		return nil, false, ErrOrderNotFound
	}
	if in.IdempotencyKey == "" {
		return nil, false, ErrIdempotencyKeyRequired
	}
	if in.UserID == "" {
		return nil, false, ErrUserRequired
	}
	switch in.Scope {
	case model.AfterSaleScopeAll, model.AfterSaleScopeDrink, model.AfterSaleScopeAddon:
	case model.AfterSaleScopeMembership:
		// 会员权益是否收回、已用天数怎么折算，规则在 membership-service（未建）。现在开这个
		// 口子只会产生一张没人执行得了的售后单。
		return nil, false, ErrAfterSaleMembershipUnsupported
	default:
		return nil, false, ErrAfterSaleScopeInvalid
	}

	// scope 与行是等价关系：整单退不带行，按行退必须带行。DB 的
	// order_after_sales_scope_matches_line 也这么约束，这里先挡一次是为了给出人话，
	// 而不是让调用方吃一句 CHECK 约束名。
	lineID := ""
	if in.OrderLineID != nil {
		lineID = strings.TrimSpace(*in.OrderLineID)
	}
	if in.Scope == model.AfterSaleScopeAll {
		if lineID != "" {
			return nil, false, ErrAfterSaleLineNotAllowed
		}
		in.OrderLineID = nil
	} else {
		if lineID == "" {
			return nil, false, ErrAfterSaleLineRequired
		}
		in.OrderLineID = &lineID
	}

	if in.Reason == "" {
		// 申请理由不是形式：客服没有理由就只能在「同意」和「拒绝」之间凭感觉选。
		return nil, false, ErrAfterSaleReasonRequired
	}
	images, err := normalizeAfterSaleImages(in.Images)
	if err != nil {
		return nil, false, err
	}

	// 受理之前先过一遍那条业务规则：**这一单赠送的福卡一张都没被用过，才允许申请退款**。
	//
	// 放在这里而不是仓储的锁内，是因为判据在**另一个服务**里（福卡的流水在账户域）：把它
	// 塞进锁里等于握着一行订单的锁去等一个下游。这是一次便宜的先看一眼，权威的那一次仍然
	// 在锁内——下面这次调用会重读订单、重拆一遍键。
	//
	// 顺序上它在形状校验之后、落库之前：形状不对的请求（scope 与行对不上、没写理由）先
	// 拿到它们各自该拿的错，不会先撞上这一句。
	if plan, checkable, err := s.repository.FortuneCardFreezeGate(ctx, in.OrderID, in.UserID, in.Scope); err != nil {
		return nil, false, err
	} else if checkable && plan.Cards > 0 {
		if err := s.assertFortuneCardsUnused(ctx, in.UserID, plan); err != nil {
			return nil, false, err
		}
	}

	// 单号在这里生成，重放时会被作废（仓储回放已有结果，不落第二张单）——同下单那条路。
	row, replayed, err := s.repository.ApplyAfterSale(ctx, repository.ApplyAfterSaleParams{
		IdempotencyKey: in.IdempotencyKey,
		TraceID:        in.TraceID,
		RequestID:      in.TraceID,
		OrderID:        in.OrderID,
		UserID:         in.UserID,
		AfterSaleNo:    generateAfterSaleNo(time.Now()),
		Scope:          in.Scope,
		OrderLineID:    in.OrderLineID,
		Reason:         in.Reason,
		Images:         images,
		ActorType:      model.ActorUser,
		ActorID:        in.ActorID,
	})
	if err != nil {
		return nil, false, mapWriteError(err)
	}
	return row, replayed, nil
}

// assertFortuneCardsUnused 执行那条业务规则：一单赠送的福卡一张都没被用过，才允许申请退款。
//
// # 判据为什么只能是「冻得上几张」
//
// 福卡的流水是一口**池子**：抽奖扣的那一笔只记 `draw:{requestId}`，从不指向它消耗的是哪
// 一次发放。所以「这一单送的那几张还在不在」在库里没有直接答案。能问出来的只有账户域的
// 那两个数，判据因此是：这几笔发放还挂着的张数（Granted）与此刻真的冻得上的张数
// （Freezable，= min(还挂着的, 账户可用)）。冻不满承诺的张数，就是「这片池子里已经有卡
// 被抽走了」——退钱也追不回来。这是池子模型的必然结论，不是这里的近似。
//
// # 两个数不能合成一个
//
// Granted 为 0 是**「发放还没落库」**，不是「用过了」：一张订单可以先申请退款再完成
// （申请早于发放是设计支持的，那时冻结行是个空壳，等发放落库时由账户域补上）。只看
// Freezable 的话这两种情形都是 0——一种是正常的，另一种要给用户一句拒绝，混了就把一条
// 正常路径堵死了。
//
// # 拒了之后
//
// 用户手上的卡没有被动过：**受理之前**就拒了，所以没有冻结行要解、没有任何补偿要做。
// 这与过去的行为正好相反——过去是照收申请、冻结时悄悄钳住，等到退款成功才发现有几张
// 追不回来。审核时那个人工确认闸门（ErrFortuneCardConfirmationRequired）仍然在，它现在
// 是第二道：申请时判得出来就不到人那里，申请之后到审核之间又抽掉的，由它兜。
func (s *OrderService) assertFortuneCardsUnused(ctx context.Context, userID string, plan repository.FortuneCardFreezePlan) error {
	if s.fortuneCards == nil {
		// 「这个部署没接账户域」。**不能当成放行**：这条规则挡的是「卡已经抽掉了还想退钱」，
		// 静默失效的代价是把追不回来的卡退出去。回 503 让调用方重试。
		return ErrFortuneCardQuoteUnavailable
	}

	quote, err := s.fortuneCards.FreezeQuote(ctx, client.FreezeQuoteInput{
		UserID:    userID,
		EntryKeys: plan.EntryKeys,
	})
	if err != nil {
		// 问不到就是没结论（见 ErrFortuneCardQuoteUnavailable）：既不放行，也不说「你的卡
		// 用过了」——那是另一个结论，只有拿到数才敢下。
		return err
	}
	if quote.Granted == 0 {
		// 还没发卡。这不是「用过了」：冻结会照建，等发放落库时补上。
		return nil
	}
	if quote.Freezable < plan.Cards {
		return fmt.Errorf("%w: order promised %d cards, only %d freezable",
			ErrAfterSaleFortuneCardsUsed, plan.Cards, quote.Freezable)
	}
	return nil
}

// ReviewAfterSaleInput 是后台的一次审核决定。
type ReviewAfterSaleInput struct {
	AfterSaleNo string
	// Action 取 model.AfterSaleActionApprove / AfterSaleActionReject（写在路径上）。
	Action                     string
	Remark                     string
	FortuneCardUnusedConfirmed bool
	// ReviewedBy 是审核人，来自令牌（subject）。
	ReviewedBy string
	TraceID    string
}

// ReviewAfterSale 审核一张售后单：通过或驳回。
//
// **通过 = 同意退这笔钱 + 当场去退。** 它不是「审核完就完了」：审核通过之后紧接着调支付域
// 建退款单，再落 refunding（见 service/refund.go 顶上那三段）。分三步而不是一步，是因为
// 中间那次调用是网络调用，不能待在事务里，而**每一段停下来都是一个准确的描述**：
//
//	挂在事务 A 与调用之间 → approved：同意退，退款单还没建成（点重试）
//	调用回来了、事务 B 失败 → 还是 approved：同上，重试会命中幂等键拿回同一张退款单
//	两段都成了 → refunding：钱在路上了，等支付侧的退款结果事件
//
// 前两种都回 ErrRefundNotStarted：审核**已经生效**了，只是钱还没上路。后台要把这两件事
// 分开告诉操作人，否则他会再点一次「通过」，而第二次只会得到「不在待审核状态」。
func (s *OrderService) ReviewAfterSale(ctx context.Context, in ReviewAfterSaleInput) (*repository.AfterSaleRow, error) {
	in.AfterSaleNo = strings.TrimSpace(in.AfterSaleNo)
	in.Remark = strings.TrimSpace(in.Remark)

	if in.AfterSaleNo == "" {
		return nil, ErrAfterSaleNotFound
	}
	var target string
	switch in.Action {
	case model.AfterSaleActionApprove:
		target = model.AfterSaleStatusApproved
	case model.AfterSaleActionReject:
		target = model.AfterSaleStatusRejected
		// 驳回是「不退」这个结论，不写理由的话用户拿到的只是一句「申请未通过」，
		// 客服第二天也说不清当时为什么拒。
		if in.Remark == "" {
			return nil, ErrAfterSaleRemarkRequired
		}
	default:
		return nil, ErrAfterSaleActionInvalid
	}
	if in.ReviewedBy == "" {
		// 审核人取自令牌。没有 subject 就没法回答「是谁同意的这笔退款」——那正是审计要的
		// 东西，所以宁可拒绝这次操作（同 cancel 的管理员路径）。
		return nil, ErrForbidden
	}
	if !CanTransitionAfterSale(model.AfterSaleStatusPending, target) {
		// 表里没这条路说明状态机被改坏了，而不是这一单不能审。这种「不可能」要炸出来，
		// 而不是回一句 409 让调用方以为是自己晚了一步。
		return nil, ErrAfterSaleNotPending
	}

	row, err := s.repository.ReviewAfterSale(ctx, repository.ReviewAfterSaleParams{
		AfterSaleNo:                in.AfterSaleNo,
		Action:                     in.Action,
		Remark:                     in.Remark,
		FortuneCardUnusedConfirmed: in.FortuneCardUnusedConfirmed,
		ReviewedBy:                 in.ReviewedBy,
		RequestID:                  in.TraceID,
		TraceID:                    in.TraceID,
	})
	if err != nil {
		return nil, mapWriteError(err)
	}
	if in.Action != model.AfterSaleActionApprove {
		// 驳回到此为止：没有钱要退，也没有下游动作。
		return row, nil
	}

	// 事务外的第二步与第三步。支付域没配（payments 为 nil）时这里会明确失败，而**审核已经
	// 落库了**——这与「审核没生效」是两句不同的话，由 ErrRefundNotStarted 分开（见上面的
	// 函数注释）。所以这里不把 row 丢掉：调用方拿它看得到「批是批了」。
	started, err := s.startRefund(ctx, row, in.ReviewedBy, in.TraceID)
	if err != nil {
		return nil, err
	}
	return started, nil
}

// CancelAfterSaleInput 是用户撤销自己的一次退款申请。
type CancelAfterSaleInput struct {
	AfterSaleNo string
	UserID      string
	Reason      string
	TraceID     string
}

// CancelAfterSale 撤销一张还没被审核的售后单。
//
// 只能撤 pending（仓储在锁内判）：审核通过之后钱已经在路上了，撤销不再由用户发起。
func (s *OrderService) CancelAfterSale(ctx context.Context, in CancelAfterSaleInput) (*repository.AfterSaleRow, error) {
	in.AfterSaleNo = strings.TrimSpace(in.AfterSaleNo)
	in.Reason = strings.TrimSpace(in.Reason)

	if in.AfterSaleNo == "" {
		return nil, ErrAfterSaleNotFound
	}
	if in.UserID == "" {
		return nil, ErrUserRequired
	}
	if in.Reason == "" {
		// 撤销可以不写理由（用户点错了一次不需要解释），但状态流水里的 reason 空着会让
		// 「这张单为什么走到 cancelled」查不出来，所以补一句默认的。
		in.Reason = "用户撤销退款申请"
	}
	if !CanTransitionAfterSale(model.AfterSaleStatusPending, model.AfterSaleStatusCancelled) {
		return nil, ErrAfterSaleNotPending
	}

	row, err := s.repository.CancelAfterSale(ctx, repository.CancelAfterSaleParams{
		AfterSaleNo: in.AfterSaleNo,
		UserID:      in.UserID,
		Reason:      in.Reason,
		RequestID:   in.TraceID,
		TraceID:     in.TraceID,
	})
	if err != nil {
		return nil, mapWriteError(err)
	}
	return row, nil
}

// ListAfterSales 读售后单列表（后台）。
//
// 这一层不做任何筛选判断：能不能看、看哪些，由路由上的权限码与调用方传进来的条件决定。
func (s *OrderService) ListAfterSales(ctx context.Context, f repository.AfterSaleFilter) ([]*repository.AfterSaleRow, int, error) {
	items, total, err := s.repository.ListAfterSales(ctx, f)
	if err != nil {
		return nil, 0, mapWriteError(err)
	}
	return items, total, nil
}

// normalizeAfterSaleImages 校验凭证图片并序列化成要写进 JSONB 的字节。
//
// 只收 http(s) URL，不收文件也不收 base64：上传链路（对象存储、签名、尺寸校验）不在本
// 服务里，这里能保证的只有「它有 host、协议不是 javascript:、条数没到一万」。
func normalizeAfterSaleImages(images []string) ([]byte, error) {
	if len(images) > maxAfterSaleImages {
		return nil, ErrAfterSaleImagesInvalid
	}
	cleaned := make([]string, 0, len(images))
	for _, raw := range images {
		value := strings.TrimSpace(raw)
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, ErrAfterSaleImagesInvalid
		}
		cleaned = append(cleaned, value)
	}
	// 空数组序列化成 []，不是 null：列的默认值就是 '[]'，让「没传图」与「传了空数组」
	// 在库里长得一样，读出来才不用判两种空。
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// generateAfterSaleNo 生成售后单号：REF + YmdHis + 6 位数字。
//
// 前缀不用 3CYM：那个前缀是收单渠道对**商户订单号**的规范要求（见 generateOrderNo），
// 售后单号不过渠道，用 REF 让客服一眼把订单号、退款单号分清楚。后 6 位与订单号同一个
// 取法（毫秒后三位 + 三位密码学随机数），撞号靠 after_sale_no 的唯一索引兜住。
func generateAfterSaleNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// 与 generateOrderNo 同一个理由：宁可炸，也不要退回一个由时间戳完全决定的单号。
		panic(fmt.Sprintf("order-service: read random for after sale number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("REF%s%03d%06d", now.Format("20060102150405"), now.UnixNano()%1000, tail%1000000)
}
