package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// RefundRequest 是一次发起退款的输入。
//
// 与 CreateRequest 一样，**金额由调用方权威给出**：order-service 在锁内校验完售后单之后
// 把 refund_amount 传进来。本服务只校验它不超过这张支付单的可退余额，不读订单库。
type RefundRequest struct {
	// AfterSaleNo 是 order-service 的售后单号。**幂等键**——同号重发拿回同一张退款单。
	AfterSaleNo string
	// PaymentNo 是要退的那一张支付，值引用（payments.payment_no）。必须已收妥。
	PaymentNo string
	// Amount 单位为分，是这一笔退款的总额。
	Amount int64
	// Reason 进渠道账单也进退款单的 reason 列。可空。
	Reason string
	// OrderLineID 是 order_lines.id，值引用；整单退为空。
	OrderLineID string
	// RequestID 是这一次调用的请求号，只用于流水与日志。**它不参与幂等**：这条链的幂等键
	// 是售后单号，那才是「一次退款只该发生一次」的出处。
	RequestID string
	// TraceID 只进日志与 payment_provider_calls。
	TraceID string
}

// RefundResult 是发起退款的结果，也是 gRPC 响应与控制器 JSON 共用的同一个形状。
//
// 与 CreateResult 三处共用同一个结构是同一条理由：两个结构各组装一遍必然漂移。
type RefundResult struct {
	RefundNo string `json:"refundNo"`
	// pending / processing / succeeded / failed，见 model 里那组常量。
	Status           string `json:"status"`
	ProviderRefundID string `json:"providerRefundId"`
	FailureCode      string `json:"failureCode"`
	FailureMessage   string `json:"failureMessage"`
}

// fundingLineTypeCoffeeBean 是「这一笔钱来自咖啡豆账本」那条出资行的 line_type。
//
// 从前这里另有一个本地常量 `fundingTypeCoffeeBean`，理由是「退款判的是落库的历史值，不是
// 用户今天选的方式」——出资渠道词表退场之后这句话不再成立：**出资行存的本来就是支付方式的
// code**（见 payment/012），所以落库的历史值与 catalog 的常量是同一个东西，再抄一份只会
// 多一个能对不上的地方。
const fundingLineTypeCoffeeBean = catalog.CodeCoffeeBean

// CreateRefund 为一笔已收妥的支付发起退款。
//
// 形状与 CreatePayment 逐字同源：三段事务，**渠道网络调用绝不放在 PG 事务里**。
//
//	事务 1  锁住支付单 + 算可退余额 + 建退款单(pending) + 逐行冲正计划(pending)
//	事务外  Operator.Execute(OperationRefund) + 记录 payment_provider_calls
//	事务 2  SUCCESS  → refunds.succeeded + refund_fundings.succeeded
//	                + payment_fundings.status='reversed' + 资金流水 + 状态流水 + outbox
//	事务 3  FAIL     → refunds.failed + failure_code/message + outbox
//	        PROCESSING / UNKNOWN → refunds.processing，**不发事件**，交给退款查询 worker
//
// # 与发起支付最大的一处不同：没有幂等键要抢占
//
// 发起支付要抢 payment_idempotency_keys（同一个 request_id 只落一张单），退款不用抢——
// `payment_refunds.after_sale_no` 整表唯一，**退款单自己就是那把钥匙**。所以这里没有
// beginPayment / abandonPaymentAttempt 那一对，也就没有「一次没走完的尝试要退回幂等键」这
// 件事：重发同一个售后单号会命中 BeginRefund 的那条查回分支，拿回上次那张单，接着往下推。
//
// # 账户出资那条路为什么不发渠道请求
//
// 一行出资走不走渠道，看它的 line_type：`coffee_bean` 是账户账本里的钱，**没有第三方要问**。
// 而且那笔冲正**不由本服务发起**——account-service 自己消费 `order.after_sale.refunded`
// （本服务的退款成功事件经订单域转出来的那一条），在退款成功那一刻把豆退回去（幂等键
// `after_sale:{afterSaleNo}`）。所以本服务在这里做的只是把那一行记账成已冲正，**不再发一次
// 冲正请求**：发第二次会被账户域的幂等键挡住，但那样一来「谁在什么时候冲正」就有两处说法
// （见 BeanLedger 的注释）。两件事落在同一拍上：钱退成了，豆才回来。
//
// 纯账户出资的单（route.Channel == nil）因此整条路都不碰渠道，建单即完成。
func (s *PaymentService) CreateRefund(ctx context.Context, in RefundRequest) (*RefundResult, error) {
	if err := validateRefund(in); err != nil {
		return nil, err
	}

	payment, err := s.repository.FindPaymentByNo(ctx, in.PaymentNo)
	if err != nil {
		return nil, err
	}
	if payment.Status != model.PaymentSucceeded {
		// 只有收妥的钱能退。这里先挡一道是因为**渠道那边挡不住**：对一张还没收妥的单发起
		// 退款，渠道的回答会是一句与本意无关的「查无此单」，而那句话会盖住真正的原因。
		return nil, fmt.Errorf("%w: payment %s is %s", repository.ErrPaymentNotRefundable,
			payment.PaymentNo, payment.Status)
	}

	refundNo := s.newRefundNo(s.now())
	refund, fundings, replayed, err := s.repository.BeginRefund(ctx, repository.BeginRefundParams{
		RefundNo:    refundNo,
		PaymentNo:   payment.PaymentNo,
		AfterSaleNo: in.AfterSaleNo,
		OrderLineID: in.OrderLineID,
		Amount:      in.Amount,
		Reason:      in.Reason,
		RequestID:   in.RequestID,
	})
	if err != nil {
		return nil, err
	}
	if replayed && refund.Status != model.RefundPending {
		// 上一次调用已经有结论了（processing / succeeded / failed），把当时的状态原样交回。
		// **不重发渠道请求**：processing 的那笔正由退款查询 worker 跟着，再发一次是在两张
		// 单上问同一件事。
		return refundResult(refund), nil
	}

	// 走到这里要么是刚建的单，要么是上次停在 pending 的那一张（建完单、发起之前进程死了）。
	// 后者重发一次渠道请求是安全的：渠道按 (商户单号, 退款单号) 判幂等，同号回的是上一次
	// 那张退货单的结果，不会再退一次钱。
	if err := s.dispatchRefund(ctx, payment, refund, fundings, in); err != nil {
		return nil, err
	}
	// dispatchRefund 只负责把结论落下来，落完的最终状态从库里读回来——那才是权威的那一份
	// （并发下可能就是别人写的），不在这里猜。
	settled, _, err := s.repository.FindRefundByNo(ctx, refund.RefundNo)
	if err != nil {
		return nil, err
	}
	return refundResult(settled), nil
}

// dispatchRefund 是退款的第二段：手上已经有一张刚建的（或上次停在 pending 的）退款单，
// 把它推到结论。
//
// 返回 error 表示**这一次尝试没有结论**（渠道没注册、请求没发出去），退款单停在 pending，
// 调用方重发同一个售后单号会接着走。渠道的明确拒绝**不是** error——那是一个结论，
// 落在 status='failed' 上。
func (s *PaymentService) dispatchRefund(ctx context.Context, payment *model.Payment, refund *model.Refund, fundings []*model.RefundFunding, in RefundRequest) error {
	channelAmount, accountLineTypes := splitRefundFundings(fundings)

	// 一行走渠道的都没有：整张退款单在本地就能结掉。
	//
	// 这不是「渠道不可用」的兜底，而是账户出资那条路**正常**的形状：豆在售后审核通过那一刻
	// 就已经被 account-service 冲正了，这里没有任何第三方可问。
	if channelAmount == 0 {
		_, err := s.repository.MarkRefundSucceeded(ctx, repository.MarkRefundSucceededParams{
			RefundID:      refund.ID,
			SucceededAt:   s.now(),
			NoOpLineTypes: accountLineTypes,
			TraceID:       in.TraceID,
		})
		return err
	}

	operator, route, adapterMethod, secrets, err := s.refundOperator(payment)
	if err != nil {
		return err
	}

	request := provider.OperationRequest{
		Operation: provider.OperationRefund,
		PaymentNo: payment.PaymentNo,
		OrderNo:   payment.OrderNo,
		RefundNo:  refund.RefundNo,
		// **退的是走渠道的那一部分，不是退款单的总额**：混合出资时账户那一半不该发到渠道去。
		// 今天两者恒等（出资恒为一行），但这个值只在这里算一次，多行落地时不用再改。
		Amount:    channelAmount,
		Reason:    refund.Reason,
		RequestID: in.RequestID,
		Method:    adapterMethod,
		Secrets:   secrets,
	}

	startedAt := s.now()
	operationResult, callErr := operator.Execute(ctx, request)
	s.recordOperationCall(ctx, in.TraceID, payment, route, model.CallOperationRefund,
		operationFactsOf(request), operationResult, callErr,
		int(s.now().Sub(startedAt).Milliseconds()))

	return s.settleRefundOperation(ctx, payment, refund, accountLineTypes,
		model.RefundPending, operationResult, callErr)
}

// refundOperator 解析出这一笔支付对应的渠道适配器、路由与凭据。
//
// 退款与退款查询两条路共用它：两条路问的是同一家渠道、用同一批凭据，而「用哪一套」这件事
// 只该有一处答案——分两份写迟早出现「发起时用的 B 渠道、查询时问的是 C 渠道」。
func (s *PaymentService) refundOperator(payment *model.Payment) (provider.Operator, paymentRoute, provider.Method, provider.Credentials, error) {
	route, err := s.resolveMethod(payment.PaymentMethod)
	if err != nil {
		return nil, paymentRoute{}, provider.Method{}, nil, err
	}
	if route.Channel == nil {
		// 账户出资的单不会有渠道操作要做：它在 dispatchRefund 里就地结掉了，查询那条路上更
		// 不该出现（那种退款不会停在 processing）。真走到这里说明数据被人改过，明确报错好过
		// 拿着 nil 渠道往下走。
		return nil, paymentRoute{}, provider.Method{}, nil,
			fmt.Errorf("%w: payment %s has no channel for a refund operation",
				ErrProviderOperationUnsupported, payment.PaymentNo)
	}
	adapter, err := s.providers.Lookup(route.Channel.Provider)
	if err != nil {
		return nil, paymentRoute{}, provider.Method{}, nil, err
	}
	// 探不到 Operator = 这一族没有退款这个能力。**维持现状、退回 error**，不把退款单标成
	// failed：那是配置问题（或者调用方选了一条不支持退款的支付方式），不是「钱退不回去」。
	// 标 failed 会让售后单落到一个终态，而其实换条路就能退。
	operator, ok := adapter.(provider.Operator)
	if !ok {
		return nil, paymentRoute{}, provider.Method{}, nil,
			fmt.Errorf("%w: provider %s does not implement refund operations",
				ErrProviderOperationUnsupported, route.Channel.Provider)
	}
	method := route.providerMethod()
	return operator, route, method, resolveSecrets(s.resolveSecret, route.Channel, adapter, method), nil
}

// settleRefundOperation 把一次渠道操作的结论落进退款单。
//
// 它是发起退款与退款查询**两条路共用的收口**：两条路拿到的都是 provider.OperationResult，
// 对它的处置也逐字相同（成功就结算、明确失败就落 failed、没有结论就维持 processing）。
// 分开写两份的写法迟早会让「查询查出来的成功」与「发起时就说成功」走到两个不同的地方。
//
// from 是这条结论是从退款单的哪个状态出发得出的，只影响状态流水上那一行的 from 列：
// 发起那条路是 pending，退款查询那条路是 processing（见 refundTransitions）。
func (s *PaymentService) settleRefundOperation(ctx context.Context, payment *model.Payment, refund *model.Refund, accountLineTypes []string, from string, result provider.OperationResult, callErr error) error {
	switch {
	case callErr != nil:
		// err 非 nil 表示**这一次调用没有结论**，它有两种来路，而这里对两者一视同仁：
		// 请求根本没发出去（渠道侧什么都没有），或者发出去了但我们在等到应答之前先没了预算
		// （渠道那边可能已经把这笔退款收下了）。**不落任何结论**——退款单停在 pending，重发
		// 同一个售后单号会接着走，而渠道按 (商户单号, 退款单号) 判幂等，所以「可能已经收下」
		// 不会变成退两次钱。落 processing 会把它交给退款查询 worker，而那边问的是一笔渠道
		// 可能压根没收到过的退款（见 operationTransportFailure：那条路一律 ResultUnknown）。
		slog.ErrorContext(ctx, "payment provider refund left no conclusion",
			"refund_no", refund.RefundNo, "payment_no", payment.PaymentNo, "error", callErr)
		return fmt.Errorf("refund %s at provider %s: %w", refund.RefundNo, payment.Provider, callErr)

	case result.Result == provider.ResultSuccess:
		_, err := s.repository.MarkRefundSucceeded(ctx, repository.MarkRefundSucceededParams{
			RefundID:         refund.ID,
			ProviderRefundID: result.ProviderRefundID,
			// 渠道的应答里没有退款成交时间（OperationResult 没有这一项），所以用我们记账的
			// 这一刻。它与支付那一路不同——那边回调/查单都带着渠道给的成交时间。
			SucceededAt:   s.now(),
			NoOpLineTypes: accountLineTypes,
			TraceID:       refund.RequestID,
		})
		return err

	case result.Result == provider.ResultFailed:
		// 渠道明确拒绝是**一个结论**：钱没有退回去，售后单落到 failed 让人来处理。返回 error
		// 会让上层把一次正常的业务拒绝当成服务端故障去重试。
		return s.refundDeclined(ctx, refund, from, result)

	default:
		// ResultTimeout / ResultUnknown / 适配器漏填。**没有结论**，退款单落 processing，
		// 由退款查询 worker 问出结局（见 worker/refund.go）。这件事必须有人跟：规范里没有
		// 退款回调，渠道不会主动来告诉我们结果。
		slog.WarnContext(ctx, "payment provider refund returned an uncertain result",
			"refund_no", refund.RefundNo, "payment_no", payment.PaymentNo,
			"result", string(result.Result))
		_, err := s.repository.MarkRefundProcessing(ctx, repository.MarkRefundProcessingParams{
			RefundID:         refund.ID,
			ProviderRefundID: result.ProviderRefundID,
		})
		return err
	}
}

// refundDeclined 把一次渠道明确拒绝落下来。
//
// from 是这条失败是从哪个状态出发的（pending 还是 processing），由调用方给：两个入口都能
// 得出「失败」这个结论（发起时就被拒，或者查询问出来是 FAIL），而状态流水上那一行要记清
// 是哪一个。
func (s *PaymentService) refundDeclined(ctx context.Context, refund *model.Refund, from string, result provider.OperationResult) error {
	code := result.FailureCode
	if code == "" {
		// 适配器没说为什么拒的。给一个非空的码而不是留白——空的 failure_code 在后台列表里
		// 看起来像「没失败」，而这一笔确实失败了（与 markDeclined 同一条理由）。
		code = "provider_declined"
	}
	_, err := s.repository.MarkRefundFailed(ctx, repository.MarkRefundFailedParams{
		RefundID:       refund.ID,
		FailureCode:    code,
		FailureMessage: result.FailureMessage,
		From:           from,
		TraceID:        refund.RequestID,
	})
	return err
}

// splitRefundFundings 把退款单的出资行分成两堆，并算出要发给渠道的那一笔金额。
//
// 返回 (走渠道的金额合计, 不走渠道的 line_type 去重列表)。
//
// **按 line_type 分，不按「哪条渠道」分**：一张退款单只对着一张支付单，而那张支付单只走过
// 一条渠道（见 paymentRoute），所以「走渠道的那一部分」就是「除了账户账本以外的全部」。
// 混合出资落地时这个判据仍然成立——多出来的只是渠道那部分会更大。
//
// 这也是**全仓唯一按出资行类型分岔的地方**。出资渠道词表退场之后 line_type 存的是支付方式
// code，所以这里读的 `coffee_bean` 是 catalog 的一个 code，而不是「不是 ums 的都是别的」那种
// 穷举——真要走这条路的分岔，判据应该是「这条方式有没有渠道」（catalog.ChannelFor），
// 不是比字符串。今天只有咖啡豆这一种无渠道方式，这个等值判断与那句判据等价。
//
// 去重是因为 MarkRefundSucceeded 收的是一组 line_type（它按它整批更新），重复的值不会出错
// 但会让日志与参数难看。
//
// **返回值必须是空切片而不是 nil，哪怕一行账户出资都没有。** 第二个返回值原样进
// MarkRefundSucceeded 的 NoOpLineTypes，而它在那条 SQL 里是一个 text[] 参数：
// pgx 把 nil 切片编成 **NULL**，于是 `NOT (line_type = ANY(NULL))` 是 NULL，走渠道的那几行
// 一行都匹配不上——退款单已经是 succeeded，它的出资行却永远停在 pending。这处踩过：
// 混合出资（豆 + 渠道）那条路一直是对的，正因为那时这个切片非空。
func splitRefundFundings(fundings []*model.RefundFunding) (int64, []string) {
	var channelAmount int64
	// 从空切片起步，见上面的注释：nil 会让那条 UPDATE 一行都匹配不上。
	accountLineTypes := []string{}
	seen := make(map[string]bool, len(fundings))
	for _, funding := range fundings {
		if funding.LineType == fundingLineTypeCoffeeBean {
			if !seen[funding.LineType] {
				seen[funding.LineType] = true
				accountLineTypes = append(accountLineTypes, funding.LineType)
			}
			continue
		}
		channelAmount += funding.Amount
	}
	return channelAmount, accountLineTypes
}

// refundResult 把一张退款单翻成对外的结果形状。
func refundResult(refund *model.Refund) *RefundResult {
	return &RefundResult{
		RefundNo:         refund.RefundNo,
		Status:           refund.Status,
		ProviderRefundID: refund.ProviderRefundID,
		FailureCode:      refund.FailureCode,
		FailureMessage:   refund.FailureMessage,
	}
}

// operationFacts 从一次渠道操作请求里取出流水里要记的那几项事实。
//
// 与 factsOf 同形不同源：操作路线上没有金额（关单与查询都不用），但多一个退款单号。
// 两件事都记进摘要，是因为 payment_provider_calls 上退款与查询长得一模一样，只靠
// operation 列分不出来问的是哪一笔。
type operationFacts struct {
	PaymentNo string
	OrderNo   string
	Amount    int64
	RequestID string
	RefundNo  string
	Method    provider.Method
}

func operationFactsOf(request provider.OperationRequest) operationFacts {
	return operationFacts{
		PaymentNo: request.PaymentNo,
		OrderNo:   request.OrderNo,
		Amount:    request.Amount,
		RequestID: request.RequestID,
		RefundNo:  request.RefundNo,
		Method:    request.Method,
	}
}

// refundNo 生成退款单号：REF + YmdHis + 6 位随机数，**共 23 位**。
//
// # 那个 23 是硬的，不是风格
//
// 退款单号会被编成渠道侧的 refundOrderId 发出去，而规范给的上限是「总长度大于 6 位、小于
// 28 位」（含渠道分配的 4 位来源编号，见 provider/ums/orderid.go 末尾那一节）。4 + 23 = 27，
// 正好卡在上限之内；再多一位就被渠道挡在门外。
//
// **所以这里没有支付单号那三个毫秒位。** 支付单号是 26 位、有自己的宽裕上限，而退款单号每
// 一位都要算着花。防撞号靠的是后 6 位密码学随机数（一百万分之一）——同一秒内的两笔退款撞
// 号是个概率问题，真撞上会被 payment_refunds_refund_no_key 挡下来，那是一次可重试的失败。
//
// 前 3 位定长 `REF` 是给**人工排查**用的：渠道账单上退款那一行与支付那一行长得很像，
// 一眼能看出这是哪一边。
func refundNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// 与 paymentNo 同一条理由：crypto/rand 在 darwin/linux 上不会失败，真失败了也绝不
		// 退回固定值——那会让同一秒内的退款单号完全由时间戳决定，撞号从概率问题变成必然问题。
		panic(fmt.Sprintf("payment-service: read random for refund number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("REF%s%06d", now.Format("20060102150405"), tail%1000000)
}

// validateRefund 做形状校验。这一层不碰数据库，所以它能在没有 PG 的情况下被测到。
func validateRefund(in RefundRequest) error {
	if strings.TrimSpace(in.AfterSaleNo) == "" {
		return ErrAfterSaleNoRequired
	}
	if strings.TrimSpace(in.PaymentNo) == "" {
		return ErrPaymentNoRequired
	}
	if in.Amount <= 0 {
		return ErrRefundAmountNotPositive
	}
	// order_line_id 是可空的（整单退），但给了就必须是个 uuid：它是 order_lines.id 的值
	// 引用，形状不对的话落库时才会炸，而那时候退款单已经建了一半。空串放行——NULLIF 会把
	// 它变成 NULL。
	if lineID := strings.TrimSpace(in.OrderLineID); lineID != "" {
		if _, err := uuid.Parse(lineID); err != nil {
			return ErrOrderLineIDInvalid
		}
	}
	return nil
}

// 退款那条路上的输入校验错误。与支付那几个分开命名，是因为它们进的错误码不同——
// 渠道的回包与调用方的排查都按 code 走。
var (
	ErrAfterSaleNoRequired     = errors.New("afterSaleNo is required")
	ErrPaymentNoRequired       = errors.New("paymentNo is required")
	ErrRefundAmountNotPositive = errors.New("refund amount must be positive")
	ErrOrderLineIDInvalid      = errors.New("orderLineId must be a uuid")
)
