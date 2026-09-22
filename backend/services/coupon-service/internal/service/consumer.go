package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/coupon-service/internal/repository"
)

// 本服务发**系统券**（不是后台人工发放）时用的幂等 scope。
//
// 一个业务凭据一个 scope：两边各自的键长什么样互不相干（会员那边是「会员 id + 订单号」，活动
// 那边是领取记录 id），混在一个 scope 里迟早会撞出一个同名的键、然后被判成 IDEMPOTENCY_CONFLICT。
const (
	scopeMemberPriceCoupon = "membership.member_price_coupon"
	scopeCampaignCoupon    = "membership.campaign_coupon"
)

// memberPriceModeCoupon 是会员价路径「靠券」的那个取值，逐字对应 membership_plans
// .member_price_mode 上的 CHECK（auto / coupon）与事件里的同名字段。
const memberPriceModeCoupon = "coupon"

// 老系统那两句发放原因，逐字照抄（`subscription_service.go:1232` 与
// `store_membership_campaign_service.go:335`）。它进 user_coupons.issue_reason，是用户在券上
// 看到的那句话，也是客服查「这张券哪来的」时唯一的线索，所以不重新起名。
const (
	reasonMemberPriceCoupon = "包月续费赠送"
	reasonCampaignCoupon    = "门店会员活动"
)

// GrantRepository 是系统发券的仓储面。
type GrantRepository interface {
	GrantSystemCoupons(context.Context, repository.GrantParams) (*dto.IssueCouponsResponse, error)
}

// HandleEvent 消费会员域的事件。它是 runtime.Options.ConsumerHandler 的实现，返回值决定这条
// 消息的归宿：nil 是 ack，非 nil 是「没处理成功」，由平台重投或送死信。
//
// # 本服务只认三种事件，都是「该发券了」
//
//   - `membership.activated` / `membership.renewed`：这个人买了（或又买了一期）coupon 模式的
//     会员套餐，该发一批会员价券。
//   - `membership.campaign.claimed`：这个人扫门店码领了会员，活动上配了券。
//
// 其余一律 ack：会员主题上还有到期、撤销这些事件（本服务订阅与否取决于 RABBITMQ_ROUTING_KEY，
// 但订阅了也不该为它们做事）。**不认识 ≠ 出错**——把不认识的事件当错误，一条别的域新增的事件
// 就能把这里的队列堵死。
func (s *CouponService) HandleEvent(ctx context.Context, event messaging.Envelope) error {
	switch event.EventType {
	case dto.EventMembershipActivated, dto.EventMembershipRenewed:
		return s.handleMembershipCoupons(ctx, event)
	case dto.EventMembershipCampaignClaimed:
		return s.handleCampaignCoupons(ctx, event)
	default:
		return nil
	}
}

// newDecoder 是本服务的解码器，与 membership-service / account-service 同一套写法。
//
// 多发一个字段就报错：契约漂移要在联调时炸出来，而不是被静默忽略——被忽略的那次漂移可能正是
// 会员域改了字段名，而我们还按旧名字在发券。**这条规则是镜像结构体上那些「本服务不读但必须
// 镜像」的字段存在的全部理由**：少镜像一个，整条消息就解不开。
func newDecoder(event messaging.Envelope) *json.Decoder {
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	return decoder
}

// ErrInvalidEvent 表示事件体本身不合法（缺字段、id 不是 uuid）。
//
// 它**要报错**（而不是 ack）：重投不会让它变合法，那是发出方坏了，得有人看。假装成功 ack 掉，
// 用户就是「付了钱/扫了码却没有券」。
var ErrInvalidEvent = errors.New("invalid coupon grant event")

// handleMembershipCoupons 是「买了 coupon 模式的会员套餐」⇒ 发一批会员价券。
//
// # 三条判据，缺一条都不发
//
//  1. **MemberPriceMode == coupon**。auto 模式的套餐是会员自动享会员价，不发券（库上那条
//     CHECK 已经把两列与模式钉在一起，这里读的是成交快照）。
//  2. **OrderID 非空**。开通与续费这两条事件在**后台人工调整有效期**时也会发（那条路上
//     orderId 传的是空串）。没有这一条，后台每改一次到期日就白送一批券。
//  3. 模板与张数齐全。快照上这两列为空而模式是 coupon 是不可能的（库上 CHECK），真出现
//     说明事件被人手改过——记一条日志 ack，不当成故障。
//
// # 发多少、发几张
//
// 每期（一次成功扣款）发 memberPriceCouponsPerPeriod 张，模板是
// memberPriceCouponTemplateId。老系统把张数写死成常量 20，V2 是套餐上的可配置列——这正是
// memberships 上那两列快照存在的理由。
//
// # 幂等键 = 会员 id + 订单号
//
// 它同时满足两件事：重投时命中上一次的批次（不再发一遍），而同一个人的第二笔订单是另一批券。
// **不用随机键**：随机键在重投时必然开出第二批券，那是白送。
func (s *CouponService) handleMembershipCoupons(ctx context.Context, event messaging.Envelope) error {
	var payload dto.MembershipChangedEvent
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	membershipID := strings.TrimSpace(payload.MembershipID)
	if _, err := uuid.Parse(membershipID); err != nil {
		return fmt.Errorf("%w: membershipId %q", ErrInvalidEvent, payload.MembershipID)
	}
	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: userId %q", ErrInvalidEvent, payload.UserID)
	}
	if payload.MemberPriceMode != memberPriceModeCoupon {
		// auto 模式：会员本人自动享会员价，没有券这回事。这是绝大多数会员（年度会员）。
		return nil
	}
	orderID := strings.TrimSpace(payload.OrderID)
	if orderID == "" {
		// 后台人工改的有效期。见上面判据 2：它**不是**一次成交。
		return nil
	}
	if _, err := uuid.Parse(orderID); err != nil {
		return fmt.Errorf("%w: orderId %q", ErrInvalidEvent, payload.OrderID)
	}
	templateID := strings.TrimSpace(payload.MemberPriceCouponTemplateID)
	quantity := int(payload.MemberPriceCouponsPerPeriod)
	if templateID == "" || quantity <= 0 {
		slog.WarnContext(ctx, "membership event asks for member price coupons but carries no template or count",
			"membership", membershipID, "order", orderID, "event", event.EventID)
		return nil
	}
	return s.grant(ctx, repository.GrantParams{
		Scope:           scopeMemberPriceCoupon,
		Key:             membershipID + ":" + orderID,
		Hash:            grantHash(templateID, userID, quantity, reasonMemberPriceCoupon),
		TemplateID:      templateID,
		UserIDs:         []string{userID},
		QuantityPerUser: quantity,
		Reason:          reasonMemberPriceCoupon,
		// 购买：这批券是随一笔支付产生的。coupon_batches.source 与 user_coupons.claim_type
		// 两张 CHECK 的取值表里都有 purchase。
		ClaimType:     "purchase",
		Source:        "purchase",
		ReferenceType: "membership_member_price",
		// 库存流水指向那笔订单：对账时「这批券是哪一单发的」是第一个要答的问题。
		ReferenceID: orderID,
		BatchPrefix: "member-",
		EventType:   "coupon.issued",
	})
}

// handleCampaignCoupons 是「扫码领了店铺码活动的会员」⇒ 发活动配的那批券。
//
// # 券与会员是两笔账，靠这条事件连起来
//
// 会员域发会员天数、发完就结束了，它**不替券记账**（本库的 user_coupons 是券唯一的账本）。
// 领取记录上因此只存「承诺发几张」的快照，实际发了几张去 user_coupons 里按
// campaign_claim_id 数——两个数字分属两个域，各记各的。
//
// # 幂等键 = 领取记录 id
//
// 一个人一场活动只能领一次（会员域那条唯一索引），所以领取记录 id 天然是一次领取的凭据。
// 重投时命中上一次的批次，不会发第二遍。
//
// # 券的门店范围强制取活动门店
//
// 老系统同一口径（`grant()` 里 `coupon.StoreIDs = []mongodb.ObjectID{claim.StoreID}`）：这批券
// **只能在这家店用**。理由与老系统一致——活动是门店的营销预算，一张平台通用券被一家店的活动
// 发出去，等于让别的店替它承担成本。所以这里不抄模板自己的适用范围，整份换成这一家店。
func (s *CouponService) handleCampaignCoupons(ctx context.Context, event messaging.Envelope) error {
	var payload dto.MembershipCampaignClaimedEvent
	if err := newDecoder(event).Decode(&payload); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	claimID := strings.TrimSpace(payload.ClaimID)
	if _, err := uuid.Parse(claimID); err != nil {
		return fmt.Errorf("%w: claimId %q", ErrInvalidEvent, payload.ClaimID)
	}
	userID := strings.TrimSpace(payload.UserID)
	if _, err := uuid.Parse(userID); err != nil {
		return fmt.Errorf("%w: userId %q", ErrInvalidEvent, payload.UserID)
	}
	templateID := strings.TrimSpace(payload.CouponTemplateID)
	quantity := int(payload.CouponCount)
	if templateID == "" || quantity <= 0 {
		// 这场活动只送会员天数，没配券。**这是常态**（券那一栏本来就是可选的），不是故障。
		return nil
	}
	storeID := strings.TrimSpace(payload.StoreID)
	if storeID != "" {
		if _, err := uuid.Parse(storeID); err != nil {
			return fmt.Errorf("%w: storeId %q", ErrInvalidEvent, payload.StoreID)
		}
	}
	return s.grant(ctx, repository.GrantParams{
		Scope:           scopeCampaignCoupon,
		Key:             claimID,
		Hash:            grantHash(templateID, userID, quantity, reasonCampaignCoupon),
		TemplateID:      templateID,
		UserIDs:         []string{userID},
		QuantityPerUser: quantity,
		Reason:          reasonCampaignCoupon,
		// 活动奖励：coupon_batches.source 里是 event，user_coupons.claim_type 里是 event_reward。
		ClaimType:     "event_reward",
		Source:        "event",
		ReferenceType: "membership_campaign_claim",
		ReferenceID:   claimID,
		// 券与那条领取记录对得上（user_coupons.campaign_claim_id，老系统同一列同一用途）。
		CampaignClaimID: claimID,
		StoreScopeID:    storeID,
		BatchPrefix:     "campaign-",
		EventType:       "coupon.issued",
	})
}

// grant 执行一次系统发券，并把「配错了」与「暂时发不出去」分开。
//
//	ErrTemplateUnavailable  → 记一条日志，**ack**。模板被停用/删掉/还没审过，重投多少次都还是
//	                          这个结果；一直重投只会把队列堵到死信，而真正要看的那条日志早就
//	                          被淹了。
//	其余错误（库存不够、数据库抖动）→ 原样返回，让平台重投。库存是可以补的，抖动是会好的。
//
// **ack 不等于没发生过**：那条 slog.Error 是这里唯一的线索，所以它带齐了模板、用户与业务
// 凭据——运营拿着这三个值就能把券手工补上（后台的发放接口就是干这个的）。
func (s *CouponService) grant(ctx context.Context, params repository.GrantParams) error {
	granter, ok := s.batches.(GrantRepository)
	if !ok {
		return errors.New("coupon grant repository is not configured")
	}
	result, err := granter.GrantSystemCoupons(ctx, params)
	if err != nil {
		if errors.Is(err, repository.ErrTemplateUnavailable) {
			slog.ErrorContext(ctx, "coupon template is not available; the coupons of this event were not issued",
				"scope", params.Scope, "key", params.Key, "template", params.TemplateID,
				"users", params.UserIDs, "quantity", params.QuantityPerUser)
			return nil
		}
		return err
	}
	slog.InfoContext(ctx, "system coupons issued",
		"scope", params.Scope, "key", params.Key, "batch", result.BatchID,
		"issued", result.IssuedQuantity)
	return nil
}

// grantHash 是系统发券的请求体摘要。
//
// 它进 coupon_idempotency_keys.request_hash，作用是**让「同一个键、不同的券」现形**：如果运营
// 把活动的券模板换了，重投回来的那条消息还会用同一个幂等键，但摘要对不上——仓储会判成
// IDEMPOTENCY_CONFLICT 并报错，而不是静默地把旧批次当成答案返回。这正是这一列存在的意义。
//
// 入参只放**决定发什么券**的那几个值：用户、模板、张数、原因。带上业务 id 没有意义——幂等键
// 本身已经是那个 id 了。
func grantHash(templateID, userID string, quantity int, reason string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", templateID, userID, quantity, reason)))
	return hex.EncodeToString(sum[:])
}
