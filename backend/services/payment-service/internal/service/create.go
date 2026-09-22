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

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

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
// 正确做法是拿商户单号去渠道查单再定论，这条已经实现：结果不明时先探一次 provider.Querier
// （见下面 settleByQuery）。落到本错误上的只剩两种情况——适配器没实现 Querier（四家饭卡渠道
// 里有几家只有回调），或者查了仍然不明。两种都维持「停在 created、等超时关单」：这不是还没
// 补的缺口，是没有更好答案时唯一安全的处置。
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
	OrderNo string
	UserID  string
	Amount  int64
	// PaymentMethod 是**支付方式的 code**（catalog 里那组常量，如 `ums_h5_wechat`），
	// 不再是 payment_methods 那一行的 uuid。
	//
	// 它从前是 id 而现在是 code，是因为支付方式已经不是数据了：运营不再有「建一行、起个
	// 名字」的能力，能选的就那六条写死在代码里的路径，而它们的 code 就是**对外契约**——
	// 客户端传它、支付单记它、后台按它筛。用一个 uuid 指向一份可变的行，只会让「用户付的
	// 是哪一种」这件事依赖一次数据库查询的结果。
	PaymentMethod string
	// Subject 是渠道收银台与账单上显示的商品描述；为空时用订单号兜底。
	Subject string
	// RequestID 是调用方给的幂等号（必填）。它与 ScopeCreatePayment 一起唯一，
	// 同一个 request_id 重复调用只落一张支付单。
	RequestID string
	// WalletOpenID 是渠道附加参数里需要用户身份的那一项（微信小程序支付要 openid）。
	WalletOpenID string
	// StoreID / DeviceID 是下单点位与设备的值引用，**可以为空**（纯会员订单没有点位）。
	//
	// 它们只有一个用途：在**建支付单的同一个事务里**按范围命中分账规则（见 buildSettlementPlan）。
	// 所以必须在发起这一刻给出来——支付成功那条事件是反方向的（Payment → Order），带不了它们，
	// 而分账规则一旦在这里命不中，整条链路就安静地退回「全归平台」。
	StoreID  string
	DeviceID string
	// BizType 是这一单的分账业务分类，取 settlement_rules.biz_type 的词表，**必填**。
	//
	// 它是规则命中键的第一段（唯一键是 biz_type + scope_type + scope_ref）：同一家门店的
	// 「咖啡单分成」与「会员单分成」靠它分开，而支付域自己猜不出来——那是订单域的事实，
	// 由 order-service 从订单行推出来（见 order-service 的 settlementBizType）。
	BizType string
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
//
// 事务 2/3 **都没走成**的那些出口（渠道没注册、调用没发出去、结果不明、账户域不可达……）由
// dispatchCreate 统一收口：这次尝试没有结论，幂等键因此被退回去，同一个 Idempotency-Key 可以
// 立刻重试（见 abandonPaymentAttempt）。
func (s *PaymentService) CreatePayment(ctx context.Context, in CreateRequest) (*CreateResult, error) {
	if err := validateCreate(in); err != nil {
		return nil, err
	}

	route, err := s.resolveMethod(in.PaymentMethod)
	if err != nil {
		return nil, err
	}

	// 分账计划在**建支付单之前**算好，然后随支付单进同一个事务（见 beginPayment）。
	// 放在这里而不是等支付成功之后：发起这一刻手上正好有全部上下文（实付金额、门店、设备、
	// 业务分类），规则也因此在那一刻冻结——运营之后改了比例，不影响已经建好的这一笔。
	//
	// 它失败就让这次发起失败（见 buildSettlementPlan）：一次分账错误比一次付不成更贵。
	plan, err := s.buildSettlementPlan(ctx, in, route)
	if err != nil {
		return nil, err
	}

	expiresAt := s.now().Add(s.paymentTTL)
	payment, replayed, err := s.beginPayment(ctx, in, route, expiresAt, plan)
	if err != nil {
		return nil, err
	}
	if replayed != nil {
		return replayed, nil
	}

	// 从这里往下的每一条失败出口都要把幂等键退回去（见 abandonPaymentAttempt）：幂等行在
	// beginPayment 里已经写成 processing 并提交了，而下面这些路**都没有产生结论**。不退回的话
	// 同一个 Idempotency-Key 在这之后 15 分钟（IdempotencyRecoveryWindow）里重试只会拿到
	// ErrIdempotencyInProgress，而那段时间里没有任何东西真的在跑。
	//
	// 收在**一个出口**上而不是逐个 `return` 去改：出口有九条（渠道没注册、调用没发出去、结果
	// 不明、账户域不可达、结算写失败……），漏掉任何一条就退回原来的样子，而「哪几条要退」不该
	// 由一份人工维护的清单来保证。
	result, err := s.dispatchCreate(ctx, payment, route, in, expiresAt, plan)
	if err != nil {
		s.abandonPaymentAttempt(ctx, payment)
		return nil, err
	}
	return result, nil
}

// dispatchCreate 是发起支付的第二段：手上已经有一张刚建的支付单（status=created），按支付
// 方式的 action 把它推到结论。
//
// 返回 error 表示**这一次尝试没有结论**（渠道拒绝不是 error，那是 status='failed' 的结果）：
// 调用方 CreatePayment 会因此把幂等键退回去，让同一个 Idempotency-Key 能立刻重试。
func (s *PaymentService) dispatchCreate(ctx context.Context, payment *model.Payment, route paymentRoute, in CreateRequest, expiresAt time.Time, plan *repository.SettlementPlan) (*CreateResult, error) {
	if route.Method.Action == provider.ActionAccount {
		// 账户出资从这里分出去，且**在碰注册表之前**：这条路合法地没有渠道，注册表按
		// provider 名字也查不到它（见 resolveMethod）。
		return s.createAccountPayment(ctx, payment, route, in.OrderID)
	}

	// 没有付款人身份时在这里收场，**不把空 openid 交给适配器**：native_pay 是小程序内
	// requestPayment，报文里必须指名付款人（微信的预支付单没有 payer.openid 建不起来），
	// 而那个人是谁只能来自用户身份，渠道配置里没有。
	//
	// 适配器那边也会本地拒（wechat_v3 与 ums 都有这条），但那种拒返回的是 error，在服务层
	// 等价于「我们这侧出了问题」（见下面 callErr 那条分支）：支付单会停在 created 等超时关单，
	// 用户拿不到一句能读懂的话，运营在后台也看不出这一笔到底怎么了。而这其实是一个**结论**——
	// 这个人没绑微信，换一种支付方式就行——所以落 failed 并给出一个能查的 failure_code，
	// 与豆不够那次（createAccountPayment）同一个形状。
	//
	// 判据用 action 而不是渠道：服务层拿得到的只有这一项，而 native_pay 今天只有微信小程序
	// 一类渠道在用。真有「同为 native_pay 却不需要 openid」的渠道（例如云闪付）进来时，
	// 这一条会把它误判成失败，判据届时得挪进适配器——这个形状写在这里，因为它是这条守卫
	// 唯一会出错的边界。
	if route.Method.Action == provider.ActionNativePay && strings.TrimSpace(in.WalletOpenID) == "" {
		return s.markDeclined(ctx, payment, provider.CreateResult{
			Result:         provider.ResultFailed,
			FailureCode:    failureCodeWalletIdentityMissing,
			FailureMessage: "用户未绑定微信账号，无法发起小程序支付",
		})
	}

	// 到这里支付单已经在库里、状态是 created。下面无论走哪条分支，都有一个终止动作
	// （推进到 pending / failed，或者刻意什么都不做）。
	adapter, err := s.providers.Lookup(route.Channel.Provider)
	if err != nil {
		return nil, err
	}

	// 槽名**问适配器**（见 provider.SecretSlotter），不在这里读 config：槽名写在协议声明
	// 里（form_md5 是 `sign.secretRef`），而协议声明长什么样是适配器独占的知识。在这里写
	// `config["sign"]["secretRef"]` 等于把 form_md5 的配置格式抄进 service，下一个协议族
	// 进来时这行就成了「有的协议族读得到、有的读不到」的分支。
	//
	// 拿到的是**一组**槽：微信 v3 一条路上要三样（私钥 / 平台证书 / apiV3Key），form_md5
	// 一样就够。这一层不区分，照适配器报的名字逐个解析。
	//
	// 手工渠道没有实现那个接口，拿到的是只有兜底那一把的 Credentials——那正是它一直以来的
	// 行为（见 provider.ChannelSecret）。
	adapterMethod := route.providerMethod()
	secrets := resolveSecrets(s.resolveSecret, route.Channel, adapter, adapterMethod)
	createRequest := provider.CreateRequest{
		PaymentNo:    payment.PaymentNo,
		OrderNo:      in.OrderNo,
		UserID:       in.UserID,
		Amount:       in.Amount,
		Subject:      subjectFor(in),
		RequestID:    in.RequestID,
		WalletOpenID: in.WalletOpenID,
		Attach:       attachFor(in),
		NotifyURL:    s.notifyURL(route.Channel.Code),
		// 回跳地址与回调地址同源（见 service.returnURL）。它对**所有**渠道都填，而不是只给
		// H5 那条路填：只有 H5 的适配器会用它（小程序那条路的客户端不跳浏览器，见
		// provider.CreateRequest.ReturnURL），而在这一层按 action 分岔等于把「谁用得上它」
		// 这件事从适配器搬到 service——那正是 create.go 里「槽名问适配器」那段要避免的形状。
		ReturnURL: s.returnURL(route.Channel.Code),
		ExpiresAt: expiresAt,
		Method:    adapterMethod,
		Secrets:   secrets,
		// 分账随这次下单一起发出去（见 provider.DivisionInstruction）：银联商务的下单报文
		// 里有分账三键，钱在支付成功那一刻就已经分出去了，没有「再发起一次分账」这一步。
		Division: divisionFor(plan),
	}

	startedAt := s.now()
	createResult, callErr := adapter.Create(ctx, createRequest)
	duration := int(s.now().Sub(startedAt).Milliseconds())
	s.recordProviderCall(ctx, in.TraceID, payment, route, model.CallOperationCreate,
		factsOf(createRequest), createResult, callErr, duration)

	switch {
	case callErr != nil:
		// 契约：err 非 nil 只在「这次调用根本没发出去」时出现（见 provider.Provider 的注释）。
		// 渠道侧什么都没有，但这是我们自己的问题（密钥没配、参数拼不出来），不是渠道拒了，
		// 所以不落 failed 结论——支付单停在 created，让调用方看到一次真正的服务端错误。
		slog.ErrorContext(ctx, "payment provider create did not go out",
			"payment_no", payment.PaymentNo, "provider", route.Channel.Provider, "error", callErr)
		return nil, fmt.Errorf("create payment at provider %s: %w", route.Channel.Provider, callErr)

	case createResult.Result == provider.ResultSuccess:
		return s.markPending(ctx, payment, route, createResult, expiresAt)

	case createResult.Result == provider.ResultFailed:
		// 渠道明确拒绝是**一个结论，不是一个错误**：它落在 CreateResult.Status='failed' 上，
		// 与 gRPC 契约里那句「failed = 发起即失败（渠道拒绝、参数不全），客户端可以换方式重试」
		// 对应。返回 error 会让上层把一次正常的业务拒绝当成服务端故障去重试。
		return s.markDeclined(ctx, payment, createResult)

	default:
		// ResultTimeout / ResultUnknown / 适配器漏填（零值被记成 unknown）。
		slog.ErrorContext(ctx, "payment provider create returned an uncertain result",
			"payment_no", payment.PaymentNo, "provider", route.Channel.Provider,
			"result", string(createResult.Result))

		// 结果不明时先问渠道一次（见 provider.Querier）。这是那条「超时不能重试、也不能
		// 当失败」的规矩唯一**有结论**的出路：不做的话支付单只能停在 created 等超时关单，
		// 而用户可能真的把那笔钱付了。
		if querier, ok := adapter.(provider.Querier); ok {
			return s.settleByQuery(ctx, querier, payment, route, createRequest, expiresAt, in.TraceID)
		}
		return nil, fmt.Errorf("%w: %s", ErrProviderResultUncertain, payment.PaymentNo)
	}
}

// settleByQuery 在发起支付结果不明时，拿商户单号去渠道查一次再定论。
//
// 三种结局分开处理，与发起那次逐字对应：查出来成了 → 推进 pending（那一刻起这笔钱是真的
// 收了）；查出来明确失败 → 落 failed（用户换一种方式付）；查不出来（没实现查单、连不上、
// 报文读不懂）→ 维持「结果不明」。
//
// **查不出来时绝不能标 failed**：我们比查之前并没有更不知道，而标 failed 会把一笔可能
// 已收的钱从账上抹掉。
func (s *PaymentService) settleByQuery(ctx context.Context, querier provider.Querier, payment *model.Payment, route paymentRoute, request provider.CreateRequest, expiresAt time.Time, traceID string) (*CreateResult, error) {
	startedAt := s.now()
	queryResult, callErr := querier.Query(ctx, provider.QueryRequest{
		PaymentNo: request.PaymentNo,
		OrderNo:   request.OrderNo,
		RequestID: request.RequestID,
		Method:    request.Method,
		// 复用发起时解出来的那一份，不重新解析：查单必须与下单用同一批凭据，否则「换过一次
		// 密钥」的那段时间里我们签得出去、却查不回来。
		Secrets: request.Secrets,
	})
	duration := int(s.now().Sub(startedAt).Milliseconds())
	s.recordProviderCall(ctx, traceID, payment, route, model.CallOperationQuery,
		factsOf(request), queryResult, callErr, duration)

	if callErr != nil {
		slog.WarnContext(ctx, "payment provider query did not answer",
			"payment_no", payment.PaymentNo, "provider", route.Channel.Provider, "error", callErr)
		return nil, fmt.Errorf("%w: %s", ErrProviderResultUncertain, payment.PaymentNo)
	}

	switch queryResult.Result {
	case provider.ResultSuccess:
		return s.markPending(ctx, payment, route, queryResult, expiresAt)
	case provider.ResultFailed:
		return s.markDeclined(ctx, payment, queryResult)
	default:
		slog.WarnContext(ctx, "payment provider query could not resolve the payment",
			"payment_no", payment.PaymentNo, "provider", route.Channel.Provider,
			"result", string(queryResult.Result))
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
//
// plan 是这一趟算好的分账计划，与支付单同一个事务落库（见仓储的 insertSettlementTask）。
// 命中幂等回放时它被丢掉，而那也是对的：任务在上一次那一趟里已经建好了，再建一条会撞
// settlement_tasks_payment_uniq。
func (s *PaymentService) beginPayment(ctx context.Context, in CreateRequest, route paymentRoute, expiresAt time.Time, plan *repository.SettlementPlan) (*model.Payment, *CreateResult, error) {
	payment, snapshot, hit, err := s.repository.BeginPayment(ctx, repository.BeginPaymentParams{
		PaymentNo:   s.newPaymentNo(s.now()),
		OrderNo:     in.OrderNo,
		UserID:      in.UserID,
		Amount:      in.Amount,
		Provider:    route.Provider(),
		Method:      route.Method.Code,
		Subject:     subjectFor(in),
		Attach:      attachFor(in),
		RequestID:   in.RequestID,
		ExpiresAt:   expiresAt,
		RequestHash: createRequestHash(in),
		// 分账任务与支付单同一个事务（见仓储的 insertSettlementTask）。命中幂等回放时这一份
		// 被丢掉是对的：任务在**上一次**那一趟里已经建好了。
		Settlement: plan,
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

// abandonPaymentAttempt 把一次**没走完**的发起尝试退回去，让同一个 Idempotency-Key 能立刻重试。
//
// 只在 dispatchCreate 返回 error 时调它（那条路已经排除了「有结论」的失败——渠道拒绝与豆不够
// 都会落成 status='failed' 的结果，幂等行在那两段事务里已经走到终态）。
//
// 退回的重活在仓储那边（PostgresRepository.AbandonPaymentAttempt，那里写清了三条硬约束各自的
// 落点）。这一层只做一件事：**失败不让它变成另一个错误**。退不回去的后果是同一个 key 要等
// 15 分钟回收窗口，而那个后果与调用方此刻手上的错误无关——把一次「渠道没注册」变成一次
// 「数据库写不进去」，只会让排查的人去看错地方。所以这里只记日志。
func (s *PaymentService) abandonPaymentAttempt(ctx context.Context, payment *model.Payment) {
	if payment == nil || payment.RequestID == "" {
		return
	}
	if err := s.repository.AbandonPaymentAttempt(ctx, payment.RequestID, payment.ID); err != nil {
		slog.ErrorContext(ctx, "failed to release the idempotency key of an abandoned payment attempt",
			"payment_no", payment.PaymentNo, "request_id", payment.RequestID, "error", err)
	}
}

// clientPayParams 把两层的支付参数合起来交给客户端。
//
// 下层是**支付方式上人工配的 params**（payment_methods.params），上层是适配器这一次算出来的
// 参数，同名键以后者为准。顺序不能反：适配器给的键是从渠道应答里读出来的当下事实（预支付
// 单号、跳转地址、二次签名），人工配的那些是运营预先定好的展示值或兜底值——冲突时以事实为准。
//
// 渠道的 config **不在这两层里**。早先的注释说「ChannelConfig 与 Params 两层合并后返回
// 客户端」，那是在 config 还只装商户号一类的年代写的；现在它装着协议声明（签名规则、渠道
// 地址），整份发到浏览器等于把对接细节送给每一个打开了收银台的人。客户端可见的那部分由
// 协议族按 `response.payParams` 显式挑出来，见 formmd5。
//
// 返回值**永不为 nil**：客户端拿到 nil 会渲染出一个空收银台，而真正的异常是适配器没给参数，
// 那件事该由适配器去报，不该变成前端的一个空白页。
func clientPayParams(route paymentRoute, createResult provider.CreateResult) map[string]string {
	merged := make(map[string]string, len(route.Method.Params)+len(createResult.PayParams))
	for key, value := range route.Method.Params {
		merged[key] = value
	}
	for key, value := range createResult.PayParams {
		merged[key] = value
	}
	return merged
}

// markPending 是成功那一段：把支付单推进到 pending、落出资行、完成幂等记录。
func (s *PaymentService) markPending(ctx context.Context, payment *model.Payment, route paymentRoute, createResult provider.CreateResult, expiresAt time.Time) (*CreateResult, error) {
	result := &CreateResult{
		PaymentNo:     payment.PaymentNo,
		Status:        model.PaymentPending,
		Action:        string(route.Method.Action),
		PayParams:     clientPayParams(route, createResult),
		ExpiresAtUnix: expiresAt.Unix(),
	}

	if _, err := s.repository.MarkPaymentPending(ctx, repository.MarkPaymentPendingParams{
		PaymentID:             payment.ID,
		ProviderTransactionID: createResult.ProviderTransactionID,
		// 本轮只有渠道出资一条线。写成切片是为了让账户出资（咖啡豆）接进来时
		// 不用改这段事务的形状。
		FundingLines: []repository.FundingLine{{
			LineNo: 1,
			// 出资行存的就是**支付方式的 code**（那套「出资渠道」词表
			// 已经退场，见 payment/012）。所以这一行说的是「用户点的支付宝」，不是「走了 UMS」。
			LineType: payment.PaymentMethod,
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
func (s *PaymentService) createAccountPayment(ctx context.Context, payment *model.Payment, route paymentRoute, orderID string) (*CreateResult, error) {
	if s.beans == nil {
		// 这个部署没接账户域。**不能**跳过扣豆直接结算——那等于白送一单。
		return nil, fmt.Errorf("%w: account funding is not configured", ErrProviderNotConfigured)
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

	// 出资行的 line_type 由 catalog 里那条支付方式声明（这一条路上必然等于 'coffee_bean'），
	// 不在这里写死。Action 同样取自目录，客户端据此知道不需要再调起任何东西。
	result := &CreateResult{
		PaymentNo: payment.PaymentNo,
		Status:    model.PaymentSucceeded,
		Action:    string(route.Method.Action),
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

// failureCodeWalletIdentityMissing 是「这一笔需要一个付款人的微信身份，而取不到」那条失败
// 结论的码。
//
// 它与渠道回的那些码（RISK_REJECTED 之类）分开，是因为**该做的事不同**：渠道拒了让用户换
// 一张卡，这一条要用户先去绑定微信。混成一个，客服在后台只能看到一句渠道的话，而真正的
// 动作在前端。
const failureCodeWalletIdentityMissing = "wallet_identity_missing"

// paymentRoute 是「这一次支付要走的支付方式，以及它落的渠道」。
//
// 渠道为 nil 是**合法**的，而且只有一种情况：账户出资那条路没有第三方（见 catalog.Method
// 的 ChannelCode）。把这两样捆成一个值而不是分两次取，是因为它们必须一起生效——分开传的
// 写法迟早出现「方式取的是这一条、渠道取的是上一条」。
type paymentRoute struct {
	Method  catalog.Method
	Channel *catalog.Channel
}

// providerMethod 把它翻成适配器只读的那个 shape（见 provider.Method）。
//
// 这个翻译仍然留在服务层而不是让适配器直接收 catalog.Method：适配器是**协议**的实现，它
// 不该认识「支付方式目录」这个概念。它今天收到的那个结构，与从前从两张表投影出来的那个
// 逐字段一致——所以这四种支付方式收口成常量之后，四个适配器里没有一行需要改。
func (r paymentRoute) providerMethod() provider.Method {
	method := provider.Method{
		Code:   r.Method.Code,
		Action: r.Method.Action,
		Params: r.Method.Params,
	}
	if r.Channel != nil {
		method.ChannelCode = r.Channel.Code
		method.Provider = r.Channel.Provider
		method.ChannelConfig = r.Channel.Config
	}
	return method
}

// Provider 是落进 payments.provider 的那个名字，账户出资没有渠道，落空串。
func (r paymentRoute) Provider() string {
	if r.Channel == nil {
		return ""
	}
	return r.Channel.Provider
}

// resolveMethod 按 code 从常量表里取支付方式，并把它的渠道一起取出来。
//
// 这里不查任何库：支付方式是代码里的常量，渠道那两行已经不存在了（见 catalog 的包注释）。
// 于是「支付方式查一次、渠道再查一次、两者的关联校验第三次」这三段查询、以及它们各自的
// not-found 分支全部消失，只剩两次 map 查找。
//
// 从前那些**状态**检查（渠道是不是 enabled、是不是 legacy_readonly、provider 名字空不空）
// 也一并没了：它们存在的理由是「渠道是运营建的，运营可能把它停掉或填半截」。今天渠道是代码
// 里的一条常量，配不出来的渠道根本不会出现在表里。
//
// 剩下唯一一个能出问题的方向是**部署没把环境变量填全**（密钥、商户号），所以这里查一次
// MissingEnv 并拒绝。点的是**环境变量名**而不是配置键名：读这条错误的人手里拿着的是 .env
// 或部署清单，不是适配器认得的那棵树。这也正是「配不全就发不出去」该有的样子——比让适配器
// 拿着空商户号去发报文、被渠道回一句与本意无关的错要好。
func (s *PaymentService) resolveMethod(code string) (paymentRoute, error) {
	method, err := s.catalog.Method(code)
	if err != nil {
		return paymentRoute{}, err
	}
	channel, err := s.catalog.ChannelFor(method)
	if err != nil {
		return paymentRoute{}, err
	}
	if method.Action != provider.ActionAccount && channel == nil {
		// 非账户出资却没有渠道：只可能是常量表自己写错了（catalog.FromConfig 里那几条
		// 声明的 ChannelCode 拼错或漏填）。在这里明确报错，好过拿着 nil 渠道走到下面
		// dispatchCreate 里空指针。
		return paymentRoute{}, fmt.Errorf("%w: method %s has no channel", ErrChannelIncomplete, method.Code)
	}
	if channel != nil && len(channel.MissingEnv) > 0 {
		return paymentRoute{}, fmt.Errorf("%w: channel %s is missing %s",
			ErrChannelIncomplete, channel.Code, strings.Join(channel.MissingEnv, ", "))
	}
	return paymentRoute{Method: method, Channel: channel}, nil
}

// divisionFor 把发起那一刻算好的分账计划翻成适配器要的指令。
//
// 两种「不下发」在这里合成同一个 nil：计划为空（账户出资——咖啡豆没有渠道资金可动，见
// buildSettlementPlan），以及计划里一个接收方都没有（没命中规则，或各家都算出 0，整单归
// 平台）。后者尤其不能翻成「一个空子单数组」：规范说 divisionFlag=true 时 subOrders 不能为
// 空，那样发过去渠道会拒——而被拒掉的是**整笔支付**，不是分账。
//
// 子商户号取 ReceiverID（账户在渠道那边的号，settlement_accounts.receiver_id），不是
// AccountID：后者是我们库里的 uuid，渠道不认。
//
// 这里不复算恒等式：`Σ子单 + 平台 = 实付` 在建任务那一刻由 checkSettlementIdentity 从库里
// 回读校验过（见 repository/settlement.go），金额到这一步已经是事实。
func divisionFor(plan *repository.SettlementPlan) *provider.DivisionInstruction {
	if plan == nil || len(plan.Receivers) == 0 {
		return nil
	}
	subOrders := make([]provider.DivisionSubOrder, 0, len(plan.Receivers))
	for _, receiver := range plan.Receivers {
		subOrders = append(subOrders, provider.DivisionSubOrder{
			Mid:    receiver.ReceiverID,
			Amount: receiver.Amount,
		})
	}
	return &provider.DivisionInstruction{
		PlatformAmount: plan.PlatformAmount,
		SubOrders:      subOrders,
	}
}

// providerCallFacts 是一条渠道调用流水里「被问的那一单」的事实。
//
// 它从前就是 provider.CreateRequest 本身，因为写这条流水的只有发起那一条路。主动查单
// （reconcile.go）加进来之后那个类型不再贴切：查单发生时本次进程里根本没有一次发起，它只是
// 拿着一个已经存在的支付单去问一句「成了没」。这里收的是两条路都真的有的那几项——摘要里
// 描述的始终是**被问的那一单**，而不是「一次新的发起」。
type providerCallFacts struct {
	PaymentNo string
	OrderNo   string
	Amount    int64
	RequestID string
	Method    provider.Method
	// Division 是这次发起**带下去**的分账指令，nil = 没带。查单那条路没有它，那一趟确实没
	// 带什么指令下去。摘要里要能看出「这一笔有没有分账」，否则运维拿一笔支付单去看出网流水，
	// 分账发没发过在流水上没有任何痕迹——而分账明细在另一个库表上，两边对不起来时先看的就是
	// 这条流水。
	Division *provider.DivisionInstruction
}

// factsOf 从一次发起请求里取出那几项事实。
func factsOf(request provider.CreateRequest) providerCallFacts {
	return providerCallFacts{
		PaymentNo: request.PaymentNo,
		OrderNo:   request.OrderNo,
		Amount:    request.Amount,
		RequestID: request.RequestID,
		Method:    request.Method,
		Division:  request.Division,
	}
}

// recordProviderCall 落一条渠道调用流水。
//
// 失败只记日志，不往上抛：流水是给运维查的，钱的状态比它重要。让一次流水写入失败
// 连带把支付单的推进回滚掉，是把「查不到」升级成了「收不到钱」。
// operation 区分「发起」与「查单」两条流水（另见 model.CallOperation*）。查单也走这条
// 函数、也用同一个 facts 形状建摘要：paymentNo / orderNo / amount / channel 对两者是
// 同一组事实。
func (s *PaymentService) recordProviderCall(ctx context.Context, traceID string, payment *model.Payment, route paymentRoute, operation string, facts providerCallFacts, result provider.CreateResult, callErr error, durationMS int) {
	summary := map[string]any{
		"paymentNo": facts.PaymentNo,
		"orderNo":   facts.OrderNo,
		"amount":    facts.Amount,
		"action":    string(facts.Method.Action),
		"channel":   facts.Method.ChannelCode,
		// 渠道返回的支付参数不进流水：里面有跳转链接一类的凭证，方案 11.5 只允许留
		// 能定位这一笔的键。这里记的是「它给了几个参数」而不是参数本身。
		"payParamKeys": sortedKeys(result.PayParams),
	}
	if facts.Division != nil {
		// 只记「分了几笔、平台留了多少」，不记子商户号与各家金额：这条流水的用处是回答
		// 「这一笔带没带分账」，明细在 settlement_receivers 上，两边分开看才不重复。
		summary["subOrderCount"] = len(facts.Division.SubOrders)
		summary["platformAmount"] = facts.Division.PlatformAmount
	}
	s.writeProviderCall(ctx, providerCallRecord{
		TraceID:         traceID,
		Provider:        route.Provider(),
		Operation:       operation,
		PaymentNo:       payment.PaymentNo,
		RequestID:       facts.RequestID,
		Summary:         summary,
		ResponseSummary: result.ResponseSummary,
		HTTPStatus:      result.HTTPStatus,
		ProviderCode:    result.FailureCode,
		ProviderMessage: result.FailureMessage,
		Result:          result.Result,
		Attempts:        result.Attempts,
		DurationMS:      durationMS,
		Err:             callErr,
	})
}

// providerCallRecord 是一条渠道调用流水里那几个与「这是什么操作」无关的字段。
//
// 它存在是因为写流水的函数有两个调用方（create.go 的发起与查单、refund.go 的退款与退款
// 查询），而它们各自要填的**摘要是不同的**——发起那条路记 payParamKeys，退款那条路记
// refundNo。把摘要收成一个 map 再传进来，比让两边各自拼一遍 ProviderCallParams 好：
// 落库那一半（零值兜底、可空的 HTTP 状态、失败只记日志）只有一处。
type providerCallRecord struct {
	TraceID   string
	Provider  string
	Operation string
	PaymentNo string
	// AgreementNo 是协议那两条路（签约、查约）的定位键。它与 PaymentNo 并列而不是互斥：
	// 一张支付流水上不会有协议号，一份协议上也永远不会有支付单号（代扣不建 payments 行，
	// 见 model.PaymentAgreementCharge.PaymentNo）。
	AgreementNo     string
	RequestID       string
	Summary         map[string]any
	ResponseSummary map[string]any
	HTTPStatus      int
	ProviderCode    string
	ProviderMessage string
	Result          provider.Result
	Attempts        int
	DurationMS      int
	Err             error
}

// writeProviderCall 是两条操作路共用的落库收口。
func (s *PaymentService) writeProviderCall(ctx context.Context, record providerCallRecord) {
	if record.Err != nil {
		// 出错时**在摘要里补一条 error**，而不是把它丢掉：一次连渠道都没发出去的调用，
		// 看流水的人要能一眼看出是「没发出去」而不是「渠道没答」。
		if record.Summary == nil {
			record.Summary = map[string]any{}
		}
		record.Summary["error"] = record.Err.Error()
	}
	attempts := record.Attempts
	if attempts == 0 {
		// 与 CreateResult.Attempts 同一条约定：0 当 1。适配器漏填时，记 0 会让「发出去过一次」
		// 显示成「一次都没发」，而流水是要拿去对账的。
		attempts = 1
	}
	durationMS := record.DurationMS
	if err := s.repository.RecordProviderCall(ctx, repository.ProviderCallParams{
		Provider:        record.Provider,
		Operation:       record.Operation,
		PaymentNo:       record.PaymentNo,
		AgreementNo:     record.AgreementNo,
		RequestID:       record.RequestID,
		TraceID:         record.TraceID,
		AttemptNo:       attempts,
		RequestSummary:  record.Summary,
		ResponseSummary: record.ResponseSummary,
		HTTPStatus:      httpStatus(record.HTTPStatus),
		ProviderCode:    record.ProviderCode,
		ProviderMessage: record.ProviderMessage,
		// 零值兜底：适配器漏填 Result 时记成 unknown，那对「我们确实不知道」是恰好正确的。
		Result:     providerCallResult(record.Result),
		DurationMS: &durationMS,
	}); err != nil {
		slog.WarnContext(ctx, "failed to record payment provider call",
			"payment_no", record.PaymentNo, "provider", record.Provider,
			"operation", record.Operation, "error", err)
	}
}

// recordOperationCall 落一条**操作**（退款 / 退款查询 / 关单）的渠道调用流水。
//
// 与 recordProviderCall 是同一次落库的两种摘要，见 providerCallRecord。
func (s *PaymentService) recordOperationCall(ctx context.Context, traceID string, payment *model.Payment, route paymentRoute, operation string, facts operationFacts, result provider.OperationResult, callErr error, durationMS int) {
	s.writeProviderCall(ctx, providerCallRecord{
		TraceID:   traceID,
		Provider:  route.Provider(),
		Operation: operation,
		PaymentNo: payment.PaymentNo,
		RequestID: facts.RequestID,
		Summary: map[string]any{
			"paymentNo": facts.PaymentNo,
			"orderNo":   facts.OrderNo,
			// 退款单号是这条流水与同一张支付单上其它调用**唯一**区分得开的键：
			// payment_provider_calls 上退款与退款查询长得一模一样，只有 operation 列不同，
			// 而一笔支付可以有多张退款单。金额同样只对退款的发起那次有意义（查询与关单不传），
			// 记 0 如实反映这一点。
			"refundNo": facts.RefundNo,
			"amount":   facts.Amount,
			"action":   string(facts.Method.Action),
			"channel":  facts.Method.ChannelCode,
		},
		ResponseSummary: result.ResponseSummary,
		HTTPStatus:      result.HTTPStatus,
		ProviderCode:    result.FailureCode,
		ProviderMessage: result.FailureMessage,
		Result:          result.Result,
		Attempts:        result.Attempts,
		DurationMS:      durationMS,
		Err:             callErr,
	})
}

// httpStatus 把适配器报的状态码翻成可空的整数。
//
// 0 落成 NULL 而不是 0：这一列的定义是「渠道回了什么」，而「没有应答」（超时、连接没建起来）
// 与「渠道回了 0」是两件事，后者根本不是一个合法的 HTTP 状态码。页面上 NULL 显示 `—`。
func httpStatus(code int) *int {
	if code <= 0 {
		return nil
	}
	return &code
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
		OrderNo       string
		UserID        string
		Amount        int64
		PaymentMethod string
	}{
		OrderNo:       in.OrderNo,
		UserID:        in.UserID,
		Amount:        in.Amount,
		PaymentMethod: in.PaymentMethod,
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
	if strings.TrimSpace(in.PaymentMethod) == "" {
		return ErrMethodRequired
	}
	// 这里**不查表**：code 认不认识由 resolveMethod 拿常量表判，判不出来的是
	// ErrMethodNotFound（一个明确的「没有这种支付方式」），而不是格式错误。
	if strings.TrimSpace(in.RequestID) == "" {
		// 没有幂等号就没有「重复发起只落一张单」的依据：用户双击一次支付按钮会变成
		// 两张支付单，而订单只能被收一次钱，第二张会在支付成功时撞上
		// payments_one_succeeded_per_order 变成一次看不懂的失败。
		return ErrRequestIDRequired
	}
	if !model.IsSettlementBizType(in.BizType) {
		// biz_type 是规则命中键的第一段，写错只会安静地少一次分账。所以它在这里被拦下来，
		// 而不是等到分账任务建出来才发现整单归了平台。
		//
		// store_id / device_id 有意不校验：它们落的是快照列（TEXT），不是 uuid 列，而值来源
		// 是订单库——在这里做一次 uuid 形状检查，只会让「订单库里存着一个非 uuid 的点位」
		// 变成用户付不了款。
		return ErrBizTypeInvalid
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
