package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// traceID 是一次请求的追踪号，进 outbox 事件的信封。
//
// 用平台那一个而不是自己从请求头里读：trace 中间件已经把它放进 context 了，两处各读一遍
// 迟早会读到两个不同的值。
func traceID(r *http.Request) string { return audit.TraceIDFromContext(r.Context()) }

// requestID 是这次请求的留痕串，写进 membership_changes.request_id。
//
// 与 traceID 分开取是有意的：trace 是**一次调用链**的号（跨服务是一样的），request_id 是
// **一次后台点击**的号。用户重试一次会得到新的 request_id，但可能共用一个 trace。审计与流水
// 上要的是后者——「这次操作」而不是「这一串调用」。
func requestID(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Request-Id"))
}

// —— 映射 ——
//
// service 回模型，这里映射成 dto。**映射只做形状转换，不做判断**：凡是「如果 X 就填 Y」的
// 写法都要停一下——那多半是业务规则，它该在 service 里，而且该被测试。
//
// 这里的映射函数一律是 `xxxResponse` / `xxxResponses` 一对，与 lottery-service、
// payment-service 同名同形。

func planResponse(plan *model.Plan) dto.PlanResponse {
	return dto.PlanResponse{
		ID:          plan.ID,
		Code:        plan.Code,
		Name:        plan.Name,
		Description: plan.Description,
		Benefits:    decodeBenefits(plan.Benefits),
		PriceCents:  plan.PriceCents,
		Period:      plan.Period,
		PeriodCount: plan.PeriodCount,
		AutoRenew:   plan.AutoRenew,

		WechatPlanID:    plan.WechatPlanID,
		MemberPriceMode: plan.MemberPriceMode,

		MemberPriceCouponTemplateID: derefString(plan.MemberPriceCouponTemplateID),
		MemberPriceCouponsPerPeriod: derefInt32(plan.MemberPriceCouponsPerPeriod),
		SortOrder:                   plan.SortOrder,
		Status:                      plan.Status,
		CreatedAt:                   plan.CreatedAt,
		UpdatedAt:                   plan.UpdatedAt,
	}
}

func planResponses(plans []*model.Plan) []dto.PlanResponse {
	responses := make([]dto.PlanResponse, 0, len(plans))
	for _, plan := range plans {
		responses = append(responses, planResponse(plan))
	}
	return responses
}

// miniappPlanResponse 映射 C 端看到的套餐。
//
// 它与 planResponse 读的是同一个模型、写的是**两个不同的结构**（见 dto 的说明）：签约模板 ID
// 与那几个运营属性（草稿态、排序、创建时间）不下发到客户端。
func miniappPlanResponse(plan *model.Plan) dto.MiniappPlanResponse {
	return dto.MiniappPlanResponse{
		ID:          plan.ID,
		Code:        plan.Code,
		Name:        plan.Name,
		Description: plan.Description,
		Benefits:    decodeBenefits(plan.Benefits),
		PriceCents:  plan.PriceCents,
		Period:      plan.Period,
		PeriodCount: plan.PeriodCount,
		AutoRenew:   plan.AutoRenew,

		MemberPriceMode:             plan.MemberPriceMode,
		MemberPriceCouponsPerPeriod: derefInt32(plan.MemberPriceCouponsPerPeriod),
	}
}

func miniappPlanResponses(plans []*model.Plan) []dto.MiniappPlanResponse {
	responses := make([]dto.MiniappPlanResponse, 0, len(plans))
	for _, plan := range plans {
		responses = append(responses, miniappPlanResponse(plan))
	}
	return responses
}

// decodeBenefits 把 JSONB 那串字节解回 []string。
//
// 解不开时回**空列表并记 warn**，不报错也 panic：权益文案只用于展示，一条读不出来的文案不该
// 让整个套餐列表接口变成 500——那样后台连改都改不了（编辑页也读这个接口）。仓储那边已经用
// COALESCE 兜了一层 NULL，这里是第二层：内容是坏的（不是 NULL）时也还能把页面打开。
func decodeBenefits(raw []byte) []string {
	benefits := make([]string, 0, 4)
	if len(raw) == 0 {
		return benefits
	}
	if err := json.Unmarshal(raw, &benefits); err != nil {
		slog.Warn("membership plan benefits are not a string array", "error", err)
		return make([]string, 0, 4)
	}
	return benefits
}

// membershipResponse 映射一条会员。
//
// `now` 由调用方传进来，**不在这里调 time.Now()**：一页 20 条各自的「现在」会差出几百微秒，
// 而那意味着同一条会员在列表里与详情里对「过期了没有」可能给出两个答案。调用方取一次
// s.Now()，整张响应共用它。
//
// `storeNames` 是**已经解好的一页门店名**（见 AdminMembershipController.responses）：名字要
// 跨服务问，而一行问一次会让一页列表变成 20 次 gRPC。解不出来的门店在这里落成空串，前端显示
// 「—」，不报错也不显示 id——门店的身份是 storeId 那一列，名字只是给人看的。
func membershipResponse(membership *model.Membership, now time.Time, storeNames map[string]string) dto.MembershipResponse {
	return dto.MembershipResponse{
		ID:              membership.ID,
		UserID:          membership.UserID,
		PlanCode:        membership.PlanCode,
		PlanName:        membership.PlanName,
		MemberPriceMode: membership.MemberPriceMode,
		StoreID:         membership.StoreID,
		StoreName:       storeNames[membership.StoreID],
		Status:          membership.Status,
		StartAt:         membership.StartAt,
		ExpireAt:        membership.ExpireAt,
		// Active 是**算出来给前端用的**（status=active 且在有效期内）。它的定义在
		// model.Membership.IsUsable 上，与 entitlement 判定用的是同一个方法——两处各写一遍
		// 「active 且没过期」，迟早会有一处漏掉 status 那一半。
		Active: membership.IsUsable(now),

		AutoRenew:      membership.AutoRenew,
		AutoRenewOffAt: membership.AutoRenewOffAt,
		RenewalCount:   membership.RenewalCount,
		LastRenewedAt:  membership.LastRenewedAt,
		FrozenAt:       membership.FrozenAt,
		FreezeReason:   membership.FreezeReason,
		RevokedAt:      membership.RevokedAt,
		RevokeReason:   membership.RevokeReason,
		CreatedAt:      membership.CreatedAt,
		UpdatedAt:      membership.UpdatedAt,
		// 列表页永远是空数组（见 dto 的说明），详情页由 membershipDetailResponse 填。
		Changes: []dto.ChangeResponse{},
	}
}

func membershipResponses(memberships []*model.Membership, now time.Time, storeNames map[string]string) []dto.MembershipResponse {
	responses := make([]dto.MembershipResponse, 0, len(memberships))
	for _, membership := range memberships {
		responses = append(responses, membershipResponse(membership, now, storeNames))
	}
	return responses
}

// membershipDetailResponse 在列表形状之上补上时间线。
func membershipDetailResponse(membership *model.Membership, changes []*model.Change, now time.Time, storeNames map[string]string) dto.MembershipResponse {
	response := membershipResponse(membership, now, storeNames)
	response.Changes = changeResponses(changes)
	return response
}

// membershipStoreIDs 取出一批会员里用到的归属门店 ID（去重、丢掉空串）。
//
// 它服务于「一页只解一次名字」：先把这一页用到的门店收齐，一次问商户域，再映射。
func membershipStoreIDs(memberships []*model.Membership) []string {
	ids := make([]string, 0, len(memberships))
	seen := make(map[string]struct{}, len(memberships))
	for _, membership := range memberships {
		if membership.StoreID == "" {
			continue
		}
		if _, ok := seen[membership.StoreID]; ok {
			continue
		}
		seen[membership.StoreID] = struct{}{}
		ids = append(ids, membership.StoreID)
	}
	return ids
}

func changeResponse(change *model.Change) dto.ChangeResponse {
	return dto.ChangeResponse{
		ID:         change.ID,
		ChangeType: change.ChangeType,
		// From/To 两对里的可空项回空串而不是 null（见 derefString 的说明）。
		FromStatus:   derefString(change.FromStatus),
		ToStatus:     derefString(change.ToStatus),
		FromExpireAt: change.FromExpireAt,
		ToExpireAt:   change.ToExpireAt,
		OrderID:      change.OrderID,
		OperatorType: change.OperatorType,
		OperatorID:   change.OperatorID,
		Reason:       change.Reason,
		Remark:       change.Remark,
		OccurredAt:   change.OccurredAt,
	}
}

func changeResponses(changes []*model.Change) []dto.ChangeResponse {
	responses := make([]dto.ChangeResponse, 0, len(changes))
	for _, change := range changes {
		responses = append(responses, changeResponse(change))
	}
	return responses
}

// —— 身份 ——

// requireAdmin 读后台调用者的身份。认证与权限码已经由路由上的中间件验过，这里只取「是谁」。
//
// 取不到就回 401 而不是继续：冻结、撤销、调整有效期都要把操作人写进审计与流水。一条**查不到
// 操作人**的人工调整记录等于没有记录——而它改的是「这个人还能不能用会员价」。
func requireAdmin(w http.ResponseWriter, r *http.Request) (service.Actor, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, msgUnauthorized)
		return service.Actor{}, false
	}
	return service.Actor{
		AdminID:   identity.UserID,
		TraceID:   traceID(r),
		RequestID: requestID(r),
	}, true
}

// requireUser 读小程序调用者的身份。
//
// 与 requireAdmin 读的是同一个东西（令牌里的 UserID），分开成两个函数是因为**它们回答的
// 问题不同**：后台那个问「这个管理员是谁」（要写进审计），这个问「这是哪个用户」（要拿它去
// 查他自己的会员）。合成一个的话，日后给两者加不同的约束（比如后台要求某个权限前缀）就得
// 先把这个函数拆开。
func requireUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	identity, ok := auth.IdentityFromRequest(r)
	if !ok || strings.TrimSpace(identity.UserID) == "" {
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, msgUnauthorized)
		return "", false
	}
	return identity.UserID, true
}

// —— 请求解析 ——

// decodeBody 解析 JSON 请求体。
//
// **不发 DisallowUnknownFields**：这里的请求体是 HTTP 契约，前端多传一个字段（比如它自己
// 的 UI 状态）不该让一次保存失败。那条规矩用在 MQ 的消息上（见 service.newDecoder）——那里
// 多发一个字段意味着**生产者改了契约**，是必须炸出来的事；而 HTTP 的调用方是我们自己发的
// 前端，多一个字段只是多一个字段。
func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", msgInvalidBody)
		return false
	}
	return true
}

// —— 错误 ——

// 几个在请求解析阶段就要回给用户的中文句子。
//
// 抽成常量而不是在各处各写一遍字面量：这些调用点本来就有同一个含义，而写多遍的结果一定是改
// 的时候漏掉几处——后台是中文界面，漏掉的那几处就会弹英文。
//
// 它们不进 userMessages：那张表按**错误值**索引，而这几句产生在错误值出现之前（请求体还
// 没解成结构体）。
const (
	msgInvalidBody  = "请求体格式不正确"
	msgUnauthorized = "未登录"
	msgInvalidQuery = "查询参数不正确"
	msgPlanMissing  = "套餐不存在"
)

// userMessages 是「服务层的错误值 → 给用户看的那句话」。
//
// **为什么要有这张表**：服务层的错误串一律是英文（`errors.New("plan name is required")`），
// 这是 Go 的惯例，也让 grep 日志的语义保持清楚。但 admin-web 的 requestErrorMessage 会
// **优先用后端返回的 errorMessage**，于是这些英文原样弹到了运营脸上。
//
// 所以人话在这一层决定：controller 是 HTTP 边界，也是唯一同时知道「这是哪一条错误」和
// 「这句话是给谁看的」的地方。
//
// **这张表必须是全集**：漏一条不会报错，只会静默退回兜底那句话（日志里留一条 warn）。
// response_test.go 拿 service 的错误全集逐个钉住，新增错误值忘了加表会红。
//
// 措辞对着「运营看到这句话之后该做什么」写，不是对着英文直译。
var userMessages = []struct {
	err  error
	text string
}{
	// —— 请求不合法（400）——
	{service.ErrPlanIDRequired, "请选择套餐"},
	{service.ErrPlanIDInvalid, "套餐 ID 不是合法的 UUID"},
	{service.ErrPlanCodeRequired, "请填写套餐编码"},
	{service.ErrPlanCodeInvalid, "套餐编码只能用小写字母、数字和下划线"},
	{service.ErrPlanNameRequired, "请填写套餐名称"},
	{service.ErrPlanPriceNegative, "套餐价格不能是负数"},
	{service.ErrPlanPriceTooLarge, "套餐价格填得太大了，请检查是不是多打了一位"},
	{service.ErrPlanPeriodInvalid, "订阅周期只能是按月或按年"},
	{service.ErrPlanPeriodCountRange, "每期时长要填 1 到 120 之间"},
	// 这一句要说清「为什么必须填」：微信代扣要有签约模板 ID，没有它协议根本建不出来，
	// 「开启了连续包月」就是一句空话。
	{service.ErrPlanWechatIDRequired, "开启了连续包月就必须填微信签约模板 ID，否则签约建不出来"},
	{service.ErrPlanWechatIDNotAllowed, "只有开启连续包月的套餐才需要微信签约模板 ID"},
	{service.ErrPlanPriceModeInvalid, "会员价方式只能是「会员自动享」或「发会员价券」"},
	{service.ErrPlanCouponFieldsRequired, "发券模式下要同时填券模板和每期张数"},
	{service.ErrPlanCouponFieldsNotAllowed, "只有发券模式才能填券模板与张数"},
	{service.ErrPlanCouponsPerPeriodRange, "每期发券张数必须大于 0"},
	{service.ErrPlanStatusInvalid, "套餐状态只能是草稿 / 已上架 / 已下架"},
	{service.ErrPlanBenefitsTooMany, "权益说明最多 20 行"},
	{service.ErrPlanBenefitTooLong, "每行权益说明不能超过 100 个字"},

	{service.ErrMembershipIDRequired, "请选择会员"},
	{service.ErrMembershipIDInvalid, "会员 ID 不是合法的 UUID"},
	{service.ErrUserIDRequired, "请选择用户"},
	{service.ErrUserIDInvalid, "用户 ID 不是合法的 UUID"},
	{service.ErrFreezeReasonRequired, "请填写冻结原因"},
	{service.ErrUnfreezeReasonRequired, "请填写解冻原因"},
	{service.ErrRevokeReasonRequired, "请填写撤销原因"},
	{service.ErrAdjustReasonRequired, "请填写调整原因"},
	{service.ErrAdjustExpireRequired, "请填写调整后的到期时间"},
	{service.ErrAdjustRemarkTooLong, "备注不能超过 500 个字"},
	{service.ErrOccurredRangeInvalid, "到期时间的结束值必须晚于开始值"},
	{service.ErrMembershipStatusInvalid, "会员状态只能是 active / frozen / expired / revoked"},
	{service.ErrReasonTooLong, "原因不能超过 200 个字"},
	{service.ErrGrantReasonRequired, "请填写开通原因"},
	// 这一句要说清「怎么办」：requestId 是客户端生成的，操作员看不到它，所以不能只说
	// 「缺少 requestId」。对应的动作是刷新页面重来。
	{service.ErrGrantRequestIDRequired, "这次提交缺少幂等标识，请刷新页面重新操作"},
	{service.ErrStoreIDInvalid, "门店 ID 不是合法的 UUID"},
	{service.ErrStoreNotFound, "选的门店不存在，请重新选择"},
	{service.ErrGrantExpireTooEarly, "到期时间必须晚于现在"},

	{service.ErrSubscriptionStatusInvalid, "订阅状态只能是待签约 / 生效中 / 已暂停 / 已取消 / 已到期"},
	{service.ErrCancelReasonRequired, "请填写取消原因"},

	{service.ErrSubscriptionRequestIDRequired, "这次提交缺少幂等标识，请退出重进再试一次"},
	// 「不是连续包月」要指出该挑什么样的：C 端的套餐列表里那几款看不出哪一款能签约，只说
	// 「不支持」等于让用户去试。
	{service.ErrSubscriptionPlanNotSubscription, "这款套餐不支持自动续费，请选择连续包月套餐"},

	{service.ErrCampaignNameRequired, "请填写活动名称"},
	{service.ErrCampaignNameTooLong, "活动名称不能超过 60 个字"},
	{service.ErrCampaignSceneRequired, "请填写活动码参数"},
	// 这一句要带上「怎么才对」：scene 是个自由填的串，只说「不合法」等于让运营去猜规则。
	{service.ErrCampaignSceneInvalid, "活动码参数要以 smc_ 开头，只能用字母、数字、下划线和连字符，且不超过 32 个字符"},
	{service.ErrStoreIDRequired, "请选择活动门店"},
	{service.ErrCampaignGiftDaysRange, "赠送天数要填 1 到 3650 之间"},
	{service.ErrCampaignWindowInvalid, "活动的结束时间必须晚于开始时间"},
	{service.ErrCampaignStatusInvalid, "活动状态只能是草稿 / 已启用 / 已停用"},
	// 说清「该挑什么样的套餐」：运营手上那几款套餐看不出哪一款能签约，只说「不支持」等于让他去试。
	{service.ErrCampaignPlanNotSubscription, "只有开了连续包月的套餐才能做店铺码活动，请换一个套餐"},
	// 「只填了一半」要指明缺的是哪一件，否则运营会盯着自己填好的那一栏看出问题来。
	{service.ErrCampaignCouponIncomplete, "赠送券的模板和张数要一起填，或者都留空"},
	// 张数越界与模板 id 不是 uuid 共用一句：两者都是「这一栏填的东西用不了」，而模板是跨库值
	// 引用，本服务查不出它存不存在——那句话只能说到「请从下拉里选」为止。
	{service.ErrCampaignCouponInvalid, "赠送券要选一个券模板，张数填 1 到 100 之间"},

	// —— 找不到（404）——
	{service.ErrPlanNotFound, "套餐不存在"},
	{service.ErrMembershipNotFound, "这个人还不是会员"},
	// 这句话在订阅页上也要说得通：那个 id 只可能来自列表里的链接，查不到就是**这条订阅不在**。
	{service.ErrSubscriptionNotFound, "这条包月订阅不存在"},
	// 这一句要同时说得通两种情形：后台打开一个不存在的活动，以及用户扫了一张本系统不认的码。
	{service.ErrCampaignNotFound, "没有找到对应的店铺码活动"},
	// 「支付那边没有这个协议号」：本地这一行的协议号是支付服务给的，所以它只可能是我们写坏了
	// 或请求带错了号。给用户的话只能指向下一步——重新签一次（那会生成一个新的协议号）。
	{service.ErrAgreementNotFound, "没有找到这次的签约记录，请重新发起签约"},

	// —— 下游没答上来（503）：不是任何一方的错，稍后再试 ——
	{service.ErrChannelUnavailable, "支付渠道暂时不可用，请稍后再试"},

	// —— 状态冲突（409）：请求本身没问题，是现在这个局面不接受它 ——
	{service.ErrPlanCodeTaken, "这个套餐编码已经被占用了，换一个再存"},
	{service.ErrPlanNotSellable, "这个套餐还没有上架，买不了"},
	// 「已经过期」要说清下一步：他不是不能买，是不能靠开关续上——去重新下单。
	{service.ErrMembershipExpired, "这个会员已经过期了，先续费再打开自动续费"},
	{service.ErrMembershipRevoked, "这个会员已经被撤销了，不能再恢复，请联系技术处理"},
	{service.ErrMembershipNotActive, "这个会员不是生效状态，冻结不了"},
	{service.ErrMembershipNotFrozen, "这个会员没有被冻结，不用解冻"},
	{service.ErrExpireBeforeStart, "调整后的到期时间必须晚于开通时间"},
	// 「已经做过了」不是故障：运营要的是「我这次点击有没有又续一遍」的答案，而答案是「没有」。
	{service.ErrDuplicateChange, "这一单的会员权益已经生效过了，没有重复开通"},
	// 「已经有了」也不是故障：后台开通撞上已有会员时，答案就是「这个人已经是会员了」。这句话
	// 必须指向下一步——他要做的是去会员详情页调整有效期，而不是在这里再点一次提交。
	{service.ErrMembershipExists, "这个人已经是会员了，不能重复开通；要改有效期请到会员详情页"},
	// 打开自动续费要先签约，而签约只能从小程序里发起。这句话必须指向那一步，否则用户会以为
	// 是系统坏了——他看到的是一个「开启」按钮点了没反应。
	{service.ErrAutoRenewNeedsSigning, "自动续费要在小程序里签约开通，不能在这里打开"},
	// 「不能取消」要带上当前状态才有用：运营看到这句话要去核对的正是那个状态（比如「它其实
	// 已经暂停了，那该走的是另一条路」）。只说「不能取消」等于让他自己去猜为什么。
	{service.ErrSubscriptionNotCancellable, "只有生效中的订阅才能取消，这条订阅当前不是生效状态"},
	{service.ErrCampaignSceneTaken, "这个活动码参数已经被别的活动用了，换一个再存"},
	// 这两句是给用户的（小程序端）：都是「今天领不到」，但原因不同——一个是被运营关掉了或窗口
	// 过了，一个是这个人已经领过。用户能做的也不一样：前者没有下一步，后者是「你已经有了」。
	{service.ErrCampaignNotClaimable, "这个活动已经结束或暂停了"},
	{service.ErrCampaignAlreadyClaimed, "这个活动你已经领过了"},
	// 改 scene / store_id 要先停用。这句话必须带上那个动作，否则运营只会看到「保存失败」。
	{service.ErrCampaignLocked, "活动启用中，不能改活动码参数和活动门店；请先停用再改"},
	// 「已经有了」：答案就是「不用再签」，用户要做的是去看自己那条订阅，不是再点一次。
	{service.ErrLiveSubscriptionExists, "你已经开通了连续包月，不用重复签约"},
	// 「这个幂等号用过、那条订阅已经结束了」：这一次点击对应的签约已经是过去的事了，要重新发起。
	// 它不能重放（那会回一条已取消的订阅，用户以为签好了），也不能重建（协议号已经作废）。
	{service.ErrSubscriptionRequestUsed, "这次签约已经失效了，请重新发起"},
	// 「上一笔还在跑」：**它是等等就有用的那一类**，所以要说「稍后再试」而不是「失败」。
	{service.ErrAgreementConflict, "上一次签约还在处理中，请稍后再试"},
	// 「渠道拒绝了这次解约」：必须说清**什么都没改**（用户可以再点一次，不会出现「点了一半」
	// 的状态），并且给一条一直不行时的下一步——客服手上那个出口是后台的「同步」。
	{service.ErrAgreementTerminateRefused, "微信侧没有同意这次解约，自动续费还是开着的；请稍后再试，或联系客服"},
	// 下面三句都指向「联系技术处理」：协议被绑到别人身上、协议号对不上、已结束的订阅被要求改成
	// 生效中——三件事都不是用户点错了什么，而是数据或渠道那边出了要人核的状态。
	{service.ErrAgreementTaken, "这份代扣协议已经绑在别的订阅上了，请联系技术处理"},
	{service.ErrAgreementMismatch, "签约协议与记录对不上，请联系技术处理"},
	{service.ErrSubscriptionNotSettleable, "这条订阅已经结束，不能再改成生效中，请联系技术处理"},
	// 「没绑小程序身份」要说清下一步：他不是不能用，是**得先用微信登录一次**。只说「签约失败」
	// 会让他反复点，而那个按钮永远点不成。
	{service.ErrWalletIdentityRequired, "请先在小程序里用微信登录一次，再开通连续包月"},

	// —— 身份缺失（401）：操作人不在请求体里，它来自令牌 ——
	{service.ErrActorRequired, "未登录"},
}

// userMessage 取这条错误给用户看的那句话；第二个返回值是「表里有它」。
//
// 用 errors.Is 逐个比而不是 map[error]string：service 与 repository 会把错误包一层上下文
// 再往上返，包装之后两个值不再相等，map 查不到。
func userMessage(err error) (string, bool) {
	for _, entry := range userMessages {
		if errors.Is(err, entry.err) {
			return entry.text, true
		}
	}
	return "", false
}

// genericErrorMessage 是表里没有这条错误时的兜底。
//
// 它**故意不是 err.Error()**：兜底存在的意义就是守住「不把英文和内部报错弹给用户」这条规矩。
const genericErrorMessage = "操作失败，请稍后重试"

// userFacingMessage 取人话，取不到就兜底并记一条 warn。
//
// 记 warn 而不是 error：漏一条表不该让一次请求以 500 收场，那是两件事。但它在日志里必须
// 找得到——不然后台只会说「操作失败」，谁也看不出漏的是哪一条。
func userFacingMessage(ctx context.Context, err error) string {
	if text, ok := userMessage(err); ok {
		return text
	}
	slog.WarnContext(ctx, "membership error has no user-facing message", "error", err)
	return genericErrorMessage
}

// 分组：writeMembershipError 按组映射 HTTP 状态，response_test.go 按组核对人话表。
//
// 分成组而不是把 errors.Is 一条条写进 switch，是为了让「新增一条 404 错误」变成一处改动，
// 而不是两处（switch 一处、测试一处）——两处的写法漏过一次之后，测试就不再可信了。
var (
	// notFoundErrors：都是「你要的那个东西找不到」。
	notFoundErrors = []error{
		service.ErrPlanNotFound,
		service.ErrMembershipNotFound,
		service.ErrSubscriptionNotFound,
		service.ErrCampaignNotFound,
		// 「支付那边没有这个协议号」（见 userMessages 里那一条的说明）。
		service.ErrAgreementNotFound,
	}

	// conflictErrors：请求本身没问题，是现在这个局面不接受它。
	conflictErrors = []error{
		service.ErrPlanCodeTaken,
		service.ErrPlanNotSellable,
		service.ErrMembershipExpired,
		service.ErrMembershipRevoked,
		service.ErrMembershipNotActive,
		service.ErrMembershipNotFrozen,
		service.ErrExpireBeforeStart,
		service.ErrDuplicateChange,
		// 后台开通撞上已有会员是**正常答案**（见 service.ErrMembershipExists）：请求本身没
		// 问题，是现在这个局面不接受它。它也是两笔支付同时被处理时的那条并发结论。
		service.ErrMembershipExists,
		// 「你得先去做另一件事」（去小程序签约）。它不是「你填错了」，所以不归 400；
		// 也不是「找不到」或「已经做过了」。409 是这一组里唯一说得通的位置——当前局面
		// 不接受这个请求，换一个做法就能成。
		service.ErrAutoRenewNeedsSigning,
		// 取消一条非 active 的订阅：请求本身没问题，是这条订阅现在的状态不接受它。
		service.ErrSubscriptionNotCancellable,
		// scene 撞车是**填重了**，不是并发，也不是请求不合法：换一个再存就成。
		service.ErrCampaignSceneTaken,
		// 「今天领不到」：活动被关了、或窗口过了。请求没有错，是这个局面不接受它。
		service.ErrCampaignNotClaimable,
		// 「已经领过了」：与 ErrDuplicateChange 同一类——不是故障，是「这件事已经做过了」。
		// 它只在并发下才走到（正常路径上领取事务先查了记录并以 200 回放）。
		service.ErrCampaignAlreadyClaimed,
		// 活动开着就不给改 scene / store_id：请求没问题，是现在这个状态不接受它。
		service.ErrCampaignLocked,
		// 「你已经有一条活着的订阅了」：多半是用户连点了两次，答案就是「已经有了，不用再签」。
		// 它与「这次点击之前落过一条订阅」不是一回事——那是重放，走的是 200 回放。
		service.ErrLiveSubscriptionExists,
		// 这个幂等号用过，而它建的那条订阅已经结束了。不是重放（那会回一条已取消的订阅，用户
		// 会以为签好了），也不能重建（撞唯一索引），所以停下来让他重新发起。
		service.ErrSubscriptionRequestUsed,
		// 同一把钥匙上的上一笔还在跑：等一会儿、或换一次点击就成。
		service.ErrAgreementConflict,
		// 「渠道拒绝了这次解约」：请求本身没有问题（这是那个订阅唯一的收场方式之一），是渠道
		// 现在给的答案不接受它。归 503 是错的——那会让用户以为「等一会儿自然就好了」，而这里
		// 要的是重试一次、不行就找人。归 500 也是错的——我们没写错任何东西。
		service.ErrAgreementTerminateRefused,
		// 这份协议已经绑在别的订阅上了。它与「扣两次钱」只差一步，必须是一条说得清的冲突。
		service.ErrAgreementTaken,
		// 回来的协议号与本地记着的对不上：**要人核**，但请求本身没问题，是现在这份数据不接受它。
		service.ErrAgreementMismatch,
		// 一条已经结束的订阅不许被改回生效中（渠道说协议活了、而本地是取消或到期）。
		service.ErrSubscriptionNotSettleable,
		// 没绑微信小程序身份：不是故障，也不是请求不合法，是「你得先去做另一件事」（去小程序里
		// 用微信登录一次）。与 ErrAutoRenewNeedsSigning 同一组、同一条理由。
		service.ErrWalletIdentityRequired,
	}

	// unavailableErrors：**下游没答上来**，不是任何一方的错。
	//
	// 它是这个域里唯一一组「重试就有用」的错误，所以必须与 409 分开：409 的每一句都在说
	// 「现在这个局面不接受它」，而用户照着那句话去改是没用的——这里要做的是稍后再点一次。
	// 归进 409 或 500 都会让人去做错的事（一个去改请求，一个来报障）。
	unavailableErrors = []error{service.ErrChannelUnavailable}

	// unauthorizedErrors：操作人不在请求体里，它来自令牌。走到这里说明装配处漏挂了认证
	// 中间件，不是用户填错了什么。
	unauthorizedErrors = []error{service.ErrActorRequired}
)

// matches 判断 err 是不是这一组里的某一个。
func matches(err error, group []error) bool {
	for _, candidate := range group {
		if errors.Is(err, candidate) {
			return true
		}
	}
	return false
}

// writeMembershipError 把业务层的错误映射成 HTTP 响应。
//
// 顺序有讲究：**先认请求不合法，再认业务结论**。两边的错误值不重叠，所以顺序本身不影响
// 结果，但它决定了新增一个错误值时最先要问的问题——「这是调用方传错了，还是一次说得清楚的
// 结果？」。
//
// 回给用户的一律是中文（见 userMessages）。`fallback` 是**给日志的**：它是「这是哪一次
// 请求」的英文描述，配合 slog 里的 error 才能定位，它不进响应体。
func writeMembershipError(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	case service.IsValidationError(err):
		// 错误码用字面量 "INVALID_ARGUMENT"，与 order-service / coupon-service 一致
		// （platform/api 的常量表里没有它，那个表是给跨服务的通用码用的）。
		api.Error(w, http.StatusBadRequest, "INVALID_ARGUMENT", userFacingMessage(r.Context(), err))

	case matches(err, notFoundErrors):
		api.Error(w, http.StatusNotFound, api.CodeNotFound, userFacingMessage(r.Context(), err))

	case matches(err, conflictErrors):
		api.Error(w, http.StatusConflict, api.CodeConflict, userFacingMessage(r.Context(), err))

	case matches(err, unavailableErrors):
		// 503 而不是 500：这段代码是全仓对「下游没答上来」的统一说法（lottery-service 的
		// 「福卡账户不可用」、authz 的「权限服务暂不可用」都是这个状态码 + CodeUnavailable）。
		// 502 在这一仓里没有先例，而 api.CodeForStatus 也认 CodeUnavailable、不认网关那一档。
		api.Error(w, http.StatusServiceUnavailable, api.CodeUnavailable, userFacingMessage(r.Context(), err))

	case matches(err, unauthorizedErrors):
		api.Error(w, http.StatusUnauthorized, api.CodeUnauthorized, msgUnauthorized)

	default:
		slog.ErrorContext(r.Context(), "membership request failed", "error", err, "op", fallback)
		api.Error(w, http.StatusInternalServerError, api.CodeInternal, genericErrorMessage)
	}
}

// derefString 把可空字符串列转成 dto 里的普通字符串。
//
// 空串而不是指针：dto 面向的是 JSON，`null` 与 `""` 在前端那一侧通常会被同一个「或空串」
// 的兜底写法吞掉，多一层指针只会让 services/membership.ts 里多一堆 `?: string | null`。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// derefInt32 同理，可空的计数列回 0。
func derefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}
