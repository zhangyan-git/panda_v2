package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// ============================================================
// 店铺码会员活动（配置侧）
// ============================================================
//
// 一场活动 = 一个门店 + 一款连续包月套餐 + 送多少天。用户扫门店里那张小程序码，带上来一串
// scene，本服务靠它找回活动、发一次会员天数。
//
// # 它送会员，也送券（券由券服务发）
//
// 老系统的领取是「会员天数 + 券」两件事一起发，V2 现在也是：会员天数本服务当场发，券走
// `membership.campaign.claimed` 事件交 coupon-service 发。**券的两个字段只是承诺**——配了
// 什么就发什么，实际发出去几张的账在券库（`user_coupons.campaign_claim_id`），本服务不写也
// 查不到（跨库）。模板 id 是个裸 UUID，本服务连它存不存在都不知道，配错了要等发券那一刻由
// 券服务记一条日志跳过；详见 migrations/membership。
//
// # 不生成小程序码
//
// `qr_code_url` 今天是空的，生成它要新增一条跨服务调用（微信的 wxacode.getUnlimited 只能由
// 拿着 appid/secret 的 user-service 发），而且码的唯一用途是被扫、扫码入口在小程序端。两件事
// 一起做才有意义，所以一起等。

// campaignParams 校验一份活动输入，翻成写库的输入。
//
// 与 planParams 同一条分工：dto 是 HTTP 契约，这里是「什么样的一份活动才算配全了」。新建与修改
// 共用它——两者的差别只在「这条记录之前存在吗」。
func (s *MembershipService) campaignParams(ctx context.Context, req dto.CampaignRequest) (repository.CampaignParams, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return repository.CampaignParams{}, ErrCampaignNameRequired
	}
	// 按**字符**算（中文一个字符三字节），与 MaxReasonLength 同一条规矩。
	if len([]rune(name)) > MaxCampaignNameLength {
		return repository.CampaignParams{}, ErrCampaignNameTooLong
	}

	scene := strings.TrimSpace(req.Scene)
	if scene == "" {
		return repository.CampaignParams{}, ErrCampaignSceneRequired
	}
	// 两条一起判、回同一个错误：格式与长度对运营是同一句话「这个码串不能用」。分开报的话，
	// 一个 33 字节的 scene 会得到「格式不对」，而它的格式明明是对的。
	if !campaignScenePattern.MatchString(scene) || len(scene) > maxCampaignSceneBytes {
		return repository.CampaignParams{}, ErrCampaignSceneInvalid
	}

	// 门店必填：活动门店同时是领到的会员的归属门店，没有它这条活动就没有意义（归属规则第三条
	// 点名了两个变更归属的渠道，这是其中之一）。存在性要走一次商户域，与后台开通同一条路。
	storeID, err := requiredID(req.StoreID, ErrStoreIDRequired, ErrStoreIDInvalid)
	if err != nil {
		return repository.CampaignParams{}, err
	}
	if err := s.checkStore(ctx, storeID); err != nil {
		return repository.CampaignParams{}, err
	}

	planID, err := requiredID(req.PlanID, ErrPlanIDRequired, ErrPlanIDInvalid)
	if err != nil {
		return repository.CampaignParams{}, err
	}

	if req.GiftDays <= 0 || req.GiftDays > MaxCampaignGiftDays {
		return repository.CampaignParams{}, ErrCampaignGiftDaysRange
	}

	// 券：要么都填、要么都不填。**模板 id 只校形状**——它在券库，本服务查不到它存不存在
	// （跨库没有外键，也没有同步查询），配错模板要等发券那一刻由券服务记日志跳过。
	couponTemplateID, couponCount, err := campaignCoupon(req.CouponTemplateID, req.CouponCount)
	if err != nil {
		return repository.CampaignParams{}, err
	}

	startAt, endAt := req.StartAt.UTC(), req.EndAt.UTC()
	if !endAt.After(startAt) {
		return repository.CampaignParams{}, ErrCampaignWindowInvalid
	}

	// 套餐现查：门槛（必须是连续包月）与「这个套餐还在不在」都靠它。
	//
	// 这里**不像后台开通那样要求套餐在售**：活动是提前配的，而运营常常在活动开跑前才把套餐上架
	// ——卡在在售上会让「先配活动、再上架套餐」这个正常顺序做不了。真正不能松的是连续包月：
	// 送出去的**会员天数**得落在一个可续期的套餐上，而 coupon 模式的套餐靠券给会员价、根本没有
	// 连续包月这一层，挑它做活动等于承诺了一个落不了地的东西。
	plan, err := s.repository.GetPlan(ctx, planID)
	if err != nil {
		return repository.CampaignParams{}, err
	}
	if !plan.AutoRenew {
		return repository.CampaignParams{}, ErrCampaignPlanNotSubscription
	}

	return repository.CampaignParams{
		Name:     name,
		Scene:    scene,
		StoreID:  storeID,
		PlanID:   plan.ID,
		GiftDays: req.GiftDays,
		StartAt:  startAt,
		EndAt:    endAt,
		// 不送券时两列都是 nil（不是空串 / 0）：库上那两列可空，而 NULL 是这里唯一说得清的
		// 「这场活动不送券」——张数为 0 是配不出来的值（CHECK 要求 > 0）。
		CouponTemplateID: couponTemplateID,
		CouponCount:      couponCount,
		// 新建一律 draft。修改那条路上这个字段不参与（见仓储：UpdateCampaign 不写 status）。
		Status: model.CampaignStatusDraft,
	}, nil
}

// CreateCampaign 新建一场活动（一律 draft）。
func (s *MembershipService) CreateCampaign(ctx context.Context, req dto.CampaignRequest, actor Actor) (*dto.CampaignResponse, error) {
	params, err := s.campaignParams(ctx, req)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.CreateCampaign(ctx, params, actor.AdminID)
	if err != nil {
		return nil, err
	}
	return s.campaignResponse(ctx, row), nil
}

// UpdateCampaign 改一场活动。**不改状态**（启停走 SetCampaignStatus）。
//
// 「enabled 时 scene / store_id 改不动」那条判据在仓储里（它锁着行判，见那里的说明），这里不为
// 它多读一次——两次读之间正好是「刚被别的人启用」的窗口。
func (s *MembershipService) UpdateCampaign(ctx context.Context, id string, req dto.CampaignRequest, actor Actor) (*dto.CampaignResponse, error) {
	campaignID, err := campaignID(id)
	if err != nil {
		return nil, err
	}
	params, err := s.campaignParams(ctx, req)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.UpdateCampaign(ctx, campaignID, params, actor.AdminID)
	if err != nil {
		return nil, err
	}
	return s.campaignResponse(ctx, row), nil
}

// SetCampaignStatus 启停一场活动（draft / enabled / disabled）。
func (s *MembershipService) SetCampaignStatus(ctx context.Context, id, status string, actor Actor) (*dto.CampaignResponse, error) {
	campaignID, err := campaignID(id)
	if err != nil {
		return nil, err
	}
	parsed, err := parseCampaignStatus(status)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.SetCampaignStatus(ctx, campaignID, parsed, actor.AdminID)
	if err != nil {
		return nil, err
	}
	return s.campaignResponse(ctx, row), nil
}

// GetCampaign 取一场活动（详情）。
func (s *MembershipService) GetCampaign(ctx context.Context, id string) (*dto.CampaignResponse, error) {
	campaignID, err := campaignID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.repository.GetCampaign(ctx, campaignID)
	if err != nil {
		return nil, err
	}
	return s.campaignResponse(ctx, row), nil
}

// ListCampaigns 是后台活动列表。
func (s *MembershipService) ListCampaigns(ctx context.Context, q dto.CampaignQuery) ([]*dto.CampaignResponse, int, error) {
	if err := normalizeCampaignQuery(&q); err != nil {
		return nil, 0, err
	}
	rows, total, err := s.repository.ListCampaigns(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	// 门店名一次解一页（不是一行一次），与会员列表同一条规矩。
	names := s.StoreNames(ctx, campaignStoreIDs(rows))
	responses := make([]*dto.CampaignResponse, 0, len(rows))
	for _, row := range rows {
		responses = append(responses, campaignResponse(row, names[row.StoreID]))
	}
	return responses, total, nil
}

// ListCampaignClaims 是一场活动的领取记录（详情页那个 tab）。
//
// **它会是空的**：领取动作要小程序端扫门店里那张码，而小程序端这一轮不写。空态不是故障。
func (s *MembershipService) ListCampaignClaims(ctx context.Context, id string, q dto.CampaignClaimQuery) ([]*dto.CampaignClaimResponse, int, error) {
	campaignID, err := campaignID(id)
	if err != nil {
		return nil, 0, err
	}
	// 活动本身要先在：否则一场不存在的活动的领取记录永远是「空的」，而那与「这场活动还没人领」
	// 在页面上长得一模一样。
	if _, err := s.repository.GetCampaign(ctx, campaignID); err != nil {
		return nil, 0, err
	}
	if q.Page < 1 {
		q.Page = 1
	}
	switch {
	case q.PageSize <= 0:
		q.PageSize = dto.DefaultPageSize
	case q.PageSize > MaxPageSize:
		q.PageSize = MaxPageSize
	}
	claims, total, err := s.repository.ListCampaignClaims(ctx, campaignID, q)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]*dto.CampaignClaimResponse, 0, len(claims))
	for _, claim := range claims {
		responses = append(responses, claimResponse(claim))
	}
	return responses, total, nil
}

// campaignCoupon 校验活动上那对券字段，翻成写库用的两个可空值。
//
// 空串 + 0 = 不送券（两列都 nil）。**只有一半**是配不出来的组合：只填模板不填张数，等于
// 「该发券」静默变成「什么都不发」，而用户在小程序上看到的活动写着送券——库上那条 CHECK
// 也是这么钉的，这里先把它翻成一句人看得懂的话。
//
// 模板 id 只校形状（是不是 uuid）。**它在券库**：本服务查不出它存不存在，也查不出它是不是
// 平台核销的券。这两件事只有发券那一刻的券服务知道，配错了它会记一条日志跳过（老系统同一
// 处理方式）。这里能挡住的是「手滑打了一串不是 id 的东西」。
func campaignCoupon(templateID string, count int32) (*string, *int32, error) {
	trimmed := strings.TrimSpace(templateID)
	if trimmed == "" && count == 0 {
		return nil, nil, nil
	}
	if trimmed == "" || count == 0 {
		return nil, nil, ErrCampaignCouponIncomplete
	}
	parsed, err := optionalUUID(trimmed, ErrCampaignCouponInvalid)
	if err != nil {
		return nil, nil, err
	}
	if parsed == "" || count < 0 || count > MaxCampaignCouponCount {
		return nil, nil, ErrCampaignCouponInvalid
	}
	return &parsed, &count, nil
}

// ClaimCampaign 是用户扫门店码领会员（小程序端）。
//
// 校验只有一条：scene 得是非空的。它的格式、活动在不在、此刻能不能领、这个人领过没有——全部
// 由仓储在那个事务里判（活动行锁着，判据与发放必须是同一个瞬间的事）。**这里不预先查一次
// 活动**：查了也只是给上面那几条判据多一个过时版本。
//
// actor 只有 TraceID 有值：操作人是**用户本人**，没有后台 id，也不记平台审计（见仓储）。
func (s *MembershipService) ClaimCampaign(ctx context.Context, req dto.CampaignClaimRequest, userID, traceID string) (*dto.CampaignClaimResult, error) {
	scene := strings.TrimSpace(req.Scene)
	if scene == "" {
		return nil, ErrCampaignSceneRequired
	}
	outcome, err := s.repository.ClaimCampaign(ctx, repository.ClaimParams{
		Scene:  scene,
		UserID: userID,
		// 领的那一刻取服务时钟：活动窗口、会员 start_at、赠送天数的起点都该是它。与到期扫描
		// 同一个时钟，测试才造得出「窗口已经过了」而不必等。
		OccurredAt: s.Now(),
		TraceID:    traceID,
	})
	if err != nil {
		return nil, err
	}
	_, couponCount := campaignCouponFields(outcome.Claim.CouponTemplateID, outcome.Claim.CouponCount)
	return &dto.CampaignClaimResult{
		MembershipExpireAt: outcome.Claim.MembershipExpireAt,
		PlanName:           outcome.PlanName,
		GiftDays:           outcome.Claim.GiftDays,
		// 券张数取自**领取记录上的快照**：重放时它也是第一次那个值。
		CouponCount:    couponCount,
		AlreadyClaimed: outcome.Replayed,
	}, nil
}

// ============================================================
// 形状转换
// ============================================================

// campaignResponse 把一行翻成后台形状。
//
// storeName 由调用方解好传进来（列表一次解一页、详情一次解一条），解不出来就是空串——见
// StoreNames 上那段说明，名字只用来显示。
func (s *MembershipService) campaignResponse(ctx context.Context, row *repository.CampaignRow) *dto.CampaignResponse {
	names := s.StoreNames(ctx, []string{row.StoreID})
	return campaignResponse(row, names[row.StoreID])
}

func campaignResponse(row *repository.CampaignRow, storeName string) *dto.CampaignResponse {
	couponTemplateID, couponCount := campaignCouponFields(row.CouponTemplateID, row.CouponCount)
	return &dto.CampaignResponse{
		ID:                row.ID,
		Name:              row.Name,
		Scene:             row.Scene,
		StoreID:           row.StoreID,
		StoreName:         storeName,
		PlanID:            row.PlanID,
		PlanName:          row.PlanName,
		GiftDays:          row.GiftDays,
		StartAt:           row.StartAt,
		EndAt:             row.EndAt,
		Status:            row.Status,
		CouponTemplateID:  couponTemplateID,
		CouponCount:       couponCount,
		QRCodeURL:         row.QRCodeURL,
		QRCodeGeneratedAt: row.QRCodeGeneratedAt,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}
}

func claimResponse(claim *model.CampaignClaim) *dto.CampaignClaimResponse {
	couponTemplateID, couponCount := campaignCouponFields(claim.CouponTemplateID, claim.CouponCount)
	return &dto.CampaignClaimResponse{
		ID:                 claim.ID,
		CampaignID:         claim.CampaignID,
		UserID:             claim.UserID,
		StoreID:            claim.StoreID,
		GiftDays:           claim.GiftDays,
		CouponTemplateID:   couponTemplateID,
		CouponCount:        couponCount,
		MembershipID:       claim.MembershipID,
		MembershipExpireAt: claim.MembershipExpireAt,
		CreatedAt:          claim.CreatedAt,
	}
}

// campaignCouponFields 把库里那两个可空列翻成对外的零值形状。
//
// NULL → 空串 / 0，也就是「这场活动不送券」。与 store_id 那条「空串 = 没有归属门店」是同一条
// 规矩：可空列在模型上是 *T（NULL 装得下），但对外的 JSON 用零值，免得每个读它的前端都要先
// 判一次 null。**只有一处做这次转换**，别在页面里再判一遍。
func campaignCouponFields(templateID *string, count *int32) (string, int32) {
	var (
		id     string
		amount int32
	)
	if templateID != nil {
		id = *templateID
	}
	if count != nil {
		amount = *count
	}
	return id, amount
}

// campaignStoreIDs 收一页活动的门店 id（去重、丢掉空的），供一次 StoreNames 用。
func campaignStoreIDs(rows []*repository.CampaignRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.StoreID != "" {
			ids = append(ids, row.StoreID)
		}
	}
	return ids
}

// ============================================================
// 小工具
// ============================================================

// campaignID 校验路径上那半个 id。
//
// 与 subscriptionID 同一条写法：**不合法与不存在回同一句话**（404）。路径上那一段是用户手改
// URL 改出来的东西，对它说「这不是一个 uuid」等于告诉攻击者这里有一张 uuid 主键的表，而对正常
// 用户没有任何帮助。
func campaignID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if _, err := uuid.Parse(trimmed); err != nil {
		return "", ErrCampaignNotFound
	}
	return trimmed, nil
}

func parseCampaignStatus(status string) (string, error) {
	trimmed := strings.TrimSpace(status)
	switch trimmed {
	case model.CampaignStatusDraft, model.CampaignStatusEnabled, model.CampaignStatusDisabled:
		return trimmed, nil
	default:
		// 空串也落这里：状态没有默认值（不是「不传就当 draft」），启停是一个明确的动作。
		return "", ErrCampaignStatusInvalid
	}
}

// normalizeCampaignQuery 校一遍筛选条件并去掉空白。
//
// 状态**要校**（与订阅列表同一条理由）：它是下拉选的，一个不在三个码里的值只可能来自手改 URL，
// 而返回空列表看起来像「没有这类活动」——空列表在这里太像一句真话了。
func normalizeCampaignQuery(q *dto.CampaignQuery) error {
	q.Keyword = strings.TrimSpace(q.Keyword)
	q.Status = strings.TrimSpace(q.Status)
	if q.Status != "" {
		if _, err := parseCampaignStatus(q.Status); err != nil {
			return err
		}
	}
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
