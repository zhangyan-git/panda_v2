package service

import (
	"context"
	"log/slog"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
)

// 本文件是后台「包月订阅」详情抽屉的那一次读：订阅本身（本库） + 两块别的域的只读回显
// （续费明细来自支付域，首月支付信息要经订单域再绕到支付域）。
//
// # 它为什么单独一个文件
//
// 因为**降级**是这一步的核心，而它在列表、同步、取消三个动作上都不存在：那三个要么全成、要么
// 报错。这里反过来——抽屉上半部分那十几个字段一个字都不许少，两块附带的回显**坏一块就空一块**。
// 把这段塞回 subscription.go 里，下一个人会照着旁边那几个函数的形状（出错就 return err）把它也
// 改成「出错就 500」。
//
// # 为什么降级是对的，而不是偷懒
//
// 这一页的权限码是 membership:read，运营点进来要回答的问题是「这个人签的是哪一档、下一次什么时候
// 扣、这一期扣到了没」。前两个在本库里，第三个在支付域——而支付域那条连接要的令牌与权限本来
// 就不是这一页的（见 ChargeReader 上那段说明）。为一个附带信息块把整页打成 500，等于让支付服务
// 的一次抖动把「谁签了连续包月」这件事变成不可查，那不划算。
//
// 降级的两处都**记日志**：块空着是给人看的，日志是给我们看的。不记的话「这一块怎么老是没有
// 数据」就只能靠猜。

// GetSubscription 取一条订阅的详情。不存在时回 ErrSubscriptionNotFound → 404。
//
// 只有本库那一次读会返回错误——两个附带块各自降级（见下）。
func (s *MembershipService) GetSubscription(ctx context.Context, id string) (*dto.SubscriptionDetailResponse, error) {
	parsed, err := subscriptionID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.GetSubscription(ctx, parsed)
	if err != nil {
		return nil, err
	}
	base := subscriptionResponse(row)
	return &dto.SubscriptionDetailResponse{
		SubscriptionResponse: base,
		// 协议号直接来自本库那一列（subscriptionColumns 里就有它）。**不复用 base 上的任何派生
		// 字段**：它是渠道那份协议的键，不是展示用的推演值。
		ContractCode: row.ContractCode,
		Charges:      s.subscriptionCharges(ctx, row.ContractCode),
		// 首月订单号取自 base 里那个派生值（FirstPaymentOrderID），不是订阅行上的 order_id：
		// 只有「会员中心」那一档的首月钱才在订阅自己的订单上，另外两条路上那笔钱在咖啡订单或
		// 活动单里——把咖啡订单号当首月支付摆出来是另一笔钱（见 firstPaymentFor）。
		FirstPayment: s.subscriptionFirstPayment(ctx, base.FirstPaymentOrderID),
	}, nil
}

// subscriptionCharges 读这一份协议下的扣款期次，**任何一种读不到都回空数组**。
//
// 三种「没有期次」在这里合并成同一个展示结果（空数组），而它们在日志里是分开的：
//
//	没有协议号          不打支付域。活动发放与后台开通那两条路本来就没有渠道侧，问也是白问——
//	                    这不是异常，所以**不记 warn**（记了会让一篇正常的日志天天有噪音）。
//	没接支付域 / 读失败  记 warn，回空数组。运营看到的是「暂无续费记录」——今天这一格确实没有
//	                    数据可信。
//	有协议、没期次      支付域回空列表（第一个扣款日还没到），原样透出。
func (s *MembershipService) subscriptionCharges(ctx context.Context, contractCode string) []dto.AgreementCharge {
	empty := []dto.AgreementCharge{}
	if strings.TrimSpace(contractCode) == "" || s.charges == nil {
		return empty
	}
	charges, err := s.charges.ListAgreementCharges(ctx, contractCode)
	if err != nil {
		slog.WarnContext(ctx, "membership: failed to read agreement charges",
			"error", err, "contractCode", contractCode)
		return empty
	}
	if charges == nil {
		// 契约上这里是「回非 nil 空切片」（见 ChargeReader 与那份客户端），但换一个实现（或一个
		// 测试里的假实现）时 nil 会从这里漏出去，而它序列化成 JSON 的 null——前端照着数组写渲染，
		// null 会把它打回错误分支。在这一层兜住比在每一处渲染里判空便宜。
		return empty
	}
	return charges
}

// subscriptionFirstPayment 拼「首月支付信息」那一块，**拿不到就回 nil**（前端整块不渲染）。
//
// 它是这条链上唯一一次**两跳**的读：订单域拿 payment_no，再拿它去支付域换渠道流水。两跳各自
// 可以断，而断在不同位置剩下的信息量不同：
//
//	订单读不到          nil —— 只有订单号本身是没用的（上面那一段就是「首月订单号」那一格），
//	                    与其摆一块只有一个号的块，不如整块不渲染。
//	订单读到了、支付没有  回只有订单信息的块 —— 订单号、金额、状态、支付方式、支付时间都在，
//	                    缺的只是渠道流水那一格。**这一格恰恰是要绕这一跳的原因**（出了争议时运营
//	                    拿它去微信商户平台查），所以它空着要记 warn。
//
// 空 orderID 是**正常的今天**（first_payment_order_id 恒为空，小程序端未接），不记日志。
func (s *MembershipService) subscriptionFirstPayment(ctx context.Context, orderID string) *dto.SubscriptionFirstPayment {
	if strings.TrimSpace(orderID) == "" || s.orders == nil {
		return nil
	}
	order, err := s.orders.GetOrder(ctx, orderID)
	if err != nil {
		slog.WarnContext(ctx, "membership: failed to read the first payment order",
			"error", err, "orderId", orderID)
		return nil
	}
	block := &dto.SubscriptionFirstPayment{
		OrderID:       order.OrderID,
		OrderNo:       order.OrderNo,
		Status:        order.Status,
		PaidAmount:    order.PaidAmount,
		PaymentMethod: order.PaymentMethod,
		PaymentNo:     order.PaymentNo,
		PaidAt:        order.PaidAt,
	}
	if strings.TrimSpace(order.PaymentNo) == "" || s.charges == nil {
		// 这张订单还没有支付单（没发起过支付），或者支付域那条连接压根没装配。两种都不值得记
		// warn：前者是订单的正常中间态，后者与上面 subscriptionCharges 那个 nil 同一处置——
		// 展示面上少一格，不该在日志里当成故障喊。
		return block
	}
	payment, err := s.charges.GetPayment(ctx, order.PaymentNo)
	if err != nil {
		slog.WarnContext(ctx, "membership: failed to read the first payment",
			"error", err, "paymentNo", order.PaymentNo)
		return block
	}
	// 只取渠道流水这一格。其余几格（金额、方式、支付时间）**以订单为准**：订单是本服务这条链上
	// 的事实来源，而支付单是支付域视角的另一个形状——两个都填会让这一页出现两套可能对不上的数。
	block.ProviderTransactionID = payment.ProviderTransactionID
	return block
}
