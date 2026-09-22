package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 这一份是**要真库**的集成测试：它跑的是 repository 里那些 SQL。
//
// # 为什么不写假的仓储
//
// repository 这一层里有 22 个列的 SELECT/INSERT/UPDATE、一串 CHECK 约束、一个 append-only
// 触发器和一条唯一索引。这一层最典型的故障是**列名或参数顺序写错**——那种错编译期看不出来，
// 只有在真库上跑一次才会浮出来，而它浮出来的方式是线上第一次有人买会员时返回 500。
// 用一个内存假仓储替掉它，等于把这一层整个排除在测试之外：测的是「我自己写的假仓储符合我
// 自己写的期望」。
//
// # 写法与其余服务一致
//
// 门禁与 account-service 的 accountIntegrationPool 一样：读 MEMBERSHIP_DATABASE_URL（拿不到
// 再退到 TEST_DATABASE_URL），没有或者连不上就 t.Skip。所以没配库的机器上 `go test ./...`
// 照样全绿，只是这些用例不出现在结果里。
//
// # 它们往库里写东西，但自己擦干净
//
// 每个用例用一个**自己生成的** user_id 与套餐 code，cleanup 里按这两个值删干净（见
// membershipFixture.cleanup）。不共用固定 ID 的理由很简单：这个库是 dev 库，不是一次性的
// testcontainers，用固定 ID 的话两次并发跑、或者上次跑挂了留了半条数据，第二个用例就会读到
// 前一次的状态，而它报出来的错误会是「续费叠加对不上」这种要查半天的话。

// membershipIntegrationPool 连真库，连不上就跳过。
func membershipIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("MEMBERSHIP_DATABASE_URL")
	if url == "" {
		url = os.Getenv("TEST_DATABASE_URL")
	}
	if url == "" {
		t.Skip("没有 MEMBERSHIP_DATABASE_URL（也没有 TEST_DATABASE_URL）：跳过会员库集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("会员库连接串用不了，跳过：%v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("会员库连不上，跳过：%v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// membershipFixture 是一个用例的全套家当：真库 + 真仓储 + 真业务层，外加一个可推进的时钟。
type membershipFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	svc  *MembershipService
	// orders 是这一份用例挂上的假订单域，与 svc 同一个生命周期（见 chargeGateway）。走扣款那条
	// 路的用例才需要它——只读会员、只签约的用例压根不该有订单域，把它们也塞上会让「没接订单域
	// 的进程」这个真实存在的情形在测试里失去代表。
	orders *fakeOrders
	// code / user 都是这个用例独占的，cleanup 靠它们收摊。
	code  string
	user  string
	clock time.Time
}

// newMembershipFixture 建一套隔离的家当。
func newMembershipFixture(t *testing.T) *membershipFixture {
	t.Helper()

	f := &membershipFixture{
		t:    t,
		pool: membershipIntegrationPool(t),
		// 编码是 slug（只允许小写字母数字下划线），而 uuid 里带横线，去掉。
		code:  "it_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16],
		user:  uuid.NewString(),
		clock: time.Now().UTC().Truncate(time.Second),
	}
	// Now 走这个可推进的时钟而不是 time.Now：entitlement 的判定与「这个人现在算不算会员」全都
	// 取它，用真实时钟的话，那几个用例就只能靠 sleep 来等一个会员过期。
	f.svc = New(repository.NewPostgresRepository(f.pool, nil), Options{
		Now: func() time.Time { return f.clock },
	})

	t.Cleanup(f.cleanup)
	return f
}

// cleanup 把这个用例写进去的行删干净。
//
// # 为什么要动触发器
//
// membership_changes 上有一条 BEFORE UPDATE OR DELETE 的触发器无条件拒删（只增不改不删，
// 与 stock_movements 同一套写法）。**那条防线是对的**，所以这里不是把它去掉，而是在这一条
// 语句的范围内暂时摘下来、删完立刻装回去——测试留下的流水如果不删，dev 库上会积一堆
// 「计划外的开通记录」，而它恰恰是客服会去翻的时间线。
//
// 顺序是外键定的：changes → memberships → plans（两条 REFERENCES 都是 ON DELETE RESTRICT）。
// outbox 那一条按套餐编码删：本服务发出去的事件体里都带 planCode，而它是这个用例独占的，
// 所以不可能误删别人的。
//
// 一切失败都只 t.Errorf 不 Fatal：cleanup 跑在测试结束之后，这里再 Fatal 会盖掉真正的失败。
func (f *membershipFixture) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	steps := []struct {
		what string
		sql  string
		args []any
	}{
		{"暂时摘掉只增不改不删的触发器", `ALTER TABLE membership_changes DISABLE TRIGGER membership_changes_append_only`, nil},
		{"删流水", `DELETE FROM membership_changes WHERE user_id = $1`, []any{f.user}},
		{"装回触发器", `ALTER TABLE membership_changes ENABLE TRIGGER membership_changes_append_only`, nil},
		// 订阅必须在会员之前删：membership_subscriptions.membership_id 是
		// ON DELETE RESTRICT，顺序反了这一步会直接失败（而失败信息指的是「删会员」那一步，
		// 真正的原因在下一条外键上）。
		{"删订阅", `DELETE FROM membership_subscriptions WHERE user_id = $1`, []any{f.user}},
		// 店铺码活动的两条也在会员之前：领取记录 REFERENCES memberships(id) 是 RESTRICT，
		// 反过来的话「删会员」那一步会失败，而它报出来的原因看着跟活动毫无关系。
		// 两个条件都要：领取记录按 user 删（别的用例不会写这个 user），活动按 scene 前缀删
		// （本用例的 scene 全都带 f.code，见 campaignFixture.scene）。
		{"删领取记录", `DELETE FROM membership_campaign_claims WHERE user_id = $1`, []any{f.user}},
		// 领取那一条事件**没有 planCode**（它说的是会员领取，不是购买），所以上面那条按编码删
		// 事件的规则收不到它。按 user_id 收——那是这个用例独占的 uuid。
		{"删领取事件", `DELETE FROM message_outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8') LIKE '%' || $2 || '%'`,
			[]any{dto.EventMembershipCampaignClaimed, f.user}},
		{"删活动", `DELETE FROM membership_campaigns WHERE scene LIKE 'smc_' || $1 || '%'`, []any{f.code}},
		{"删会员", `DELETE FROM memberships WHERE user_id = $1`, []any{f.user}},
		// 按**前缀**删而不是等值：一个用例要两款套餐时（券模式的与能签约的），第二款挂在
		// f.code 后面（见 campaignFixture.createSuffixedPlan）。前缀是这个用例独占的随机串，
		// 不会碰到别人的。
		{"删套餐", `DELETE FROM membership_plans WHERE code LIKE $1 || '%'`, []any{f.code}},
		// payload 是 BYTEA，先按 UTF8 解回 JSON 再找编码。条件用 %code%（带引号的那个值），
		// 而不是 LIKE '%code%'，免得撞上恰好在别处出现的同一串字符。
		{"删事件", `DELETE FROM message_outbox WHERE convert_from(payload, 'UTF8') LIKE $1`,
			[]any{fmt.Sprintf(`%%%q%%`, f.code)}},
	}

	for _, step := range steps {
		if _, err := f.pool.Exec(ctx, step.sql, step.args...); err != nil {
			f.t.Errorf("cleanup 第「%s」步失败：%v", step.what, err)
			return
		}
	}
}

// —— 造数据的小工具 ——

// couponPlanRequest 是一个「9.9 包月、会员价靠券」的套餐：老系统里那个连续包月的样子。
func couponPlanRequest() dto.PlanRequest {
	return dto.PlanRequest{
		Name:        "连续包月（集成测试）",
		Description: "每月自动发放会员价券",
		Benefits:    []string{"每月 20 张会员价券"},
		PriceCents:  990,
		Period:      model.PeriodMonth,
		PeriodCount: 1,
		// 套餐层面不签代扣：签约那条链路（payment-service 的委托代扣）还没落地。
		AutoRenew:       false,
		WechatPlanID:    "",
		MemberPriceMode: model.MemberPriceModeCoupon,
		// 券模板 ID 是一个**跨库的 uuid** 值引用（coupon-service 的 coupon_templates.id），
		// 服务层会校它是不是 uuid——库里那一列就是 UUID 类型，随便给个字符串会在写库时炸成
		// 一条 22P02（500）。
		MemberPriceCouponTemplateID: uuid.NewString(),
		MemberPriceCouponsPerPeriod: 20,
		SortOrder:                   1,
	}
}

// autoPlanRequest 是一个「会员价直接算」的年度套餐，带签约模板 ID（老系统表单里那串 214488）。
func autoPlanRequest() dto.PlanRequest {
	return dto.PlanRequest{
		Name:            "年度会员（集成测试）",
		PriceCents:      9900,
		Period:          model.PeriodYear,
		PeriodCount:     1,
		AutoRenew:       true,
		WechatPlanID:    "214488",
		MemberPriceMode: model.MemberPriceModeAuto,
	}
}

// createPlan 建一个套餐，失败即终止。
func (f *membershipFixture) createPlan(req dto.PlanRequest) *model.Plan {
	f.t.Helper()

	plan, err := f.svc.CreatePlan(context.Background(),
		dto.CreatePlanRequest{Code: f.code, PlanRequest: req}, uuid.NewString())
	if err != nil {
		f.t.Fatalf("建套餐失败：%v", err)
	}
	return plan
}

// createSuffixedPlan 建**第二个**套餐。
//
// 一个用例要两款套餐（能签约的与券模式的、或者活动送的那一档）时得换一个编码：
// membership_plans.code 上有唯一索引，而 membershipFixture 给的是一个用例一个 code。后缀挂在
// f.code 后面，cleanup 那一步按前缀删，一并收走。
func (f *membershipFixture) createSuffixedPlan(req dto.PlanRequest, suffix string) *model.Plan {
	f.t.Helper()

	plan, err := f.svc.CreatePlan(context.Background(),
		dto.CreatePlanRequest{Code: f.code + suffix, PlanRequest: req}, uuid.NewString())
	if err != nil {
		f.t.Fatalf("建套餐失败：%v", err)
	}
	return plan
}

// couponFields 从套餐上取下券模板与张数，没配就是空串与 0。
//
// 事件里的这两个字段是普通值（不是指针），所以这里要摊平一次。auto 模式的套餐必须给出
// 空串与 0——服务层会校这一对配对。
func couponFields(plan *model.Plan) (string, int32) {
	if plan.MemberPriceCouponTemplateID == nil || plan.MemberPriceCouponsPerPeriod == nil {
		return "", 0
	}
	return *plan.MemberPriceCouponTemplateID, *plan.MemberPriceCouponsPerPeriod
}

// pay 投一条 order.paid 给消费者，模拟「这个人的这一单付款成功了」。
//
// payload 是**手写的 JSON**，不是 json.Marshal(dto.OrderPaidEventPayload)：这一层要钉的是
// 两边 json tag 对得上（消费者用 DisallowUnknownFields 解它），拿自己的结构体序列化再解回来
// 是个自证——某个 tag 打错了照样绿。手写的话，订单域那边一旦改了字段名，这里会红。
func (f *membershipFixture) pay(orderID string, plan *model.Plan, paidAt time.Time) error {
	f.t.Helper()
	return f.payWithStore(orderID, plan, paidAt, nil)
}

// payWithStore 就是 pay，外加「这一单是在哪家店成交的」——它是会员**归属门店**的两个来源之一
// （另一个是后台开通时选的门店）。传 nil 与「发 null」是同一件事：这一单没有门店，订单域发
// 小程序线上单时就是这样。
func (f *membershipFixture) payWithStore(orderID string, plan *model.Plan, paidAt time.Time, storeID *string) error {
	f.t.Helper()

	templateID, perPeriod := couponFields(plan)
	short := orderID[:8]
	// storeId 编成 null 或一个带引号的 uuid，与 order-service 那边 map[string]any 里放
	// *string 的效果一致。
	store := "null"
	if storeID != nil {
		store = fmt.Sprintf("%q", *storeID)
	}
	payload := fmt.Sprintf(`{
		"orderId": %q,
		"orderNo": %q,
		"userId": %q,
		"paidAt": %q,
		"paidAmount": %d,
		"paymentNo": %q,
		"paymentMethod": "wechat",
		"fundings": [],
		"storeId": %s,
		"membership": {
			"planId": %q,
			"planCode": %q,
			"planName": %q,
			"priceCents": %d,
			"period": %q,
			"periodCount": %d,
			"autoRenew": %t,
			"memberPriceMode": %q,
			"memberPriceCouponTemplateId": %q,
			"memberPriceCouponsPerPeriod": %d
		}
	}`, orderID, "ORD-"+short, f.user, paidAt.UTC().Format(time.RFC3339), plan.PriceCents,
		"PAY-"+short, store, plan.ID, plan.Code, plan.Name, plan.PriceCents, plan.Period, plan.PeriodCount,
		plan.AutoRenew, plan.MemberPriceMode, templateID, perPeriod)

	return f.svc.HandleEvent(context.Background(), messaging.Envelope{
		EventID:      uuid.NewString(),
		EventType:    dto.EventOrderPaid,
		EventVersion: dto.EventVersion,
		TraceID:      uuid.NewString(),
		Payload:      []byte(payload),
	})
}

// mustPay 是 pay 的失败即终止版本，绝大多数用例用它。
func (f *membershipFixture) mustPay(orderID string, plan *model.Plan, paidAt time.Time) {
	f.t.Helper()
	if err := f.pay(orderID, plan, paidAt); err != nil {
		f.t.Fatalf("消费 order.paid 失败：%v", err)
	}
}

// mustPayWithStore 是 payWithStore 的失败即终止版本。
func (f *membershipFixture) mustPayWithStore(orderID string, plan *model.Plan, paidAt time.Time, storeID *string) {
	f.t.Helper()
	if err := f.payWithStore(orderID, plan, paidAt, storeID); err != nil {
		f.t.Fatalf("消费 order.paid 失败：%v", err)
	}
}

// activate 把套餐上架——小程序与下单校验都只认 active 的套餐。
func (f *membershipFixture) activate(plan *model.Plan) {
	f.t.Helper()
	if _, err := f.svc.SetPlanStatus(context.Background(), plan.ID, model.PlanStatusActive); err != nil {
		f.t.Fatalf("上架套餐失败：%v", err)
	}
}

// membership 读回这个用例那位用户的会员，不存在即终止。
func (f *membershipFixture) membership() *model.Membership {
	f.t.Helper()
	membership, err := f.svc.GetMembershipByUser(context.Background(), f.user)
	if err != nil {
		f.t.Fatalf("读会员失败：%v", err)
	}
	return membership
}

// changes 读回这个用例那位用户的全部流水，**最新的在前**（ListChanges 就是这么排的：
// occurred_at DESC——后台详情页的时间线要一眼看到最近发生了什么）。断言「第几条是什么」时
// 别把它当成正序。
func (f *membershipFixture) changes() []*model.Change {
	f.t.Helper()
	changes, err := f.svc.ListChanges(context.Background(), f.membership().ID)
	if err != nil {
		f.t.Fatalf("读流水失败：%v", err)
	}
	return changes
}

// changeTypes 把一串流水压成可读的一行，失败信息里用。
//
// 有它是因为「第 2 条是 renew，实际是 activate」这种话在看到全部流水之前没法判断——到底是顺序
// 反了，还是那条流水压根没记。
func changeTypes(changes []*model.Change) string {
	types := make([]string, 0, len(changes))
	for _, change := range changes {
		types = append(types, change.ChangeType)
	}
	return strings.Join(types, ", ")
}

func (f *membershipFixture) actor() Actor {
	return Actor{AdminID: uuid.NewString(), TraceID: uuid.NewString(), RequestID: uuid.NewString()}
}

// ============================================================
// 套餐
// ============================================================

// TestIntegrationPlanLifecycle 走一遍套餐从草稿到上架到下架，用的是真库上的 CHECK、触发器与
// 唯一索引。
//
// 钉住三件事：新建的是草稿（不是一建就卖）、编码唯一、**没上架的套餐不出现在小程序列表里**
// ——最后一条是下单侧唯一的护栏，套餐还在配置时被用户看见就会有人下单。
func TestIntegrationPlanLifecycle(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(couponPlanRequest())
	if plan.Status != model.PlanStatusDraft {
		t.Fatalf("新建的套餐状态是 %q，期望 draft", plan.Status)
	}
	if plan.Code != f.code {
		t.Fatalf("编码是 %q，期望 %q", plan.Code, f.code)
	}

	// 券的配对原样落了库：这是老系统那 20 张券的来源。
	if plan.MemberPriceCouponsPerPeriod == nil || *plan.MemberPriceCouponsPerPeriod != 20 {
		t.Fatalf("每期张数没落上：%v", plan.MemberPriceCouponsPerPeriod)
	}

	// 编码唯一：库上的唯一索引 + 仓储的 23505 映射。
	_, err := f.svc.CreatePlan(ctx, dto.CreatePlanRequest{Code: f.code, PlanRequest: couponPlanRequest()}, uuid.NewString())
	if !errors.Is(err, ErrPlanCodeTaken) {
		t.Fatalf("重名的套餐编码回的是 %v，期望 ErrPlanCodeTaken", err)
	}

	// 草稿不该被小程序看见。
	if containsPlan(f.activePlans(t), plan.ID) {
		t.Fatal("还没上架的套餐出现在小程序列表里了")
	}

	f.activate(plan)
	if !containsPlan(f.activePlans(t), plan.ID) {
		t.Fatal("上架了却没出现在小程序列表里")
	}

	// 后台列表：编码是唯一的，按它筛出来必然只有这一条。
	plans, total, err := f.svc.ListPlans(ctx, dto.PlanQuery{
		Status: model.PlanStatusActive, Keyword: f.code, Page: 1, PageSize: dto.DefaultPageSize,
	})
	if err != nil {
		t.Fatalf("后台套餐列表失败：%v", err)
	}
	if total != 1 || len(plans) != 1 || plans[0].ID != plan.ID {
		t.Fatalf("按编码 %q 筛出 total=%d len=%d，期望正好那一条", f.code, total, len(plans))
	}

	// 下架之后小程序列表里就没了。
	if _, err := f.svc.SetPlanStatus(ctx, plan.ID, model.PlanStatusDisabled); err != nil {
		t.Fatalf("下架失败：%v", err)
	}
	if containsPlan(f.activePlans(t), plan.ID) {
		t.Fatal("下架了还出现在小程序列表里")
	}
}

func (f *membershipFixture) activePlans(t *testing.T) []*model.Plan {
	t.Helper()
	plans, err := f.svc.ListActivePlans(context.Background())
	if err != nil {
		t.Fatalf("小程序套餐列表失败：%v", err)
	}
	return plans
}

func containsPlan(plans []*model.Plan, id string) bool {
	for _, plan := range plans {
		if plan.ID == id {
			return true
		}
	}
	return false
}

// ============================================================
// 开通与续期（消费 order.paid）
// ============================================================

// TestIntegrationPaidOrderActivatesThenRenews 是这一刀的核心闭环：付钱 → 生效 → 再付钱 → 叠加。
//
// 续费那半段盯的是**叠加基准**：新的到期时间必须从原来的 expire_at 往后加，不是从此刻往后加。
// 从此刻加的话，一个还剩 20 天的人再买一个月只会得到 30 天——他提前续费反而亏了 20 天，而这
// 在多买几期之后才会被人发现。
func TestIntegrationPaidOrderActivatesThenRenews(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)

	// 付款时刻比「现在」早一小时：用事件里的 paidAt 而不是处理时刻，补投一条事件时会员的
	// 起始时间才是那一单真正付款的时间。
	paidAt := f.clock.Add(-time.Hour)
	firstOrder := uuid.NewString()
	f.mustPay(firstOrder, plan, paidAt)

	membership := f.membership()
	wantExpire := model.AddPeriod(paidAt, plan.Period, plan.PeriodCount)
	if membership.Status != model.MembershipStatusActive {
		t.Fatalf("开通后状态是 %q，期望 active", membership.Status)
	}
	if !membership.StartAt.UTC().Equal(paidAt) {
		t.Fatalf("start_at = %s，期望 %s（事件里的 paidAt，不是处理时刻）",
			membership.StartAt.UTC().Format(time.RFC3339), paidAt.Format(time.RFC3339))
	}
	if !membership.ExpireAt.UTC().Equal(wantExpire.UTC()) {
		t.Fatalf("expire_at = %s，期望 %s",
			membership.ExpireAt.UTC().Format(time.RFC3339), wantExpire.UTC().Format(time.RFC3339))
	}
	if membership.RenewalCount != 0 {
		t.Fatalf("首次开通的 renewal_count 是 %d，期望 0", membership.RenewalCount)
	}
	// 成交快照：套餐是什么样，会员就是什么样。
	if membership.PlanCode != f.code || membership.MemberPriceMode != model.MemberPriceModeCoupon {
		t.Fatalf("成交快照不对：code=%q mode=%q", membership.PlanCode, membership.MemberPriceMode)
	}
	if membership.MemberPriceCouponsPerPeriod == nil || *membership.MemberPriceCouponsPerPeriod != 20 {
		t.Fatalf("每期券张数没抄进快照：%v", membership.MemberPriceCouponsPerPeriod)
	}
	// 套餐的 auto_renew 说的是「这个产品能签代扣」，用户开关是另一回事，默认必须是关的。
	if membership.AutoRenew {
		t.Fatal("刚开通的会员自动续费就是开着的：没有任何协议能扣款")
	}

	// 开通那一条流水：from_status 必须是 NULL（之前没有这条会员）。
	changes := f.changes()
	if len(changes) != 1 {
		t.Fatalf("开通后有 %d 条流水，期望 1 条", len(changes))
	}
	opening := changes[0]
	if opening.ChangeType != model.ChangeActivate || opening.OperatorType != model.OperatorSystem {
		t.Fatalf("第一条流水是 %s/%s，期望 activate/system", opening.ChangeType, opening.OperatorType)
	}
	if opening.FromStatus != nil {
		t.Fatalf("开通流水的 from_status 是 %q，期望 NULL", *opening.FromStatus)
	}
	if opening.ToStatus == nil || *opening.ToStatus != model.MembershipStatusActive {
		t.Fatalf("开通流水的 to_status 不对：%v", opening.ToStatus)
	}
	if opening.OrderID == nil || *opening.OrderID != firstOrder {
		t.Fatalf("开通流水没挂上订单：%v", opening.OrderID)
	}

	// coupon 模式的会员：是会员，但会员价要凭券，不能直接算。
	entitlement, err := f.svc.MemberPriceEntitlement(ctx, f.user)
	if err != nil {
		t.Fatalf("查会员价资格失败：%v", err)
	}
	if !entitlement.Active {
		t.Fatal("刚开通的人不算会员")
	}
	if entitlement.GrantsMemberPrice {
		t.Fatal("coupon 模式的会员被算成可以直接享会员价：券就白发了")
	}
	if entitlement.MemberPriceMode != model.MemberPriceModeCoupon || entitlement.PlanCode != f.code {
		t.Fatalf("资格里的快照不对：mode=%q code=%q", entitlement.MemberPriceMode, entitlement.PlanCode)
	}

	// 续费：第二单，付款时刻就是「现在」——此时原来的 expire_at 还在未来，叠加基准是它。
	secondOrder := uuid.NewString()
	f.mustPay(secondOrder, plan, f.clock)

	renewed := f.membership()
	wantStacked := model.AddPeriod(membership.ExpireAt, plan.Period, plan.PeriodCount)
	if !renewed.ExpireAt.UTC().Equal(wantStacked.UTC()) {
		t.Fatalf("续费后 expire_at = %s，期望从原到期时间叠加得到 %s",
			renewed.ExpireAt.UTC().Format(time.RFC3339), wantStacked.UTC().Format(time.RFC3339))
	}
	if renewed.RenewalCount != 1 {
		t.Fatalf("续费后 renewal_count = %d，期望 1", renewed.RenewalCount)
	}
	if renewed.LastRenewedAt == nil {
		t.Fatal("续费了但 last_renewed_at 是空的")
	}
	if renewed.ID != membership.ID {
		t.Fatal("续费新开了一行：会员必须还是同一个人才对得上积分、券和历史流水")
	}

	// 最新的在前：续费那条在打开那条之前（开通用的是事件里的 paidAt，一小时前）。
	changes = f.changes()
	if len(changes) != 2 {
		t.Fatalf("续费后有 %d 条流水，期望 2 条", len(changes))
	}
	if changes[0].ChangeType != model.ChangeRenew || changes[1].ChangeType != model.ChangeActivate {
		t.Fatalf("两条流水是 %s / %s，期望最新的是 renew", changes[0].ChangeType, changes[1].ChangeType)
	}
	renewal := changes[0]
	if renewal.ChangeType != model.ChangeRenew || renewal.OperatorType != model.OperatorSystem {
		t.Fatalf("第二条流水是 %s/%s，期望 renew/system", renewal.ChangeType, renewal.OperatorType)
	}
	if renewal.FromExpireAt == nil || !renewal.FromExpireAt.UTC().Equal(membership.ExpireAt.UTC()) {
		t.Fatalf("续费流水的 from_expire_at 是 %v，期望 %s", renewal.FromExpireAt, membership.ExpireAt.UTC())
	}
	if renewal.ToExpireAt == nil || !renewal.ToExpireAt.UTC().Equal(wantStacked.UTC()) {
		t.Fatalf("续费流水的 to_expire_at 是 %v，期望 %s", renewal.ToExpireAt, wantStacked.UTC())
	}

	// 后台按用户查出这唯一一条。
	list, total, err := f.svc.ListMemberships(ctx, dto.MembershipQuery{UserID: f.user, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("后台会员列表失败：%v", err)
	}
	if total != 1 || len(list) != 1 || list[0].ID != membership.ID {
		t.Fatalf("按用户筛出 total=%d len=%d，期望正好那一条", total, len(list))
	}

	// 两次变更各发了一条事件出去，同一个事务里落的。
	assertOutboxEvent(t, f, dto.EventMembershipActivated)
	assertOutboxEvent(t, f, dto.EventMembershipRenewed)
}

// TestIntegrationPaidOrderReplayIsNotARenewal 是幂等：支付成功回调会被重放，而重放**不能**
// 把会员续两次。
//
// 挡住它的是 membership_changes_order_unique (order_id, change_type) 这条不变式，不是服务层
// 的自觉——重投迟早会发生（网络抖动、消费者重启、RabbitMQ 的 redeliver），而一次多续的会员
// 是收不回来的。
func TestIntegrationPaidOrderReplayIsNotARenewal(t *testing.T) {
	f := newMembershipFixture(t)

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)

	orderID := uuid.NewString()
	f.mustPay(orderID, plan, f.clock)

	first := f.membership()

	// 同一条 order.paid 再投一次：消费者必须 ack（返回 nil），而不是报错进死信。
	if err := f.pay(orderID, plan, f.clock); err != nil {
		t.Fatalf("重投同一条 order.paid 报了错：%v（它该被当成「这件事已经做过了」ack 掉）", err)
	}

	replayed := f.membership()
	if !replayed.ExpireAt.UTC().Equal(first.ExpireAt.UTC()) {
		t.Fatalf("重投之后 expire_at 从 %s 变成了 %s：这一单的会员权益被续了两次",
			first.ExpireAt.UTC().Format(time.RFC3339), replayed.ExpireAt.UTC().Format(time.RFC3339))
	}
	if replayed.RenewalCount != first.RenewalCount {
		t.Fatalf("重投之后 renewal_count 从 %d 变成了 %d", first.RenewalCount, replayed.RenewalCount)
	}
	if changes := f.changes(); len(changes) != 1 {
		t.Fatalf("重投之后有 %d 条流水，期望还是 1 条", len(changes))
	}
}

// TestIntegrationOrderWithoutMembershipIsAcked 确认「不是买会员的单」被安静地放过。
//
// 这是**常态而不是异常**：这个消费者的队列上绝大多数消息都是奶茶、咖啡、加购。把这一类报成
// 错误的话，每一条正常订单都会进死信，那条死信队列会在一天之内变得没人看。
func TestIntegrationOrderWithoutMembershipIsAcked(t *testing.T) {
	f := newMembershipFixture(t)

	payload := fmt.Sprintf(`{
		"orderId": %q, "orderNo": "ORD-PLAIN", "userId": %q, "paidAt": %q,
		"paidAmount": 1800, "paymentNo": "PAY-PLAIN",
		"paymentMethod": "wechat", "fundings": []
	}`, uuid.NewString(), f.user, f.clock.Format(time.RFC3339))

	err := f.svc.HandleEvent(context.Background(), messaging.Envelope{
		EventID: uuid.NewString(), EventType: dto.EventOrderPaid, Payload: []byte(payload),
	})
	if err != nil {
		t.Fatalf("不带会员的快照的单报了错：%v", err)
	}

	if _, err := f.svc.GetMembershipByUser(context.Background(), f.user); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("不买会员的单却建出了会员：err=%v", err)
	}
}

// ============================================================
// 会员价资格跟着成交快照走
// ============================================================

// TestIntegrationEntitlementFollowsTheSnapshot 钉住「后台改套餐不改已经卖出去的会员」。
//
// auto 模式（会员价直接算）的会员，在套餐被改成 coupon 模式之后，**仍然**是直接算。反过来也一样。
// 不这么做的话，一次后台编辑会同时改写所有历史会员的权益——而那时候没有任何东西会报错，只是
// 某一天开始小程序上多了一批「会员价没生效」的投诉。
func TestIntegrationEntitlementFollowsTheSnapshot(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(autoPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock)

	entitlement, err := f.svc.MemberPriceEntitlement(ctx, f.user)
	if err != nil {
		t.Fatalf("查会员价资格失败：%v", err)
	}
	if !entitlement.Active || !entitlement.GrantsMemberPrice {
		t.Fatalf("auto 模式的会员没有被算成可以直接享会员价：%+v", entitlement)
	}

	// 后台把套餐改成 coupon 模式（张数与模板一起给，库上那一对 CHECK 钉着配对）。
	updated := autoPlanRequest()
	updated.MemberPriceMode = model.MemberPriceModeCoupon
	updated.MemberPriceCouponTemplateID = uuid.NewString()
	updated.MemberPriceCouponsPerPeriod = 5
	if _, err := f.svc.UpdatePlan(ctx, plan.ID, updated); err != nil {
		t.Fatalf("改套餐失败：%v", err)
	}

	after, err := f.svc.MemberPriceEntitlement(ctx, f.user)
	if err != nil {
		t.Fatalf("改完套餐再查资格失败：%v", err)
	}
	if after.MemberPriceMode != model.MemberPriceModeAuto || !after.GrantsMemberPrice {
		t.Fatalf("后台改了套餐之后，已经卖出去的会员资格跟着变了：%+v", after)
	}
}

// ============================================================
// 后台人工干预
// ============================================================

// TestIntegrationFreezeUnfreezeRevoke 走一遍那三个状态机动作，并钉住冻结的一条**关键语义**：
// 冻结不动 expire_at。
//
// 把到期时间一起往后推的话，一次误冻（或者一次因为结算问题被冻了两天）就变成一次免费延期，
// 而且它是从客服那一侧看不出来的——用户只会发现自己的会员比预想的多活了几天。
func TestIntegrationFreezeUnfreezeRevoke(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock)

	opened := f.membership()

	// 冻结：状态变 frozen，到期时间与续费次数一动不动。
	frozen, err := f.svc.FreezeMembership(ctx, opened.ID, "客服工单 1234：账号疑似被盗", f.actor())
	if err != nil {
		t.Fatalf("冻结失败：%v", err)
	}
	if frozen.Status != model.MembershipStatusFrozen {
		t.Fatalf("冻结后状态是 %q，期望 frozen", frozen.Status)
	}
	if !frozen.ExpireAt.UTC().Equal(opened.ExpireAt.UTC()) {
		t.Fatalf("冻结把到期时间从 %s 推到了 %s：一次误冻会变成一次免费延期",
			opened.ExpireAt.UTC().Format(time.RFC3339), frozen.ExpireAt.UTC().Format(time.RFC3339))
	}
	if frozen.FrozenAt == nil || frozen.FreezeReason == "" {
		t.Fatal("冻结没留下时间与理由")
	}

	// 冻结中的人不算会员价，而且**连模式都不给**：给了它就有被下游拿去发券的可能。
	entitlement, err := f.svc.MemberPriceEntitlement(ctx, f.user)
	if err != nil {
		t.Fatalf("查资格失败：%v", err)
	}
	if entitlement.Active || entitlement.MemberPriceMode != "" {
		t.Fatalf("冻结中的人还算会员价：%+v", entitlement)
	}

	// 再冻一次：状态不对，得说清楚。
	if _, err := f.svc.FreezeMembership(ctx, opened.ID, "重复冻结", f.actor()); !errors.Is(err, ErrMembershipNotActive) {
		t.Fatalf("重复冻结回的是 %v，期望 ErrMembershipNotActive", err)
	}

	// 解冻：回到 active，且剩余天数原样还在。
	thawed, err := f.svc.UnfreezeMembership(ctx, opened.ID, "工单核实无误，恢复", f.actor())
	if err != nil {
		t.Fatalf("解冻失败：%v", err)
	}
	if thawed.Status != model.MembershipStatusActive {
		t.Fatalf("解冻后状态是 %q，期望 active", thawed.Status)
	}
	if !thawed.ExpireAt.UTC().Equal(opened.ExpireAt.UTC()) {
		t.Fatalf("解冻把到期时间改成了 %s（原 %s）", thawed.ExpireAt.UTC(), opened.ExpireAt.UTC())
	}

	// 没冻的人解冻：说的是另一句话。
	if _, err := f.svc.UnfreezeMembership(ctx, opened.ID, "没冻的解冻", f.actor()); !errors.Is(err, ErrMembershipNotFrozen) {
		t.Fatalf("没冻的人解冻回的是 %v，期望 ErrMembershipNotFrozen", err)
	}

	// 撤销：不可逆。
	revoked, err := f.svc.RevokeMembership(ctx, opened.ID, "风控判定刷单", f.actor())
	if err != nil {
		t.Fatalf("撤销失败：%v", err)
	}
	if revoked.Status != model.MembershipStatusRevoked {
		t.Fatalf("撤销后状态是 %q，期望 revoked", revoked.Status)
	}

	// 撤销之后不接受任何自动恢复——解冻也不行。
	if _, err := f.svc.UnfreezeMembership(ctx, opened.ID, "想恢复一个撤销掉的", f.actor()); !errors.Is(err, ErrMembershipRevoked) {
		t.Fatalf("解冻撤销过的会员回的是 %v，期望 ErrMembershipRevoked", err)
	}
	if entitlement, err := f.svc.MemberPriceEntitlement(ctx, f.user); err != nil {
		t.Fatalf("查资格失败：%v", err)
	} else if entitlement.Active {
		t.Fatal("撤销掉的人还算会员")
	}

	// 四次动作各自留下了流水，且都记着是哪个后台账号做的。
	// 读回来是最新在前，这里翻成发生顺序再看。
	changes := f.changes()
	wantTypes := []string{model.ChangeActivate, model.ChangeFreeze, model.ChangeUnfreeze, model.ChangeRevoke}
	if len(changes) != len(wantTypes) {
		t.Fatalf("有 %d 条流水，期望 %d 条", len(changes), len(wantTypes))
	}
	for i, want := range wantTypes {
		change := changes[len(changes)-1-i]
		if change.ChangeType != want {
			t.Fatalf("第 %d 条流水是 %s，期望 %s（全部流水：%s）",
				i+1, change.ChangeType, want, changeTypes(changes))
		}
		// 开通那条是系统记的，其余三条是后台点出来的。
		if i == 0 {
			continue
		}
		if change.OperatorType != model.OperatorAdmin {
			t.Fatalf("第 %d 条流水（%s）的操作人类型是 %q，期望 admin",
				i+1, change.ChangeType, change.OperatorType)
		}
		if change.OperatorID == nil || change.Reason == "" {
			t.Fatalf("第 %d 条流水（%s）没留下操作人或理由", i+1, change.ChangeType)
		}
	}
}

// TestIntegrationAdjustExpireAt 钉住后台那个破坏力最大的动作：它没有订单、没有支付跟着发生，
// 只是把一个人的 expire_at 挪一下。
//
// 两条：改到开通时间之前要被拒（那不是「调整」而是「这条会员压根不成立」），改到未来要真的
// 生效并留下一条 admin_adjust 流水。
func TestIntegrationAdjustExpireAt(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock)

	opened := f.membership()

	// 早于 start_at：拒。
	_, err := f.svc.AdjustExpireAt(ctx, opened.ID, dto.AdjustRequest{
		ExpireAt: opened.StartAt.Add(-24 * time.Hour),
		Reason:   "对账 2026-09 工单",
	}, f.actor())
	if !errors.Is(err, ErrExpireBeforeStart) {
		t.Fatalf("把到期时间改到开通时间之前回的是 %v，期望 ErrExpireBeforeStart", err)
	}

	// 没有理由：拒（那是一次没有任何解释的权益变更）。
	if _, err := f.svc.AdjustExpireAt(ctx, opened.ID, dto.AdjustRequest{
		ExpireAt: f.clock.AddDate(1, 0, 0),
	}, f.actor()); !errors.Is(err, ErrAdjustReasonRequired) {
		t.Fatalf("没填理由回的是 %v，期望 ErrAdjustReasonRequired", err)
	}

	want := f.clock.AddDate(1, 0, 0).UTC()
	adjusted, err := f.svc.AdjustExpireAt(ctx, opened.ID, dto.AdjustRequest{
		ExpireAt: want,
		Reason:   "客服工单 5678：按对账结果补一个月",
		Remark:   "工单链接见 CRM-5678",
	}, f.actor())
	if err != nil {
		t.Fatalf("调整有效期失败：%v", err)
	}
	if !adjusted.ExpireAt.UTC().Equal(want) {
		t.Fatalf("调整后 expire_at = %s，期望 %s", adjusted.ExpireAt.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// 最新的一条就是这次调整（人工动作发生在开通之后）。
	changes := f.changes()
	last := changes[0]
	if last.ChangeType != model.ChangeAdminAdjust {
		t.Fatalf("最新一条流水是 %s，期望 admin_adjust（全部：%s）", last.ChangeType, changeTypes(changes))
	}
	if last.FromExpireAt == nil || !last.FromExpireAt.UTC().Equal(opened.ExpireAt.UTC()) {
		t.Fatalf("调整流水的 from_expire_at 是 %v，期望 %s", last.FromExpireAt, opened.ExpireAt.UTC())
	}
	if last.OperatorType != model.OperatorAdmin || last.OperatorID == nil {
		t.Fatalf("调整流水没记下操作人：%s / %v", last.OperatorType, last.OperatorID)
	}
}

// TestIntegrationAutoRenewCanOnlyBeTurnedOff 钉住 C 端那个开关的方向。
//
// 「开」必须被拒：打开自动续费要一份微信委托代扣协议，而签约那条链路（payment-service 的
// 委托代扣）还没落地。让它在「开」上静默成功的话，用户会看到一个「已开启自动续费」的界面，
// 名下却没有任何协议——下个月不扣款、会员断掉，而他以为系统坏了。
func TestIntegrationAutoRenewCanOnlyBeTurnedOff(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	// 还不是会员：查不到。
	if _, err := f.svc.SetAutoRenewByUser(ctx, f.user, false, uuid.NewString(), uuid.NewString()); !errors.Is(err, ErrMembershipNotFound) {
		t.Fatalf("非会员开关自动续费回的是 %v，期望 ErrMembershipNotFound", err)
	}

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock)

	if _, err := f.svc.SetAutoRenewByUser(ctx, f.user, true, uuid.NewString(), uuid.NewString()); !errors.Is(err, ErrAutoRenewNeedsSigning) {
		t.Fatalf("打开自动续费回的是 %v，期望 ErrAutoRenewNeedsSigning", err)
	}
	// 被拒之后库里不能留下「已开启」的痕迹——那正是这个接口要防的那个界面。
	if f.membership().AutoRenew {
		t.Fatal("被拒的「打开自动续费」还是把开关写开了")
	}
}

// ============================================================
// 到期扫描
// ============================================================

// TestIntegrationExpireDueFlipsStatus 走一遍定时扫描。
//
// 它盯的是一个**时间差**：用户看到的是 expire_at 过没过（entitlement 就是这么判的），而
// status 要等扫描才翻成 expired。所以扫之前那一小段时间里，这个人已经是「不是会员」了但
// status 还是 active——两处都得对，后台列表按 status 筛出来的才是真相。
func TestIntegrationExpireDueFlipsStatus(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)

	// 两个月前买的包月：到期时间落在过去，但还没人扫过它。
	f.mustPay(uuid.NewString(), plan, f.clock.AddDate(0, -2, 0))

	due := f.membership()
	if due.Status != model.MembershipStatusActive {
		t.Fatalf("扫描前状态是 %q，期望还是 active（扫之前没有人翻它）", due.Status)
	}
	if !due.ExpireAt.Before(f.clock) {
		t.Fatalf("这条用例的前提不成立：expire_at=%s 不在过去", due.ExpireAt)
	}
	if entitlement, err := f.svc.MemberPriceEntitlement(ctx, f.user); err != nil {
		t.Fatalf("查资格失败：%v", err)
	} else if entitlement.Active {
		t.Fatal("已经过了 expire_at 的人还算会员：扫描没跑之前也不能算")
	}

	// 这个库是共享的，别的行也可能到期，所以只断言「至少处理了一条」。
	count, err := f.svc.ExpireDue(ctx, 200, uuid.NewString())
	if err != nil {
		t.Fatalf("到期扫描失败：%v", err)
	}
	if count < 1 {
		t.Fatalf("扫描处理了 %d 条，期望至少 1 条", count)
	}

	expired := f.membership()
	if expired.Status != model.MembershipStatusExpired {
		t.Fatalf("扫描后状态是 %q，期望 expired", expired.Status)
	}

	// 最新的一条是这次扫描写下的（扫描用的是真实时钟，晚于两个月前的付款时刻）。
	changes := f.changes()
	last := changes[0]
	if last.ChangeType != model.ChangeExpire {
		t.Fatalf("最新一条流水是 %s，期望 expire（全部：%s）", last.ChangeType, changeTypes(changes))
	}
	// worker 而不是 system：排查时要能一眼看出「这是被扫出来的」还是「被事件推出来的」。
	if last.OperatorType != model.OperatorWorker {
		t.Fatalf("到期流水的操作人类型是 %q，期望 worker", last.OperatorType)
	}

	// 幂等：同一批行扫第二遍选不到它们了（status 已经不是 active）。
	again, err := f.svc.ExpireDue(ctx, 200, uuid.NewString())
	if err != nil {
		t.Fatalf("第二次扫描失败：%v", err)
	}
	if again != 0 {
		t.Fatalf("第二次扫描又处理了 %d 条：扫描不幂等，每次跑都会写一批重复流水", again)
	}
}

// ============================================================
// 库上那几条物理约束
// ============================================================

// TestIntegrationChangeLedgerIsAppendOnly 确认只增不改不删那条防线在 dev 库里真的装着。
//
// 它同时也是上面 cleanup 里那段「摘掉触发器再删」的**前提**：如果这条触发器哪天被人删了，
// 这个用例会红，而 cleanup 里那段就成了一段没人看得懂的多余动作。
func TestIntegrationChangeLedgerIsAppendOnly(t *testing.T) {
	f := newMembershipFixture(t)
	ctx := context.Background()

	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)
	f.mustPay(uuid.NewString(), plan, f.clock)

	if _, err := f.pool.Exec(ctx,
		`UPDATE membership_changes SET reason = 'tampered' WHERE user_id = $1`, f.user); err == nil {
		t.Fatal("membership_changes 能被改：只增不改不删的触发器没装")
	}
	if _, err := f.pool.Exec(ctx,
		`DELETE FROM membership_changes WHERE user_id = $1`, f.user); err == nil {
		t.Fatal("membership_changes 能被删：只增不改不删的触发器没装")
	}

	// 两次尝试都没改成，流水还是原来那条。
	changes := f.changes()
	if len(changes) != 1 || changes[0].Reason == "tampered" {
		t.Fatalf("流水被动过了：%+v", changes)
	}
}

// ============================================================
// 小工具
// ============================================================

// assertOutboxEvent 确认这个用例发过某一种事件。
//
// 事件是这一刀唯一能让别的域知道「会员变了」的途径（会员库没有外键能指过来，别的服务手里
// 只有一个 user_id），所以「事件确实落下来了」本身就是要验的东西——它和业务行在同一个事务里，
// 事务回滚时事件一起没。
func assertOutboxEvent(t *testing.T, f *membershipFixture, eventType string) {
	t.Helper()

	var found bool
	err := f.pool.QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM message_outbox
			WHERE event_type = $1
			  AND convert_from(payload, 'UTF8') LIKE $2
		)`, eventType, fmt.Sprintf("%%%q%%", f.code)).Scan(&found)
	if err != nil {
		t.Fatalf("查 outbox 失败：%v", err)
	}
	if !found {
		t.Fatalf("没有落 %s 事件：下游（发券、别的域）唯一的通知途径就是它", eventType)
	}
}

// ============================================================
// 后台直接开通（会员域唯一的创建入口）
// ============================================================

// TestIntegrationGrantMembership 走一遍后台开通：没有订单，只有操作员在弹窗里选的一个套餐。
//
// 钉住四件事：会员建起来了、归属门店落进去了、流水上记的是 **admin 干的**，以及同一个
// requestId 重发回的是同一条会员而不是「这个人已经是会员了」——最后一条是「网络抖动后的重试
// 会不会变成假警报」的分界。
func TestIntegrationGrantMembership(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)

	store := uuid.NewString()
	actor := f.actor()
	req := dto.GrantRequest{
		UserID:    f.user,
		PlanID:    plan.ID,
		StoreID:   store,
		Reason:    "客服补偿：门店答应的会员",
		RequestID: uuid.NewString(),
	}

	granted, err := f.svc.GrantMembership(context.Background(), req, actor)
	if err != nil {
		t.Fatalf("后台开通失败：%v", err)
	}
	if granted.StoreID != store {
		t.Fatalf("归属门店没落库：想要 %s，得到 %q", store, granted.StoreID)
	}
	if !granted.ExpireAt.After(f.clock) {
		t.Fatalf("没给到期日时应该按套餐算出一个未来时刻，得到 %s", granted.ExpireAt)
	}

	changes := f.changes()
	if len(changes) != 1 {
		t.Fatalf("流水应该只有一条，得到 %d 条：%s", len(changes), changeTypes(changes))
	}
	if changes[0].ChangeType != model.ChangeActivate {
		t.Fatalf("变更类型应该是 %s，得到 %s", model.ChangeActivate, changes[0].ChangeType)
	}
	if changes[0].OperatorType != model.OperatorAdmin {
		t.Fatalf("后台开通的 operator_type 应该是 %s，得到 %s——记成 user 就等于把人工白送钱"+
			"记成了用户自己买的", model.OperatorAdmin, changes[0].OperatorType)
	}
	if changes[0].RequestID != req.RequestID {
		t.Fatalf("幂等键没落进流水：想要 %s，得到 %q", req.RequestID, changes[0].RequestID)
	}
	assertOutboxEvent(t, f, dto.EventMembershipActivated)

	// 同一个 requestId 重发 = 同一次点击的第二次到达。
	replayed, err := f.svc.GrantMembership(context.Background(), req, actor)
	if err != nil {
		t.Fatalf("同一次点击的重发被拒了（操作员会看到假警报）：%v", err)
	}
	if replayed.ID != granted.ID {
		t.Fatalf("重放应该回原来那条会员：想要 %s，得到 %s", granted.ID, replayed.ID)
	}
	if got := len(f.changes()); got != 1 {
		t.Fatalf("重放不该多记流水，得到 %d 条：%s", got, changeTypes(f.changes()))
	}

	// 换一个 requestId = 另一次点击。已有会员（哪怕是同一个人刚建的这条）一律拒。
	req.RequestID = uuid.NewString()
	if _, err := f.svc.GrantMembership(context.Background(), req, actor); !errors.Is(err, ErrMembershipExists) {
		t.Fatalf("已有会员时应该回 ErrMembershipExists，得到 %v", err)
	}
}

// TestIntegrationGrantMembershipRejects 是开通的三道门：没上架的套餐、不是 uuid 的门店、
// 以及没有归属门店时留空串（不是报错——小程序线上买会员就是没有门店）。
func TestIntegrationGrantMembershipRejects(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.createPlan(couponPlanRequest()) // 建出来是草稿，没上架

	req := dto.GrantRequest{
		UserID:    f.user,
		PlanID:    plan.ID,
		Reason:    "客服补偿",
		RequestID: uuid.NewString(),
	}
	if _, err := f.svc.GrantMembership(context.Background(), req, f.actor()); !errors.Is(err, ErrPlanNotSellable) {
		t.Fatalf("没上架的套餐应该回 ErrPlanNotSellable（后台那边是 409），得到 %v", err)
	}

	f.activate(plan)
	req.StoreID = "not-a-uuid"
	if _, err := f.svc.GrantMembership(context.Background(), req, f.actor()); !errors.Is(err, ErrStoreIDInvalid) {
		t.Fatalf("门店 id 不是 uuid 时应该回 ErrStoreIDInvalid，得到 %v", err)
	}

	// 不选门店：开通照样成功，归属是空串。这里 Stores 没装配，checkStore 直接放行——商户域
	// 那一侧的问询由 client.StoreClient 自己的单测钉（它是 lottery-service 的同形拷贝）。
	req.StoreID = ""
	granted, err := f.svc.GrantMembership(context.Background(), req, f.actor())
	if err != nil {
		t.Fatalf("不选门店应该能开通：%v", err)
	}
	if granted.StoreID != "" {
		t.Fatalf("没选门店时归属应该是空串，得到 %q", granted.StoreID)
	}
}

// TestIntegrationStoreOwnershipFreezesThenReopens 钉的是归属门店的固化规则——三条规则里最容易
// 写反的那条。
//
// 判据挂在 renewMembership 里那个 base 分叉上（有效期过了没有），所以用例必须**显式**把时间
// 推过到期日，而不是靠续费自然走到：那是唯一能区分「判据挂对了没有」的写法。
func TestIntegrationStoreOwnershipFreezesThenReopens(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.createPlan(couponPlanRequest())
	f.activate(plan)

	first := uuid.NewString()
	f.mustPayWithStore(uuid.NewString(), plan, f.clock, &first)
	if got := f.membership().StoreID; got != first {
		t.Fatalf("第一次成为会员时应该固化归属：想要 %s，得到 %q", first, got)
	}

	// 规则 2：还在有效期内，换一家店买（提前续费 / 升级 / 换套餐都是这一支）——归属不动。
	second := uuid.NewString()
	f.mustPayWithStore(uuid.NewString(), plan, f.clock, &second)
	if got := f.membership().StoreID; got != first {
		t.Fatalf("有效期内续费不该改归属：想要 %s，得到 %q", first, got)
	}

	// 规则 3 的例外：上一段会员关系结束了，这一次是新的拉新。
	f.clock = f.clock.AddDate(0, 2, 0)
	third := uuid.NewString()
	f.mustPayWithStore(uuid.NewString(), plan, f.clock, &third)
	if got := f.membership().StoreID; got != third {
		t.Fatalf("过期后重新开通应该改归属：想要 %s，得到 %q", third, got)
	}

	// 过期重开但这一单没有门店（小程序线上单）：**保留原值**，不是清空——「这一次没有归属可定」
	// 与「这个人的归属没了」是两件事。
	f.clock = f.clock.AddDate(0, 2, 0)
	f.mustPayWithStore(uuid.NewString(), plan, f.clock, nil)
	if got := f.membership().StoreID; got != third {
		t.Fatalf("没有门店的单不该把归属擦掉：想要 %s，得到 %q", third, got)
	}
}
