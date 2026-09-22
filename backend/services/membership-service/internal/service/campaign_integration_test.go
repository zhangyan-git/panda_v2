package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
)

// 店铺码活动的集成测试。门禁、真库、时钟与收摊那一套都在 membershipFixture 里（见
// membership_integration_test.go 的开头），这里只补活动自己的三个小工具与用例。

// campaignFixture 给用例发几样活动专用的东西：门店 id、scene。
//
// 门店 id 是**现造的 uuid**：本服务不认识商户库，checkStore 在没装配商户客户端时直接放行
// （单测路径）。这里要验的是「这个值原样落到 memberships.store_id 上」，不是商户域的校验。
type campaignFixture struct {
	*membershipFixture
	store string
}

func newCampaignFixture(t *testing.T) *campaignFixture {
	t.Helper()
	return &campaignFixture{membershipFixture: newMembershipFixture(t), store: uuid.NewString()}
}

// scene 造一个本用例独占的码参数。前缀 `smc_` 是格式要求，后面挂 f.code ——cleanup 靠这个
// 前缀认出哪些活动是这个用例留下的（见 membershipFixture.cleanup 里删活动那一步）。
func (f *campaignFixture) scene(suffix string) string {
	return "smc_" + f.code + "_" + suffix
}

// create 建一场活动，失败即终止。
func (f *campaignFixture) create(plan *model.Plan, scene string, giftDays int32) *dto.CampaignResponse {
	f.t.Helper()

	campaign, err := f.svc.CreateCampaign(context.Background(), dto.CampaignRequest{
		Name:     "集成测试活动",
		Scene:    scene,
		StoreID:  f.store,
		PlanID:   plan.ID,
		GiftDays: giftDays,
		StartAt:  f.clock.Add(-time.Hour),
		EndAt:    f.clock.Add(time.Hour),
	}, Actor{AdminID: uuid.NewString(), TraceID: "it"})
	if err != nil {
		f.t.Fatalf("建活动失败：%v", err)
	}
	return campaign
}

// enable 把活动切到 enabled。
func (f *campaignFixture) enable(id string) *dto.CampaignResponse {
	f.t.Helper()

	campaign, err := f.svc.SetCampaignStatus(context.Background(), id, model.CampaignStatusEnabled, Actor{AdminID: uuid.NewString()})
	if err != nil {
		f.t.Fatalf("启用活动失败：%v", err)
	}
	return campaign
}

// claim 领一次，返回回执。
func (f *campaignFixture) claim(scene string) *dto.CampaignClaimResult {
	f.t.Helper()

	result, err := f.svc.ClaimCampaign(context.Background(),
		dto.CampaignClaimRequest{Scene: scene}, f.user, "it")
	if err != nil {
		f.t.Fatalf("领取失败：%v", err)
	}
	return result
}

// TestCampaignCRUDAndStatus 走一遍配置侧：新建是 draft、改、启停、读回带套餐名。
func TestCampaignCRUDAndStatus(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	scene := f.scene("crud")

	campaign := f.create(plan, scene, 7)
	// **新建一律 draft**：一个还没配完的活动被扫到，用户领到的是半成品，而页面上看不出哪里不对。
	if campaign.Status != model.CampaignStatusDraft {
		t.Fatalf("新建的活动应当是 draft，实际是 %q", campaign.Status)
	}
	if campaign.PlanName == "" {
		t.Errorf("活动要带套餐名（join 出来的），实际是空的")
	}
	if campaign.StoreID != f.store {
		t.Errorf("活动门店应当是 %s，实际是 %s", f.store, campaign.StoreID)
	}

	// 改：天数、名称。**不改状态**（那是 SetCampaignStatus 的活）。
	updated, err := f.svc.UpdateCampaign(ctx, campaign.ID, dto.CampaignRequest{
		Name:     "集成测试活动（改）",
		Scene:    scene,
		StoreID:  f.store,
		PlanID:   plan.ID,
		GiftDays: 30,
		StartAt:  f.clock.Add(-time.Hour),
		EndAt:    f.clock.Add(time.Hour),
	}, Actor{AdminID: uuid.NewString()})
	if err != nil {
		t.Fatalf("改活动失败：%v", err)
	}
	if updated.GiftDays != 30 {
		t.Errorf("改完之后的天数应当是 30，实际是 %d", updated.GiftDays)
	}
	if updated.Status != model.CampaignStatusDraft {
		t.Errorf("改活动不该动状态，实际变成了 %q", updated.Status)
	}

	enabled := f.enable(campaign.ID)
	if enabled.Status != model.CampaignStatusEnabled {
		t.Fatalf("启用后应当是 enabled，实际是 %q", enabled.Status)
	}

	// 列表：按 scene 关键字能搜到它（keyword 走 name/scene 的 ILIKE）。
	list, total, err := f.svc.ListCampaigns(ctx, dto.CampaignQuery{Keyword: scene, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("列表失败：%v", err)
	}
	if total != 1 || len(list) != 1 || list[0].ID != campaign.ID {
		t.Fatalf("按 scene 搜应当只搜到这一场，实际 total=%d len=%d", total, len(list))
	}
	// 状态筛：它现在是 enabled，按 draft 筛就不该出现。
	drafts, draftTotal, err := f.svc.ListCampaigns(ctx, dto.CampaignQuery{Status: model.CampaignStatusDraft, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("按状态筛失败：%v", err)
	}
	for _, row := range drafts {
		if row.ID == campaign.ID {
			t.Errorf("这场活动已经是 enabled，不该出现在 draft 的筛选结果里（共 %d 条）", draftTotal)
		}
	}

	// 领取记录：**今天一定是空的**（领取动作在小程序端，那一端还没接）。空列表不是错误。
	claims, claimTotal, err := f.svc.ListCampaignClaims(ctx, campaign.ID, dto.CampaignClaimQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("读领取记录失败：%v", err)
	}
	if claimTotal != 0 || len(claims) != 0 {
		t.Errorf("还没人领过，领取记录应当是空的，实际 total=%d", claimTotal)
	}

	// 读一条不存在的活动 → 404 那一组，不是 500。
	if _, err := f.svc.GetCampaign(ctx, uuid.NewString()); !errors.Is(err, ErrCampaignNotFound) {
		t.Errorf("取一场不存在的活动应当是 ErrCampaignNotFound，实际是 %v", err)
	}
}

// TestCampaignValidation 是几条会在运营手上发生、又必须被挡在保存之前的输入。
func TestCampaignValidation(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	autoPlan := f.createPlan(autoPlanRequest())
	// 券模式的套餐不能拿来做活动：送出去的会员天数在它身上给不了会员价。
	couponPlan := f.createSuffixedPlan(couponPlanRequest(), "_c")

	base := func() dto.CampaignRequest {
		return dto.CampaignRequest{
			Name:     "集成测试活动",
			Scene:    f.scene("valid"),
			StoreID:  f.store,
			PlanID:   autoPlan.ID,
			GiftDays: 7,
			StartAt:  f.clock.Add(-time.Hour),
			EndAt:    f.clock.Add(time.Hour),
		}
	}

	t.Run("券模式套餐被拒", func(t *testing.T) {
		req := base()
		req.PlanID = couponPlan.ID
		if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignPlanNotSubscription) {
			t.Errorf("auto_renew=false 的套餐应当被拒，实际是 %v", err)
		}
	})

	t.Run("scene 格式", func(t *testing.T) {
		for _, scene := range []string{"campaign", "smc_", "smc_中文", strings.Repeat("smc_a", 10)} {
			req := base()
			req.Scene = scene
			if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignSceneInvalid) {
				t.Errorf("scene %q 应当被拒，实际是 %v", scene, err)
			}
		}
	})

	t.Run("赠送天数", func(t *testing.T) {
		for _, days := range []int32{0, -1, MaxCampaignGiftDays + 1} {
			req := base()
			req.GiftDays = days
			if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignGiftDaysRange) {
				t.Errorf("送 %d 天应当被拒，实际是 %v", days, err)
			}
		}
	})

	t.Run("时间窗口", func(t *testing.T) {
		req := base()
		req.EndAt = req.StartAt
		if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignWindowInvalid) {
			t.Errorf("结束不晚于开始应当被拒，实际是 %v", err)
		}
	})

	t.Run("门店必填", func(t *testing.T) {
		req := base()
		req.StoreID = ""
		if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrStoreIDRequired) {
			t.Errorf("没选门店应当被拒，实际是 %v", err)
		}
	})

	t.Run("scene 撞车", func(t *testing.T) {
		req := base()
		if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); err != nil {
			t.Fatalf("第一次建活动失败：%v", err)
		}
		// 同一个 scene 再建一场：码上只有 scene，两场活动共用一个就无从判断手里的码是哪一场。
		if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignSceneTaken) {
			t.Errorf("scene 撞车应当是 ErrCampaignSceneTaken，实际是 %v", err)
		}
	})
}

// createWithCoupons 建一场配了券的活动。
//
// 与 create 分开而不是给它加两个参数：绝大多数用例不配券，多两个每次都要传 0 与空串的位置参数
// 只会让调用行变长。
func (f *campaignFixture) createWithCoupons(plan *model.Plan, scene string, templateID string, count int32) *dto.CampaignResponse {
	f.t.Helper()

	campaign, err := f.svc.CreateCampaign(context.Background(), dto.CampaignRequest{
		Name:     "集成测试活动（带券）",
		Scene:    scene,
		StoreID:  f.store,
		PlanID:   plan.ID,
		GiftDays: 7,
		StartAt:  f.clock.Add(-time.Hour),
		EndAt:    f.clock.Add(time.Hour),
		// 模板 id 是跨库值引用（券库的 coupon_templates.id），本服务只校形状。
		CouponTemplateID: templateID,
		CouponCount:      count,
	}, Actor{AdminID: uuid.NewString(), TraceID: "it"})
	if err != nil {
		f.t.Fatalf("建带券活动失败：%v", err)
	}
	return campaign
}

// campaignClaimedPayloads 取本用例留下的领取事件体（原始 JSON）。
//
// 按 user_id 筛：那条事件里没有套餐编码（它说的是领取，不是购买），而 user_id 是这个用例独占的。
func (f *campaignFixture) campaignClaimedPayloads() []string {
	f.t.Helper()

	rows, err := f.pool.Query(context.Background(),
		`SELECT convert_from(payload, 'UTF8') FROM message_outbox
		  WHERE event_type = $1 AND convert_from(payload, 'UTF8') LIKE '%' || $2 || '%'
		  ORDER BY created_at`,
		dto.EventMembershipCampaignClaimed, f.user)
	if err != nil {
		f.t.Fatalf("查领取事件失败：%v", err)
	}
	defer rows.Close()

	var payloads []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatalf("读领取事件失败：%v", err)
		}
		payloads = append(payloads, raw)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("遍历领取事件失败：%v", err)
	}
	return payloads
}

// TestCampaignCouponValidation 钉住活动上那对券字段的三条规矩。
//
// 「要么都填、要么都不填」不是洁癖：只填模板不填张数的话，**该发券会静默变成什么都不发**，
// 而用户在小程序上看到的活动写着送券——那是最难查的一类故障。
func TestCampaignCouponValidation(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	template := uuid.NewString()

	base := func(scene string) dto.CampaignRequest {
		return dto.CampaignRequest{
			Name:     "集成测试活动",
			Scene:    scene,
			StoreID:  f.store,
			PlanID:   plan.ID,
			GiftDays: 7,
			StartAt:  f.clock.Add(-time.Hour),
			EndAt:    f.clock.Add(time.Hour),
		}
	}

	t.Run("只填一半被拒", func(t *testing.T) {
		onlyTemplate := base(f.scene("half1"))
		onlyTemplate.CouponTemplateID = template
		if _, err := f.svc.CreateCampaign(ctx, onlyTemplate, Actor{}); !errors.Is(err, ErrCampaignCouponIncomplete) {
			t.Errorf("只填模板应当被拒，实际是 %v", err)
		}

		onlyCount := base(f.scene("half2"))
		onlyCount.CouponCount = 3
		if _, err := f.svc.CreateCampaign(ctx, onlyCount, Actor{}); !errors.Is(err, ErrCampaignCouponIncomplete) {
			t.Errorf("只填张数应当被拒，实际是 %v", err)
		}
	})

	t.Run("张数为 0 算没填", func(t *testing.T) {
		// 0 与空串一样是**没填**：运营清了张数那一栏却没清模板，报「只填了一半」比报「张数不
		// 合法」更贴着他眼前看到的东西。库上那条 CHECK 同样把 0 排除在合法值之外。
		req := base(f.scene("zero"))
		req.CouponTemplateID = template
		req.CouponCount = 0
		if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignCouponIncomplete) {
			t.Errorf("张数为 0 应当被当成没填，实际是 %v", err)
		}
	})

	t.Run("张数越界与模板不是 uuid", func(t *testing.T) {
		for name, mutate := range map[string]func(*dto.CampaignRequest){
			"负数": func(r *dto.CampaignRequest) { r.CouponTemplateID = template; r.CouponCount = -1 },
			"超过上限": func(r *dto.CampaignRequest) {
				r.CouponTemplateID = template
				r.CouponCount = MaxCampaignCouponCount + 1
			},
			"模板不是 uuid": func(r *dto.CampaignRequest) {
				r.CouponTemplateID = "不是-uuid"
				r.CouponCount = 1
			},
		} {
			req := base(f.scene("bad"))
			mutate(&req)
			if _, err := f.svc.CreateCampaign(ctx, req, Actor{}); !errors.Is(err, ErrCampaignCouponInvalid) {
				t.Errorf("%s 应当被拒，实际是 %v", name, err)
			}
		}
	})

	t.Run("都填了就能存下来", func(t *testing.T) {
		campaign := f.createWithCoupons(plan, f.scene("ok"), template, 3)
		if campaign.CouponTemplateID != template || campaign.CouponCount != 3 {
			t.Errorf("读回来应当是 %s / 3，实际是 %q / %d", template, campaign.CouponTemplateID, campaign.CouponCount)
		}
		// 详情那条路也要带上（它走的是另一个函数）。
		got, err := f.svc.GetCampaign(ctx, campaign.ID)
		if err != nil {
			t.Fatalf("读活动详情失败：%v", err)
		}
		if got.CouponTemplateID != template || got.CouponCount != 3 {
			t.Errorf("详情里的券应当是 %s / 3，实际是 %q / %d", template, got.CouponTemplateID, got.CouponCount)
		}
	})

	t.Run("都不填就是只送会员天数", func(t *testing.T) {
		campaign := f.create(plan, f.scene("none"), 7)
		// 库上那两列是 NULL，对外的形状是零值——**只有一处**做这次转换（见 campaignCouponFields），
		// 页面里不该再判一次 null。
		if campaign.CouponTemplateID != "" || campaign.CouponCount != 0 {
			t.Errorf("没配券应当是空串与 0，实际是 %q / %d", campaign.CouponTemplateID, campaign.CouponCount)
		}
	})
}

// TestClaimCampaignCarriesCouponSnapshot 钉住券那一半的交接：领取记录存快照、事件带快照、
// 回执说「还送了 N 张券」。
//
// **本服务一个字都不写券库**：这里能证明的只是「承诺了什么已经发出去了」，券真的有没有发出来
// 要看 coupon-service 那一侧（它的消费链路由 coupon-service 自己的测试与端到端验证覆盖）。
func TestClaimCampaignCarriesCouponSnapshot(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	template := uuid.NewString()
	scene := f.scene("coupon")
	campaign := f.createWithCoupons(plan, scene, template, 3)
	f.enable(campaign.ID)

	result := f.claim(scene)
	if result.CouponCount != 3 {
		t.Errorf("回执里的券张数应当是 3，实际是 %d", result.CouponCount)
	}

	claims, total, err := f.svc.ListCampaignClaims(ctx, campaign.ID, dto.CampaignClaimQuery{Page: 1, PageSize: 20})
	if err != nil || total != 1 {
		t.Fatalf("读领取记录失败：total=%d err=%v", total, err)
	}
	claim := claims[0]
	if claim.CouponTemplateID != template || claim.CouponCount != 3 {
		t.Errorf("领取记录上的券快照应当是 %s / 3，实际是 %q / %d", template, claim.CouponTemplateID, claim.CouponCount)
	}

	payloads := f.campaignClaimedPayloads()
	if len(payloads) != 1 {
		t.Fatalf("一次领取应当正好落一条事件，实际 %d 条", len(payloads))
	}
	// 手写结构体断言而不是解成 dto.CampaignClaimedEvent：这里要钉的是**字段名**与
	// coupon-service 那份镜像逐字一致（它用 DisallowUnknownFields 解），拿自己的结构体解回来
	// 是个自证——tag 打错了照样绿。
	var event struct {
		ClaimID          string `json:"claimId"`
		CampaignID       string `json:"campaignId"`
		UserID           string `json:"userId"`
		StoreID          string `json:"storeId"`
		CouponTemplateID string `json:"couponTemplateId"`
		CouponCount      int32  `json:"couponCount"`
	}
	if err := json.Unmarshal([]byte(payloads[0]), &event); err != nil {
		t.Fatalf("事件体解不开（字段名与券服务的镜像对不上？）：%v", err)
	}
	if event.ClaimID != claim.ID || event.CampaignID != campaign.ID || event.UserID != f.user {
		t.Errorf("事件里的身份不对：%+v", event)
	}
	// 券服务拿 storeId 把券锁在活动门店（老系统同一口径）。
	if event.StoreID != f.store {
		t.Errorf("事件里的门店应当是活动门店 %s，实际是 %q", f.store, event.StoreID)
	}
	if event.CouponTemplateID != template || event.CouponCount != 3 {
		t.Errorf("事件里的券应当是 %s / 3，实际是 %q / %d", template, event.CouponTemplateID, event.CouponCount)
	}
}

// TestClaimCampaignCouponSnapshotSurvivesCampaignEdit 钉住「快照」二字：活动改了券配置，已经
// 领过的那一次不跟着变。
func TestClaimCampaignCouponSnapshotSurvivesCampaignEdit(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	original := uuid.NewString()
	scene := f.scene("snap")
	campaign := f.createWithCoupons(plan, scene, original, 3)
	f.enable(campaign.ID)
	first := f.claim(scene)

	// 运营把券换成另一张模板、张数也改了。
	replacement := uuid.NewString()
	if _, err := f.svc.UpdateCampaign(ctx, campaign.ID, dto.CampaignRequest{
		Name: "集成测试活动（带券）", Scene: scene, StoreID: f.store, PlanID: plan.ID,
		GiftDays: 7, StartAt: f.clock.Add(-time.Hour), EndAt: f.clock.Add(time.Hour),
		CouponTemplateID: replacement, CouponCount: 9,
	}, Actor{AdminID: uuid.NewString()}); err != nil {
		t.Fatalf("改活动失败：%v", err)
	}

	// 同一个人再扫一次：回的是**第一次那个结果**，不是活动现在的配置。
	again := f.claim(scene)
	if !again.AlreadyClaimed {
		t.Errorf("第二次扫码应当是重放")
	}
	if again.CouponCount != 3 || !again.MembershipExpireAt.Equal(first.MembershipExpireAt) {
		t.Errorf("重放要回第一次的快照（3 张 / %v），实际是 %d 张 / %v",
			first.MembershipExpireAt, again.CouponCount, again.MembershipExpireAt)
	}

	claims, _, err := f.svc.ListCampaignClaims(ctx, campaign.ID, dto.CampaignClaimQuery{Page: 1, PageSize: 20})
	if err != nil || len(claims) != 1 {
		t.Fatalf("读领取记录失败：len=%d err=%v", len(claims), err)
	}
	if claims[0].CouponTemplateID != original || claims[0].CouponCount != 3 {
		t.Errorf("领取记录上的快照不该被后来的编辑改掉，实际是 %q / %d",
			claims[0].CouponTemplateID, claims[0].CouponCount)
	}
	// 事件也不会因为重放而多发一条。
	if payloads := f.campaignClaimedPayloads(); len(payloads) != 1 {
		t.Errorf("重放不该再发事件，实际 %d 条", len(payloads))
	}
}

// TestClaimCampaignWithoutCouponsStillEmitsEvent 钉住「没配券的活动也发事件」。
//
// 事件名说的是「发生过的事」（有人领了），不是「请去发券」——券那一半为空时下游什么都不做。
// 生产者在发之前分支的话，将来别的域（统计、风控）就收不到这些领取了。
func TestClaimCampaignWithoutCouponsStillEmitsEvent(t *testing.T) {
	f := newCampaignFixture(t)
	plan := f.createPlan(autoPlanRequest())
	scene := f.scene("nocoupon")
	campaign := f.create(plan, scene, 7)
	f.enable(campaign.ID)

	result := f.claim(scene)
	if result.CouponCount != 0 {
		t.Errorf("没配券的活动回执里应当是 0，实际是 %d", result.CouponCount)
	}

	payloads := f.campaignClaimedPayloads()
	if len(payloads) != 1 {
		t.Fatalf("没配券也要发一条事件，实际 %d 条", len(payloads))
	}
	// 两个券字段带 omitempty：空值下**键本身不在**，下游看见 absent 就是「这场活动只送天数」。
	var raw map[string]any
	if err := json.Unmarshal([]byte(payloads[0]), &raw); err != nil {
		t.Fatalf("事件体解不开：%v", err)
	}
	if _, ok := raw["couponTemplateId"]; ok {
		t.Errorf("没配券时不该出现 couponTemplateId：%s", payloads[0])
	}
	if _, ok := raw["couponCount"]; ok {
		t.Errorf("没配券时不该出现 couponCount：%s", payloads[0])
	}
	if raw["claimId"] == nil {
		t.Errorf("事件必须带 claimId（它是券服务的幂等键）：%s", payloads[0])
	}
}

// TestCampaignUpdateLockedWhileEnabled 钉住「开着就不给改 scene / store_id」。
//
// 这条判据挂在仓储里（锁着行判），所以它必须在真库上跑——判据挪进 service 之后这里会红。
func TestCampaignUpdateLockedWhileEnabled(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	campaign := f.create(plan, f.scene("lock"), 7)
	f.enable(campaign.ID)

	req := dto.CampaignRequest{
		Name: "集成测试活动",
		// 换一个码参数。**后缀要短**：scene 有 32 字节的硬上限（微信那条），而 f.code 已经占了
		// 19 个字节，写长了会先被格式校验挡下来，这个用例就验不到「开着不给改」了。
		Scene:    f.scene("lock2"),
		StoreID:  f.store,
		PlanID:   plan.ID,
		GiftDays: 7,
		StartAt:  f.clock.Add(-time.Hour),
		EndAt:    f.clock.Add(time.Hour),
	}
	if _, err := f.svc.UpdateCampaign(ctx, campaign.ID, req, Actor{AdminID: uuid.NewString()}); !errors.Is(err, ErrCampaignLocked) {
		t.Fatalf("活动开着时改 scene 应当被拒，实际是 %v", err)
	}

	// 换成改门店，同样被拒。
	req.Scene = campaign.Scene
	req.StoreID = uuid.NewString()
	if _, err := f.svc.UpdateCampaign(ctx, campaign.ID, req, Actor{AdminID: uuid.NewString()}); !errors.Is(err, ErrCampaignLocked) {
		t.Fatalf("活动开着时改门店应当被拒，实际是 %v", err)
	}

	// 只改天数（不动那两列）是允许的：活动开着的时候运营最常做的就是调天数与名称。
	req.StoreID = f.store
	req.GiftDays = 14
	updated, err := f.svc.UpdateCampaign(ctx, campaign.ID, req, Actor{AdminID: uuid.NewString()})
	if err != nil {
		t.Fatalf("活动开着时改天数被拒了：%v", err)
	}
	if updated.GiftDays != 14 {
		t.Errorf("天数应当是 14，实际是 %d", updated.GiftDays)
	}
}

// TestClaimCampaign 是领取那一条链路的三个分支：没有会员（开通）、还在有效期内（叠加、归属不动）、
// 已经过期（重新起算、归属改成活动门店）。
//
// **归属那两条是这一刀最容易写反的地方**：续费不覆盖、过期重开覆盖，判据挂在 renewMembership
// 的 base 分叉上（见 repository/renewMembership 的说明）。过期那一条必须显式把 expire_at 造到
// 过去，靠事件自然走到的话这个用例就什么都验不到。
func TestClaimCampaign(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	scene := f.scene("claim")
	campaign := f.create(plan, scene, 30)
	f.enable(campaign.ID)

	t.Run("没有会员时开通", func(t *testing.T) {
		result := f.claim(scene)
		if result.AlreadyClaimed {
			t.Errorf("第一次领取不该是重放")
		}
		if result.GiftDays != 30 {
			t.Errorf("回执里的天数应当是 30，实际是 %d", result.GiftDays)
		}
		if result.PlanName == "" {
			t.Errorf("回执要带套餐名（用户看的是「你领到了哪一款会员」）")
		}
		if !result.MembershipExpireAt.After(f.clock) {
			t.Errorf("到期时刻应当在未来，实际是 %v", result.MembershipExpireAt)
		}

		membership, err := f.svc.GetMembershipByUser(ctx, f.user)
		if err != nil {
			t.Fatalf("领完应当有一条会员：%v", err)
		}
		if membership.StoreID != f.store {
			t.Errorf("归属门店应当是活动门店 %s，实际是 %q", f.store, membership.StoreID)
		}
		// 这条链路没有任何签约：置 true 会让界面显示「已开启自动续费」却永远扣不了款。
		if membership.AutoRenew {
			t.Errorf("活动送出来的会员不该带自动续费")
		}
		// 到期 = 领取时刻 + 30 天，按天算（不是套餐的 period/period_count）。
		want := f.clock.AddDate(0, 0, 30)
		if !membership.ExpireAt.Equal(want) {
			t.Errorf("到期时刻应当是 %v，实际是 %v", want, membership.ExpireAt)
		}
	})

	t.Run("重复领取回同一条", func(t *testing.T) {
		before, err := f.svc.GetMembershipByUser(ctx, f.user)
		if err != nil {
			t.Fatalf("读会员失败：%v", err)
		}
		result := f.claim(scene)
		if !result.AlreadyClaimed {
			t.Errorf("第二次扫码应当是重放（页面要说「你已经领过了」）")
		}
		if !result.MembershipExpireAt.Equal(before.ExpireAt) {
			t.Errorf("重放要回同一个结果：到期 %v，实际 %v", before.ExpireAt, result.MembershipExpireAt)
		}
		after, err := f.svc.GetMembershipByUser(ctx, f.user)
		if err != nil {
			t.Fatalf("读会员失败：%v", err)
		}
		if !after.ExpireAt.Equal(before.ExpireAt) {
			t.Errorf("重放不该再发一次：到期从 %v 变成了 %v", before.ExpireAt, after.ExpireAt)
		}
		claims, total, err := f.svc.ListCampaignClaims(ctx, campaign.ID, dto.CampaignClaimQuery{Page: 1, PageSize: 20})
		if err != nil {
			t.Fatalf("读领取记录失败：%v", err)
		}
		if total != 1 || len(claims) != 1 {
			t.Errorf("一个人一场活动只能有一条领取记录，实际 total=%d", total)
		}
		if claims[0].UserID != f.user {
			t.Errorf("领取记录的用户应当是本人，实际是 %s", claims[0].UserID)
		}
	})

	// 上一段结束时会员还有效。把时钟推到这个会员的中间（还没过期）再领一次第二场活动——
	// 这一次走的是「叠加、归属不动」那一支。
	t.Run("有效期内领取叠加天数且不改归属", func(t *testing.T) {
		otherStore := uuid.NewString()
		second := "smc_" + f.code + "_claim2"
		campaign2, err := f.svc.CreateCampaign(ctx, dto.CampaignRequest{
			Name: "集成测试活动二", Scene: second, StoreID: otherStore, PlanID: plan.ID,
			GiftDays: 10, StartAt: f.clock.Add(-time.Hour), EndAt: f.clock.Add(time.Hour),
		}, Actor{AdminID: uuid.NewString()})
		if err != nil {
			t.Fatalf("建第二场活动失败：%v", err)
		}
		f.enable(campaign2.ID)

		before, err := f.svc.GetMembershipByUser(ctx, f.user)
		if err != nil {
			t.Fatalf("读会员失败：%v", err)
		}
		f.claim(second)

		after, err := f.svc.GetMembershipByUser(ctx, f.user)
		if err != nil {
			t.Fatalf("读会员失败：%v", err)
		}
		// 还有剩余有效期：在剩余的基础上叠加，不是从今天重新算。
		if want := before.ExpireAt.AddDate(0, 0, 10); !after.ExpireAt.Equal(want) {
			t.Errorf("有效期内领取应当在剩余基础上叠加：期望 %v，实际 %v", want, after.ExpireAt)
		}
		// **归属不覆盖**：还在有效期内，多领一次不改变他是谁拉来的。
		if after.StoreID != f.store {
			t.Errorf("有效期内领取不该改归属门店：期望 %s，实际 %s", f.store, after.StoreID)
		}
	})

	// 把会员造到过期，再领一次第三场——这一次走「重新起算、归属改成本次活动门店」那一支。
	t.Run("过期后领取重开且改归属", func(t *testing.T) {
		thirdStore := uuid.NewString()
		third := "smc_" + f.code + "_claim3"
		campaign3, err := f.svc.CreateCampaign(ctx, dto.CampaignRequest{
			Name: "集成测试活动三", Scene: third, StoreID: thirdStore, PlanID: plan.ID,
			GiftDays: 5, StartAt: f.clock.Add(-time.Hour), EndAt: f.clock.Add(time.Hour * 100),
		}, Actor{AdminID: uuid.NewString()})
		if err != nil {
			t.Fatalf("建第三场活动失败：%v", err)
		}
		f.enable(campaign3.ID)

		// 直接改库把这一期推到过去：这是**唯一**能区分「判据挂对了没有」的造法，靠事件自然走到
		// 要等真实时间。
		//
		// start_at 也得一起往回挪：库上有 CHECK (expire_at > start_at)，只把到期时间推到过去
		// 会撞上它（一条 23514），而报出来的原因看着跟「过期」毫无关系。
		if _, err := f.pool.Exec(ctx,
			`UPDATE memberships SET start_at = $1, expire_at = $2, status = $3 WHERE user_id = $4`,
			f.clock.AddDate(0, 0, -40), f.clock.Add(-time.Hour), model.MembershipStatusExpired, f.user); err != nil {
			t.Fatalf("把会员造到过期失败：%v", err)
		}

		f.claim(third)

		membership, err := f.svc.GetMembershipByUser(ctx, f.user)
		if err != nil {
			t.Fatalf("读会员失败：%v", err)
		}
		// 已经过期：从这一刻重新起算，而不是接着上一段的残值（残值还是负的）。
		if want := f.clock.AddDate(0, 0, 5); !membership.ExpireAt.Equal(want) {
			t.Errorf("过期后领取要从领取时刻重新起算：期望 %v，实际 %v", want, membership.ExpireAt)
		}
		// **归属改成本次活动门店**：上一段会员关系已经结束，这一次是新的拉新。
		if membership.StoreID != thirdStore {
			t.Errorf("过期后领取应当改归属门店：期望 %s，实际 %s", thirdStore, membership.StoreID)
		}
		// 续费会把 expired 翻回 active。
		if membership.Status != model.MembershipStatusActive {
			t.Errorf("领取之后会员应当是 active，实际是 %q", membership.Status)
		}
	})
}

// TestClaimCampaignRejections 是领取的三条拒绝路径。
func TestClaimCampaignRejections(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())

	// 不存在的 scene：码本身有问题（打印错了、或扫的是别的系统的码）。
	if _, err := f.svc.ClaimCampaign(ctx, dto.CampaignClaimRequest{Scene: "smc_nobody"}, f.user, "it"); !errors.Is(err, ErrCampaignNotFound) {
		t.Errorf("不认识的 scene 应当是 ErrCampaignNotFound，实际是 %v", err)
	}

	// 空 scene：客户端漏传。它进的是 400 那一组，不是 404——用户没扫错码，是请求本身不完整。
	if _, err := f.svc.ClaimCampaign(ctx, dto.CampaignClaimRequest{}, f.user, "it"); !errors.Is(err, ErrCampaignSceneRequired) {
		t.Errorf("空 scene 应当是 ErrCampaignSceneRequired，实际是 %v", err)
	}

	// draft（还没启用）：存在，但今天领不了。它与「不存在」分开——用户手里的码是运营给的，
	// 说「活动不存在」会让他去找一张根本不存在的码。
	draft := f.create(plan, f.scene("draft"), 7)
	if _, err := f.svc.ClaimCampaign(ctx, dto.CampaignClaimRequest{Scene: draft.Scene}, f.user, "it"); !errors.Is(err, ErrCampaignNotClaimable) {
		t.Errorf("draft 的活动应当是 ErrCampaignNotClaimable，实际是 %v", err)
	}

	// 窗口外：活动存在、状态是 enabled，但今天不在起止窗口里。
	past, err := f.svc.CreateCampaign(ctx, dto.CampaignRequest{
		Name: "集成测试活动（已结束）", Scene: f.scene("past"), StoreID: f.store, PlanID: plan.ID,
		GiftDays: 7, StartAt: f.clock.Add(-time.Hour * 48), EndAt: f.clock.Add(-time.Hour * 24),
	}, Actor{AdminID: uuid.NewString()})
	if err != nil {
		t.Fatalf("建活动失败：%v", err)
	}
	f.enable(past.ID)
	if _, err := f.svc.ClaimCampaign(ctx, dto.CampaignClaimRequest{Scene: past.Scene}, f.user, "it"); !errors.Is(err, ErrCampaignNotClaimable) {
		t.Errorf("窗口外的活动应当是 ErrCampaignNotClaimable，实际是 %v", err)
	}
}

// TestClaimCampaignLeavesChangeAndEvent 钉住领取留下的痕迹：一条 user 操作人的流水 + 一条事件。
//
// **不记平台审计**：那是用户自己扫的码，不是后台人工操作（见 repository.ClaimCampaign 的说明）。
func TestClaimCampaignLeavesChangeAndEvent(t *testing.T) {
	f := newCampaignFixture(t)
	ctx := context.Background()
	plan := f.createPlan(autoPlanRequest())
	scene := f.scene("trace")
	campaign := f.create(plan, scene, 7)
	f.enable(campaign.ID)
	f.claim(scene)

	membership, err := f.svc.GetMembershipByUser(ctx, f.user)
	if err != nil {
		t.Fatalf("读会员失败：%v", err)
	}
	changes, err := f.svc.ListChanges(ctx, membership.ID)
	if err != nil {
		t.Fatalf("读流水失败：%v", err)
	}
	// ListChanges 是按时间正序的，最后一条就是这次领取。
	last := changes[len(changes)-1]
	if last.ChangeType != model.ChangeActivate {
		t.Errorf("没有会员时领取记的是开通，实际是 %q", last.ChangeType)
	}
	if last.OperatorType != model.OperatorUser {
		t.Errorf("操作人应当是用户本人，实际是 %q", last.OperatorType)
	}
	if last.Reason != "店铺码活动领取" {
		t.Errorf("流水上的原因应当是「店铺码活动领取」，实际是 %q", last.Reason)
	}
	if !strings.Contains(last.Remark, campaign.Name) {
		t.Errorf("备注里应当带上活动名 %q，实际是 %q", campaign.Name, last.Remark)
	}

	// 事件：member.activated 之类的那一条，落在这个用例独占的套餐编码上。
	var count int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM message_outbox WHERE convert_from(payload, 'UTF8') LIKE $1`,
		"%"+f.code+"%").Scan(&count); err != nil {
		t.Fatalf("查事件失败：%v", err)
	}
	if count == 0 {
		t.Errorf("领取应当发出一条会员事件")
	}
}
