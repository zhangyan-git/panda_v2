package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/membership-service/internal/repository"
)

// 连续包月签约的三条路：小程序的「发起 + 确认」、后台的「同步」、支付域推过来的协议事件。
//
// 三条路落的是同一个结论（repository.SettleSubscription），所以这一份用例盯的是**它们的分歧
// 处**：什么时候该动本地、什么时候必须一动不动、动的时候写进去的是哪几个值。
//
// # 渠道是假的，库是真的
//
// 签约要素（协议号、openid）用假实现喂（见 fakeAgreements / fakeWallets），其余全走真库：
// 状态机、next_charge_at 的算法、幂等索引、只增不改的流水，它们的错都只在真库上才浮出来
// （理由与 membership_integration_test.go 开头那段一样）。
//
// 门禁同上：没有 MEMBERSHIP_DATABASE_URL 就 skip。

// ============================================================
// 假的渠道与身份域
// ============================================================

// fakeAgreements 是 AgreementGateway 的假实现：它不认识微信，只记住被要求做了什么、按脚本回答。
//
// 记住调用次数是这一份里最要紧的一件事：**「同一次点击的重试不该再建一份协议」只能靠
// 「Create 被叫了几次」验**，库里看不出第二次调用（支付服务那边幂等，回来的还是同一份）。
type fakeAgreements struct {
	creates []dto.CreateAgreementParams
	queries []string
	// createResult / createErr 是 Create 的脚本；为零值时回一份像模像样的协议号。
	createResult *dto.AgreementSigningResult
	createErr    error
	// queryResult / queryErr 是 Query 的脚本。零值等于「渠道没答上来」——那不该被当成「没签成」，
	// 所以这里不给它一个像样的默认值。
	queryResult *dto.AgreementState
	queryErr    error
	// charges / terminates 记下代扣与解约被要求的每一次，脚本同上：调用次数本身就是判据
	// （「同一期只发一次」只能靠数出来）。
	charges      []dto.ChargeAgreementParams
	chargeErr    error
	terminates   []dto.TerminateAgreementParams
	terminate    *dto.AgreementTermination
	terminateErr error
}

func (f *fakeAgreements) Create(_ context.Context, in dto.CreateAgreementParams) (*dto.AgreementSigningResult, error) {
	f.creates = append(f.creates, in)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResult != nil {
		return f.createResult, nil
	}
	return &dto.AgreementSigningResult{
		AgreementID: uuid.NewString(),
		AgreementNo: "AGR-" + uuid.NewString()[:8],
		Status:      dto.AgreementStatusPending,
		Action:      "jump_miniapp",
		PayParams:   map[string]string{"sign": "fake", "plan_id": in.ProviderPlanID},
	}, nil
}

func (f *fakeAgreements) Query(_ context.Context, agreementNo, _ string) (*dto.AgreementState, error) {
	f.queries = append(f.queries, agreementNo)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.queryResult == nil {
		return nil, errors.New("fakeAgreements: Query 没有被脚本化")
	}
	return f.queryResult, nil
}

// Charge 的默认答案是 **charging**，不是 succeeded：渠道同步回的受理只说明请求被收下了
// （见 dto.AgreementChargeResult 的说明）。给假实现一个「默认扣到了钱」的答案，会让每一个忘了
// 脚本化的用例都恰好走在那条最不该被默认的路径上。
func (f *fakeAgreements) Charge(_ context.Context, in dto.ChargeAgreementParams) (*dto.AgreementChargeResult, error) {
	f.charges = append(f.charges, in)
	if f.chargeErr != nil {
		return nil, f.chargeErr
	}
	return &dto.AgreementChargeResult{
		AgreementNo:           in.AgreementNo,
		BizPeriod:             in.BizPeriod,
		Status:                dto.ChargeStatusCharging,
		ProviderTransactionID: "TXN-" + uuid.NewString()[:8],
	}, nil
}

// Terminate 照脚本回；没脚本时回一份「解约成功」。与 Charge 不同，这里给一个像样的默认值：
// 解约这条路没做成是**要显式脚本化的异常**，而一个忘了脚本化的用例默认落在「渠道说解约好了」
// 上，不会掩盖「本该解约却没解」这种错。
func (f *fakeAgreements) Terminate(_ context.Context, in dto.TerminateAgreementParams) (*dto.AgreementTermination, error) {
	f.terminates = append(f.terminates, in)
	if f.terminateErr != nil {
		return nil, f.terminateErr
	}
	if f.terminate != nil {
		return f.terminate, nil
	}
	return &dto.AgreementTermination{
		AgreementNo: in.AgreementNo,
		Status:      dto.AgreementStatusTerminated,
	}, nil
}

// fakeWallets 是 WalletOpenIDReader 的假实现。found=false 是「这个人没绑小程序」，不是故障。
type fakeWallets struct {
	openID string
	found  bool
	err    error
	// asked 记下被问过几次——重放那条路**必须**再问一次（它要拿 openid 重发一次 Create）。
	asked int
}

func (f *fakeWallets) MiniappOpenID(_ context.Context, _ string) (string, bool, error) {
	f.asked++
	return f.openID, f.found, f.err
}

// wireSigning 把业务层重装一遍，接上假的渠道、身份域与订单域。
//
// 重装而不是给 fixture 加字段：绝大多数用例与签约无关（它们连 Agreements 都不该有），把这三个
// 依赖塞进每个用例的家当里，会让「没接渠道的进程」这个真实存在的情形（比如只跑后台读路径的
// 进程）在测试里失去代表。
//
// orders 可以是 nil，而那是**有意义的一格**：签约与后台读路径都不用它，只有扣款成功那一支用
// （见 renewal_order_integration_test.go）。哪个用例接、哪个用例不接，本身就是被钉住的行为之一。
func (f *membershipFixture) wireSigning(agreements AgreementGateway, wallets WalletOpenIDReader, orders OrderGateway) {
	f.t.Helper()
	f.svc = New(repository.NewPostgresRepository(f.pool, nil), Options{
		Now:        func() time.Time { return f.clock },
		Agreements: agreements,
		Wallets:    wallets,
		Orders:     orders,
	})
}

// signedPlan 造一个**能签**的套餐：auto_renew 打开、带渠道模板 id。
//
// 与 autoPlanRequest 分开：那一个是「年度会员、会员价直接算」，auto_renew 只是它的一个属性；
// 签约用例要的套餐还必须真的走到上架（不能签下架的套餐是另一条判据）。
func (f *membershipFixture) signedPlan() *model.Plan {
	f.t.Helper()
	// 带后缀：seedMembership / createPlan 已经用掉 f.code 了（套餐编码上有唯一索引）。
	plan := f.createSuffixedPlan(dto.PlanRequest{
		Name:            "连续包月（签约集成测试）",
		PriceCents:      990,
		Period:          model.PeriodMonth,
		PeriodCount:     1,
		AutoRenew:       true,
		WechatPlanID:    "214488",
		MemberPriceMode: model.MemberPriceModeAuto,
	}, "_sign")
	f.activate(plan)
	return plan
}

// subscribe 发起一次签约，失败即终止。
func (f *membershipFixture) subscribe(plan *model.Plan, requestID string) *dto.SubscriptionSigningResponse {
	f.t.Helper()
	response, err := f.svc.CreateSubscription(context.Background(), f.user, dto.CreateSubscriptionRequest{
		PlanID:    plan.ID,
		RequestID: requestID,
	}, uuid.NewString())
	if err != nil {
		f.t.Fatalf("发起签约失败：%v", err)
	}
	return response
}

// changeOfType 从流水里挑出某一类的第一条（没有就回 nil）。
//
// 这些用例不按「第 N 条」断言：同一秒里发生的两条流水按 occurred_at DESC 排，谁在前是任意的
// （fixture 的时钟不自己走），照位置断言会变成一条时灵时不灵的用例。
func (f *membershipFixture) changeOfType(changeType string) *model.Change {
	f.t.Helper()
	for _, change := range f.changes() {
		if change.ChangeType == changeType {
			return change
		}
	}
	return nil
}

// countOfType 数一数流水里有几条这一类。
func countOfType(changes []*model.Change, changeType string) int {
	count := 0
	for _, change := range changes {
		if change.ChangeType == changeType {
			count++
		}
	}
	return count
}

// findByRequest 用流水上的幂等键反查那一条订阅（服务的重放路径就是这么找的）。
func (f *membershipFixture) findByRequest(requestID string) *repository.SubscriptionRow {
	f.t.Helper()
	row, err := f.svc.repository.FindSubscriptionByChangeRequest(context.Background(), f.user, requestID)
	if err != nil {
		f.t.Fatalf("按 requestId 反查订阅失败：%v", err)
	}
	return row
}

// subscription 读回一条订阅（按 id）。
func (f *membershipFixture) subscription(id string) *repository.SubscriptionRow {
	f.t.Helper()
	row, err := f.svc.repository.GetSubscription(context.Background(), id)
	if err != nil {
		f.t.Fatalf("读订阅失败：%v", err)
	}
	return row
}

// agreementsOf 造一份假的渠道 + 假的身份域，两个都挂上，返回它们供断言。
func (f *membershipFixture) agreementsOf() (*fakeAgreements, *fakeWallets) {
	f.t.Helper()
	agreements := &fakeAgreements{}
	wallets := &fakeWallets{openID: "o_" + uuid.NewString()[:12], found: true}
	// 订单域不挂：签约这条路上一个订单都不会建（订单是扣款成功那一刻的事）。
	f.wireSigning(agreements, wallets, nil)
	return agreements, wallets
}

// agreementEvent 投一条协议事件给消费者。
//
// payload **手写 JSON**，与 pay 那条同一个理由：这一层要钉的正是两边 json tag 对得上
// （消费者用 DisallowUnknownFields 解它），拿自己的结构体序列化再解回来是个自证。
func (f *membershipFixture) agreementEvent(eventType, agreementID, agreementNo, contractNo, userID, status string) error {
	f.t.Helper()
	payload := fmt.Sprintf(`{
		"agreementId": %q,
		"agreementNo": %q,
		"contractNo": %q,
		"userId": %q,
		"status": %q
	}`, agreementID, agreementNo, contractNo, userID, status)

	return f.svc.HandleEvent(context.Background(), messaging.Envelope{
		EventID:      uuid.NewString(),
		EventType:    eventType,
		EventVersion: dto.EventVersion,
		TraceID:      uuid.NewString(),
		Payload:      []byte(payload),
	})
}

// ============================================================
// 发起签约
// ============================================================

// TestIntegrationSubscribeLandsPendingSign 钉住发起签约落下来的这一行是什么样。
//
// 四件事：状态是 pending_sign（**不是 active**：用户还没在微信里点同意）、协议号此刻就写进
// contract_code（事后回渠道查这份协议的唯一钥匙）、价格与模板 id 是**签约那一刻的快照**、
// 流水里那一条 subscribe 带着这次点击的幂等号。
func TestIntegrationSubscribeLandsPendingSign(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	plan := f.signedPlan()
	agreements, wallets := f.agreementsOf()

	requestID := uuid.NewString()
	response := f.subscribe(plan, requestID)

	if got := response.Subscription.Status; got != model.SubscriptionStatusPendingSign {
		t.Fatalf("发起签约后的状态 = %q，想要 %q（用户还没点同意）", got, model.SubscriptionStatusPendingSign)
	}
	if got := response.Action; got != "jump_miniapp" {
		t.Errorf("action = %q，想要 jump_miniapp（客户端只 switch 它）", got)
	}
	if len(response.PayParams) == 0 {
		t.Error("payParams 是空的：客户端拿不到跳转参数")
	}

	// 交给渠道的签约要素：模板与金额只能来自套餐，openid 只能来自身份域。
	if len(agreements.creates) != 1 {
		t.Fatalf("Create 被调了 %d 次，想要 1 次", len(agreements.creates))
	}
	created := agreements.creates[0]
	if created.ProviderPlanID != plan.WechatPlanID {
		t.Errorf("交给渠道的模板 id = %q，想要套餐上的 %q", created.ProviderPlanID, plan.WechatPlanID)
	}
	if created.MaxChargeAmount != plan.PriceCents {
		t.Errorf("单次扣款上限 = %d，想要套餐价 %d", created.MaxChargeAmount, plan.PriceCents)
	}
	if created.WalletOpenID != wallets.openID {
		t.Errorf("签约用的 openid = %q，想要身份域给的 %q", created.WalletOpenID, wallets.openID)
	}
	if created.RequestID != requestID {
		t.Errorf("交给支付服务的幂等号 = %q，想要 %q", created.RequestID, requestID)
	}

	row := f.subscription(response.Subscription.ID)
	if row.AgreementID == nil || *row.AgreementID == "" {
		t.Error("订阅上没有 agreement_id：事件与同步都靠它命中这一行")
	}
	if row.ContractCode == "" {
		t.Error("订阅上没有 contract_code：签约这一刻就该有，它是事后回渠道查协议的钥匙")
	}
	if row.PriceCents != plan.PriceCents {
		t.Errorf("冻在订阅上的价格 = %d，想要 %d", row.PriceCents, plan.PriceCents)
	}
	if row.WechatPlanID != plan.WechatPlanID {
		t.Errorf("冻在订阅上的模板 id = %q，想要 %q", row.WechatPlanID, plan.WechatPlanID)
	}
	if row.NextChargeAt != nil {
		t.Errorf("pending_sign 不该有下一期扣款时间，实际是 %v", row.NextChargeAt)
	}

	// 流水多了一条 subscribe，带这次点击的幂等号（重放靠它反查）。
	//
	// **不按「第几条」找它**：这条用例里 activate 与 subscribe 的 occurred_at 是同一个时刻
	// （fixture 的时钟不自己走），ListChanges 按 occurred_at DESC 排，谁在前是任意的。按类型
	// 挑再断言条数，才是在钉「记了什么」而不是「数据库碰巧怎么排」。
	signed := f.changeOfType(model.ChangeSubscribe)
	if signed == nil {
		t.Fatalf("流水 = %s，里面没有 %s", changeTypes(f.changes()), model.ChangeSubscribe)
	}
	if signed.RequestID != requestID {
		t.Errorf("流水上的幂等号 = %q，想要 %q", signed.RequestID, requestID)
	}
	if signed.OperatorType != model.OperatorSystem {
		t.Errorf("签约流水的操作人类型 = %q，想要 %q（写下这一行的是服务端编排）",
			signed.OperatorType, model.OperatorSystem)
	}
}

// TestIntegrationSubscribeReplayReusesTheSubscription 钉住「同一次点击的第二次」。
//
// 它**不能在库里多出一行**，也不能在渠道那边多建一份协议：第二次 Create 会带上与第一次一模一
// 样的要素（支付服务按请求哈希幂等，换一个值就得到冲突），回来的还是同一份协议。
//
// 用的是**已冻的快照**而不是套餐现值：这条用例中途把套餐改了价，重放拿到的必须还是签约那一刻
// 的 990，否则支付服务判成「同一个幂等号换了请求体」。
func TestIntegrationSubscribeReplayReusesTheSubscription(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	plan := f.signedPlan()
	agreements, _ := f.agreementsOf()

	requestID := uuid.NewString()
	first := f.subscribe(plan, requestID)

	// 套餐调价 + 换模板：重放不该跟着变。
	updated := plan.PriceCents + 1000
	changedModel := plan.WechatPlanID + "9"
	if _, err := f.svc.UpdatePlan(context.Background(), plan.ID, dto.PlanRequest{
		Name: plan.Name, PriceCents: updated, Period: plan.Period, PeriodCount: plan.PeriodCount,
		AutoRenew: true, WechatPlanID: changedModel, MemberPriceMode: model.MemberPriceModeAuto,
	}); err != nil {
		t.Fatalf("改套餐失败：%v", err)
	}

	second := f.subscribe(plan, requestID)

	if second.Subscription.ID != first.Subscription.ID {
		t.Fatalf("重放回了另一条订阅：%s ≠ %s", second.Subscription.ID, first.Subscription.ID)
	}
	if len(agreements.creates) != 2 {
		t.Fatalf("Create 被调了 %d 次，想要 2 次（重放要重新领一份跳转参数，但同一份协议）",
			len(agreements.creates))
	}
	replay := agreements.creates[1]
	if replay.MaxChargeAmount != plan.PriceCents {
		t.Errorf("重放带去的金额 = %d，想要**已冻的** %d（拿新价去重发会得到一次冲突）",
			replay.MaxChargeAmount, plan.PriceCents)
	}
	if replay.ProviderPlanID != plan.WechatPlanID {
		t.Errorf("重放带去的模板 id = %q，想要**已冻的** %q", replay.ProviderPlanID, plan.WechatPlanID)
	}

	// 重放**不写库**：subscribe 那一条还是只有一条（activate 是 seedMembership 买会员留下的）。
	if changes := f.changes(); countOfType(changes, model.ChangeSubscribe) != 1 {
		t.Fatalf("重放后流水 = %s，想要只有一条 %s", changeTypes(changes), model.ChangeSubscribe)
	}
}

// TestIntegrationSubscribeRefusesWhatCannotBeSigned 是发起签约的四道闸门。
//
// 每一条挡的都是一种钱上的错，所以分开断言而不是合成一个：下架的套餐不能签（那是我们已经决定
// 不卖的东西）、不能自动续费的套餐不能签（渠道那份协议没有模板可挂）、没绑小程序身份的人不能签
// （签的是谁说不清）、**已经有一条活订阅的人不能签**（这一条是「名下凭空多出第二份授权」）。
func TestIntegrationSubscribeRefusesWhatCannotBeSigned(t *testing.T) {
	t.Run("下架的套餐", func(t *testing.T) {
		f := newMembershipFixture(t)
		f.seedMembership(t)
		plan := f.signedPlan()
		if _, err := f.svc.SetPlanStatus(context.Background(), plan.ID, model.PlanStatusDisabled); err != nil {
			t.Fatalf("下架套餐失败：%v", err)
		}
		f.agreementsOf()

		_, err := f.svc.CreateSubscription(context.Background(), f.user,
			dto.CreateSubscriptionRequest{PlanID: plan.ID, RequestID: uuid.NewString()}, uuid.NewString())
		if !errors.Is(err, ErrPlanNotSellable) {
			t.Fatalf("下架套餐签约的错误 = %v，想要 ErrPlanNotSellable", err)
		}
	})

	t.Run("不能自动续费的套餐", func(t *testing.T) {
		f := newMembershipFixture(t)
		f.seedMembership(t)
		// 券模式那一个：auto_renew=false、没有微信模板 id。
		plan := f.createSuffixedPlan(couponPlanRequest(), "_coupon")
		f.activate(plan)
		f.agreementsOf()

		_, err := f.svc.CreateSubscription(context.Background(), f.user,
			dto.CreateSubscriptionRequest{PlanID: plan.ID, RequestID: uuid.NewString()}, uuid.NewString())
		if !errors.Is(err, ErrSubscriptionPlanNotSubscription) {
			t.Fatalf("非连续包月套餐签约的错误 = %v，想要 ErrSubscriptionPlanNotSubscription", err)
		}
	})

	t.Run("没绑小程序身份", func(t *testing.T) {
		f := newMembershipFixture(t)
		f.seedMembership(t)
		plan := f.signedPlan()
		f.wireSigning(&fakeAgreements{}, &fakeWallets{found: false}, nil)

		_, err := f.svc.CreateSubscription(context.Background(), f.user,
			dto.CreateSubscriptionRequest{PlanID: plan.ID, RequestID: uuid.NewString()}, uuid.NewString())
		if !errors.Is(err, ErrWalletIdentityRequired) {
			t.Fatalf("没绑小程序身份的错误 = %v，想要 ErrWalletIdentityRequired", err)
		}
	})

	t.Run("已经有一条活订阅", func(t *testing.T) {
		f := newMembershipFixture(t)
		f.seedMembership(t)
		plan := f.signedPlan()
		agreements, _ := f.agreementsOf()
		f.subscribe(plan, uuid.NewString())

		_, err := f.svc.CreateSubscription(context.Background(), f.user,
			dto.CreateSubscriptionRequest{PlanID: plan.ID, RequestID: uuid.NewString()}, uuid.NewString())
		if !errors.Is(err, ErrLiveSubscriptionExists) {
			t.Fatalf("重复签约的错误 = %v，想要 ErrLiveSubscriptionExists", err)
		}
		// 顺带钉住更重要的一件事：**这一次没有再去渠道建协议**。判据排在 Create 之前，晚一步
		// 就是微信里多出一份用户能点开的待签协议。
		if len(agreements.creates) != 1 {
			t.Fatalf("Create 被调了 %d 次，想要 1 次（第二次该在本地就被挡下）", len(agreements.creates))
		}
	})

	t.Run("没有幂等号", func(t *testing.T) {
		f := newMembershipFixture(t)
		f.seedMembership(t)
		plan := f.signedPlan()
		f.agreementsOf()

		_, err := f.svc.CreateSubscription(context.Background(), f.user,
			dto.CreateSubscriptionRequest{PlanID: plan.ID}, uuid.NewString())
		if !errors.Is(err, ErrSubscriptionRequestIDRequired) {
			t.Fatalf("缺幂等号的错误 = %v，想要 ErrSubscriptionRequestIDRequired", err)
		}
	})
}

// ============================================================
// 确认签约
// ============================================================

// TestIntegrationConfirmActivatesAndSetsNextCharge 走一遍「用户从微信回来」。
//
// 断言的是收口之后的三个值：状态 active、next_charge_at 就是**会员到期那一刻**（见
// repository.applySettle 那条规则）、以及**会员本身没有被这次签约改动**（签约只让「以后可以
// 扣款」成立，钱是另一件事）。
func TestIntegrationConfirmActivatesAndSetsNextCharge(t *testing.T) {
	f := newMembershipFixture(t)
	plan := f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	row := f.subscription(sub.Subscription.ID)

	membershipBefore := f.membership()
	agreements.queryResult = &dto.AgreementState{
		AgreementNo:   row.ContractCode,
		Status:        dto.AgreementStatusActive,
		ProviderState: "signed",
	}

	confirmed, err := f.svc.ConfirmSubscription(context.Background(), row.ID, f.user, uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("确认签约失败：%v", err)
	}
	if confirmed.Status != model.SubscriptionStatusActive {
		t.Fatalf("确认后的状态 = %q，想要 %q", confirmed.Status, model.SubscriptionStatusActive)
	}

	after := f.subscription(row.ID)
	if after.NextChargeAt == nil {
		t.Fatal("生效之后没有下一期扣款时间（库上 active 时它是 NOT NULL 的 CHECK）")
	}
	// 有会员且还没过期 ⇒ 第一次扣款就落在**会员到期那一刻**：那一期是用户已经付过钱的，续费
	// 必须在它结束的当口接上，代扣成功之后再把到期日往后推一期（见 charge.go）。
	//
	// 这里曾经断言的是「到期日再往后推一个周期」，比正确值晚了整整一期：从到期日到那一刻用户
	// 持币却没有会员，而扫描器那一刻才看得见这一行——自动续费开着，中间断了一期。取「此刻 +
	// 一个周期」同样是错的：那会把已经买过、还没用完的这段会员吞掉。
	if !after.NextChargeAt.Equal(membershipBefore.ExpireAt) {
		t.Errorf("下一期扣款时间 = %v，想要会员到期日 %v（第一次扣的就是到期那一刻该续的那一期）",
			after.NextChargeAt.UTC(), membershipBefore.ExpireAt.UTC())
	}

	if now := f.membership(); !now.ExpireAt.Equal(membershipBefore.ExpireAt) {
		t.Errorf("签约改动了会员到期日：%v → %v（签约只让「以后可以扣款」成立，不是一次续期）",
			membershipBefore.ExpireAt.UTC(), now.ExpireAt.UTC())
	}
	_ = plan

	// 第二次确认**不再问渠道**：已经是终态了（见 ConfirmSubscription 的短路）。
	asked := len(agreements.queries)
	if _, err := f.svc.ConfirmSubscription(context.Background(), row.ID, f.user, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("重复确认报错了：%v", err)
	}
	if len(agreements.queries) != asked {
		t.Errorf("重复确认又问了渠道一次（%d → %d），它该按「已经是这个状态」短路",
			asked, len(agreements.queries))
	}
}

// TestIntegrationConfirmWithLapsedMembershipStartsFromNow 是上面那条用例的另一半：会员**已经
// 过期**时签约，第一次扣款落在「这一刻 + 一个周期」。
//
// 两半合起来才是 applySettle 那条规则的全部（有没有权益可接，决定取哪个基点），所以它们必须
// 一起看：只钉住前一半的话，把整条规则写死成「到期日 + 一个周期」或「这一刻 + 一个周期」都能
// 让前一半之外的那一格悄悄地错。
//
// 这一刻而不是立刻扣，是有意的：签约本身一分钱都不动（用户刚在微信点完同意，再扣他一笔是另
// 一件事），而这一期从第一次扣款那一刻起算。
func TestIntegrationConfirmWithLapsedMembershipStartsFromNow(t *testing.T) {
	f := newMembershipFixture(t)
	// 两个月前买的包月会员：一个月前就到期了。行还挂在库里（签约要求先有会员），只是此刻没有
	// 任何权益可接——这正是这一格与上一格的分界。
	lapsed := f.createPlan(couponPlanRequest())
	f.activate(lapsed)
	f.mustPay(uuid.NewString(), lapsed, f.clock.AddDate(0, -2, 0))

	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	row := f.subscription(sub.Subscription.ID)
	if before := f.membership(); before.ExpireAt.After(f.clock) {
		t.Fatalf("这一格要求会员已经过期，实际到期日是 %v（此刻 %v）", before.ExpireAt.UTC(), f.clock.UTC())
	}
	agreements.queryResult = &dto.AgreementState{
		AgreementNo:   row.ContractCode,
		Status:        dto.AgreementStatusActive,
		ProviderState: "signed",
	}

	if _, err := f.svc.ConfirmSubscription(context.Background(), row.ID, f.user, uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("确认签约失败：%v", err)
	}

	after := f.subscription(row.ID)
	if after.NextChargeAt == nil {
		t.Fatal("生效之后没有下一期扣款时间（库上 active 时它是 NOT NULL 的 CHECK）")
	}
	want := model.AddPeriod(f.clock, after.Period, after.PeriodCount)
	if !after.NextChargeAt.Equal(want) {
		t.Errorf("下一期扣款时间 = %v，想要 %v（没有剩余权益 ⇒ 这一刻 + 一个周期 %s×%d）",
			after.NextChargeAt.UTC(), want.UTC(), after.Period, after.PeriodCount)
	}
}

// TestIntegrationConfirmWhenTheChannelSaysPending 钉住「渠道还在等用户点同意」。
//
// **什么都不动**是这个用例的全部要点：把一份用户其实已经签好的协议在本地标成没签成，比晚一天
// 知道结果糟得多。所以断言的是状态还是 pending_sign，而不是「回了一句错误」。
func TestIntegrationConfirmWhenTheChannelSaysPending(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	agreements.queryResult = &dto.AgreementState{
		Status:        dto.AgreementStatusPending,
		ProviderState: "pending",
	}

	confirmed, err := f.svc.ConfirmSubscription(context.Background(), sub.Subscription.ID, f.user,
		uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("渠道说还等着时报错了：%v", err)
	}
	if confirmed.Status != model.SubscriptionStatusPendingSign {
		t.Fatalf("状态 = %q，想要原样停在 %q", confirmed.Status, model.SubscriptionStatusPendingSign)
	}
	if row := f.subscription(sub.Subscription.ID); row.NextChargeAt != nil {
		t.Errorf("还没签成就有下一期扣款时间了：%v", row.NextChargeAt)
	}
}

// TestIntegrationConfirmRefusesSomeoneElsesSubscription 钉住归属。
//
// 别人的订阅回 404（**不是 403**：403 会说「这条存在，只是不是你的」）。一个人不能替别人确认
// 签约——那一步决定的是「谁的微信会被扣款」。
func TestIntegrationConfirmRefusesSomeoneElsesSubscription(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	agreements.queryResult = &dto.AgreementState{
		AgreementNo: f.subscription(sub.Subscription.ID).ContractCode,
		Status:      dto.AgreementStatusActive,
	}

	_, err := f.svc.ConfirmSubscription(context.Background(), sub.Subscription.ID, uuid.NewString(),
		uuid.NewString(), uuid.NewString())
	if !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("别人的订阅确认的错误 = %v，想要 ErrSubscriptionNotFound", err)
	}
	if len(agreements.queries) != 0 {
		t.Error("归属不符时还去问了渠道")
	}
}

// ============================================================
// 协议事件
// ============================================================

// TestIntegrationAgreementEventDrivesTheSubscription 是支付域推过来的那两条。
//
// 这条路上没有人可以问（事件是已经发生的事实），所以幂等全靠目标状态本身：同一条 signed 投两
// 次，第二次必须是「没改动」而不是「又续了一期」。终止那一条同时钉住 cancel_reason——它在后台
// 详情里直接显示，是排查时唯一能看出「这不是人手动取消的」的东西。
func TestIntegrationAgreementEventDrivesTheSubscription(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	row := f.subscription(sub.Subscription.ID)
	contractNo := "WX-" + uuid.NewString()[:8]

	signed := func() error {
		return f.agreementEvent(dto.EventAgreementSigned, *row.AgreementID, row.ContractCode, contractNo,
			f.user, dto.AgreementStatusActive)
	}

	if err := signed(); err != nil {
		t.Fatalf("消费签约事件失败：%v", err)
	}
	after := f.subscription(row.ID)
	if after.Status != model.SubscriptionStatusActive {
		t.Fatalf("签约事件之后的状态 = %q，想要 %q", after.Status, model.SubscriptionStatusActive)
	}
	if after.NextChargeAt == nil {
		t.Fatal("签约事件之后没有下一期扣款时间")
	}
	firstNext := *after.NextChargeAt

	// 重投：状态已经等于目标，仓储短路，**下一次扣款时间必须一动不动**。跟着重投往后推一期的
	// 话，用户每一次重投都被多送一个月的续费窗口。
	if err := signed(); err != nil {
		t.Fatalf("重投签约事件失败：%v", err)
	}
	replayed := f.subscription(row.ID)
	if !replayed.NextChargeAt.Equal(firstNext) {
		t.Errorf("重投把下一期扣款时间改了：%v → %v", firstNext.UTC(), replayed.NextChargeAt.UTC())
	}
	if replayed.Status != model.SubscriptionStatusActive {
		t.Fatalf("重投之后的状态 = %q", replayed.Status)
	}

	// 解约事件。
	if err := f.agreementEvent(dto.EventAgreementTerminated, *row.AgreementID, row.ContractCode,
		contractNo, f.user, dto.AgreementStatusTerminated); err != nil {
		t.Fatalf("消费解约事件失败：%v", err)
	}
	cancelled := f.subscription(row.ID)
	if cancelled.Status != model.SubscriptionStatusCancelled {
		t.Fatalf("解约事件之后的状态 = %q，想要 %q", cancelled.Status, model.SubscriptionStatusCancelled)
	}
	if cancelled.CancelAt == nil {
		t.Error("解约之后没有取消时间（库上 cancelled 时它是 NOT NULL 的 CHECK）")
	}
	if cancelled.CancelReason != "微信侧已解约" {
		t.Errorf("取消原因 = %q，想要「微信侧已解约」（后台详情里直接显示这一列）", cancelled.CancelReason)
	}
	if cancelled.CancelledBy != nil && *cancelled.CancelledBy != "" {
		t.Errorf("取消人 = %q，想要空（解约是渠道那边发生的，不是后台某个人点的）", *cancelled.CancelledBy)
	}
}

// TestIntegrationAgreementEventForAnUnknownAgreementIsAcked 钉住「这份协议名下没有订阅」。
//
// **ack 而不是报错**：协议是支付域的通用能力，将来别的域也可能签，为一条不是本域的事件让整条
// 队列堵在重投上不划算。
func TestIntegrationAgreementEventForAnUnknownAgreementIsAcked(t *testing.T) {
	f := newMembershipFixture(t)
	f.agreementsOf()

	err := f.agreementEvent(dto.EventAgreementSigned, uuid.NewString(), "AGR-nobody", "", f.user,
		dto.AgreementStatusActive)
	if err != nil {
		t.Fatalf("没有对应订阅的协议事件报错了：%v（它该被 ack 掉）", err)
	}
}

// TestIntegrationAgreementEventRejectsBadPayloads 钉住「不合法就进死信」。
//
// 这些都不是「重投就能好」的：状态词不认识说明发出方加了第三种状态而我们按两种理解，报错让它
// 停在有人看得见的地方；事件体解不开同理。**不能 ack** ——静默吞掉会让订阅永远停在错的档位上。
func TestIntegrationAgreementEventRejectsBadPayloads(t *testing.T) {
	f := newMembershipFixture(t)
	f.agreementsOf()

	cases := []struct {
		what    string
		payload string
	}{
		{"状态词不认识", `{"agreementId":"` + uuid.NewString() + `","agreementNo":"A","contractNo":"","userId":"` + f.user + `","status":"suspended"}`},
		{"协议 id 不是 uuid", `{"agreementId":"not-a-uuid","agreementNo":"A","contractNo":"","userId":"` + f.user + `","status":"active"}`},
		{"用户 id 不是 uuid", `{"agreementId":"` + uuid.NewString() + `","agreementNo":"A","contractNo":"","userId":"nope","status":"active"}`},
		{"多了不认识的字段", `{"agreementId":"` + uuid.NewString() + `","agreementNo":"A","contractNo":"","userId":"` + f.user + `","status":"active","extra":1}`},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			err := f.svc.HandleEvent(context.Background(), messaging.Envelope{
				EventID:      uuid.NewString(),
				EventType:    dto.EventAgreementSigned,
				EventVersion: dto.EventVersion,
				TraceID:      uuid.NewString(),
				Payload:      []byte(c.payload),
			})
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("错误 = %v，想要 ErrInvalidEvent（进死信，不能 ack）", err)
			}
		})
	}
}

// ============================================================
// 后台同步
// ============================================================

// TestIntegrationSyncReportsWhetherItChangedAnything 是后台那个「同步」按钮。
//
// 两个结果都是成功，差别在**有没有真的改到本地**：渠道说还等着时 changed=false 且状态原样；
// 渠道说签成了就转 active。后台拿这个 true/false 决定说「已同步」还是「无需更正」。
func TestIntegrationSyncReportsWhetherItChangedAnything(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	row := f.subscription(sub.Subscription.ID)

	// 渠道说还等着：一个字段都不动。
	agreements.queryResult = &dto.AgreementState{Status: dto.AgreementStatusPending, ProviderState: "pending"}
	result, err := f.svc.SyncSubscription(context.Background(), row.ID, f.actor(), uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if result.Changed {
		t.Error("渠道说还等着，changed 却是 true")
	}
	if result.Subscription.Status != model.SubscriptionStatusPendingSign {
		t.Fatalf("状态 = %q，想要原样停在 %q", result.Subscription.Status, model.SubscriptionStatusPendingSign)
	}
	if result.ProviderState != "pending" {
		t.Errorf("providerState = %q，想要渠道的原话 pending", result.ProviderState)
	}

	// 渠道说签成了：转 active，changed=true。
	agreements.queryResult = &dto.AgreementState{
		AgreementNo: row.ContractCode, Status: dto.AgreementStatusActive, ProviderState: "signed",
	}
	result, err = f.svc.SyncSubscription(context.Background(), row.ID, f.actor(), uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("同步失败：%v", err)
	}
	if !result.Changed {
		t.Error("渠道说签成了，changed 却是 false")
	}
	if result.Subscription.Status != model.SubscriptionStatusActive {
		t.Fatalf("状态 = %q，想要 %q", result.Subscription.Status, model.SubscriptionStatusActive)
	}
	if result.Subscription.CancelReason != "" {
		t.Errorf("生效时写了取消原因：%q", result.Subscription.CancelReason)
	}
}

// TestIntegrationSyncUsesTheOperatorAsTheAuditActor 钉住「同步是人点的」。
//
// 与确认（用户点的）和事件（渠道推的）不同，这一条要把**操作人**写进审计，而且解约时写一句
// 人话的原因——老系统那句话照抄。审计落在身份库（admin_operation_logs），所以这里只钉住服务层
// 拿到的那一份 Actor 确实传下去了：AdminID 为空的一路走不通。
func TestIntegrationSyncCancelsAndRecordsTheOperator(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	row := f.subscription(sub.Subscription.ID)

	// 先签成，再解约：guardSettleToActive 只放行 pending_sign，所以直接从 pending 解约与
	// 「签成后又被用户解了」是两条不同的路，这里走后者。
	agreements.queryResult = &dto.AgreementState{
		AgreementNo: row.ContractCode, Status: dto.AgreementStatusActive, ProviderState: "signed",
	}
	if _, err := f.svc.SyncSubscription(context.Background(), row.ID, f.actor(), uuid.NewString(), uuid.NewString()); err != nil {
		t.Fatalf("同步失败：%v", err)
	}

	agreements.queryResult = &dto.AgreementState{
		Status: dto.AgreementStatusTerminated, ProviderState: "terminated",
	}
	result, err := f.svc.SyncSubscription(context.Background(), row.ID, f.actor(), uuid.NewString(), uuid.NewString())
	if err != nil {
		t.Fatalf("解约同步失败：%v", err)
	}
	if !result.Changed || result.Subscription.Status != model.SubscriptionStatusCancelled {
		t.Fatalf("解约同步后 = %q（changed=%v），想要 cancelled", result.Subscription.Status, result.Changed)
	}
	if result.Subscription.CancelReason == "" {
		t.Error("后台同步解约没有写原因")
	}
	if result.Subscription.CancelledBy != "" {
		t.Errorf("取消人 = %q，想要空（留痕在审计里，这一列记的是解约发起人）",
			result.Subscription.CancelledBy)
	}
}

// TestIntegrationSyncRefusesToReviveAFinishedSubscription 钉住那条不变式。
//
// 一条已经结束的订阅**不能**因为渠道此刻说 active 就复活：那意味着给一个已经终止的关系重新
// 打开扣款。渠道那边真要重来，走的是一次新的签约。
func TestIntegrationSyncRefusesToReviveAFinishedSubscription(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	row := f.subscription(sub.Subscription.ID)
	for _, step := range []*dto.AgreementState{
		{AgreementNo: row.ContractCode, Status: dto.AgreementStatusActive, ProviderState: "signed"},
		{Status: dto.AgreementStatusTerminated, ProviderState: "terminated"},
	} {
		agreements.queryResult = step
		if _, err := f.svc.SyncSubscription(context.Background(), row.ID, f.actor(), uuid.NewString(), uuid.NewString()); err != nil {
			t.Fatalf("同步失败：%v", err)
		}
	}

	agreements.queryResult = &dto.AgreementState{
		AgreementNo: row.ContractCode, Status: dto.AgreementStatusActive, ProviderState: "signed",
	}
	_, err := f.svc.SyncSubscription(context.Background(), row.ID, f.actor(), uuid.NewString(), uuid.NewString())
	if !errors.Is(err, ErrSubscriptionNotSettleable) {
		t.Fatalf("复活一条已取消订阅的错误 = %v，想要 ErrSubscriptionNotSettleable", err)
	}
	if after := f.subscription(row.ID); after.Status != model.SubscriptionStatusCancelled {
		t.Errorf("状态 = %q，想要还是 cancelled", after.Status)
	}
}

// TestIntegrationSyncRefusesWhenTheAgreementDoesNotMatch 钉住「回来的不是这一行记着的那份协议」。
//
// 渠道答了一份**别的**协议号（写错了、或者库里的 contract_code 被改坏了）时不能照结论往下写：
// 那会把别人的协议绑到这个人的订阅上——这个人以后被扣的钱进了另一个人的账。所以宁可报错。
func TestIntegrationSyncRefusesWhenTheAgreementDoesNotMatch(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()
	agreements, _ := f.agreementsOf()

	sub := f.subscribe(signable, uuid.NewString())
	agreements.queryResult = &dto.AgreementState{
		AgreementNo: "AGR-somebody-else", Status: dto.AgreementStatusActive, ProviderState: "signed",
	}

	_, err := f.svc.SyncSubscription(context.Background(), sub.Subscription.ID, f.actor(),
		uuid.NewString(), uuid.NewString())
	if !errors.Is(err, ErrAgreementMismatch) {
		t.Fatalf("协议号对不上的错误 = %v，想要 ErrAgreementMismatch", err)
	}
}

// TestIntegrationSigningWithoutTheChannelIsUnavailable 钉住「装配漏了」这一条。
//
// 没有渠道就是**签不成**，不是「降级成不签约」：一个没接渠道的进程如果在这里假装成功，用户会
// 得到一个「已开通自动续费」的界面，而名下没有任何协议。
func TestIntegrationSigningWithoutTheChannelIsUnavailable(t *testing.T) {
	f := newMembershipFixture(t)
	f.seedMembership(t)
	signable := f.signedPlan()

	// newMembershipFixture 造出来的服务**没有** Agreements / Wallets（Options 上那两条：
	// 生产装配必须传）。
	_, err := f.svc.CreateSubscription(context.Background(), f.user,
		dto.CreateSubscriptionRequest{PlanID: signable.ID, RequestID: uuid.NewString()}, uuid.NewString())
	if !errors.Is(err, ErrChannelUnavailable) {
		t.Fatalf("没接渠道时发起签约的错误 = %v，想要 ErrChannelUnavailable", err)
	}
}
