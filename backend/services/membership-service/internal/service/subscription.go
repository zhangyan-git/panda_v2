package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 连续包月的订阅管理（后台一侧）：列表、两张统计卡、详情、同步、取消。
//
// 这一段是从老后台照抄过来的形状，砍掉了一处：**没有创建**。老后台也建不了，订阅只能由小程序
// 端签约产生——发起签约与确认都在 /v1/miniapp/membership/subscriptions（见 signing.go），
// 这里是事后管理。所以这些页面在没有小程序接进来之前必然是空的：**列表是空的，不代表链路空着**。
//
// # 「取消」与「同步」的区别就是「谁主动」
//
// 两个都从渠道那边收场，区别在谁先动：
//
//   - **取消**是我们这边先说「不续了」——用户在小程序点「关闭自动续费」，或运营点后台的
//     「取消订阅」。两者都要**先**让渠道把那份协议解掉，成功了才改本地（见 CancelSubscription
//     与 terminateForSubscription 上那段顺序说明）。老系统反着来、而且把微信的失败吞掉，
//     留下的正是「用户以为关了、下个月照扣」。
//   - **同步**是我们去问渠道「现在是什么状态」，按它的原话纠正本地（见 SyncSubscription）。
//     它不改用户的意愿，只把本地追平到事实。
//
// 后台取消一条订阅**不退款、也不改会员有效期**：这个人这一期已经买了，钱与权益都还在他手上，
// 我们只是不让他下个月被扣。

// SubscriptionResponse 是这一层对外的形状。仓储回的 SubscriptionRow 多带一个 join 出来的
// 套餐名，这里把它与两个派生字段一起摊平。
func subscriptionResponse(row *repository.SubscriptionRow) *dto.SubscriptionResponse {
	s := row.Subscription
	return &dto.SubscriptionResponse{
		ID:                     s.ID,
		MembershipID:           s.MembershipID,
		UserID:                 s.UserID,
		PlanID:                 s.PlanID,
		PlanName:               row.PlanName,
		Status:                 s.Status,
		PriceCents:             s.PriceCents,
		Period:                 s.Period,
		PeriodCount:            s.PeriodCount,
		SignScene:              signSceneFor(s),
		FirstPaymentOrderID:    firstPaymentFor(s),
		ChargeCount:            s.ChargeCount,
		FailedCount:            s.FailedCount,
		ConsecutiveFailedCount: s.ConsecutiveFailedCount,
		NextChargeAt:           s.NextChargeAt,
		LastChargeAt:           s.LastChargeAt,
		SuspendedAt:            s.SuspendedAt,
		CancelAt:               s.CancelAt,
		CancelReason:           s.CancelReason,
		CancelledBy:            textOrEmpty(s.CancelledBy),
		CreatedAt:              s.CreatedAt,
		UpdatedAt:              s.UpdatedAt,
	}
}

// signSceneFor 算「签约场景」。库里没有这一列（见 migrations/membership/005），三个来源 id
// 的**优先级**就是判据：一次签约可能既有咖啡订单又有活动单（从活动页扫了码再点单），
// 老系统先看咖啡订单——那是用户当下真正在做的事。
func signSceneFor(s *model.Subscription) string {
	switch {
	case s.CoffeeOrderID != "":
		return dto.SubscriptionSceneCoffeeOrder
	case s.CampaignClaimID != "":
		return dto.SubscriptionSceneStoreCampaign
	default:
		return dto.SubscriptionSceneMemberCenter
	}
}

// firstPaymentFor 算「首月支付」那一列。
//
// **只对会员中心那一档填**：另外两条路上首月那笔钱在咖啡订单或活动单里，不在订阅上，硬把
// 咖啡订单号摆到这一列会让人以为那是订阅的首期扣款（那是两笔不同的钱）。老系统同一条口径。
func firstPaymentFor(s *model.Subscription) string {
	if signSceneFor(s) != dto.SubscriptionSceneMemberCenter {
		return ""
	}
	return s.OrderID
}

// ListSubscriptions 是后台订阅列表。
func (s *MembershipService) ListSubscriptions(ctx context.Context, q dto.SubscriptionQuery) ([]*dto.SubscriptionResponse, int, error) {
	if err := normalizeSubscriptionQuery(&q); err != nil {
		return nil, 0, err
	}
	rows, total, err := s.repository.ListSubscriptions(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]*dto.SubscriptionResponse, 0, len(rows))
	for _, row := range rows {
		responses = append(responses, subscriptionResponse(row))
	}
	return responses, total, nil
}

// normalizeSubscriptionQuery 校一遍筛选条件并去掉空白。
//
// 状态**要校**（与会员列表不同，那边是「传错的码返回空列表」）：这两张页面的状态是下拉选的，
// 一个不在五个码里的值只可能来自手改 URL。让它返回空列表看起来像「没有这类订阅」，而真相是
// 「这个状态根本不存在」——空列表在这里太像一句真话了。
func normalizeSubscriptionQuery(q *dto.SubscriptionQuery) error {
	q.UserID = strings.TrimSpace(q.UserID)
	// 用户 ID 是 UUID 列上的等值匹配：不校的话一个随手敲的串会以 22P02（invalid input syntax
	// for type uuid）落到 PostgreSQL，变成一条 500——而它其实只是「查错了个人」。
	if _, err := optionalUUID(q.UserID, ErrUserIDInvalid); err != nil {
		return err
	}
	q.Status = strings.TrimSpace(q.Status)
	if q.Status != "" && !isSubscriptionStatus(q.Status) {
		return ErrSubscriptionStatusInvalid
	}
	// 分页兜底与会员列表逐字一致（见 normalizeMembershipQuery）：Page 是**从 1 起算**的，
	// 0 会在 SQL 里算出一个负的 OFFSET，而 PostgreSQL 报的是「OFFSET must not be negative」
	// ——一句与「页码」毫无关系的话。
	if q.Page < 1 {
		q.Page = 1
	}
	switch {
	case q.PageSize <= 0:
		q.PageSize = dto.DefaultPageSize
	case q.PageSize > MaxPageSize:
		q.PageSize = MaxPageSize
	}
	return nil
}

func isSubscriptionStatus(status string) bool {
	switch status {
	case model.SubscriptionStatusPendingSign, model.SubscriptionStatusActive,
		model.SubscriptionStatusSuspended, model.SubscriptionStatusCancelled,
		model.SubscriptionStatusExpired:
		return true
	default:
		return false
	}
}

// SubscriptionStats 是列表页头上那两张卡。时刻取自服务时钟（与到期扫描同一个），
// 这样测试能把「该扣了」造到过去而不必等。
func (s *MembershipService) SubscriptionStats(ctx context.Context) (dto.SubscriptionStats, error) {
	return s.repository.SubscriptionStats(ctx, s.Now())
}

// CancelSubscription 是后台的「取消订阅」：**先解约、成功后才改本地**。
//
// # 顺序不能反
//
// 反过来的写法（先翻本地、再去解约）在一次网络抖动里留下的是「后台显示已取消、而微信那边协议
// 还活着」——协议活着就意味着下个月照样会扣款。先解约则失败时什么都不变：运营看到一句错误，
// 再点一次即可。两条路都要在一次抖动里选一条，选「什么都没发生」那条（详见
// terminateForSubscription）。
//
// # 前置状态交给仓储判
//
// 只有 active / suspended 能取消，而那一条必须在**锁着行之后**判（见 repository），所以这里
// 先读一次只为拿到协议号，真正的守卫在仓储里。两次读之间订阅可能变（比如刚被扣款推到
// suspended），但那不改这一行的协议号，也不影响解约——解约解的是渠道那份协议。
func (s *MembershipService) CancelSubscription(ctx context.Context, id string, req dto.CancelSubscriptionRequest, actor Actor) (*dto.SubscriptionResponse, error) {
	parsed, err := subscriptionID(id)
	if err != nil {
		return nil, err
	}
	reason, err := checkReason(req.Reason, ErrCancelReasonRequired)
	if err != nil {
		return nil, err
	}
	current, err := s.repository.GetSubscription(ctx, parsed)
	if err != nil {
		return nil, err
	}
	if _, err := s.terminateForSubscription(ctx, current.Subscription, reason, actor.RequestID); err != nil {
		return nil, err
	}
	row, err := s.repository.CancelSubscription(ctx, repository.CancelSubscriptionParams{
		SubscriptionID:  parsed,
		CancelledBy:     actor.AdminID,
		CancelledByType: model.OperatorAdmin,
		Reason:          reason,
		OccurredAt:      s.Now(),
	})
	if err != nil {
		return nil, err
	}
	return subscriptionResponse(row), nil
}

// terminateForSubscription 在我们这边宣告「不续了」之前，先把渠道那份协议解掉。
//
// # 为什么顺序不能反（这是这一整块唯一重要的一件事）
//
// 先翻本地、再去解约，中间的失败（网络、渠道超时）留下的是**用户以为关了、而协议还活着**——
// 而协议活着就意味着下一轮的到期扫描照样会为这一行发起扣款。反过来，解不成就什么都不改：
// 用户看到的是一句错误、名下状态一个字节都没动，再点一次即可。两条路都要在一次网络抖动里选
// 一条，就选「什么都没发生」那条。
//
// # 什么时候根本不调渠道
//
// 只有 active / suspended 才真的在渠道那边有一份**用户点过头的**协议：
//
//   - pending_sign 是签了一半的（用户去了微信、没点同意，或者点完还没回来确认）。V2 的签约
//     是客户端拿着本地生成的跳转参数去微信的（见 signing.go 的 Create），**那一刻渠道侧什么
//     都还没建**——所以这里没有约可解。对它调 deletecontract 只会换来一句「这份约不存在」，
//     把用户挡在门外，而它本身是安全的：我们不会去扣一笔没有协议的款。
//   - 没有协议号的行（店铺码活动发放、后台开通那两条路）压根没有渠道侧。
//
// # pending_sign 那一格的代价，说清楚
//
// 如果用户其实已经在微信点了同意、而通知还在路上，我们这次本地取消会让随后到达的
// payment.agreement.signed 撞上「已结束的订阅不许改回生效中」（见 guardSettleToActive），
// 那条事件进死信、有人会看到。这是**有意选的一边**：宁可让人来看一眼，也不能把一个用户刚
// 关掉的签约重新接上扣款。
//
// # 渠道明确拒绝是结论，不是故障
//
// 拿到 FailureCode 时什么都不改、回一条要人来看的错（见 ErrAgreementTerminateRefused）。不能
// 吞掉它继续改本地：那正是「用户以为关了、下个月照扣」那一格的正面。客服手上的出口是后台的
// 「同步」——它拿渠道的原话纠正本地。
//
// 返回渠道给的协议状态（进审计的 after 快照），没调渠道时回空串。
func (s *MembershipService) terminateForSubscription(ctx context.Context, sub *model.Subscription, reason, requestID string) (string, error) {
	if sub.Status != model.SubscriptionStatusActive && sub.Status != model.SubscriptionStatusSuspended {
		return "", nil
	}
	if sub.AgreementID == nil || strings.TrimSpace(*sub.AgreementID) == "" {
		// 没有协议 id 的行压根没有渠道侧那一份（见上面那段「什么时候根本不调渠道」）：店铺码活动
		// 发放与后台开通那两条路给的是一段会员，不是一份代扣授权。对它调 deletecontract 只会换来
		// 一句「这份约不存在」，把这一行**永远挡在取消之外**——而它本来就不可能被扣款。
		return "", nil
	}
	contractCode := strings.TrimSpace(sub.ContractCode)
	if contractCode == "" {
		// 有协议 id 却没有协议号：两个值都在签约那一刻写进去（见 repository.CreateSubscription），
		// 只剩一个只可能是本服务的 bug。与 SyncSubscription 同一处置——原样抛成 500，不编一句话
		// 糊过去：这里没有可查的替代键。
		return "", errors.New("membership: subscription " + sub.ID + " has no contract code")
	}
	if s.agreements == nil {
		// 没有支付域这条连接就解不了约。回 503（不是 500）：这是依赖缺席，不是我们写错了。
		// **不能退化成「只改本地」**——那正是这个函数要防的那一格。
		return "", ErrChannelUnavailable
	}

	result, err := s.agreements.Terminate(ctx, dto.TerminateAgreementParams{
		AgreementNo: contractCode,
		Reason:      reason,
		RequestID:   requestID,
	})
	if err != nil {
		// 渠道那边没答上来（超时、读不懂报文）。支付侧一个字段都没改，重发是安全的——什么都
		// 不改地回错给用户。
		return "", err
	}
	if result.FailureCode != "" {
		return "", fmt.Errorf("%w: %s %s", ErrAgreementTerminateRefused,
			result.FailureCode, result.FailureMessage)
	}
	return result.Status, nil
}

// SyncSubscription 是后台的「同步」：拿这条订阅的协议号回渠道核一次，按渠道的结论纠正本地。
//
// # 它与小程序那条「确认」是同一条判据，不是两套
//
// 落库走的是同一个 repository.SettleSubscription，分歧只有一处：这里**没有 user_id 这一关**
// ——运营点的是后台的「同步」，他看的就是别人（客服接到电话、或事后核对）的订阅，所以归属检查
// 在这里不成立。取而代之的是这一枚权限码（membership:manage），以及每一次都写进审计的那个
// AdminID（见 SettleParams）。
//
// # 渠道说「还等着」时什么都不动
//
// pending 不进 settleTargetFor（见那段说明），所以渠道还在等用户点同意时这里原样返回——**不猜
// 「没签成」**：把一份用户其实已经签好的协议在本地标掉，比晚一天同步糟得多。老系统对
// contract_state=9（未签约）也是这个处理。此时 Changed=false，后台提示「无需更正」。
//
// # 一条订阅只能纠正成它该在的状态
//
// 已经 active 的再同步一次不会续期（仓储在状态相同时短路）；已经结束的（cancelled / expired）
// 不会因为渠道此刻说 active 就复活（guardSettleToActive 只放行 pending_sign，见仓储）。所以
// 这个按钮点几次都安全——它**不产生任何财产后果**，那是代扣那一刀的事。
func (s *MembershipService) SyncSubscription(ctx context.Context, rawID string, actor Actor, requestID, traceID string) (*dto.SubscriptionSyncResponse, error) {
	id, err := subscriptionID(rawID)
	if err != nil {
		return nil, err
	}
	if s.agreements == nil {
		// 没有支付域这条连接就核不了协议。回 503（不是 500）：这是依赖缺席，不是我们写错了。
		return nil, ErrChannelUnavailable
	}
	current, err := s.repository.GetSubscription(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(current.ContractCode) == "" {
		// 签约那一刻就把协议号写进去了（见 repository.CreateSubscription），空值只可能是本服务
		// 的 bug——而它没有可查的替代键。原样抛成 500，不编一句「这条订阅没签过」。
		return nil, errors.New("membership: subscription " + current.ID + " has no contract code")
	}

	state, err := s.agreements.Query(ctx, current.ContractCode, requestID)
	if err != nil {
		return nil, err
	}
	target, ok := settleTargetFor(state.Status)
	if !ok {
		return &dto.SubscriptionSyncResponse{
			Subscription:  subscriptionResponse(current),
			ProviderState: state.ProviderState,
		}, nil
	}

	row, changed, err := s.repository.SettleSubscription(ctx, repository.SettleParams{
		SubscriptionID: current.ID,
		Target:         target,
		AgreementNo:    state.AgreementNo,
		// 是运营点的那一下，所以写原因与操作人：老系统那句话照抄（「管理员同步：微信侧已解约」），
		// 后台订阅详情里直接显示这一列。
		CancelReason:  "管理员同步：微信侧已解约",
		ProviderState: state.ProviderState,
		OccurredAt:    s.Now(),
		AdminID:       actor.AdminID,
		TraceID:       traceID,
	})
	if err != nil {
		return nil, err
	}
	return &dto.SubscriptionSyncResponse{
		Subscription:  subscriptionResponse(row),
		Changed:       changed,
		ProviderState: state.ProviderState,
	}, nil
}

// textOrEmpty 把可空列摊成空串。nil 与空串在这一列上是同一件事——「没人取消过」，而
// 解约发起人里不可能有一个叫空字符串的人。
func textOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// subscriptionID 校路径上那个订阅 ID。
//
// 非 uuid **回「订阅不存在」而不是「ID 不合法」**：这一个 id 只可能来自后台列表里的链接，
// 手敲一个乱串进来的结果是「查不到这条订阅」——那也正是事实。为它单开一条校验错误，只会让
// controller 的人话表里多一句在这一页上没有意义的提示。
func subscriptionID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if _, err := uuid.Parse(trimmed); err != nil {
		return "", ErrSubscriptionNotFound
	}
	return trimmed, nil
}
