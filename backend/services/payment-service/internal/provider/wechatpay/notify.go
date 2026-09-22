package wechatpay

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 本文件是**入站**的那一半：读懂并验证一份签约/解约通知。
//
// 它是「纯签约」那条路的收尾——签约在服务端只算了一个签名（见 client.go 的 SignParams），
// 用户拿它去微信那个小程序里点确认，然后微信往 notify_url 推一条**协议变更通知**，那才是
// 「这份协议现在有效了」这句话第一次到达我们这边。
//
// # 与查约那条路的关系
//
// 同一件事的两个入口：这条是渠道路过来说的，QueryContract 是我们主动去问的。两条最后必须
// 落回同一个状态机（见 repository.applyAgreementTarget）——所以这里翻出来的 provider.State
// 与 QueryContract 翻出来的 ContractState 是**同一套词**，不许各写一份。

// 微信在协议变更通知里用的两个 change_type 取值。
//
// 只有这两个：`change_type == ""` 的报文是**支付中签约的付款结果**（下单与签约合成一次请求
// 那个入口的回传），它说的是钱不是协议，本刀不做（见 plan §五）。
const (
	changeTypeAdd    = "ADD"
	changeTypeDelete = "DELETE"
)

// 三件「验签过了、但这条路上没有可做的事」，共用 provider.ErrNotificationNotActionable：
//
//   - `change_type` 为空：支付中签约的付款结果通知，本刀不做；
//   - `change_type` 是我们不认识的取值（微信加了新类型，或者报文是拼出来的）；
//   - `return_code != SUCCESS`：微信自己在通知里说了这次操作没成。
//
// 这三件在 service 那边的处置完全相同：记一条留痕、回失败应答、一个字段都不动。**不静默
// 丢掉**——老系统那条路（subscription_service.go:301）对前两种回的是成功应答然后什么都不做，
// 而那样做的前提是「我们知道它是什么、且知道它不需要动作」。这里恰恰是我们不知道。
// 回失败应答的代价只是微信按它自己的重投计划再推几次：有界，且同一个报文重推会撞上
// UNIQUE (provider, notification_id)，不会刷出一堆行。
//
// 三种在错误串里都带着各自的原话（change_type 是空还是别的、return_code 是什么），所以
// 看到那条失败记录的人不必猜是哪种。

// AgreementNotify 是一份读懂了的协议变更通知。
type AgreementNotify struct {
	// ChangeType 是报文里的原话（ADD / DELETE）。
	ChangeType string
	// ContractCode 是**我们自己的**协议号（报文里的 contract_code）。
	//
	// **解约通知不带它**——微信的 DELETE 报文里只有 contract_id。所以它是「可能有值」而不是
	// 「一定有值」，调用方必须能按 ProviderContractID 找回协议（见 provider.AgreementNotification
	// 的同名字段）。
	ContractCode string
	// ProviderContractID 是微信侧的协议号（contract_id）。两类通知都带着它，签约成功之后
	// 它是扣款与解约的输入，所以要落进 payment_agreements.contract_no。
	ProviderContractID string
	// ProviderState 是报文里的 contract_state。
	//
	// **多半是空的**：微信的协议变更通知并不保证带这个字段（老系统的 ADD 分支根本不读它）。
	// 留成 ContractState 的零值而不是猜一个——「报文没写」与「写的是 0」是两件事。
	ProviderState ContractState
}

// State 把 change_type 翻成渠道无关的协议状态。**这是个全函数**：两个合法取值各有一个结论，
// 其余取值在这之前就已经被拒了（见 ParseAgreementNotify），所以这里不需要默认分支去猜。
//
// 为什么不把这一步放在 service 里：它是**微信的**词汇（ADD / DELETE）到我们词汇的翻译，
// 与 QueryContract 里那三个错误码的翻译是同一类事——渠道特有的东西只在这一个包里出现。
func (n AgreementNotify) State() provider.AgreementState {
	if n.ChangeType == changeTypeDelete {
		return provider.AgreementTerminated
	}
	return provider.AgreementSigned
}

// ParseAgreementNotify 验签并读懂一份协议变更通知。
//
// # 顺序：先验签，再读字段
//
// 与支付回调那条路逐字同一条（见 service.HandleNotification 的注释）：报文里的 contract_code
// 是不可信输入，拿它去找协议等于让攻击者决定我们去看哪一行。所以 `sign` 验不过就地返回，
// 后面那些字段一个都不读。
//
// 返回的 params 是解析出来的原始键值表，只给调用方做**脱敏摘要**用（见 responseSummary 的
// 白名单）——报文原文除 payment_notifications.body 外不落到任何地方（方案 11.5）。
//
// # 两个 change_type 的必填字段不同
//
//	ADD    contract_code **与** contract_id 都要有（老系统这里是两个 if 连着判）
//	DELETE 只要 contract_id —— 微信的解约通知里根本没有 contract_code
//
// 这条差别是这个函数最要紧的一件事：它意味着「按协议号找回那份协议」对 DELETE 是走不通的，
// 调用方必须能按 contract_id 找（见 repository.FindAgreementForNotification）。
//
// 缺字段一律返回错误而**不是**填一个空值往下走：一份 ADD 通知没有 contract_code，说明它
// 要么不是我们该处理的、要么是拼出来的，而两种情况下按空协议号去查库都会查出一片空白，
// 最终表现为一句语焉不详的「查无此约」。
func ParseAgreementNotify(body []byte, secret string) (AgreementNotify, map[string]string, error) {
	var notify AgreementNotify

	params, err := decodeXML(body)
	if err != nil {
		return notify, nil, fmt.Errorf("%w: %s", provider.ErrInvalidNotification, err)
	}
	// 验签**在碰任何字段之前**。verifySign 自己会判密钥为空（见它的注释）：密钥没配时这里
	// 回的是 ErrSecretNotConfigured，而不是「签名不对」——那两件事的排查方向完全不同。
	if err := verifySign(params, secret); err != nil {
		return notify, params, err
	}

	// 走到这里报文一定是我们认的（签名盖住了全部字段），下面读到的每个值都可信。
	if code := strings.TrimSpace(params["return_code"]); code != returnCodeSuccess {
		return notify, params, fmt.Errorf("%w: return_code is %q (return_msg=%q)",
			provider.ErrNotificationNotActionable, code, strings.TrimSpace(params["return_msg"]))
	}

	notify.ChangeType = strings.TrimSpace(params["change_type"])
	notify.ContractCode = strings.TrimSpace(params["contract_code"])
	notify.ProviderContractID = strings.TrimSpace(params["contract_id"])
	notify.ProviderState = ContractState(strings.TrimSpace(params["contract_state"]))

	switch notify.ChangeType {
	case changeTypeAdd:
		if notify.ContractCode == "" || notify.ProviderContractID == "" {
			return notify, params, fmt.Errorf("%w: an ADD notification needs both contract_code and contract_id (contract_code=%q contract_id=%q)",
				provider.ErrInvalidNotification, notify.ContractCode, notify.ProviderContractID)
		}
	case changeTypeDelete:
		if notify.ProviderContractID == "" {
			return notify, params, fmt.Errorf("%w: a DELETE notification has no contract_id (contract_code=%q)",
				provider.ErrInvalidNotification, notify.ContractCode)
		}
	default:
		// 空串落在这一支：那是支付中签约的付款结果（见上面那三种）。
		return notify, params, fmt.Errorf("%w: change_type is %q",
			provider.ErrNotificationNotActionable, notify.ChangeType)
	}
	return notify, params, nil
}

// ChargeNotify 是一份读懂了的扣款结果通知（`pay/pappayapply` 的回调）。
//
// # 它说的是「这一期扣到了没有」，不是「这份协议怎样」
//
// 协议在成功与失败两种情形下都是 active（用户没失去授权，只是这一期没扣到钱），所以这份
// 报文里**没有**任何能改协议状态的字段。它的全部用处是把 payment_agreement_charges 的某一行
// 从「受理中」推到「扣到了」或「没扣到」。
type ChargeNotify struct {
	// OutTradeNo 是**我们的商户单号**（报文里的 out_trade_no），也是这一期唯一的关联键。
	OutTradeNo string
	// ProviderTransactionID 是微信支付订单号（transaction_id）。渠道拒一笔时它是**空的**
	// ——钱没动过，自然没有单号。
	ProviderTransactionID string
	// ProviderContractID 是微信侧的协议号（contract_id）。只进流水与排查。
	ProviderContractID string
	// Amount 是**实付金额**（total_fee），单位分。
	//
	// 它必须与本地那一期记的金额一致才算数（见 repository.ErrChargeAmountMismatch）。这是
	// 老系统完全没有的一道闸：它拿到报文就把那笔账记成这一期的账，金额是报文说了算。
	Amount int64
	// Result 是结论：ResultSuccess / ResultFailed。
	Result provider.Result
	// FailureCode / FailureMessage 是微信的 err_code / err_code_des，只在 ResultFailed 时有值。
	FailureCode    string
	FailureMessage string
}

// ParseChargeNotify 验签并读懂一份扣款结果通知。
//
// # 顺序与协议通知逐字同一条
//
// 先验签、再判两层 return_code / result_code、最后才读字段。报文里的 out_trade_no 是不可信
// 输入，拿它去查库等于让攻击者决定我们去看哪一行——所以 `sign` 验不过就地返回，后面那些字段
// 一个都不读（见 service.HandleNotification 的注释）。
//
// # 两层码的分工
//
//	return_code != SUCCESS      微信自己在通知里说了这次通知没送成 → 不是结论，拒
//	result_code != SUCCESS      微信收下了扣款请求、但**钱没扣到**（余额不足、风控）→ 是结论
//
// 第二层正是老系统搞错的地方：它把同步应答的 result_code=SUCCESS 当成了扣款成功。而这条
// 通知里的 result_code=FAIL 说的是「这一期没扣到」，它同样是一条**结论**——调用方要把它记成
// failed 并排下一次重试，不是当成一条读不懂的报文丢掉。
func ParseChargeNotify(body []byte, secret string) (ChargeNotify, map[string]string, error) {
	var notify ChargeNotify

	params, err := decodeXML(body)
	if err != nil {
		return notify, nil, fmt.Errorf("%w: %s", provider.ErrInvalidNotification, err)
	}
	if err := verifySign(params, secret); err != nil {
		return notify, params, err
	}

	if code := strings.TrimSpace(params["return_code"]); code != returnCodeSuccess {
		return notify, params, fmt.Errorf("%w: return_code is %q (return_msg=%q)",
			provider.ErrNotificationNotActionable, code, strings.TrimSpace(params["return_msg"]))
	}

	notify.OutTradeNo = strings.TrimSpace(params["out_trade_no"])
	if notify.OutTradeNo == "" {
		// 没有它这一条通知就没有任何用处：报文里没有协议号、没有期次，别的字段都定位不到
		// 那一期（见 provider.AgreementChargeNotification 的注释）。
		return notify, params, fmt.Errorf("%w: a charge notification carries no out_trade_no",
			provider.ErrInvalidNotification)
	}

	notify.ProviderTransactionID = strings.TrimSpace(params["transaction_id"])
	notify.ProviderContractID = strings.TrimSpace(params["contract_id"])

	// 金额是**必填**，而且解析不了就是报文不对：把读不懂的金额当成 0 往下走，会以一次
	// 「金额对不上」的拒绝收场——那句话说的是「渠道报了一个不同的数」，而事实是我们根本没
	// 读懂它。两种要人去查的方向完全不同。
	totalFee := strings.TrimSpace(params["total_fee"])
	amount, err := strconv.ParseInt(totalFee, 10, 64)
	if err != nil {
		return notify, params, fmt.Errorf("%w: total_fee is %q", provider.ErrInvalidNotification, totalFee)
	}
	notify.Amount = amount

	if code := strings.TrimSpace(params["result_code"]); code == returnCodeSuccess {
		notify.Result = provider.ResultSuccess
	} else {
		notify.Result = provider.ResultFailed
		notify.FailureCode = strings.TrimSpace(params["err_code"])
		notify.FailureMessage = strings.TrimSpace(params["err_code_des"])
	}
	return notify, params, nil
}
