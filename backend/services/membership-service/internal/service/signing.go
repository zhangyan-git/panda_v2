package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 小程序端的签约（连续包月）。两个接口，一条链：
//
//	POST /v1/miniapp/membership/subscriptions          发起签约 → 客户端拿跳转参数去微信
//	POST /v1/miniapp/membership/subscriptions/confirm  用户从微信回来，问渠道签成没有
//
// # 钱与时长一个字都不从请求体来
//
// 请求体里只有「选哪一款套餐」与一个幂等号（见 dto.CreateSubscriptionRequest）。每期扣多少、
// 扣多久、渠道侧的模板 id 全部取自套餐，并在签约那一刻**冻进订阅行**——套餐后来调价不改变
// 已签约的人，这正是那组快照列存在的理由。
//
// # 谁在签，只认令牌
//
// 没有 user_id 参数。更重要的是 **openid 也不从请求体来**：它是「用谁的身份签这份代扣协议」
// 的凭据，只能由服务间的事实在身份域问出来（见 WalletOpenIDReader）。
//
// # 这一条链落在哪一步为止
//
// 发起之后是**用户拿着跳转参数去微信里点「同意」**——那一步不在我们的进程里，也不在这一次
// 应答里。所以发起成功返回的订阅状态是 pending_sign，确认之前它不是一分钱的关系。把 pending
// 当签成是这条链上最容易犯的错：用户会被告知「已开通」，而他根本没点。

// ============================================================
// 发起签约
// ============================================================

// CreateSubscription 是「开通连续包月」的第一次点击。
//
// # 顺序是这一步的全部要点
//
// 四道检查按这个次序排，每一条都在挡一种**钱上的错**：
//
//  1. 幂等（反查 requestId）——同一次点击的重试**不再建协议**，回上一次那一条。放在最前，
//     因为后面每一条对重试来说都是错的答案：「你已经有活着的订阅了」那个订阅正是它自己建的。
//  2. 活订阅——**必须在调支付服务之前**。晚一步就会在微信里多建一份协议，而那一份是用户能
//     点开的：他名下凭空多了第二份「每月自动续费」的授权。
//  3. 套餐能签——auto_renew 且 wechat_plan_id 非空。少了后者，渠道那份协议没有模板可挂。
//  4. openid——签约要素，问不到就不往下走（见 ErrWalletIdentityRequired）。
//
// 建协议在写库**之前**：反过来的话，一次「协议没建成」的失败会在库里留下一条 pending_sign 的
// 活订阅，而它是活着的——这个人之后每一次签约都会被自己那条永远签不成的订阅挡住。
func (s *MembershipService) CreateSubscription(ctx context.Context, userID string, req dto.CreateSubscriptionRequest, traceID string) (*dto.SubscriptionSigningResponse, error) {
	uid, err := requiredID(userID, ErrUserIDRequired, ErrUserIDInvalid)
	if err != nil {
		return nil, err
	}
	planID, err := requiredID(req.PlanID, ErrPlanIDRequired, ErrPlanIDInvalid)
	if err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		// 不替客户端编一个：签约会在渠道那边留一份协议，而幂等号是**认出「同一次点击」的唯一
		// 凭据**。服务端替他生成一个，等于让每一次重试都签一份新的。
		return nil, ErrSubscriptionRequestIDRequired
	}
	if s.agreements == nil || s.wallets == nil {
		// 装配漏了（见 Options 上那两条）。它**不是**「降级成不签约」——没接渠道就是签不成。
		return nil, ErrChannelUnavailable
	}

	previous, err := s.repository.FindSubscriptionByChangeRequest(ctx, uid, requestID)
	switch {
	case err == nil:
		// 同一次点击的第二次。什么都不写，只重新领一份跳转参数（渠道按幂等号回放同一份协议）。
		return s.issueSigningParams(ctx, uid, previous, requestID, traceID)
	case !errors.Is(err, ErrSubscriptionNotFound):
		return nil, err
	}

	plan, err := s.repository.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if !plan.IsActive() {
		// 下架的套餐不能再签：签约是**长期**关系，让一个已经不再卖的套餐续上，等于给一个我们
		// 已经决定不卖的东西开了一条自动扣款。
		return nil, ErrPlanNotSellable
	}
	if !plan.AutoRenew || strings.TrimSpace(plan.WechatPlanID) == "" {
		return nil, ErrSubscriptionPlanNotSubscription
	}

	// 会员行必须先存在：membership_subscriptions.membership_id 是 NOT NULL 带外键，而「先有
	// 会员、才谈得上续费」也是本域的规矩（他得先买过一次，才谈得上自动续）。没有会员时这里是
	// 404，前端据此说的是「你还不是会员」。
	membership, err := s.repository.GetMembership(ctx, uid)
	if err != nil {
		return nil, err
	}
	if _, err := s.repository.GetLiveSubscription(ctx, membership.ID); err == nil {
		// 见上面第 2 条：这一条必须在建协议之前。
		return nil, ErrLiveSubscriptionExists
	} else if !errors.Is(err, ErrSubscriptionNotFound) {
		return nil, err
	}

	openID, found, err := s.wallets.MiniappOpenID(ctx, uid)
	if err != nil {
		return nil, ErrChannelUnavailable
	}
	if !found {
		// 没绑定小程序身份不是故障：他可能只在 H5 登录过。收场是让他先去小程序里登录一次，
		// 而这句话说得清楚比「请稍后再试」有用得多。
		return nil, ErrWalletIdentityRequired
	}

	signed, err := s.agreements.Create(ctx, dto.CreateAgreementParams{
		UserID:   uid,
		PlanCode: plan.Code,
		// 签约模板按套餐配，所以由本服务给（套餐住在会员库）。
		ProviderPlanID: plan.WechatPlanID,
		WalletOpenID:   openID,
		// Subject 是渠道账单上的那一句话，给人看的。
		Subject: plan.Name,
		// 单次扣款上限随签约一起冻在渠道与协议上：**只能等于套餐价**。给大了它就失去意义，
		// 给小了以后每一期都扣不动。
		MaxChargeAmount: plan.PriceCents,
		RequestID:       requestID,
	})
	if err != nil {
		return nil, err
	}

	outcome, err := s.repository.CreateSubscription(ctx, repository.CreateSubscriptionParams{
		UserID:      uid,
		PlanID:      plan.ID,
		PlanName:    plan.Name,
		AgreementID: signed.AgreementID,
		// 商户协议号就是交给渠道的 contract_code，签约这一刻写进这一行：它是事后回渠道查这份
		// 协议的唯一钥匙，而此刻它已经存在了（协议在支付那边建好了）。
		AgreementNo:  signed.AgreementNo,
		PriceCents:   plan.PriceCents,
		WechatPlanID: plan.WechatPlanID,
		Period:       plan.Period,
		PeriodCount:  plan.PeriodCount,

		OrderID:         signOrderID(req),
		CoffeeOrderID:   strings.TrimSpace(req.CoffeeOrderID),
		CampaignClaimID: strings.TrimSpace(req.CampaignClaimID),
		RequestID:       requestID,
		OccurredAt:      s.Now(),
	})
	if err != nil {
		return nil, err
	}
	// Replayed 为真说明「事务外那道反查」与「锁会员行的反查」之间被插了一行（同一个 requestId
	// 的两个并发请求）。结论一样：回的就是那一条，只是它比我们先到，跳转参数照发。
	return signingResponse(outcome.Subscription, signed.Action, signed.PayParams), nil
}

// issueSigningParams 是重放那一条路：订阅已经落过了，**一个字段都不写库**，只回上一次那份
// 协议的跳转参数。
//
// 它必须重新调一次支付服务而不是把参数存起来复用：跳转参数里含签名与时间戳，是一次性凭据；
// 而支付服务那边按 requestId 幂等——同一个号回来的还是**同一份协议**（同样的 contract_code、
// 同样的 request_serial），客户端不会在微信里看到第二份待签协议。
//
// 传下去的签约要素全部取自**那一行已冻住的快照**（模板 id、金额），不是当前套餐的现值：
// 支付服务把「同一个幂等号换了请求体」判成冲突（createAgreementRequestHash），拿今天的价格去
// 重发一次，用户看到的会是一句「正在处理中」而永远拿不到参数。
func (s *MembershipService) issueSigningParams(ctx context.Context, userID string, previous *repository.SubscriptionRow, requestID, traceID string) (*dto.SubscriptionSigningResponse, error) {
	plan, err := s.repository.GetPlan(ctx, previous.PlanID)
	if err != nil {
		return nil, err
	}
	openID, found, err := s.wallets.MiniappOpenID(ctx, userID)
	if err != nil {
		return nil, ErrChannelUnavailable
	}
	if !found {
		return nil, ErrWalletIdentityRequired
	}
	signed, err := s.agreements.Create(ctx, dto.CreateAgreementParams{
		UserID:         userID,
		PlanCode:       plan.Code,
		ProviderPlanID: previous.WechatPlanID,
		WalletOpenID:   openID,
		// Subject 不参与支付侧的请求哈希（它是展示文案），取当下的套餐名即可。
		Subject:         plan.Name,
		MaxChargeAmount: previous.PriceCents,
		RequestID:       requestID,
	})
	if err != nil {
		return nil, err
	}
	return signingResponse(previous, signed.Action, signed.PayParams), nil
}

// signOrderID 读「首月那一笔」的订单号，只对会员中心那一档保留。
//
// 另外两条路上首月那笔钱在咖啡订单或活动单里，不在订阅上——硬摆到「首月支付」那一列会让人
// 以为那是订阅的首期扣款（那是两笔不同的钱）。判据与 firstPaymentFor 是同一条（场景优先级），
// 所以这里按同一套规则丢掉它。
func signOrderID(req dto.CreateSubscriptionRequest) string {
	if strings.TrimSpace(req.CoffeeOrderID) != "" || strings.TrimSpace(req.CampaignClaimID) != "" {
		return ""
	}
	return strings.TrimSpace(req.OrderID)
}

// signingResponse 拼签约应答。三个字段一个都不少：Action 决定客户端跳哪儿，PayParams 是跳过去
// 要带的东西（含签名，一次性），Subscription 是新落的那一条（客户端拿它的 id 来确认）。
func signingResponse(row *repository.SubscriptionRow, action string, payParams map[string]string) *dto.SubscriptionSigningResponse {
	if payParams == nil {
		// JSON 里的 null 与 `{}` 对客户端是两种东西（前者要判空，后者直接读）。渠道没给参数时
		// 也回一个能读的对象。
		payParams = map[string]string{}
	}
	return &dto.SubscriptionSigningResponse{
		Subscription: subscriptionResponse(row),
		Action:       action,
		PayParams:    payParams,
	}
}

// ============================================================
// 确认签约
// ============================================================

// ConfirmSubscription 是用户从微信回来之后的「签成了没有」。
//
// # 它问的是渠道，不是「用户说签完了」
//
// 客户端回来说「我点过同意了」什么都证明不了——那一步的结果只在渠道那边。所以这里回渠道核
// 一次（payment 的 QueryAgreement **是写不是读**：渠道说这份协议没了而支付库还记着生效中时，
// 纠正就发生在那一次调用里），拿回来的才是事实。
//
// # 按 user_id 认领，别人的订阅回 404
//
// 与这一棵树上的另外几个接口同一条规矩。订阅 id 泄露出去不该等于「别人能确认/查看我的签约」，
// 所以归属不符时回的是 ErrSubscriptionNotFound（**不是 403**）：403 会说「这条存在，只是不是
// 你的」，那本身就是一条不该泄露的信息。
func (s *MembershipService) ConfirmSubscription(ctx context.Context, rawID, userID, requestID, traceID string) (*dto.SubscriptionResponse, error) {
	uid, err := requiredID(userID, ErrUserIDRequired, ErrUserIDInvalid)
	if err != nil {
		return nil, err
	}
	id, err := subscriptionID(rawID)
	if err != nil {
		return nil, err
	}
	if s.agreements == nil {
		return nil, ErrChannelUnavailable
	}
	current, err := s.repository.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.UserID != uid {
		return nil, ErrSubscriptionNotFound
	}
	if current.Status != model.SubscriptionStatusPendingSign {
		// 已经是终态或已生效：**回它现在的样子**，不再问渠道。用户连点两次「我签好了」不该
		// 让第二次变成一次错误——第一次的结果就是答案。
		return subscriptionResponse(current), nil
	}
	if strings.TrimSpace(current.ContractCode) == "" {
		// 我们自己写坏的行（签约那一刻就把协议号写进去了，空的只可能是本服务的 bug）。原样往上
		// 抛成 500：编一句话把它说成用户的错更难查。
		return nil, errors.New("membership: subscription " + current.ID + " has no contract code")
	}

	state, err := s.agreements.Query(ctx, current.ContractCode, requestID)
	if err != nil {
		return nil, err
	}
	target, ok := settleTargetFor(state.Status)
	if !ok {
		// 渠道说还等着（pending）：**不动本地状态**，把当前这一行回给客户端，让他过一会儿再来。
		// 这一步不能猜——猜成「没签成」会把一份用户其实已经签好的协议在本地标掉。
		return subscriptionResponse(current), nil
	}
	row, _, err := s.repository.SettleSubscription(ctx, repository.SettleParams{
		SubscriptionID: current.ID,
		Target:         target,
		AgreementNo:    state.AgreementNo,
		ProviderState:  state.ProviderState,
		OccurredAt:     s.Now(),
		TraceID:        traceID,
	})
	if err != nil {
		return nil, err
	}
	return subscriptionResponse(row), nil
}

// settleTargetFor 把渠道/支付那边给的状态翻成本地该落的目标状态。
//
// **pending 不在映射里**（ok=false）：那是「还没结果」，不是一个目标状态。让它有一席之地的
// 话，调用方迟早会拿它去改库——而「把一条订阅改成 pending」在本地没有任何意义。
//
// terminated 与 active 的对应关系见 dto.AgreementState.Status 上的说明：**terminated 同时包括
// 「查无此约」**，对订阅来说这两件事的结论一样——这份授权今天不能用了。
func settleTargetFor(status string) (string, bool) {
	switch status {
	case dto.AgreementStatusActive:
		return model.SubscriptionStatusActive, true
	case dto.AgreementStatusTerminated:
		return model.SubscriptionStatusCancelled, true
	default:
		return "", false
	}
}
