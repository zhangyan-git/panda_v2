package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// ListMemberships 是后台会员列表。
func (s *MembershipService) ListMemberships(ctx context.Context, query dto.MembershipQuery) ([]*model.Membership, int, error) {
	if err := normalizeMembershipQuery(&query); err != nil {
		return nil, 0, err
	}
	return s.repository.ListMemberships(ctx, query)
}

// normalizeMembershipQuery 校验筛选条件并收好分页参数。
//
// 与 normalizePlanQuery 不同，这里**会报错**：会员列表比套餐列表多两个用户手输的筛选
// （到期时间区间、用户 ID）。区间反了不报的话会回一个空列表，而运营看到的是「这段时间没有
// 会员到期」——一句他没法反驳、也不该相信的话。用户 ID 填错同理。
func normalizeMembershipQuery(query *dto.MembershipQuery) error {
	if _, err := optionalUUID(query.UserID, ErrUserIDInvalid); err != nil {
		return err
	}
	if query.Status != "" {
		if _, err := parseEnum(query.Status, membershipStatuses, ""); err != nil {
			return ErrMembershipStatusInvalid
		}
	}
	// 闭开区间 [ExpireFrom, ExpireTo)：结束端**必须**晚于开始端。相等虽然不会回空列表
	// （那是一个零宽区间，本来就该是空的），但它同样是「填错了」，而报出来比让运营自己去
	// 琢磨为什么是空的好。
	if query.ExpireFrom != nil && query.ExpireTo != nil && !query.ExpireTo.After(*query.ExpireFrom) {
		return ErrOccurredRangeInvalid
	}
	if query.Page < 1 {
		query.Page = 1
	}
	switch {
	case query.PageSize <= 0:
		query.PageSize = dto.DefaultPageSize
	case query.PageSize > MaxPageSize:
		query.PageSize = MaxPageSize
	}
	return nil
}

// membershipStatuses 是会员的四个状态，与库上的 CHECK 逐字一致。
var membershipStatuses = []string{
	model.MembershipStatusActive, model.MembershipStatusFrozen,
	model.MembershipStatusExpired, model.MembershipStatusRevoked,
}

// GetMembershipByUser 按用户 ID 取会员。
//
// 这是**会员价判定**与小程序会员中心共用的读路径。查不到时回 ErrMembershipNotFound，而不是
// 一条空记录：调用方（下单、会员中心）要据此显示「还不是会员」，而一条零值的会员记录带着
// 零值的时间戳，会让「他的会员是不是过期了」这个问题有一个荒唐的答案。
func (s *MembershipService) GetMembershipByUser(ctx context.Context, userID string) (*model.Membership, error) {
	id, err := requiredID(userID, ErrUserIDRequired, ErrUserIDInvalid)
	if err != nil {
		return nil, err
	}
	return s.repository.GetMembership(ctx, id)
}

// GetMembershipByID 按会员 ID 取会员。后台列表点进详情走这一条。
//
// 与 GetMembershipByUser 是两个方法而不是一个带 flag 的：**它们回答的是两个问题**——「这个人
// 是不是会员」与「这条会员记录是什么」。后者只有后台问得出来（它手里有列表给的会员 ID），
// C 端手里根本没有这个 ID（见 SetAutoRenewByUser 的说明）。
func (s *MembershipService) GetMembershipByID(ctx context.Context, membershipID string) (*model.Membership, error) {
	id, err := requiredID(membershipID, ErrMembershipIDRequired, ErrMembershipIDInvalid)
	if err != nil {
		return nil, err
	}
	return s.repository.GetMembershipByID(ctx, id)
}

// ListChanges 取一条会员的时间线。后台详情页用。
func (s *MembershipService) ListChanges(ctx context.Context, membershipID string) ([]*model.Change, error) {
	id, err := requiredID(membershipID, ErrMembershipIDRequired, ErrMembershipIDInvalid)
	if err != nil {
		return nil, err
	}
	return s.repository.ListChanges(ctx, id)
}

// ============================================================
// 后台人工干预
// ============================================================

// FreezeMembership 暂停一个会员的权益。
//
// 冻结**不动 expire_at**：它表达的是「这个人的权益现在不算数」，不是「他的会员提前结束了」。
// 解冻之后剩下的天数原样还在——把到期时间一起往后推会让一次误冻变成一次免费延期。
func (s *MembershipService) FreezeMembership(ctx context.Context, id, reason string, actor Actor) (*model.Membership, error) {
	membershipID, trimmedReason, err := s.mutationInput(id, reason, ErrFreezeReasonRequired)
	if err != nil {
		return nil, err
	}
	return s.repository.FreezeMembership(ctx, membershipID, actor.AdminID, trimmedReason, actor.TraceID, actor.RequestID)
}

// UnfreezeMembership 恢复被暂停的权益。
func (s *MembershipService) UnfreezeMembership(ctx context.Context, id, reason string, actor Actor) (*model.Membership, error) {
	// 解冻也要求理由：它是一次**恢复**，「为什么恢复」与「为什么冻结」一样，事后只有这句话
	// 答得上来。与冻结共用 ErrFreezeReasonRequired 会让提示说成「请填写冻结原因」，所以这里
	// 用一条自己的。
	membershipID, trimmedReason, err := s.mutationInput(id, reason, ErrUnfreezeReasonRequired)
	if err != nil {
		return nil, err
	}
	return s.repository.UnfreezeMembership(ctx, membershipID, actor.AdminID, trimmedReason, actor.TraceID, actor.RequestID)
}

// RevokeMembership 撤销会员。不可逆。
func (s *MembershipService) RevokeMembership(ctx context.Context, id, reason string, actor Actor) (*model.Membership, error) {
	membershipID, trimmedReason, err := s.mutationInput(id, reason, ErrRevokeReasonRequired)
	if err != nil {
		return nil, err
	}
	return s.repository.RevokeMembership(ctx, membershipID, actor.AdminID, trimmedReason, actor.TraceID, actor.RequestID)
}

// AdjustExpireAt 后台直接把有效期改到某个时刻。
//
// 这是本域破坏力最大的一个接口：它**没有任何订单、支付或流水跟着发生**，只是把一个人的
// expire_at 挪一下。所以理由必填、备注可选，且每一次调用都写平台审计（方案 §11.6
// 「影响用户资产归属的人工操作」）——那一层在仓储里，与改行在同一个事务中。
//
// **它不做「延长 N 天」**：后台处理的是「这个人应该在 2027-01-01 到期」这类具体结论（对账、
// 客服工单里给的就是一个日期），而「延长 N 天」要求操作的人自己先算一遍，算错的那次没有任何
// 东西能发现。
func (s *MembershipService) AdjustExpireAt(ctx context.Context, id string, req dto.AdjustRequest, actor Actor) (*model.Membership, error) {
	membershipID, err := requiredID(id, ErrMembershipIDRequired, ErrMembershipIDInvalid)
	if err != nil {
		return nil, err
	}
	reason, err := checkReason(req.Reason, ErrAdjustReasonRequired)
	if err != nil {
		return nil, err
	}
	remark := strings.TrimSpace(req.Remark)
	if len([]rune(remark)) > MaxRemarkLength {
		return nil, ErrAdjustRemarkTooLong
	}
	if req.ExpireAt.IsZero() {
		return nil, ErrAdjustExpireRequired
	}
	return s.repository.AdjustExpireAt(ctx, membershipID, actor.AdminID,
		req.ExpireAt.UTC(), reason, remark, actor.TraceID, actor.RequestID)
}

// ============================================================
// 后台直接开通
// ============================================================

// GrantMembership 由后台直接给一个用户开会员，不经过任何订单或支付。
//
// # 它补的是哪个口子
//
// 本域在此之前**只有**状态迁移（冻结/解冻/撤销/改有效期）——全是「已经有一条会员」之后的事。
// 客服补偿、线下活动、渠道争议里那句「你们店长答应送我一年会员」因此在系统里做不出来：
// 没有会员就无从调整。这个接口是那个口子的出口。
//
// # 「快照说了算」在这里的唯一例外
//
// 订单那条路带的是**下单那一刻**的套餐快照；后台开通没有订单，只能当场取套餐。取完立刻落进
// memberships 的快照列，之后套餐再改价、改名、下架都不追溯——最终形状与成交那条路一致，
// 只是取值时点从「下单」变成了「开通」。
//
// # 已有会员一律拒（见 dto.GrantRequest 与仓储的说明）
//
// 不叠加、不改归属门店。要改有效期走 AdjustExpireAt，那条路本来就是干这个的。
func (s *MembershipService) GrantMembership(ctx context.Context, req dto.GrantRequest, actor Actor) (*model.Membership, error) {
	userID, err := requiredID(req.UserID, ErrUserIDRequired, ErrUserIDInvalid)
	if err != nil {
		return nil, err
	}
	planID, err := requiredID(req.PlanID, ErrPlanIDRequired, ErrPlanIDInvalid)
	if err != nil {
		return nil, err
	}
	reason, err := checkReason(req.Reason, ErrGrantReasonRequired)
	if err != nil {
		return nil, err
	}
	remark := strings.TrimSpace(req.Remark)
	if len([]rune(remark)) > MaxRemarkLength {
		return nil, ErrAdjustRemarkTooLong
	}
	// 幂等键必填：见 ErrGrantRequestIDRequired。它在仓储层同时是流水上的留痕串。
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		return nil, ErrGrantRequestIDRequired
	}
	storeID, err := optionalUUID(req.StoreID, ErrStoreIDInvalid)
	if err != nil {
		return nil, err
	}
	// 门店存在性只在**选了门店**时问。问不到（err）让整次开通失败——放一条查无此店的归属
	// 进去，之后没有任何东西能发现它，而归属门店的全部意义就是事后能对得上账。
	if storeID != "" {
		if err := s.checkStore(ctx, storeID); err != nil {
			return nil, err
		}
	}

	plan, err := s.repository.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	// 只允许开通在售的套餐：草稿是还没配完，下架是不该再卖。这里复用下单那条路上的同一句话
	// （ErrPlanNotSellable 已经是 409），而不是新造一条意思一样的错误。
	if !plan.IsActive() {
		return nil, ErrPlanNotSellable
	}

	startAt := s.Now()
	expireAt := req.ExpireAt
	if expireAt.IsZero() {
		// 没给到期日就按套餐自带时长算。给了就用给的——补偿场景常是「送到年底」这种具体结论，
		// 而那不是一个时长能表达的。
		expireAt = model.AddPeriod(startAt, plan.Period, plan.PeriodCount)
	} else {
		expireAt = expireAt.UTC()
		if !expireAt.After(startAt) {
			return nil, ErrGrantExpireTooEarly
		}
	}

	// 快照逐字取自**当前**的套餐：这就是上面那段说的那个例外。券那两个字段在 auto 模式下是
	// NULL，快照里跟着留空——写库时由 MembershipSnapshot.couponColumns 按模式决定写不写。
	snapshot := repository.MembershipSnapshot{
		PlanID:          plan.ID,
		PlanCode:        plan.Code,
		PlanName:        plan.Name,
		MemberPriceMode: plan.MemberPriceMode,
		Period:          plan.Period,
		PeriodCount:     plan.PeriodCount,
	}
	if plan.MemberPriceCouponTemplateID != nil {
		snapshot.MemberPriceCouponTemplateID = *plan.MemberPriceCouponTemplateID
	}
	if plan.MemberPriceCouponsPerPeriod != nil {
		snapshot.MemberPriceCouponsPerPeriod = *plan.MemberPriceCouponsPerPeriod
	}

	return s.repository.GrantMembership(ctx, repository.GrantParams{
		UserID:   userID,
		Snapshot: snapshot,
		StartAt:  startAt,
		ExpireAt: expireAt,
		StoreID:  storeID,
		AdminID:  actor.AdminID,
		Reason:   reason,
		Remark:   remark,
		TraceID:  actor.TraceID,
		// 这次点击的幂等键。**不是** actor.RequestID（那个读的是追踪头 X-Request-Id，
		// 每个请求都不同，当幂等键等于没有）。
		RequestID: requestID,
	})
}

// checkStore 问一次商户域这家门店在不在。
//
// 「不存在」与「问不到」分开处理（见 repository 与 client 上那两段说明）：不存在是操作员选错
// 了，回一句能显示的话；问不到是商户服务的问题，原样往上报成 500——**不能**把后者读成
// 「存在」，那正好是这条校验唯一不该犯的错。没装配商户客户端时直接放行（单测路径）。
func (s *MembershipService) checkStore(ctx context.Context, storeID string) error {
	if s.stores == nil {
		return nil
	}
	exists, err := s.stores.Exists(ctx, storeID)
	if err != nil {
		return fmt.Errorf("resolve membership store: %w", err)
	}
	if !exists {
		return ErrStoreNotFound
	}
	return nil
}

// StoreNames 把一批归属门店 ID 解成 ID→名字。
//
// **一次解一页**：调用点是「渲染一页列表」与「渲染一条详情」，两处都只该付一次跨服务往返
// （不是一行一次）。
//
// 解不出来（商户域抖了、这个 id 商户域不认识）时返回空 map，**不返回错误**：名字只用来显示。
// 让整个会员列表因为商户域抖一下就打不开——连冻结都做不了——代价比几个空格子大得多，而空格子
// 也不构成谎言：门店的身份是 store_id 那一列，它来自本库。
func (s *MembershipService) StoreNames(ctx context.Context, storeIDs []string) map[string]string {
	if s.stores == nil {
		return map[string]string{}
	}
	ids := make([]string, 0, len(storeIDs))
	seen := make(map[string]struct{}, len(storeIDs))
	for _, id := range storeIDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return map[string]string{}
	}
	names, err := s.stores.Names(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "membership: failed to resolve store names", "error", err, "stores", len(ids))
		return map[string]string{}
	}
	return names
}

// mutationInput 是三个人工干预动作共用的入参校验：会员 ID 与理由。
//
// required 由调用方给，因为三处给用户看的话不一样（「请填写冻结原因」「请填写撤销原因」）。
func (s *MembershipService) mutationInput(id, reason string, required error) (string, string, error) {
	membershipID, err := requiredID(id, ErrMembershipIDRequired, ErrMembershipIDInvalid)
	if err != nil {
		return "", "", err
	}
	trimmedReason, err := checkReason(reason, required)
	if err != nil {
		return "", "", err
	}
	return membershipID, trimmedReason, nil
}

// Actor 是**谁做的这次后台操作**。
//
// 打包成一个结构而不是三个字符串参数：这三样（操作人、追踪号、请求号）每一处都要一起传下去，
// 而漏掉其中任何一个的后果在编译期看不出来——审计里少一个 ActorID，那条记录就没法回答
// 「谁动的」。打包之后，调用方漏传是编译错误。
type Actor struct {
	AdminID   string
	TraceID   string
	RequestID string
}

// ============================================================
// 用户自己的开关
// ============================================================

// SetAutoRenewByUser 是用户在小程序上开关自动续费。
//
// # 只能关，不能开
//
// 打开自动续费要签一份微信委托代扣协议，而那一步只能从客户端发起（微信的签约页面在小程序
// 里）：走的是 CreateSubscription 那条路（见 signing.go），不是这个接口。所以这里只接受
// 「关」——让它在「开」上静默成功，用户会得到一个「已开启自动续费」的界面，而他名下**没有
// 任何协议**，下个月不会扣款，会员会断，他还会以为系统坏了。
//
// # 「关」有两件事，顺序不能反
//
// `memberships.auto_renew` 是 `membership_subscriptions` 的投影（见 repository.applyAutoRenew），
// 所以「关掉自动续费」在名下有活订阅时**就是解约**，而不是翻一个 flag：
//
//  1. 先让支付域把渠道那份协议解掉。解不成（超时、渠道明确拒绝）就**什么都不改**地回错。
//  2. 成功了才把订阅收口成 cancelled——同一个事务里那个 flag 跟着灭（走的是
//     repository.SettleSubscription，它顺带写一条 auto_renew_off 流水）。
//
// 反过来的顺序（先灭 flag、再去解约）在一次失败里留下的是「用户看到已关闭、微信那边协议还
// 活着」，而协议活着就意味着下个月照扣（见 terminateForSubscription）。
//
// 名下**没有**活订阅的那两条路（店铺码活动发放、后台开通）没有渠道侧，开关就是开关本身：
// 直接翻 flag，与今天一致。
//
// # 按 user_id 找，不按 membership_id
//
// C 端手里只有自己的登录态（user_id），拿不到会员 ID，也不该拿得到——会员 ID 是后台的概念。
// 查不到会员时原样回仓储那条 ErrMembershipNotFound，前端据此显示「你还不是会员」。
func (s *MembershipService) SetAutoRenewByUser(ctx context.Context, userID string, enabled bool, traceID, requestID string) (*model.Membership, error) {
	id, err := requiredID(userID, ErrUserIDRequired, ErrUserIDInvalid)
	if err != nil {
		return nil, err
	}
	if enabled {
		return nil, ErrAutoRenewNeedsSigning
	}
	current, err := s.repository.GetMembership(ctx, id)
	if err != nil {
		return nil, err
	}

	live, err := s.repository.GetLiveSubscription(ctx, current.ID)
	switch {
	case errors.Is(err, repository.ErrSubscriptionNotFound):
		// 没有代扣协议可解（券发放、后台开通那两条路）。operatorType=user：仓储据此**不记
		// 审计**（方案 §11.6 审的是人工干预，用户关自己的开关不在其中）。它的痕迹在
		// membership_changes 里——客服要查的是「他什么时候关的」，不是「哪个运营关的」。
		return s.repository.SetAutoRenew(ctx, current.ID, false, model.OperatorUser, id, "", traceID, requestID)
	case err != nil:
		return nil, err
	}

	if _, err := s.terminateForSubscription(ctx, live, "用户关闭自动续费", requestID); err != nil {
		return nil, err
	}
	if _, _, err := s.repository.SettleSubscription(ctx, repository.SettleParams{
		SubscriptionID: live.ID,
		Target:         model.SubscriptionStatusCancelled,
		CancelReason:   "用户关闭自动续费",
		// 发起人写进 cancelled_by：这条订阅是**用户自己**在小程序上关掉的，与「渠道说这份协议
		// 已经作废了」（同一列留空，见 SettleParams）在看板上是两件事。
		CancelledBy:     id,
		CancelledByType: model.OperatorUser,
		// 渠道的原话进审计的 after 快照（这里 AdminID 为空所以不记审计，留着是为了这一行将来
		// 若被别的调用方复用时不必再想一次）。解约成功只可能是这一个状态。
		ProviderState: dto.AgreementStatusTerminated,
		OccurredAt:    s.Now(),
		TraceID:       traceID,
	}); err != nil {
		return nil, err
	}
	// 回读会员：这一行的 auto_renew 是刚刚在 settle 那个事务里灭的，客户端要拿它回显。
	return s.repository.GetMembership(ctx, id)
}

// ============================================================
// 定时扫描
// ============================================================

// ExpireDue 跑一轮到期扫描，返回这一轮处理了几条。
//
// 由 worker 按周期调用（见 internal/worker）。它是**幂等**的：同一批行被扫两遍，第一遍之后
// status 已经不是 active，第二遍的 WHERE 就选不到它们了。
func (s *MembershipService) ExpireDue(ctx context.Context, limit int, traceID string) (int, error) {
	return s.repository.ExpireDue(ctx, limit, traceID)
}

// Now 返回业务层认为的「现在」。测试与 entitlement 判定共用它。
func (s *MembershipService) Now() time.Time { return s.now().UTC() }
