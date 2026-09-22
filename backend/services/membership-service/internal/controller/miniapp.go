package controller

import (
	"errors"
	"net/http"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/api"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/service"
)

// MiniappMembershipController 是小程序的会员接口。三个，都只读或只动「自己的」那一条。
//
// **它不接受任何用户 ID 参数**：用户身份一律从令牌里取（requireUser）。开一个「按 userId
// 查会员」的 C 端接口，等于让任何人拿别人的 ID 去查他是不是会员、什么时候到期——这些是能
// 被拿去做精准营销、也让人不舒服的信息。后台有那个接口，因为后台是另一个信任边界。
type MiniappMembershipController struct {
	membership *service.MembershipService
}

// NewMiniappMembershipController 构造小程序控制器。
func NewMiniappMembershipController(s *service.MembershipService) *MiniappMembershipController {
	return &MiniappMembershipController{membership: s}
}

// Plans 是小程序会员页上的套餐列表：只有上架的，没有筛选，没有分页。
//
// 见 service.ListActivePlans 的说明：卖的东西就是那么几款，而 C 端的套餐列表**必须完整**
// ——少一个用户就买不到那一款，而他在界面上看不出少了什么。
func (c *MiniappMembershipController) Plans(w http.ResponseWriter, r *http.Request) {
	plans, err := c.membership.ListActivePlans(r.Context())
	if err != nil {
		writeMembershipError(w, r, err, "failed to list active membership plans")
		return
	}
	api.Success(w, miniappPlanResponses(plans))
}

// Me 是小程序会员中心的「我的会员」。
//
// **不是会员时回 200 加一个 null**，不是 404：绝大多数用户打开这一屏时都不是会员，把它做成
// 错误会让前端每一次正常访问都走错误分支（见 dto.MiniappMembershipResponse 的说明）。
//
// 它同时是前端回答「我的会员价还有没有」的地方——Active 与 MemberPriceMode 一起说明了这个人
// 现在到底能不能享会员价、以哪种方式享。
func (c *MiniappMembershipController) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	membership, err := c.membership.GetMembershipByUser(r.Context(), userID)
	if err != nil {
		// 「查不到」不是错误：见上面那段说明。除它之外的一切（库连不上、扫行失败）照常走错误
		// 分支——把那些也吞成 null 的话，用户会在系统真的坏掉时看到一个「你还不是会员」。
		if errors.Is(err, service.ErrMembershipNotFound) {
			api.Success(w, dto.MiniappMembershipResponse{})
			return
		}
		writeMembershipError(w, r, err, "failed to get own membership")
		return
	}
	api.Success(w, dto.MiniappMembershipResponse{Membership: miniappMembership(membership, c.membership.Now())})
}

// AutoRenew 开关自动续费。**只接受关，而且「关」是一次解约**。
//
// 打开自动续费要签一份微信委托代扣协议，而那一步只能从小程序端发起（微信的签约页面在小程序
// 里）：走的是 /subscriptions 那条路，不是这里。所以传 true 时回的是一条说得清的错误，而不是
// 静默成功——静默成功会让用户看到一个「已开启」的开关，而他名下没有任何协议，下个月不会扣款、
// 会员会断，他还会以为系统坏了。
//
// 关的那一下**不是翻一个 flag**：先让支付域把渠道那份协议解掉，成功了才改本地（见
// service.SetAutoRenewByUser）。所以它有失败的可能，而失败时本地**一个字节都没变**——回给用户
// 的话必须让他知道「还是开着的」，不能让他以为点过了（那正是「他以为关了、下个月照扣」那一格）。
func (c *MiniappMembershipController) AutoRenew(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body dto.AutoRenewRequest
	if !decodeBody(w, r, &body) {
		return
	}
	membership, err := c.membership.SetAutoRenewByUser(r.Context(), userID, body.Enabled, traceID(r), requestID(r))
	if err != nil {
		writeMembershipError(w, r, err, "failed to set auto renew")
		return
	}
	api.Success(w, dto.MiniappMembershipResponse{Membership: miniappMembership(membership, c.membership.Now())})
}

// ClaimCampaign 是用户扫门店里那张小程序码领会员。
//
// # 用户是谁只认令牌
//
// 与这一棵树上的另外两个接口同一条规矩：**没有 user_id 参数**。扫码的人就是领的人，客户端
// 能给本服务的影响只有「扫的是哪张码」（scene）。
//
// # 重复扫码回 200，不是错误
//
// 一个人一场活动只能领一次，第二次扫回的是**第一次的结果**（仓储在同一个事务里判的），只是
// 多一个 alreadyClaimed。页面据此说「你已经领过了」——把它做成 409 会让「我刚扫完又扫了一次」
// 在界面上变成一个红色错误，而那件事没有任何不对。
func (c *MiniappMembershipController) ClaimCampaign(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body dto.CampaignClaimRequest
	if !decodeBody(w, r, &body) {
		return
	}
	result, err := c.membership.ClaimCampaign(r.Context(), body, userID, traceID(r))
	if err != nil {
		writeMembershipError(w, r, err, "failed to claim campaign membership")
		return
	}
	api.Success(w, result)
}

// Subscribe 是「开通连续包月」：发起一次微信委托代扣签约。
//
// # 应答里那串 payParams 是一次性凭据
//
// 它带着签名，客户端拿它跳去微信的签约页。**它不进日志、不进任何流水**（见
// dto.SubscriptionSigningResponse）：一个能被重放的签约参数等于一张别人可以替你签的代扣授权。
//
// # 它只回「发起了」，不回「签好了」
//
// 返回的订阅是 pending_sign。用户到底点没点「同意」只由 Confirm 回渠道问出来——这条边界是
// 这一整条链上最要紧的一句话，前端不能拿这一步的成功去显示「已开通」。
//
// # 请求体里没有用户 id，也没有金额
//
// 用户是令牌里的那个人，金额与时长全部取自套餐（见 dto.CreateSubscriptionRequest）。
func (c *MiniappMembershipController) Subscribe(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body dto.CreateSubscriptionRequest
	if !decodeBody(w, r, &body) {
		return
	}
	result, err := c.membership.CreateSubscription(r.Context(), userID, body, traceID(r))
	if err != nil {
		writeMembershipError(w, r, err, "failed to create a membership subscription")
		return
	}
	api.Success(w, result)
}

// ConfirmSubscription 是用户从微信回来之后的「签成了没有」。
//
// 客户端回来说「我点过同意了」什么都证明不了，那一步的结果只在渠道那边——所以这里回渠道核
// 一次（见 service.ConfirmSubscription）。**渠道说还等着就不改本地状态**，把当前这一条原样
// 回给客户端，让他过一会儿再来。
//
// 别人的订阅回 404：归属在服务层判（按令牌里的 user_id 认领）。
func (c *MiniappMembershipController) ConfirmSubscription(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUser(w, r)
	if !ok {
		return
	}
	var body dto.ConfirmSubscriptionRequest
	if !decodeBody(w, r, &body) {
		return
	}
	subscription, err := c.membership.ConfirmSubscription(r.Context(), body.SubscriptionID, userID, requestID(r), traceID(r))
	if err != nil {
		writeMembershipError(w, r, err, "failed to confirm a membership subscription")
		return
	}
	api.Success(w, subscription)
}

// miniappMembership 映射用户自己看到的那份会员（见 dto.MiniappMembership 的说明）。
func miniappMembership(membership *model.Membership, now time.Time) *dto.MiniappMembership {
	return &dto.MiniappMembership{
		ID:       membership.ID,
		PlanName: membership.PlanName,

		MemberPriceMode: membership.MemberPriceMode,
		Status:          membership.Status,
		StartAt:         membership.StartAt,
		ExpireAt:        membership.ExpireAt,
		Active:          membership.IsUsable(now),

		AutoRenew:                   membership.AutoRenew,
		MemberPriceCouponsPerPeriod: derefInt32(membership.MemberPriceCouponsPerPeriod),
	}
}
