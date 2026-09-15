package service

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// ErrMethodChannelMissing：支付方式声明的 action 需要外部渠道，但它没有关联渠道行。
//
// 这是**配置错误**，不是调用方的错：payment_methods.channel_id 为空只对 action=account
// 合法，其余五个 action 都必须有渠道。返回它而不是 ErrProviderNotConfigured，是因为
// 「渠道行都没有」和「渠道行指向了没注册的实现」要修的是两张不同的表。
var ErrMethodChannelMissing = errors.New("payment method requires a channel but has none")

// ErrProviderResultUncertain：渠道调用没有给出确定的结论（超时，或连发出去没有都不确定）。
//
// 处理方式与「渠道拒绝」**完全不同**，这是这个错误存在的全部理由：
//
//   - 渠道拒绝（ResultFailed）是确定的——预支付单没建起来，钱不可能到。于是把支付单标
//     failed 并告诉用户换一种方式。
//   - 结果不明（ResultTimeout / ResultUnknown）时渠道侧可能已经有一张能付的预支付单。
//     这时**不能**把本地支付单标成 failed：用户可能真的把那笔钱付了，而我们会看到一张
//     「已失败」的单收到成功回调，落进 ErrPaymentAlreadySettled 那条人工介入路径。
//
// 所以不确定时支付单**停在 created 不动**，由超时关单扫描把它收走（created→expired）。
// 那是一条走得通的状态迁移，不是把问题扫到地毯下。
//
// 正确做法是拿商户单号去渠道查单再定论，而 Provider 接口本轮没有 Query（见 provider.go
// 的注释）——这是真实渠道接进来时必须补上的第一个能力，留在这里以免后来的人以为
// 「超时当失败」是设计而不是缺口。
var ErrProviderResultUncertain = errors.New("provider call result is uncertain; payment left in created")

// CreateRequest 是一次发起支付的输入。
//
// 金额由调用方权威给出：order-service 在锁内校验完订单（归属、状态、应付额）之后通过
// gRPC 传进来。本服务不读订单库、也不复核——订单事实的归属方是订单服务。
type CreateRequest struct {
	// OrderID 是 orders.id。**必填**，即便渠道支付用不到它：账户出资扣豆时，账户域的
	// 幂等键由订单 ID 派生（`order:{orderId}`）。设成「选了豆才必填」的话，缺它的那次
	// 一定发生在调用方以为自己在测渠道支付的时候，而被砸到的是真的选豆的用户。
	OrderID string
	// OrderNo 是 orders.order_no，值引用，进渠道商户单号。
	OrderNo         string
	UserID          string
	Amount          int64
	PaymentMethodID string
	// Subject 是渠道收银台与账单上显示的商品描述；为空时用订单号兜底。
	Subject string
	// RequestID 是调用方给的幂等号（必填）。它与 ScopeCreatePayment 一起唯一，
	// 同一个 request_id 重复调用只落一张支付单。
	RequestID string
	// WalletOpenID 是渠道附加参数里需要用户身份的那一项（微信小程序支付要 openid）。
	WalletOpenID string
	// Attach 是其余渠道附加数据（设备号等）。**不放密钥**。
	Attach map[string]string
	// TraceID 只用于日志与 payment_provider_calls，不参与幂等哈希。
	TraceID string
}

// CreateResult 是发起支付的结果，也是**三个地方共用的同一个形状**：
// gRPC 响应、控制器 JSON 响应、以及写进幂等表供回放的快照。
//
// 三处共用一个结构是有意的：幂等回放的字节必须与首次响应一致，用两个结构各组装一遍是
// 一个必然漂移的写法（改了响应忘了改快照，用户第二次点支付拿到的字段少一个）。
type CreateResult struct {
	PaymentNo string `json:"paymentNo"`
	// Status 取 model.PaymentPending / model.PaymentSucceeded / model.PaymentFailed。
	// succeeded 只有账户出资会给（扣豆成功就是成功，没有第三方要等）。
	Status string `json:"status"`
	// Action 是给**客户端**用的：客户端只按它决定怎么调起支付，不 switch 渠道 code。
	// 这是「新增一家同形态渠道不用改客户端」的全部依据（见 provider.Action）。
	Action string `json:"action"`
	// PayParams 是渠道返回的支付参数，扁平字符串键值。**只有非敏感的、客户端可见的键**。
	PayParams map[string]string `json:"payParams"`
	// ExpiresAtUnix 是待支付超时（Unix 秒）。0 表示没有超时。
	ExpiresAtUnix int64 `json:"expiresAtUnix"`
	// FailureCode / FailureMessage 在失败时说明原因，供渠道侧排查对账。
	FailureCode    string `json:"failureCode"`
	FailureMessage string `json:"failureMessage"`
}

// CreatePayment 为一张订单发起一次支付。
//
// 两套流程，按支付方式的 action 分：`account`（咖啡豆）走 createAccountPayment，其余五个
// 走下面这条渠道流程。**建单与抢占幂等键是两套共用的**（beginPayment），分歧从第一个
// 终止动作开始——渠道流程要问第三方，账户流程不用。
//
// 渠道流程三段事务，渠道网络调用**绝不放在 PG 事务里**（一次几秒的第三方往返不该让数据库
// 事务一直开着锁）：
//
//	事务 1  抢占幂等键 + 建支付单(status=created) + 写状态流水 ''
//	事务外  provider.Create + 记录 payment_provider_calls（成败都记）
//	事务 2  status=created→pending + 落出资行(reserved) + 完成幂等记录（成功时）
//	事务 3  status=created→failed + 失败幂等记录（渠道明确拒绝时）
//
// 渠道调用「没给出确定结论」时两个事务都不走：支付单停在 created，由超时关单收走。
// 理由见 ErrProviderResultUncertain。
func (s *PaymentService) CreatePayment(ctx context.Context, in CreateRequest) (*CreateResult, error) {
	if err := validateCreate(in); err != nil {
		return nil, err
	}

	method, err := s.resolveMethod(ctx, in.PaymentMethodID)
	if err != nil {
		return nil, err
	}

	expiresAt := s.now().Add(s.paymentTTL)
	payment, replayed, err := s.beginPayment(ctx, in, method, expiresAt)
	if err != nil {
		return nil, err
	}
	if replayed != nil {
		return replayed, nil
	}

	if provider.Action(method.Method.Action) == provider.ActionAccount {
		// 账户出资从这里分出去，且**在碰注册表之前**：这条路合法地没有渠道行，注册表按
		// provider 名字也查不到它（见 resolveMethod）。
		return s.createAccountPayment(ctx, payment, method, in.OrderID)
	}

	// 到这里支付单已经在库里、状态是 created。下面无论走哪条分支，都有一个终止动作
	// （推进到 pending / failed，或者刻意什么都不做）。
	adapter, err := s.providers.Lookup(method.Channel.Provider)
	if err != nil {
		return nil, err
	}

	secret := s.resolveSecret(method.Channel)
	createRequest := provider.CreateRequest{
		PaymentNo:    payment.PaymentNo,
		OrderNo:      in.OrderNo,
		UserID:       in.UserID,
		Amount:       in.Amount,
		Subject:      subjectFor(in),
		RequestID:    in.RequestID,
		WalletOpenID: in.WalletOpenID,
		Attach:       attachFor(in),
		NotifyURL:    s.notifyURL(method.Channel.Code),
		ExpiresAt:    expiresAt,
		Method:       methodToProvider(method),
		Secret:       secret,
	}

	startedAt := s.now()
	createResult, callErr := adapter.Create(ctx, createRequest)
	duration := int(s.now().Sub(startedAt).Milliseconds())
	s.recordProviderCall(ctx, in.TraceID, payment, method, createRequest, createResult, callErr, duration)

	switch {
	case callErr != nil:
		// 契约：err 非 nil 只在「这次调用根本没发出去」时出现（见 provider.Provider 的注释）。
		// 渠道侧什么都没有，但这是我们自己的问题（密钥没配、参数拼不出来），不是渠道拒了，
		// 所以不落 failed 结论——支付单停在 created，让调用方看到一次真正的服务端错误。
		slog.ErrorContext(ctx, "payment provider create did not go out",
			"payment_no", payment.PaymentNo, "provider", method.Channel.Provider, "error", callErr)
		return nil, fmt.Errorf("create payment at provider %s: %w", method.Channel.Provider, callErr)

	case createResult.Result == provider.ResultSuccess:
		return s.markPending(ctx, payment, method, createResult, expiresAt)

	case createResult.Result == provider.ResultFailed:
		// 渠道明确拒绝是**一个结论，不是一个错误**：它落在 CreateResult.Status='failed' 上，
		// 与 gRPC 契约里那句「failed = 发起即失败（渠道拒绝、参数不全），客户端可以换方式重试」
		// 对应。返回 error 会让上层把一次正常的业务拒绝当成服务端故障去重试。
		return s.markDeclined(ctx, payment, createResult)

	default:
		// ResultTimeout / ResultUnknown / 适配器漏填（零值被记成 unknown）。
		slog.ErrorContext(ctx, "payment provider create returned an uncertain result",
			"payment_no", payment.PaymentNo, "provider", method.Channel.Provider,
			"result", string(createResult.Result))
		return nil, fmt.Errorf("%w: %s", ErrProviderResultUncertain, payment.PaymentNo)
	}
}

// beginPayment 是发起支付的第一段事务：抢占幂等键、建支付单、写状态流水。
//
// 三种结果分得很清，调用方靠它们决定要不要继续：
//
//	(新建的支付单, nil, nil)   刚建的，接着去调渠道
//	(nil, 回放的响应, nil)     上次已经有结论了——把当时那份响应原样交回，不再有任何副作用
//
// 回放不区分成功与失败：两种快照回的都是同一个 CreateResult 形状，调用方看 Status 分。
// 这正是「不会重复执行」与「重复调用得到同一个答复」两件事都实现的方式。
func (s *PaymentService) beginPayment(ctx context.Context, in CreateRequest, method *repository.PaymentMethodWithChannel, expiresAt time.Time) (*model.Payment, *CreateResult, error) {
	payment, snapshot, hit, err := s.repository.BeginPayment(ctx, repository.BeginPaymentParams{
		PaymentNo:   s.newPaymentNo(s.now()),
		OrderNo:     in.OrderNo,
		UserID:      in.UserID,
		Amount:      in.Amount,
		FundingType: method.Method.FundingType,
		ChannelID:   channelID(method),
		MethodID:    method.Method.ID,
		Subject:     subjectFor(in),
		Attach:      attachFor(in),
		RequestID:   in.RequestID,
		ExpiresAt:   expiresAt,
		RequestHash: createRequestHash(in),
	})
	if err != nil {
		return nil, nil, err
	}
	if !hit {
		return payment, nil, nil
	}
	if len(snapshot) == 0 {
		// 幂等行存在但快照是空的：只可能是一行被标记 succeeded 却没写 response 的脏数据。
		// 返回一个错误让人来查，好过在这里猜一个支付参数给客户端。
		return nil, nil, fmt.Errorf("idempotent create payment %s has no recorded response", in.RequestID)
	}
	var replayed CreateResult
	if err := json.Unmarshal(snapshot, &replayed); err != nil {
		return nil, nil, fmt.Errorf("decode replayed create payment response: %w", err)
	}
	return nil, &replayed, nil
}

// markPending 是成功那一段：把支付单推进到 pending、落出资行、完成幂等记录。
func (s *PaymentService) markPending(ctx context.Context, payment *model.Payment, method *repository.PaymentMethodWithChannel, createResult provider.CreateResult, expiresAt time.Time) (*CreateResult, error) {
	result := &CreateResult{
		PaymentNo:     payment.PaymentNo,
		Status:        model.PaymentPending,
		Action:        string(method.Method.Action),
		PayParams:     createResult.PayParams,
		ExpiresAtUnix: expiresAt.Unix(),
	}
	if result.PayParams == nil {
		// 客户端拿到 nil 会渲染出一个空收银台，而真正的异常是适配器没给参数。
		result.PayParams = map[string]string{}
	}

	if _, err := s.repository.MarkPaymentPending(ctx, repository.MarkPaymentPendingParams{
		PaymentID:             payment.ID,
		ProviderTransactionID: createResult.ProviderTransactionID,
		// 本轮只有渠道出资一条线。写成切片是为了让账户出资（咖啡豆）接进来时
		// 不用改这段事务的形状。
		FundingLines: []repository.FundingLine{{
			LineNo:   1,
			LineType: payment.FundingType,
			// 出资额等于支付单的应付额：本轮没有混合出资，重复一遍是为了让出资行
			// 单独拿出来也能算账，而不是要靠 payment_id 去 join 才知道每笔多少。
			Amount:                payment.Amount,
			ProviderTransactionID: createResult.ProviderTransactionID,
		}},
		IdempotencyResponse: result,
		RequestID:           payment.RequestID,
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// markDeclined 把一次渠道明确拒绝落下来，返回 status='failed' 的结果。
//
// 返回的是**结果不是错误**，理由见 CreatePayment 里那条分支的注释。失败也要提交（payments
// 表的注释：「第一次支付失败、超时被关，用户可以再发起一次，那是新的一行」），并且把这份
// 失败快照写进幂等表——同一个 request_id 再调，回放出来的还是同一个结论。
func (s *PaymentService) markDeclined(ctx context.Context, payment *model.Payment, createResult provider.CreateResult) (*CreateResult, error) {
	code := createResult.FailureCode
	if code == "" {
		// 适配器没说为什么拒的。给一个非空的码而不是留白：空的 failure_code 在后台列表里
		// 看起来像「没失败」，而这一单确实失败了。
		code = "provider_declined"
	}
	result := &CreateResult{
		PaymentNo:      payment.PaymentNo,
		Status:         model.PaymentFailed,
		FailureCode:    code,
		FailureMessage: createResult.FailureMessage,
	}
	if _, err := s.repository.MarkPaymentFailed(ctx, repository.MarkPaymentFailedParams{
		PaymentID:           payment.ID,
		FailureCode:         code,
		FailureMessage:      createResult.FailureMessage,
		IdempotencyResponse: result,
		RequestID:           payment.RequestID,
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// createAccountPayment 是账户出资（纯咖啡豆）那条路。
//
// 与渠道那条路的根本差别：**没有第三方**。钱在我们自己的库里（账户域的豆账本），扣成功就是
// 成功，所以没有 pending 这个中间态，也没有渠道调用流水可记（payment_provider_calls 记的是
// 「我们向谁发了什么」，这里谁也没发）。
//
// 顺序，以及每一步失败之后支付单的落点：
//
//  1. 扣豆（跨服务同步 RPC，在 PG 事务之外）
//     余额不足        → 落 failed，返回 status='failed' 的结果（客户端换一种方式重试）
//     账户域不可达    → 返回 error，**支付单停在 created**，由超时关单收走
//  2. 留痕（一条 UPDATE）→ payments.account_entry_id / account_funded_at
//  3. 结算（一个事务）→ status='succeeded' + 出资行 + 资金流水 + 状态流水 + outbox + 幂等
//
// 第 1 步那两个失败为什么分得这么开：余额不足是账户域给出的**结论**（它查过账本了），
// 而连不上是**没有结论**。把没有结论的也标成 failed，会让一张其实已经扣过豆的支付单显示
// 失败——用户会以为没付成，再付一次。留在 created 的代价只是等一次超时关单，而那个窗口里
// 用户重发同一个 request_id 会拿到同一张支付单。
//
// 第 2 步为什么单独存在：第 1 步与第 3 步各自提交，中间那个窗口里豆**已经扣走了**，而本地
// 什么都没留下——第 3 步的出资行在没提交的那个事务里。没有第 2 步，这张单要么被超时关单
// 当成没付过一样关掉（钱却没了），要么停在 created 等一个人拿着订单号去账户库翻账变。
// 有了它，这张单既不会被关掉，也会被补偿任务捡起来结算（见 expire.go）。
//
// 第 3 步失败时的其它兜底仍在：支付单停在 created，同 requestId 重发时账户域按
// `order:{orderId}` 命中同一笔流水、回放原账变（DeductResult.Replayed），不会扣第二次。
// 但那一条只在「用户还会重试」时成立，兜不住「用户再也没回来」——那是第 2 步与补偿任务
// 要管的事。
func (s *PaymentService) createAccountPayment(ctx context.Context, payment *model.Payment, method *repository.PaymentMethodWithChannel, orderID string) (*CreateResult, error) {
	if s.beans == nil {
		// 这个部署没接账户域。**不能**跳过扣豆直接结算——那等于白送一单。
		return nil, fmt.Errorf("%w: account funding is not configured", ErrMethodChannelMissing)
	}

	// orderID 是调用方给的原值，**不是**从 payment 行上读的：payments 表没有 order_id 列，
	// 它靠 order_no 指回订单。这不影响扣豆——账户域要的是订单 ID（幂等键的来源），
	// 而我们手上正好有（CreateRequest.OrderID，validateCreate 已经校验过它是 uuid）。
	deducted, err := s.beans.Deduct(ctx, client.DeductRequest{
		UserID:  payment.UserID,
		OrderID: orderID,
		OrderNo: payment.OrderNo,
		Amount:  payment.Amount,
		// 支付单号只进账变的备注，让两侧人工对账时能互相对上号。它**不参与**幂等：
		// 账户域不认识支付单（见 client.DeductRequest 的注释）。
		PaymentNo: payment.PaymentNo,
	})
	if err != nil {
		if errors.Is(err, ErrInsufficientCoffeeBeans) {
			return s.markDeclined(ctx, payment, provider.CreateResult{
				Result:         provider.ResultFailed,
				FailureCode:    failureCodeInsufficientBeans,
				FailureMessage: fmt.Sprintf("咖啡豆余额不足，需要 %d 分", payment.Amount),
			})
		}
		return nil, fmt.Errorf("deduct coffee beans for payment %s: %w", payment.PaymentNo, err)
	}

	// 豆已经扣走了，先把这件事登记到支付单上，再去结算。**这一步不能省，也不能并进结算
	// 那个事务**：它是唯一能让「扣了豆、结算没成」这张单在本地留下痕迹的东西，并进去的话
	// 结算一失败，这条留痕跟着回滚，那笔账变就再没有线索指向它（见 005 迁移）。
	//
	// 写失败**不阻断**结算：结算那个事务自己也会把 account_entry_id 写进 payment_fundings，
	// 那才是权威的出资留痕。这一步要覆盖的只是结算失败的那个窗口，写到那一步都失败是两次
	// 独立的库故障，报错留着就够了，没必要把一次本来能成的支付挡在这里。
	fundedAt := s.now()
	if err := s.repository.RecordAccountDeduction(ctx, payment.ID, deducted.EntryID, fundedAt); err != nil {
		slog.ErrorContext(ctx, "failed to record a coffee bean deduction on the payment",
			"payment_no", payment.PaymentNo, "account_entry_id", deducted.EntryID, "error", err)
	}

	// 出资行的 line_type 取支付方式的 funding_type（这一条路上必然等于 'coffee_bean'，
	// 由 payment_methods 的那行数据决定，不在这里写死）。Action 取自支付方式，
	// 客户端据此知道不需要再调起任何东西。
	result := &CreateResult{
		PaymentNo: payment.PaymentNo,
		Status:    model.PaymentSucceeded,
		Action:    string(method.Method.Action),
		// 账户出资没有支付参数、没有待支付超时：不会有第二个动作要客户端做。
		PayParams: map[string]string{},
	}

	if _, err := s.repository.SettleAccountPayment(ctx, repository.SettleAccountPaymentParams{
		PaymentID:      payment.ID,
		AccountEntryID: deducted.EntryID,
		// 成交时间用扣豆那一刻（上面那个 fundedAt），不用结算这一刻：这两步之间可能隔了
		// 一次重试甚至一次补偿任务，而钱是在扣豆那一刻离开账户的。同一个值写两处
		// （payments.account_funded_at 与 payment_fundings.succeeded_at），它们必须相等。
		PaidAt:              fundedAt,
		IdempotencyResponse: result,
		RequestID:           payment.RequestID,
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// failureCodeInsufficientBeans 是「豆不够」这条失败结论的码。
//
// 它落进 payments.failure_code，是给**运营与客户端**看的：客户端按它决定是不是该提示用户
// 去充值，运营按它区分「用户没钱」和「渠道拒了」。写成常量而不是就地拼一个字符串，是为了
// 让 e2e 与后台筛选能引用同一个值。
const failureCodeInsufficientBeans = "insufficient_coffee_beans"

// resolveMethod 读支付方式并把它翻译成适配器要的 shape。
//
// 账户出资**合法地没有渠道行**（payment_methods.channel_id 为空只对 action=account 合法），
// 所以它跳过下面那两个渠道检查直接返回：它没有渠道行可查，也没有 provider 名字可分派，
// CreatePayment 会在碰注册表之前把它分走。
func (s *PaymentService) resolveMethod(ctx context.Context, methodID string) (*repository.PaymentMethodWithChannel, error) {
	method, err := s.repository.FindPaymentMethod(ctx, methodID)
	if err != nil {
		return nil, err
	}
	action := provider.Action(method.Method.Action)
	if !action.Valid() {
		// 数据库的 CHECK 挡得住写坏的数据，但挡不住「代码不认识新加的那个 action」。
		// 在这里明确报错，好过在下面某个 switch 的 default 里静默走偏。
		return nil, fmt.Errorf("%w: action %q", ErrUnauthorizedAction, method.Method.Action)
	}
	if action == provider.ActionAccount {
		return method, nil
	}
	if method.Channel == nil {
		return nil, fmt.Errorf("%w: method %s", ErrMethodChannelMissing, method.Method.Code)
	}
	if strings.TrimSpace(method.Channel.Provider) == "" {
		return nil, fmt.Errorf("%w: channel %s has no provider", ErrMethodChannelMissing, method.Channel.Code)
	}
	return method, nil
}

// recordProviderCall 落一条渠道调用流水。
//
// 失败只记日志，不往上抛：流水是给运维查的，钱的状态比它重要。让一次流水写入失败
// 连带把支付单的推进回滚掉，是把「查不到」升级成了「收不到钱」。
func (s *PaymentService) recordProviderCall(ctx context.Context, traceID string, payment *model.Payment, method *repository.PaymentMethodWithChannel, request provider.CreateRequest, result provider.CreateResult, callErr error, durationMS int) {
	summary := map[string]any{
		"paymentNo": request.PaymentNo,
		"orderNo":   request.OrderNo,
		"amount":    request.Amount,
		"action":    string(request.Method.Action),
		"channel":   request.Method.ChannelCode,
		// 渠道返回的支付参数不进流水：里面有跳转链接一类的凭证，方案 11.5 只允许留
		// 能定位这一笔的键。这里记的是「它给了几个参数」而不是参数本身。
		"payParamKeys": sortedKeys(result.PayParams),
	}
	if callErr != nil {
		summary["error"] = callErr.Error()
	}
	if err := s.repository.RecordProviderCall(ctx, repository.ProviderCallParams{
		ChannelID:       channelID(method),
		Provider:        method.Channel.Provider,
		Operation:       model.CallOperationCreate,
		PaymentNo:       payment.PaymentNo,
		RequestID:       request.RequestID,
		TraceID:         traceID,
		AttemptNo:       1,
		RequestSummary:  summary,
		ResponseSummary: result.ResponseSummary,
		ProviderCode:    result.FailureCode,
		ProviderMessage: result.FailureMessage,
		// 零值兜底：适配器漏填 Result 时记成 unknown，那对「我们确实不知道」是恰好正确的。
		Result:     providerCallResult(result.Result),
		DurationMS: &durationMS,
	}); err != nil {
		slog.WarnContext(ctx, "failed to record payment provider call",
			"payment_no", payment.PaymentNo, "provider", method.Channel.Provider, "error", err)
	}
}

// providerCallResult 把适配器的结果分类落成流水里的取值，零值兜底为 unknown。
func providerCallResult(result provider.Result) string {
	switch result {
	case provider.ResultSuccess, provider.ResultFailed, provider.ResultTimeout, provider.ResultUnknown:
		return string(result)
	default:
		return string(provider.ResultUnknown)
	}
}

// methodToProvider 把两张表的投影翻成适配器只读的那个 shape。
func methodToProvider(method *repository.PaymentMethodWithChannel) provider.Method {
	return provider.Method{
		ID:            method.Method.ID,
		Code:          method.Method.Code,
		Action:        provider.Action(method.Method.Action),
		FundingType:   method.Method.FundingType,
		Params:        method.MethodParams,
		ChannelID:     method.Channel.ID,
		ChannelCode:   method.Channel.Code,
		Provider:      method.Channel.Provider,
		Mode:          method.Channel.Mode,
		ChannelConfig: method.ChannelConfig,
		SecretRef:     method.Channel.SecretRef,
	}
}

// sortedKeys 返回 map 的键并排序，用于日志与流水摘要。
//
// 排序是必须的：map 的遍历顺序在 Go 里是随机的，直接遍历会让同一笔调用每次记出来的
// 摘要顺序都不同，对账时按文本比对就会一直报差异。**只返回键，不返回值**——调用它的
// 地方（渠道调用流水的 payParamKeys）本来就不该把支付参数的值落到任何地方。
func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// channelID 取渠道 ID，账户出资方式没有渠道时返回空串（仓储用 NULLIF 落成 NULL）。
func channelID(method *repository.PaymentMethodWithChannel) string {
	if method.Channel == nil {
		return ""
	}
	return method.Channel.ID
}

// subjectFor 给渠道收银台一个可读描述。空描述在渠道账单上会显示成一片空白，
// 对账时看不出这笔是什么。
func subjectFor(in CreateRequest) string {
	if subject := strings.TrimSpace(in.Subject); subject != "" {
		return subject
	}
	return "订单 " + in.OrderNo
}

// attachFor 拼渠道附加数据。
//
// walletOpenId 由服务端写入而不是从调用方的 attach 里读：它是发起支付时随请求传来的
// 独立字段，让调用方能从 attach 里覆盖它等于把「用谁的身份发起支付」交给了一个不够明确的
// 入口。服务端覆盖一次，语义只有一种。
func attachFor(in CreateRequest) map[string]string {
	attach := make(map[string]string, len(in.Attach)+1)
	for key, value := range in.Attach {
		attach[key] = value
	}
	if openID := strings.TrimSpace(in.WalletOpenID); openID != "" {
		attach["walletOpenId"] = openID
	}
	return attach
}

// createRequestHash 是幂等键上那条「同一个 key 换了请求体」判定用的哈希。
//
// 只哈希**业务字段**：order_no / user_id / amount / payment_method_id。subject 与 attach
// 不进——它们是展示与渠道附加数据，改了它们不该让一次重试变成冲突；request_id 也不进，
// 它就是 key 本身；trace_id 更不进，每次请求都不同。
//
// order_id 也不进：它与 order_no 是同一张订单的两个名字（1:1，订单号唯一），进了只会
// 让哈希多一个必然同步的字段，而不会多识别出一种冲突。它该不该存在由 validateCreate 管，
// 不由哈希管。
//
// 用一个具名结构体而不是 map：map 的序列化顺序按 key 排序，虽然稳定，但字段多一个少一个
// 要靠读代码才发现。结构体让这份字段集在一处可见，改动会体现在 diff 里。
func createRequestHash(in CreateRequest) string {
	return repository.RequestHash(struct {
		OrderNo         string
		UserID          string
		Amount          int64
		PaymentMethodID string
	}{
		OrderNo:         in.OrderNo,
		UserID:          in.UserID,
		Amount:          in.Amount,
		PaymentMethodID: in.PaymentMethodID,
	})
}

// validateCreate 做形状校验。这一层不碰数据库，所以它能在没有 PG 的情况下被测到。
func validateCreate(in CreateRequest) error {
	if strings.TrimSpace(in.OrderID) == "" {
		return ErrOrderIDRequired
	}
	if _, err := uuid.Parse(in.OrderID); err != nil {
		return ErrOrderIDInvalid
	}
	if strings.TrimSpace(in.OrderNo) == "" {
		return ErrOrderNoRequired
	}
	if strings.TrimSpace(in.UserID) == "" {
		return ErrUserIDRequired
	}
	if _, err := uuid.Parse(in.UserID); err != nil {
		return ErrUserIDInvalid
	}
	if in.Amount <= 0 {
		return ErrAmountNotPositive
	}
	if strings.TrimSpace(in.PaymentMethodID) == "" {
		return ErrMethodIDRequired
	}
	if _, err := uuid.Parse(in.PaymentMethodID); err != nil {
		return ErrMethodIDInvalid
	}
	if strings.TrimSpace(in.RequestID) == "" {
		// 没有幂等号就没有「重复发起只落一张单」的依据：用户双击一次支付按钮会变成
		// 两张支付单，而订单只能被收一次钱，第二张会在支付成功时撞上
		// payments_one_succeeded_per_order 变成一次看不懂的失败。
		return ErrRequestIDRequired
	}
	return nil
}

// paymentNo 生成支付单号：PAY + YmdHis + 6 位数字。
//
// 与订单号不同，支付单号**没有渠道规范约束**（订单号那串是银联商务要求的商户订单号格式，
// 见 order-service 的 generateOrderNo），所以这里按可读性取。后 6 位是三位毫秒 + 三位
// 密码学随机数：同一秒内的两笔撞号靠随机数，撞了会被 payments_payment_no_key 挡下来，
// 那是一次可重试的失败，不是安全问题。
func paymentNo(now time.Time) string {
	random := make([]byte, 3)
	if _, err := rand.Read(random); err != nil {
		// crypto/rand 在 darwin/linux 上不会失败；真失败了也不该退回固定值——那会让同一
		// 秒内的支付单号完全由时间戳决定，撞号从概率问题变成必然问题。
		panic(fmt.Sprintf("payment-service: read random for payment number: %v", err))
	}
	tail := uint32(random[0])<<16 | uint32(random[1])<<8 | uint32(random[2])
	return fmt.Sprintf("PAY%s%03d%06d", now.Format("20060102150405"), now.UnixNano()%1000, tail%1000000)
}
