package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组测试覆盖发起支付里**不碰渠道网络**的那部分：形状校验、分派前的拒绝、以及三种
// 渠道结论（成功 / 拒绝 / 不确定）各自对支付单做什么。它们都用假仓储，因此跑得快、也不
// 需要 PG——这一层里最容易写错的恰恰是这些判断（金额该不该建单、结果不明时该不该标失败），
// 而不是 SQL 本身。
//
// 渠道网络那一段用 stubProvider 顶替：它直接返回一个预设的 provider.CreateResult，
// 就像适配器从渠道那里拿到的一样。真实渠道的协议由 provider/manual 的测试守。

const (
	testUserID  = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	testOrderID = "5c2f2b5a-3d1c-4a6e-9b0f-8e7d6c5b4a39"
	testOrderNo = "ORD20260914000001"
	testRequest = "req-1"
	// testMethodCode 是绝大多数用例用的那条支付方式：银联商务 H5 的支付宝那条（action=h5、
	// 有渠道、要付款人身份以外的处理）。想走小程序那条（action=native_pay 要 openid）或
	// 豆支付那条的用例自己指定 code——它们各有几条自己的边界要验。
	testMethodCode = catalog.CodeUMSH5Alipay
	// stubProviderName 是假适配器注册进 Registry 用的名字。它必须与目录里那唯一一条渠道的
	// Provider 相等（`ums`），否则每一次发起都会先撞上 ErrProviderNotConfigured——那不是
	// 用例想验的东西。要验「指向没注册的 provider」的那一条自己把名字改掉。
	stubProviderName = "ums"
	// stubSecretSlot 是假适配器默认声明的那个凭据槽。真实的适配器各自声明自己的（ums 是
	// appKey / commKey），假的只需要一个名字，好让「按声明的槽解析」这件事有东西可断言。
	stubSecretSlot = "appKey"
	// 下单点位与设备：值引用，形状与订单库里的那两列一致（TEXT，不是 uuid 列，见 create.go
	// 的 validateCreate）。分账规则按它们命中。
	testStoreID  = "a1b2c3d4-0000-4000-8000-000000000001"
	testDeviceID = "a1b2c3d4-0000-4000-8000-000000000002"
)

// fakeRepository 是一个够用的假仓储：它只记下「被调用了哪些方法、参数是什么」，
// 并按预设返回。没有并发保护——这些测试都是单 goroutine 的。
type fakeRepository struct {
	// notification / notificationErr 编排 InsertNotification 的回答，供回调那几条用例
	// 模拟「这条通知已经在库里了」。
	notification    *repository.NotificationRecord
	notificationErr error
	// notificationCalls 记下落库的实参。签约通知那条路要断言的是**通知号怎么来的**（报文
	// 摘要，不是渠道给的号）与事件类型，而那些值只出现在实参里——真仓储不会把它们回给调用方。
	notificationCalls []repository.NotificationParams
	// markedNotifications 记下每一次「把一条通知标成某个状态」（重复投递标 ignored、
	// 被拒的标 failed）。真仓储那条路走的是独立连接，这里能验的只有标了什么。
	markedNotifications []markCall

	beginPayment *model.Payment
	beginSnap    []byte
	beginHit     bool
	beginErr     error

	pendingCalls []repository.MarkPaymentPendingParams
	failedCalls  []repository.MarkPaymentFailedParams
	providerCall []repository.ProviderCallParams

	settle    *repository.PaymentSettlement
	settleErr error
	// settleCalls 记下每一次结算的实参。回调那条路用它验「验签之前一个字段都不动」，
	// 主动查单那条路用它验「落下去的是查单得到的结论，而且没有回调记录可标」。
	settleCalls []repository.SettleNotificationParams

	accountSettles []repository.SettleAccountPaymentParams
	accountErr     error
	// accountErrOn 按 paymentID 编排 SettleAccountPayment 的失败：命中就返回那一个错误。
	// 用来在同一批补偿里造出「一条坏、一条好」，验证坏的不挡住好的。
	accountErrOn map[string]error

	// —— 账户出资的扣减留痕与补偿（见 createAccountPayment、expire.go）——
	deductions   []accountDeduction
	deductionErr error
	// overdue 是 FindOverdueAccountFundedPayments 的回答：一批「豆已经扣了、到点还没结算」
	// 的支付单。
	overdue    []model.Payment
	overdueErr error

	// —— 主动查单（见 reconcile.go）——
	//
	// stalePending 是 ListStalePendingPayments 的回答：一批「发起之后一直没结论」的支付单。
	// staleQueries 记下每一次扫描的实参——「多久没动过才算该问一句、一次最多问几笔」是调用方
	// 的决定（worker 里那两个常量），假仓储不替它做，但得能断言它传对了。
	stalePending []model.Payment
	staleErr     error
	staleQueries []staleQuery
	// paymentByNo 编排 FindPaymentByNo 的回答。零值保持原来的「查不到」。
	paymentByNo *model.Payment
	// failedErr 编排 MarkPaymentFailed 的回答。零值保持「写成功了」。
	failedErr error

	// —— 分账（见 settlement.go）——
	// rule 是 FindSettlementRule 的回答。零值保持「没命中规则」，那是一个正常结论：
	// 任务照样建，整单归平台。
	rule    *repository.SettlementRule
	ruleErr error
	// ruleQueries 记下问过的维度。断言「问的是这一单的门店/设备/业务分类」只能靠它——
	// 请求里的那三个字段是不是真的走到了命中这一步，别的地方都看不出来。
	ruleQueries []repository.SettlementRuleQuery
	// beginParams 记下每一次 BeginPayment 的实参，分账计划就在里面。
	beginParams []repository.BeginPaymentParams

	// —— 幂等键的释放（见 abandonPaymentAttempt）——
	//
	// beginLocks 模拟库里的那一行 payment_idempotency_keys：BeginPayment 建单时这个 key
	// 变成「在途」，AbandonPaymentAttempt 把它放开。**没有这层状态，「失败之后同一个 key
	// 立刻能重试」就不是可断言的**——假仓储每次都会老老实实建一张新单，那条断言在改写之前
	// 也一样为真，等于什么都没验。
	beginLocks map[string]bool
	// abandonCalls 记下每一次放开：谁（requestID）放开了哪一张单（paymentID）。
	abandonCalls []abandonCall
	// abandonErr 编排放开失败（库不可达）。它不该改变调用方拿到的那次错误，见
	// abandonPaymentAttempt 的注释里那条「失败不让它变成另一个错误」。
	abandonErr error

	// —— 退款（见 refund.go / refund_reconcile.go）——
	//
	// refund 是这**一张**退款单的当前状态，三个 Mark* 就地改它、FindRefundByNo 读它。
	// 用「一张会变状态的对象」而不是「每次调用记一笔」，是因为退款那条链的形状就是
	// 「同一张单被推着走」：建单、发起、落结论，调用方拿回的是它最终的样子。
	//
	// refundFundings 是 BeginRefund 建出来的那几行（逐笔出资的冲正计划）。它由用例直接摆，
	// 因为「钱是从哪几笔出资来的」是支付那一步的事实，不是这一段能推出来的。
	refund           *model.Refund
	refundFundings   []*model.RefundFunding
	beginRefundHit   bool
	beginRefundErr   error
	beginRefundCalls []repository.BeginRefundParams
	// 三个 Mark* 的实参。断言「哪几行不走渠道」「失败是从哪个状态出发的」只能靠它们。
	refundSettles    []repository.MarkRefundSucceededParams
	refundFailures   []repository.MarkRefundFailedParams
	refundProcessing []repository.MarkRefundProcessingParams
	// refundByNoErr 编排 FindRefundByNo 的失败（那一条只在「建完单之后读不回来」时出现，
	// 是一次库故障）。
	refundByNoErr error

	// —— 退款查询（见 refund_reconcile.go）——
	//
	// processingRefunds 是 ListStaleProcessingRefunds 的回答；refundQueries 记下扫描的实参
	// （多久没动过才算该问、一次最多问几笔——与查单那条路同一组断言）。
	processingRefunds []model.Refund
	staleRefundErr    error
	refundQueries     []staleQuery
	// touchedRefunds 记下被推到队尾的退款单 ID。它是「一笔问不出结论的退款不会一直占着队首」
	// 那件事的全部证据。
	touchedRefunds []string
	touchErr       error

	// —— 签约协议（见 agreement.go）——
	//
	// agreement 是**这一份**协议的当前状态，CreateAgreement 建它、SettleAgreement 就地改它，
	// FindAgreementByNo 读它——与退款那边同一个形状（「同一份协议被推着走」）。
	agreement            *model.PaymentAgreement
	createAgreementSnap  []byte
	createAgreementHit   bool
	createAgreementErr   error
	createAgreementCalls []repository.CreateAgreementParams
	// settleCalls 记下每一次落结论的实参。断言「查约把渠道说的状态翻成了哪一个本地状态」
	// 只能靠它——翻好的那个值在真仓储里才会变成行。
	// agreementSettleChanged 编排 SettleAgreement 的回答「这次到底改了没有」。零值表示
	// 「没改」——大多数用例只关心 service 送下去的**目标状态**，那个由 agreementSettles 记。
	agreementSettles       []repository.SettleAgreementParams
	agreementSettleChanged bool
	agreementSettleErr     error
	agreementByNoErr       error
	// —— 签约通知（见 agreement_notify.go）——
	//
	// 与上面那组分开记：两条路送下来的目标状态要**分别**断言，共用一个切片的话，
	// 「通知那条路翻错了状态」会被查约那条路留下的记录盖住。
	agreementNotificationSettles []repository.AgreementNotificationParams
	agreementNotificationErr     error
	// agreementNotificationChanged 编排「这次到底改了没有」。与 agreementSettleChanged 分开：
	// 两条路各断言各的，共用一个开关时一个用例的编排会漏到另一个用例上。
	agreementNotificationChanged bool

	// —— 代扣（见 agreement_charge.go）——
	//
	// charge 是**这一期**的当前状态，与 agreement 同一个形状（同一行被推着走）。零值时
	// CreateOrFindCharge 会照实参造一行出来。
	charge             *model.PaymentAgreementCharge
	createChargeErr    error
	createChargeCalls  []repository.CreateOrFindChargeParams
	chargeAttemptErr   error
	chargeAttemptCalls []repository.ChargeAttemptParams
	// chargeNotificationSettles 与上面那两组分开记：三条路送下来的目标状态要分别断言。
	chargeNotificationSettles []repository.ChargeNotificationParams
	chargeNotificationErr     error
	chargeNotificationChanged bool
}

// abandonCall 是一次 AbandonPaymentAttempt 调用的实参。
type abandonCall struct {
	requestID string
	paymentID string
}

// accountDeduction 是一次 RecordAccountDeduction 调用的实参。
//
// fundedAt 也记下来：结算用的成交时间必须**等于**扣豆那一刻，而不是结算那一刻（见
// createAccountPayment），只记 paymentID 与 entryID 的话这条断言就没法写。
type accountDeduction struct {
	paymentID string
	entryID   string
	fundedAt  time.Time
}

func (f *fakeRepository) FindPaymentByNo(context.Context, string) (*model.Payment, error) {
	if f.paymentByNo != nil {
		return f.paymentByNo, nil
	}
	return nil, repository.ErrPaymentNotFound
}

// ListChargesByAgreementNo 是订阅详情那两个只读投影里的一格（见 internal_query.go）。
//
// 它**不在**这个假仓储的编排面上（那两条读有自己的用例面），默认回一个空列表——空而不是
// nil，与真仓储一致：调用方要的是 `[]`，不是 `null`。
func (f *fakeRepository) ListChargesByAgreementNo(context.Context, string) ([]*model.PaymentAgreementCharge, error) {
	return []*model.PaymentAgreementCharge{}, nil
}

func (f *fakeRepository) FindSettlementRule(_ context.Context, q repository.SettlementRuleQuery) (*repository.SettlementRule, error) {
	f.ruleQueries = append(f.ruleQueries, q)
	if f.ruleErr != nil {
		return nil, f.ruleErr
	}
	return f.rule, nil
}

func (f *fakeRepository) BeginPayment(_ context.Context, p repository.BeginPaymentParams) (*model.Payment, []byte, bool, error) {
	f.beginParams = append(f.beginParams, p)
	// 在途的 key 挡住第二次发起，与库里那条 processing 的行同形。beginLocks 为 nil 时
	// 这条路上什么都没有（大多数用例不关心幂等键），行为与改动之前一致。
	if f.beginLocks[p.RequestID] {
		return nil, nil, false, repository.ErrIdempotencyInProgress
	}
	if f.beginErr != nil {
		return nil, nil, false, f.beginErr
	}
	if f.beginHit {
		return nil, f.beginSnap, true, nil
	}
	payment := f.beginPayment
	if payment == nil {
		payment = &model.Payment{
			ID: "payment-1", PaymentNo: p.PaymentNo, OrderNo: p.OrderNo, UserID: p.UserID,
			Amount: p.Amount, PaymentMethod: p.Method, Status: model.PaymentCreated,
			RequestID: p.RequestID,
		}
	}
	// 建单与占幂等键在真仓储里是同一个事务（见 BeginPayment），所以这里也一起发生。
	if f.beginLocks != nil {
		f.beginLocks[p.RequestID] = true
	}
	return payment, nil, false, nil
}

func (f *fakeRepository) AbandonPaymentAttempt(_ context.Context, requestID, paymentID string) error {
	if f.abandonErr != nil {
		return f.abandonErr
	}
	f.abandonCalls = append(f.abandonCalls, abandonCall{requestID: requestID, paymentID: paymentID})
	// 真仓储里这一步是 DELETE 掉那条还在 processing 的幂等行（见那份实现）：这里的
	// delete 就是它的模型，删掉之后同一个 key 的下一次 BeginPayment 不再被挡。
	delete(f.beginLocks, requestID)
	return nil
}

func (f *fakeRepository) MarkPaymentPending(_ context.Context, p repository.MarkPaymentPendingParams) (*model.Payment, error) {
	f.pendingCalls = append(f.pendingCalls, p)
	return &model.Payment{ID: p.PaymentID, PaymentNo: "PAY-FAKE", Status: model.PaymentPending}, nil
}

func (f *fakeRepository) MarkPaymentFailed(_ context.Context, p repository.MarkPaymentFailedParams) (*model.Payment, error) {
	f.failedCalls = append(f.failedCalls, p)
	if f.failedErr != nil {
		return nil, f.failedErr
	}
	return &model.Payment{ID: p.PaymentID, Status: model.PaymentFailed}, nil
}

func (f *fakeRepository) RecordProviderCall(_ context.Context, p repository.ProviderCallParams) error {
	f.providerCall = append(f.providerCall, p)
	return nil
}

func (f *fakeRepository) InsertNotification(_ context.Context, p repository.NotificationParams) (*repository.NotificationRecord, error) {
	// 记下每一次落库的实参：签约通知那条路上要断言的是**通知号怎么来的**（报文摘要，不是渠道
	// 给的号）与事件类型，而那些值只出现在实参里——真仓储不会把它们回给调用方。
	f.notificationCalls = append(f.notificationCalls, p)
	// 零值保持「新到的一条」：建单那几条用例看不到这个开关。
	if f.notificationErr != nil {
		return nil, f.notificationErr
	}
	if f.notification != nil {
		return f.notification, nil
	}
	return &repository.NotificationRecord{ID: "n-1", Inserted: true}, nil
}

// MarkNotification 记下实参。它是「一条被拒/失败的通知有没有留下痕」的全部证据：这个方法在
// 真仓储里是**独立连接**上的一次更新（业务事务已经回滚了），假仓储看不出连接，能验的是
// 「标了哪一行、标成什么、理由是什么」。
func (f *fakeRepository) MarkNotification(_ context.Context, id, status, reason string) error {
	f.markedNotifications = append(f.markedNotifications, markCall{id: id, status: status, reason: reason})
	return nil
}

// markCall 是一次 MarkNotification 调用的实参。
type markCall struct {
	id     string
	status string
	reason string
}

func (f *fakeRepository) SettlePayment(_ context.Context, p repository.SettleNotificationParams) (*repository.PaymentSettlement, error) {
	f.settleCalls = append(f.settleCalls, p)
	if f.settleErr != nil {
		return nil, f.settleErr
	}
	return f.settle, nil
}

func (f *fakeRepository) ExpireOverduePayments(context.Context, int) (int, error) { return 0, nil }

func (f *fakeRepository) SettleAccountPayment(_ context.Context, p repository.SettleAccountPaymentParams) (*model.Payment, error) {
	if err := f.accountErrOn[p.PaymentID]; err != nil {
		return nil, err
	}
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	f.accountSettles = append(f.accountSettles, p)
	return &model.Payment{ID: p.PaymentID, Status: model.PaymentSucceeded}, nil
}

func (f *fakeRepository) RecordAccountDeduction(_ context.Context, paymentID, accountEntryID string, fundedAt time.Time) error {
	if f.deductionErr != nil {
		return f.deductionErr
	}
	f.deductions = append(f.deductions, accountDeduction{paymentID: paymentID, entryID: accountEntryID, fundedAt: fundedAt})
	return nil
}

func (f *fakeRepository) FindOverdueAccountFundedPayments(context.Context, int) ([]model.Payment, error) {
	if f.overdueErr != nil {
		return nil, f.overdueErr
	}
	return f.overdue, nil
}

func (f *fakeRepository) ListStalePendingPayments(_ context.Context, staleBefore time.Time, limit int) ([]model.Payment, error) {
	f.staleQueries = append(f.staleQueries, staleQuery{staleBefore: staleBefore, limit: limit})
	if f.staleErr != nil {
		return nil, f.staleErr
	}
	return f.stalePending, nil
}

// staleQuery 是一次 ListStalePendingPayments 调用的实参。
type staleQuery struct {
	staleBefore time.Time
	limit       int
}

// stubProvider 是一个可编排的适配器：Create 原样返回预设结果，Verify 永不通过。
type stubProvider struct {
	// name 覆盖注册名。零值用 stubProviderName（与目录里那条渠道对得上）。
	name string
	// slots 是它向装配处声明的凭据槽。零值用 stubSecretSlot。
	//
	// 声明槽这件事本身是被测的行为之一（见 TestCreateAsksForExactlyTheDeclaredSlots）：
	// 装配处**只**解析适配器报上来的那几个名字，一个都不多给。
	slots   []string
	result  provider.CreateResult
	err     error
	calls   []provider.CreateRequest
	ackTrue provider.Ack
	// verifyOK 打开后 Verify 返回 notification/verifyErr 而不是默认的验签失败。
	verifyOK     bool
	notification provider.Notification
	verifyErr    error
	// verifyCalls 记下每一次入站验签收到的请求。回跳那条路要靠它断言「交下去的是
	// Query 而不是 Body、方法是 GET」——那些事实只在这一层看得见，适配器那边看到的是
	// 已经拆好的字段。
	verifyCalls []provider.NotificationRequest
}

// TestCreateAsksForExactlyTheDeclaredSlots 钉住「交给适配器的凭据是且仅是它自己声明的那几把
// 槽，加上兜底那一把」。
//
// 同一件事在试跑那条路上已经被钉过，但**下单这条才是真的会花钱的那条**：这里漏解一把槽，微信
// v3 的签名会在线上一直对不上，而三个包全绿——那种错没有任何一层会拦住它。
//
// 断言的是**集合与顺序**，不是「至少问了一次」：一个「声明的槽解不到、于是拿了兜底值」的渠道
// 必须被认出来，而它只在顺序上看得出来（兜底那把永远是最后问的，见 resolveSecrets）。
func TestCreateAsksForExactlyTheDeclaredSlots(t *testing.T) {
	stub := &stubProvider{
		result: provider.CreateResult{Result: provider.ResultSuccess},
		slots:  []string{"signKey", "appKey"},
	}
	secrets := &stubSecretResolver{values: map[string]string{"signKey": "k1", "appKey": "k2"}}
	svc := newServiceWith(t, &fakeRepository{}, stub, testMethodCode, nil, secrets.resolve)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	// 问的与给的都**只有**适配器声明的那两把：第三个「兜底」的名字没有了——它从前来自
	// 渠道行的 secret_ref，而那张表连同它存在的前提一起没了。
	secrets.askedOnly(t, "signKey", "appKey")

	if len(stub.calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(stub.calls))
	}
	got := stub.calls[0].Secrets
	if len(got) != 2 {
		t.Fatalf("交给适配器的凭据 = %v，期望就是声明的那两把", got)
	}
	for name, want := range map[string]string{"signKey": "k1", "appKey": "k2"} {
		if got.Get(name) != want {
			t.Errorf("槽 %s = %q，期望 %q", name, got.Get(name), want)
		}
	}
}

func (s *stubProvider) Name() string {
	if s.name != "" {
		return s.name
	}
	return stubProviderName
}

// SecretSlots 声明这次调用要用的槽名。**必须实现**：装配处今天不再有「渠道行 secret_ref
// 兜底那一把」那条路（那张表没了），一把槽都不声明的适配器拿到的是一张空凭据表，随后自己
// 因为签不了名而拒——而那会把用例的失败原因指向签名，而不是它真正在验的那件事。
func (s *stubProvider) SecretSlots(provider.Method) []string {
	if s.slots == nil {
		return []string{stubSecretSlot}
	}
	return s.slots
}

func (s *stubProvider) Create(_ context.Context, req provider.CreateRequest) (provider.CreateResult, error) {
	s.calls = append(s.calls, req)
	return s.result, s.err
}

func (s *stubProvider) Verify(_ context.Context, req provider.NotificationRequest) (provider.Notification, error) {
	s.verifyCalls = append(s.verifyCalls, req)
	// 零值保持「验签不过」：只有回调那几条用例会把这个开关打开。
	if !s.verifyOK {
		return provider.Notification{}, provider.ErrSignatureMismatch
	}
	return s.notification, s.verifyErr
}

func (s *stubProvider) Ack(_ provider.Method, accepted bool) provider.Ack {
	if accepted {
		return s.ackTrue
	}
	return provider.Ack{Status: 400}
}

// stubLedger 是可编排的假账本：Deduct 原样返回预设结果，并记下请求，让「扣的是不是这张
// 订单、这个金额、这个用户」变成可断言的。
type stubLedger struct {
	result client.DeductResult
	err    error
	calls  []client.DeductRequest
}

func (s *stubLedger) Deduct(_ context.Context, req client.DeductRequest) (client.DeductResult, error) {
	s.calls = append(s.calls, req)
	return s.result, s.err
}

// testCatalog 是一份**配齐了**的目录：银联商务那条渠道的五个账户值全给了，所以它没有
// MissingEnv，任何一条方式都能起支付。要验「没配齐」那条路的用例自己造一份缺项的
// （见 TestCreatePaymentRejectsAChannelMissingItsEnvironment）。
//
// 用真的 FromConfig 而不是手搭一个 Catalog：装配处就是这么构造它的，用例跟着走，目录里
// 那条渠道的 Config 树（ums.Parse 要读的形状）也就顺带被验了一遍。
func testCatalog() *catalog.Catalog { return testCatalogWith(catalog.Config{}) }

// testCatalogWith 在测试用的那组账户值上叠加调用方给的覆盖。
func testCatalogWith(overrides catalog.Config) *catalog.Catalog {
	cfg := catalog.Config{
		UMSBaseURL:    "https://ums.example.test",
		UMSAppID:      "ums-test-app",
		UMSMID:        "M0001",
		UMSTID:        "T0001",
		UMSSourceCode: "3CYM",
		UMSDomainName: "www.example.com",
	}
	if overrides.UMSBaseURL != "" {
		cfg.UMSBaseURL = overrides.UMSBaseURL
	}
	if overrides.UMSAppID != "" {
		cfg.UMSAppID = overrides.UMSAppID
	}
	if overrides.UMSMID != "" {
		cfg.UMSMID = overrides.UMSMID
	}
	if overrides.UMSTID != "" {
		cfg.UMSTID = overrides.UMSTID
	}
	if overrides.UMSSourceCode != "" {
		cfg.UMSSourceCode = overrides.UMSSourceCode
	}
	if overrides.UMSDomainName != "" {
		cfg.UMSDomainName = overrides.UMSDomainName
	}
	cfg.UMSSceneType = overrides.UMSSceneType
	cfg.UMSMerAppName = overrides.UMSMerAppName
	cfg.UMSMerAppID = overrides.UMSMerAppID

	// 微信直连那条渠道也默认给齐账户值：它只有 appId / mchId 两个必填项（证书不是必填，
	// 只有代扣与解约要它，见 catalog.wechatPayChannel），缺了它们任何一条走签约的用例都会
	// 先撞上一句 ErrChannelIncomplete——那看起来像被测的代码坏了。**密钥不在这里**：
	// APIv2 密钥是凭据，走凭据槽，装配处不把它放进 Catalog。
	cfg.WeChatPayAppID = "wxtestappid0000001"
	cfg.WeChatPayMchID = "1668145209"
	if overrides.WeChatPayAppID != "" {
		cfg.WeChatPayAppID = overrides.WeChatPayAppID
	}
	if overrides.WeChatPayMchID != "" {
		cfg.WeChatPayMchID = overrides.WeChatPayMchID
	}
	cfg.WeChatPayBaseURL = overrides.WeChatPayBaseURL
	cfg.WeChatPayCertPath = overrides.WeChatPayCertPath
	cfg.WeChatPayKeyPath = overrides.WeChatPayKeyPath
	return catalog.FromConfig(cfg)
}

// stubSecretResolver 是一个记账用的假凭据解析器：它记下**问过哪些槽、按什么顺序**，
// 并按预设给值。解析顺序是「适配器声明的槽，一个不多一个不少」那条契约的证据。
type stubSecretResolver struct {
	values map[string]string
	asked  []string
}

func (s *stubSecretResolver) resolve(_ *catalog.Channel, slot string) string {
	s.asked = append(s.asked, slot)
	return s.values[slot]
}

// askedOnly 断言问过的槽**集合与顺序**都与给定的完全一致。
//
// 顺序也是契约的一部分：解析是按适配器报上来的名单逐个做，多问一个不存在的槽在运维眼里
// 就是一次无谓的 os.Getenv，少问一个则是「配了不生效」。
func (s *stubSecretResolver) askedOnly(t *testing.T, slots ...string) {
	t.Helper()
	if !reflect.DeepEqual(s.asked, slots) {
		t.Fatalf("解析器被问的槽 = %v，期望 %v", s.asked, slots)
	}
}

// newTestService 组装一个「目录里的 code 指向 stub 适配器」的业务层，不带账户域。
func newTestService(t *testing.T, repo *fakeRepository, stub *stubProvider, code string) *PaymentService {
	t.Helper()
	return newTestServiceWith(t, repo, stub, code, nil)
}

// newBeanService 组装账户出资那条路的业务层：目录里那个没有渠道的 code（咖啡豆），
// 账本是传入的那个假账本。
func newBeanService(t *testing.T, repo *fakeRepository, ledger BeanLedger) *PaymentService {
	t.Helper()
	return newTestServiceWith(t, repo, &stubProvider{}, catalog.CodeCoffeeBean, ledger)
}

func newTestServiceWith(t *testing.T, repo *fakeRepository, stub *stubProvider, code string, beans BeanLedger) *PaymentService {
	t.Helper()
	// 假的解析器：不看渠道也不看槽名，给什么都回同一把密钥。这一层要验的是「拿到的密钥被
	// 原样交给了适配器」，解析顺序是装配处的事（见 TestCreateAsksForExactlyTheDeclaredSlots）。
	return newServiceWith(t, repo, stub, code, beans,
		func(*catalog.Channel, string) string { return "secret" })
}

// newServiceWith 是上面那个的底层：适配器与凭据解析器都由调用方给。
//
// 拆成两层是因为「声明了槽的适配器 + 能记下问过哪些槽的解析器」这一组合只在少数几条用例里
// 需要（见 TestCreateAsksForExactlyTheDeclaredSlots），其余几十条要的是那句「给什么都回
// 同一把」——把参数加到上面那个函数上，等于让每一条用例都多写一遍同一段闭包。
func newServiceWith(t *testing.T, repo *fakeRepository, adapter provider.Provider, code string, beans BeanLedger, secrets SecretResolver) *PaymentService {
	t.Helper()
	return newServiceWithCatalog(t, repo, testCatalog(), adapter, code, beans, secrets)
}

// newServiceWithCatalog 让调用方连目录一起给（现在只有验「渠道没配齐」的那条用例需要）。
//
// extra 是**额外的**适配器：绝大多数用例只有一族渠道，一个适配器就够；而「这条支付方式压根
// 不会签约」那一条需要同时注册两个（一条会签、一条不会），否则它撞上的是「适配器没注册」，
// 验的就不是它想验的那句话了。
func newServiceWithCatalog(t *testing.T, repo *fakeRepository, directory *catalog.Catalog,
	adapter provider.Provider, code string, beans BeanLedger, secrets SecretResolver,
	extra ...provider.Provider) *PaymentService {
	t.Helper()
	// code 必须真的在目录里：传错的那一天，用例会在 resolveMethod 上拿到
	// ErrPaymentMethodNotFound，而那看起来像被测的代码坏了。
	if _, err := directory.Method(code); err != nil {
		t.Fatalf("测试要用的支付方式 code %q 不在目录里：%v", code, err)
	}
	return New(repo, directory, provider.NewRegistry(append([]provider.Provider{adapter}, extra...)...), secrets, beans, Options{
		NotifyBaseURL: "https://pay.example.test",
		Now:           func() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) },
		NewPaymentNo:  func(time.Time) string { return "PAY20260914120000000001" },
		// 退款单号也钉死：它是 23 位那个硬约束的产物（见 refundNo），而用例要断言的是
		// 「发去渠道的是不是同一个号」，不是随机数生成器本身——那一条由
		// TestRefundNoFitsTheChannelLengthBudget 直接验。
		NewRefundNo: func(time.Time) string { return "REF20260914120000000001" },
	})
}

func validCreateRequest() CreateRequest { return validCreateRequestFor(testMethodCode) }

// validBeanRequest 是账户出资（咖啡豆）那条路的请求。
//
// 那些用例必须自己点名 coffee_bean：方式不再是一行数据、也不再由假仓储决定，请求里这个 code
// 是**唯一**决定走哪条路的东西。它们要是拿到银联商务的 code，验的就不再是扣豆那条路，而会
// 在适配器上撞出一次「结果不明」——症状离原因很远。
func validBeanRequest() CreateRequest { return validCreateRequestFor(catalog.CodeCoffeeBean) }

// validCreateRequestFor 造一条用指定的那条支付方式的请求。方式不再是一行数据，所以用例必须
// 自己说清楚它在付哪一种——这正是这次收口想要的效果。
func validCreateRequestFor(code string) CreateRequest {
	return CreateRequest{
		OrderID: testOrderID, OrderNo: testOrderNo, UserID: testUserID, Amount: 1980,
		PaymentMethod: code, RequestID: testRequest,
		WalletOpenID: "openid-1", Subject: "拿铁",
		// 分账的三个维度：绝大多数用例要判的不是分账那一段，但它们**必填**（biz_type 是规则
		// 命中键的第一段），所以给一组像真的一样的默认值，而不是让每条用例各写一遍。
		StoreID: testStoreID, DeviceID: testDeviceID, BizType: model.SettlementBizCoffee,
	}
}

// TestCreatePaymentValidation 校验是形状层面的：一条都不能走到仓储。
//
// 「没走到仓储」是断言的一部分，不是顺带的：一条建不了单的请求在库上留下任何痕迹都是错的。
func TestCreatePaymentValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateRequest)
		want   error
	}{
		{"缺订单 ID", func(r *CreateRequest) { r.OrderID = "" }, ErrOrderIDRequired},
		{"订单 ID 不是 uuid", func(r *CreateRequest) { r.OrderID = "o-1" }, ErrOrderIDInvalid},
		{"缺订单号", func(r *CreateRequest) { r.OrderNo = " " }, ErrOrderNoRequired},
		{"缺用户", func(r *CreateRequest) { r.UserID = "" }, ErrUserIDRequired},
		{"用户不是 uuid", func(r *CreateRequest) { r.UserID = "u-1" }, ErrUserIDInvalid},
		{"金额为 0", func(r *CreateRequest) { r.Amount = 0 }, ErrAmountNotPositive},
		{"金额为负", func(r *CreateRequest) { r.Amount = -1 }, ErrAmountNotPositive},
		{"缺支付方式", func(r *CreateRequest) { r.PaymentMethod = " " }, ErrMethodRequired},
		{"缺幂等号", func(r *CreateRequest) { r.RequestID = "" }, ErrRequestIDRequired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{}
			svc := newTestService(t, repo, &stubProvider{}, testMethodCode)
			in := validCreateRequest()
			tc.mutate(&in)
			if _, err := svc.CreatePayment(context.Background(), in); !errors.Is(err, tc.want) {
				t.Fatalf("CreatePayment error = %v, want %v", err, tc.want)
			}
			// 「没走到仓储」是断言的一部分，不是顺带的：一条建不了单的请求在库上留下任何
			// 痕迹都是错的。从前这里看的是「有没有去查支付方式」，而那次查询今天没有了
			// ——方式在代码里，查不到什么；会在库里留下痕迹的只有建单。
			if len(repo.beginParams) != 0 {
				t.Fatal("invalid input reached the repository")
			}
		})
	}
}

// TestCreateAccountPaymentDeductsAndSettles 账户出资的成功路径：扣豆 → 一次事务结算。
//
// 断言的三件事分别对应这条路上最容易写错的三处：扣的是**订单 ID**（不是支付单号，
// 那是幂等键的来源）、出资行的账变 ID 被原样带进结算、以及返回的是 succeeded 而不是
// pending（没有第三方要等）。
func TestCreateAccountPaymentDeductsAndSettles(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
	svc := newBeanService(t, repo, ledger)

	result, err := svc.CreatePayment(context.Background(), validBeanRequest())
	if err != nil {
		t.Fatalf("CreatePayment error = %v", err)
	}
	if result.Status != model.PaymentSucceeded {
		t.Fatalf("status = %q, want %q", result.Status, model.PaymentSucceeded)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("Deduct called %d times, want 1", len(ledger.calls))
	}
	call := ledger.calls[0]
	if call.OrderID != testOrderID {
		t.Fatalf("Deduct orderID = %q, want %q", call.OrderID, testOrderID)
	}
	if call.Amount != 1980 || call.UserID != testUserID {
		t.Fatalf("Deduct got user %q amount %d, want %q / 1980", call.UserID, call.Amount, testUserID)
	}
	if len(repo.accountSettles) != 1 {
		t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
	}
	if got := repo.accountSettles[0].AccountEntryID; got != "entry-1" {
		t.Fatalf("settled account entry = %q, want entry-1", got)
	}
	if len(repo.providerCall) != 0 {
		t.Fatal("an account payment recorded a provider call; there is no provider")
	}
	if len(repo.pendingCalls) != 0 {
		t.Fatal("an account payment went through MarkPaymentPending; it never waits on a third party")
	}
}

// TestCreateAccountPaymentRecordsTheDeductionBeforeSettling 守「扣豆成功先留痕」。
//
// 这条留痕要覆盖的是**结算失败**那个窗口：豆在账户域是独立提交的，结算一旦失败，
// payment_fundings 那一行会随事务回滚，本地就再没有任何东西指向账户域那笔账变。所以断言
// 分两半，缺一不可：成功的路上留痕存在，且成交时间与结算用的是同一个值（两处记的时间不
// 一样，对账时就得猜哪一个是钱真正走掉的时刻）；失败的路上留痕**依然在**——那才是它存在
// 的全部理由，只测成功那条等于什么都没守住。
func TestCreateAccountPaymentRecordsTheDeductionBeforeSettling(t *testing.T) {
	t.Run("结算成功时两边的时间一致", func(t *testing.T) {
		repo := &fakeRepository{}
		ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
		svc := newBeanService(t, repo, ledger)

		if _, err := svc.CreatePayment(context.Background(), validBeanRequest()); err != nil {
			t.Fatalf("CreatePayment error = %v", err)
		}
		if len(repo.deductions) != 1 {
			t.Fatalf("RecordAccountDeduction called %d times, want 1", len(repo.deductions))
		}
		recorded := repo.deductions[0]
		if recorded.entryID != "entry-1" || recorded.paymentID != "payment-1" {
			t.Fatalf("recorded %+v, want entry-1 on payment-1", recorded)
		}
		if len(repo.accountSettles) != 1 {
			t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
		}
		if got := repo.accountSettles[0].PaidAt; !got.Equal(recorded.fundedAt) {
			t.Fatalf("结算用的成交时间 = %v，留痕记的是 %v，两者必须相等", got, recorded.fundedAt)
		}
	})

	t.Run("结算失败时留痕仍然落下", func(t *testing.T) {
		repo := &fakeRepository{accountErr: errors.New("the ledger write failed")}
		ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
		svc := newBeanService(t, repo, ledger)

		if _, err := svc.CreatePayment(context.Background(), validBeanRequest()); err == nil {
			t.Fatal("CreatePayment error = nil, want the settle failure")
		}
		// 这一条就是这次修复本身：豆已经扣了，结算没成，但本地必须留下指向那笔账变的线索。
		if len(repo.deductions) != 1 {
			t.Fatalf("结算失败时留痕丢了（RecordAccountDeduction 调用 %d 次）——豆扣走了，本地却什么都没留下",
				len(repo.deductions))
		}
	})
}

// TestCreateAccountPaymentSettlesEvenIfTheDeductionCannotBeRecorded 留痕写不下去不该挡住结算。
//
// 结算那个事务自己会把 account_entry_id 写进 payment_fundings，那才是权威的出资留痕；
// 这一步要覆盖的只是结算失败的那个窗口。为了它把一次本来能成的收款推回去，是把轻重搞反了。
func TestCreateAccountPaymentSettlesEvenIfTheDeductionCannotBeRecorded(t *testing.T) {
	repo := &fakeRepository{deductionErr: errors.New("the ledger write failed")}
	ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1", BalanceAfter: 3020}}
	svc := newBeanService(t, repo, ledger)

	result, err := svc.CreatePayment(context.Background(), validBeanRequest())
	if err != nil {
		t.Fatalf("CreatePayment error = %v, want the payment to go through anyway", err)
	}
	if result.Status != model.PaymentSucceeded {
		t.Fatalf("status = %q, want %q", result.Status, model.PaymentSucceeded)
	}
	if len(repo.accountSettles) != 1 {
		t.Fatalf("SettleAccountPayment called %d times, want 1", len(repo.accountSettles))
	}
}

// TestCreateAccountPaymentInsufficientBeansIsADecline 豆不够是**结论**，不是故障。
//
// 它落成 status='failed' 的结果（客户端可以换一种方式重试），并且**不能**碰结算——
// 把一张没扣到钱的支付单推成 succeeded 是这条路线上最严重的一种错。
func TestCreateAccountPaymentInsufficientBeansIsADecline(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{err: fmt.Errorf("%w: user %s", ErrInsufficientCoffeeBeans, testUserID)}
	svc := newBeanService(t, repo, ledger)

	result, err := svc.CreatePayment(context.Background(), validBeanRequest())
	if err != nil {
		t.Fatalf("CreatePayment error = %v, want a failed result instead", err)
	}
	if result.Status != model.PaymentFailed {
		t.Fatalf("status = %q, want %q", result.Status, model.PaymentFailed)
	}
	if result.FailureCode != failureCodeInsufficientBeans {
		t.Fatalf("failureCode = %q, want %q", result.FailureCode, failureCodeInsufficientBeans)
	}
	if len(repo.accountSettles) != 0 {
		t.Fatal("a declined account payment was settled anyway")
	}
	if len(repo.failedCalls) != 1 {
		t.Fatalf("MarkPaymentFailed called %d times, want 1", len(repo.failedCalls))
	}
}

// TestCreateAccountPaymentAccountUnreachableLeavesPaymentCreated 账户域不可达时**得不出结论**。
//
// 支付单必须停在 created（由超时关单收走），调用方拿到一个普通错误——**不是**一次
// status='failed' 的结论。标成 failed 会让用户以为没付成，而豆可能已经扣了。
func TestCreateAccountPaymentAccountUnreachableLeavesPaymentCreated(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{err: errors.New("account service is unreachable")}
	svc := newBeanService(t, repo, ledger)

	if _, err := svc.CreatePayment(context.Background(), validBeanRequest()); err == nil {
		t.Fatal("CreatePayment error = nil, want a plain error")
	}
	if len(repo.failedCalls) != 0 || len(repo.accountSettles) != 0 {
		t.Fatal("an unreachable account service produced a payment outcome")
	}
	if len(repo.pendingCalls) != 0 {
		t.Fatal("an unreachable account service advanced the payment")
	}
}

// TestCreateAccountPaymentWithoutLedgerFails 没接账户域的部署选了豆支付时明确失败，
// 而不是跳过扣豆直接结算——那等于白送一单。
func TestCreateAccountPaymentWithoutLedgerFails(t *testing.T) {
	repo := &fakeRepository{}
	svc := newBeanService(t, repo, nil)

	if _, err := svc.CreatePayment(context.Background(), validBeanRequest()); err == nil {
		t.Fatal("CreatePayment error = nil, want a failure when the ledger is not configured")
	}
	if len(repo.accountSettles) != 0 {
		t.Fatal("a payment was settled without any ledger to deduct from")
	}
}

// TestCreatePaymentRejectsAnUnknownMethodCode 目录里没有这个 code 时明确报错。
//
// 这是「方式收成常量」之后新出现的一类拒绝：从前调用方传的是一个 uuid，指错行的表现是
// 「查不到那一行」；今天它传的是一个**必须与代码版本对得上**的名字——客户端拼错、或者
// order-service 回滚到了旧版本，都会走到这里。静默当成 default 处理会把一次版本错配变成
// 一笔没人看得懂的钱。
func TestCreatePaymentRejectsAnUnknownMethodCode(t *testing.T) {
	repo := &fakeRepository{}
	svc := newTestService(t, repo, &stubProvider{}, testMethodCode)
	in := validCreateRequestFor("some_future_method")
	if _, err := svc.CreatePayment(context.Background(), in); !errors.Is(err, ErrPaymentMethodNotFound) {
		t.Fatalf("CreatePayment error = %v, want ErrPaymentMethodNotFound", err)
	}
	if len(repo.beginParams) != 0 {
		t.Fatal("不认识的支付方式不该建支付单")
	}
}

// TestCreatePaymentUnregisteredProviderIsExplicit 目录里的渠道指向一个没注册的适配器时
// 是一次**明确的失败**，不是静默降级。
//
// 这事今天只可能是装配错（main 里 catalog 与 Registry 用了两套名字），但那条路径上出错的
// 后果是「钱不知道走到谁那儿去」，所以它必须炸得清清楚楚。
func TestCreatePaymentUnregisteredProviderIsExplicit(t *testing.T) {
	repo := &fakeRepository{}
	// 目录里那条渠道的 Provider 是 `ums`，而注册表里只有一个叫 wechat 的适配器。
	stub := &stubProvider{name: "wechat"}
	svc := newTestService(t, repo, stub, testMethodCode)
	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("CreatePayment error = %v, want ErrProviderNotConfigured", err)
	}
	// 这条路上**支付单已经建出来了**：单号是渠道那边的商户单号，没有它就没有可发的报文，
	// 所以建单必然在查适配器之前（见 create.go 的事务 1 与 dispatchCreate 的顺序）。库里的
	// 那张单停在 created，由超时关单收走——不能断言「什么都没建」，那是另一条流程的形状。
	//
	// 能断言、也真正要紧的是这三件：报文没发出去（钱没有到任何人手里）、这张单没有被推进到
	// 任何一个终态、幂等键退了回去（装配错是运维自己的事，不该把调用方锁在 in_progress 上
	// 15 分钟）。
	if len(stub.calls) != 0 {
		t.Fatalf("provider calls = %d，适配器没查到时不该发出任何报文", len(stub.calls))
	}
	if len(repo.pendingCalls) != 0 || len(repo.failedCalls) != 0 {
		t.Fatalf("pending=%d failed=%d，这张单该停在 created 等超时关单",
			len(repo.pendingCalls), len(repo.failedCalls))
	}
	if len(repo.abandonCalls) != 1 {
		t.Fatalf("放开幂等键的次数 = %d，期望 1", len(repo.abandonCalls))
	}
}

// TestCreatePaymentRejectsAChannelMissingItsEnvironment 渠道没配齐时**发起不了新支付**。
//
// 这是渠道那一侧唯一还在的「不可用」：从前它是 payment_channels.status（运营停用一行），
// 今天它是缺环境变量——目录里的渠道在，但这次部署没给它 appId / mid 那几个账户值。
//
// 判据在支付单建出来之前，所以被拦下的那几种都要断言**库与渠道都没被碰过**：一次被拦下的
// 发起不该在库里留下任何痕迹。而错误串里必须点名缺的是哪几个变量——那条串是运维唯一的线索，
// 只说「渠道没配齐」会让人去翻整个 .env。
func TestCreatePaymentRejectsAChannelMissingItsEnvironment(t *testing.T) {
	cases := []struct {
		name     string
		override catalog.Config
		wantMiss string
	}{
		{"配齐了放行", catalog.Config{}, ""},
		{"缺 appId", catalog.Config{UMSAppID: " "}, "PAYMENT_UMS_APP_ID"},
		{"缺 mid 与 tid", catalog.Config{UMSMID: " ", UMSTID: " "}, "PAYMENT_UMS_MID, PAYMENT_UMS_TID"},
		{"缺来源编号", catalog.Config{UMSSourceCode: " "}, "PAYMENT_UMS_SOURCE_CODE"},
		// baseURL 不在其中：它有唯一正确的默认值（生产地址），由适配器套上，见 ums/config.go。
		{"缺 baseURL 不算缺", catalog.Config{UMSBaseURL: " "}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}
			repo := &fakeRepository{}
			svc := newServiceWithCatalog(t, repo, testCatalogWith(tc.override), stub, testMethodCode, nil,
				func(*catalog.Channel, string) string { return "secret" })

			result, err := svc.CreatePayment(context.Background(), validCreateRequest())
			if tc.wantMiss == "" {
				if err != nil {
					t.Fatalf("CreatePayment: %v", err)
				}
				if result.Status != model.PaymentPending {
					t.Fatalf("result = %+v, want a pending payment", result)
				}
				return
			}
			if !errors.Is(err, ErrChannelIncomplete) {
				t.Fatalf("CreatePayment error = %v, want ErrChannelIncomplete", err)
			}
			if !strings.Contains(err.Error(), tc.wantMiss) {
				t.Fatalf("错误里没点名缺的变量：%v，期望含 %q", err, tc.wantMiss)
			}
			if len(repo.beginParams) != 0 {
				t.Error("被拦下的发起不该建支付单")
			}
			if len(stub.calls) != 0 {
				t.Error("被拦下的发起不该碰渠道")
			}
		})
	}
}

// TestCreatePaymentWithoutAChannelIsStillAllowedForAccountFunding 没有渠道的支付方式
// 不该被上面那条渠道检查误伤。
//
// 账户出资（咖啡豆）**合法地**没有渠道：钱从账户域的豆账本出，一分都不经过渠道，渠道配没
// 配齐都与它无关（它压根没接银联商务的部署上也该能用）。这条用例把「无渠道」与「渠道没配齐」
// 明确分开——把前者也拦下来，就是让豆支付在任何没接第三方的部署上直接用不了。
func TestCreatePaymentWithoutAChannelIsStillAllowedForAccountFunding(t *testing.T) {
	repo := &fakeRepository{}
	ledger := &stubLedger{result: client.DeductResult{EntryID: "entry-1"}}
	svc := newBeanService(t, repo, ledger)
	// 目录里咖啡豆那条的 ChannelCode 是空的（没有第三方），所以 resolveMethod 取不到渠道。
	// 这不是配置问题，是这条路的定义。

	result, err := svc.CreatePayment(context.Background(), validBeanRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if result.Status != model.PaymentSucceeded {
		t.Fatalf("result = %+v, want a succeeded account-funded payment", result)
	}
}

// TestCreatePaymentReleasesTheIdempotencyKeyOnFailure 发起失败之后，同一个 Idempotency-Key
// 必须**立刻**能重试。
//
// 这就是那条「幂等行卡在 processing」的缺口：beginPayment 已经把幂等行写成 processing 并提交，
// 后面任何一个失败出口若不给它一个交代，同一个 key 在 IdempotencyRecoveryWindow（15 分钟）
// 里重试只会拿到 ErrIdempotencyInProgress——而那段时间里并没有任何东西真的在跑。
//
// 假仓储用 beginLocks 模拟那一行（见它的注释），所以这条用例在改动之前是**红的**：第二次
// CreatePayment 会撞上在途的 key。
func TestCreatePaymentReleasesTheIdempotencyKeyOnFailure(t *testing.T) {
	// 适配器编排成「调用没发出去」：这是 dispatchCreate 的一个典型出口（密钥读不到、参数拼
	// 不出来，见 provider.Provider 的注释），支付单留在 created、幂等行留成 processing。
	stub := &stubProvider{err: errors.New("secret is missing")}
	repo := &fakeRepository{beginLocks: map[string]bool{}}
	svc := newTestService(t, repo, stub, testMethodCode)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("渠道调用没发出去时 CreatePayment 必须返回错误")
	}
	if len(repo.abandonCalls) != 1 {
		t.Fatalf("AbandonPaymentAttempt 调了 %d 次，期望 1 次——失败出口要放开幂等键",
			len(repo.abandonCalls))
	}
	if got := repo.abandonCalls[0]; got.requestID != testRequest || got.paymentID != "payment-1" {
		t.Fatalf("放开的是 %+v，期望 (%s, payment-1)", got, testRequest)
	}

	// 同一个 request_id 再发一次。适配器这次是好的：这一笔要能真的建出来，而不是被那条
	// 还挂着的 processing 挡住。
	stub.err = nil
	stub.result = provider.CreateResult{Result: provider.ResultSuccess, ProviderTransactionID: "STUB-1"}
	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("同一个幂等键的立即重试失败了：%v", err)
	}
	if result.Status != model.PaymentPending {
		t.Fatalf("result = %+v, want a pending payment on retry", result)
	}
	if len(repo.beginParams) != 2 {
		t.Fatalf("BeginPayment 调了 %d 次，期望 2 次（一次失败、一次重试）", len(repo.beginParams))
	}
}

// TestCreatePaymentDoesNotReleaseTheKeyOnADecline 渠道**明确拒绝**不是「没走完的尝试」，
// 它的结论（failed）已经与那把 key 一起落库了，不能被当成失败出口退回去。
//
// 退回去的后果是实打实的：同一个 Idempotency-Key 再调会去建**第二张**支付单、再问一次渠道，
// 而契约说的「重复调用得到同一个答复」就不成立了。
func TestCreatePaymentDoesNotReleaseTheKeyOnADecline(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultFailed, FailureCode: "RISK_REJECTED"}}
	repo := &fakeRepository{beginLocks: map[string]bool{}}
	svc := newTestService(t, repo, stub, testMethodCode)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("渠道拒绝是结果不是错误：%v", err)
	}
	if result.Status != model.PaymentFailed {
		t.Fatalf("result = %+v, want failed", result)
	}
	if len(repo.abandonCalls) != 0 {
		t.Fatalf("渠道拒绝不该放开幂等键，实际放开了 %d 次", len(repo.abandonCalls))
	}
	if len(repo.failedCalls) != 1 {
		t.Fatalf("落库的失败结论 = %d 条，期望 1 条", len(repo.failedCalls))
	}
}

// TestCreatePaymentReleasesTheKeyEvenWhenTheDeductionCannotBeRecorded 扣豆那条路的结尾
// 失败时同样要放开。
//
// 走的是这一条：豆已经扣走（账户域那边扣成功）、留痕写失败、结算也失败。此时 payment 行上
// 的 account_entry_id 还是空的，所以真仓储的两个安全阀都不挡它——这一笔**可以**立刻重试，
// 而重试是安全的：账户域的幂等键是 order:{orderId}，重试只会回放同一笔账变。
//
// 顺带钉住「放开失败不改变调用方拿到的错误」：库在退的时候正好抖了一下，调用方看到的还应该是
// 那次渠道故障，而不是一句数据库错误。
func TestCreatePaymentReleasesTheKeyEvenWhenTheDeductionCannotBeRecorded(t *testing.T) {
	repo := &fakeRepository{beginLocks: map[string]bool{}, abandonErr: errors.New("database is unreachable")}
	ledger := &stubLedger{err: errors.New("account service is unreachable")}
	svc := newBeanService(t, repo, ledger)

	_, err := svc.CreatePayment(context.Background(), validBeanRequest())
	if err == nil {
		t.Fatal("账户域不可达时 CreatePayment 必须返回错误")
	}
	if errors.Is(err, repo.abandonErr) {
		t.Fatalf("放开幂等键失败把原来那个错误换掉了：%v", err)
	}
	if !strings.Contains(err.Error(), "account service is unreachable") {
		t.Fatalf("调用方拿到的错误 = %v，期望还是那次扣豆故障", err)
	}
	if len(repo.abandonCalls) != 0 {
		t.Fatal("假仓储已经在 AbandonPaymentAttempt 里返回了错误，不该记下一次成功的放开")
	}
}

// TestCreatePaymentSuccess 成功那一段：推进到 pending、落一条出资行、写幂等快照。
func TestCreatePaymentSuccess(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{
		Result:                provider.ResultSuccess,
		ProviderTransactionID: "STUB-1",
		PayParams:             map[string]string{"payUrl": "https://pay.example.test/x"},
	}}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, testMethodCode)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if result.Status != model.PaymentPending || result.Action != string(provider.ActionH5) {
		t.Fatalf("result = %+v, want status pending with action h5", result)
	}
	if result.ExpiresAtUnix == 0 {
		t.Error("ExpiresAtUnix is 0; 客户端拿不到超时就没法倒计时")
	}
	if len(repo.pendingCalls) != 1 {
		t.Fatalf("MarkPaymentPending called %d times, want 1", len(repo.pendingCalls))
	}
	pending := repo.pendingCalls[0]
	if len(pending.FundingLines) != 1 || pending.FundingLines[0].Amount != 1980 {
		t.Fatalf("funding lines = %+v, want one line of 1980", pending.FundingLines)
	}
	// 快照必须与回给客户端的响应是同一份字节，否则第二次点支付拿到的参数会不一样。
	snapshot, err := json.Marshal(pending.IdempotencyResponse)
	if err != nil {
		t.Fatalf("marshal idempotency snapshot: %v", err)
	}
	var fromSnapshot CreateResult
	if err := json.Unmarshal(snapshot, &fromSnapshot); err != nil {
		t.Fatalf("unmarshal idempotency snapshot: %v", err)
	}
	if fromSnapshot.PaymentNo != result.PaymentNo || fromSnapshot.Status != result.Status {
		t.Errorf("snapshot = %+v, want the same result the caller got (%+v)", fromSnapshot, result)
	}
	// 渠道调用流水成败都要有。
	if len(repo.providerCall) != 1 || repo.providerCall[0].Result != string(provider.ResultSuccess) {
		t.Fatalf("provider calls = %+v, want one success record", repo.providerCall)
	}
	if got := stub.calls[0].NotifyURL; got != "https://pay.example.test/v1/payments/callback/ums" {
		t.Errorf("NotifyURL = %q", got)
	}
	// 回跳地址与回调地址同源，而且**每一条支付都填**（不是只给 H5 填：谁用得上它是适配器
	// 的事，见 dispatchCreate 里那一行）。这条路走的是 action=h5，所以 stub 收到的那个值
	// 就是 H5 报文里 returnUrl 的来源。
	if got := stub.calls[0].ReturnURL; got != "https://pay.example.test/v1/payments/return/ums" {
		t.Errorf("ReturnURL = %q", got)
	}
	// walletOpenId 由服务端写入，调用方不能从 attach 里覆盖它。
	if got := stub.calls[0].Attach["walletOpenId"]; got != "openid-1" {
		t.Errorf("attach walletOpenId = %q, want openid-1", got)
	}
}

// TestCreatePaymentDeclinedIsAResultNotAnError 渠道明确拒绝返回的是**结果**，不是错误。
//
// 这是与 gRPC 契约对齐的那一条：failed = 发起即失败，客户端可以换方式重试。返回 error 会
// 让 order-service 把一次正常的业务拒绝当成服务端故障去重试。
func TestCreatePaymentDeclinedIsAResultNotAnError(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{
		Result: provider.ResultFailed, FailureCode: "RISK_REJECTED", FailureMessage: "风控拒绝",
	}}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, testMethodCode)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("a declined payment must not be an error, got %v", err)
	}
	if result.Status != model.PaymentFailed || result.FailureCode != "RISK_REJECTED" {
		t.Fatalf("result = %+v, want status failed with the provider's code", result)
	}
	if len(repo.failedCalls) != 1 || len(repo.pendingCalls) != 0 {
		t.Fatalf("failed=%d pending=%d, want exactly one failed write and no pending write",
			len(repo.failedCalls), len(repo.pendingCalls))
	}
}

// TestCreatePaymentDeclinedWithoutCodeGetsOne 适配器没说为什么拒的，也要留一个非空的码。
//
// 空的 failure_code 在后台列表里看起来像「没失败」，而这一单确实失败了。
func TestCreatePaymentDeclinedWithoutCodeGetsOne(t *testing.T) {
	stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultFailed}}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, testMethodCode)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if result.FailureCode == "" {
		t.Fatal("a declined payment was recorded without a failure code")
	}
}

// TestCreatePaymentNativePayWithoutAWalletIdentityIsADecline 是 openid 链路的下半段：
// 需要付款人身份的那条路（小程序内 requestPayment）取不到 openid 时，这一笔要**明确地失败**。
//
// 由服务层先判，而不是等适配器本地拒：适配器拒的是 error，在服务层等价于「我们这侧出了
// 问题」——支付单停在 created 等超时关单，用户拿不到一句能读懂的话，后台也看不出这一笔到底
// 怎么了。没绑微信其实是一个**结论**（换一种方式付就行），所以落 failed + failure_code，
// 并且一个字节都不发给渠道。
func TestCreatePaymentNativePayWithoutAWalletIdentityIsADecline(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		t.Run(fmt.Sprintf("%q", blank), func(t *testing.T) {
			// 适配器被编排成「会成功」：守卫要是漏了，这一笔就会推进到 pending 并且报文里
			// 的 openid 是空的——也就是这一条用例真正在防的那个结局。
			stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}
			repo := &fakeRepository{}
			svc := newTestService(t, repo, stub, catalog.CodeUMSMiniappWechat)
			in := validCreateRequestFor(catalog.CodeUMSMiniappWechat)
			in.WalletOpenID = blank

			result, err := svc.CreatePayment(context.Background(), in)
			if err != nil {
				t.Fatalf("没绑微信是一个结论不是一个错误: %v", err)
			}
			if result.Status != model.PaymentFailed {
				t.Fatalf("status = %q, want %q", result.Status, model.PaymentFailed)
			}
			if result.FailureCode != failureCodeWalletIdentityMissing {
				t.Fatalf("failureCode = %q, want %q", result.FailureCode, failureCodeWalletIdentityMissing)
			}
			if result.FailureMessage == "" {
				t.Error("失败没有留下说明：后台只能看见一个码")
			}
			if len(stub.calls) != 0 {
				t.Fatalf("provider calls = %d; 没有付款人身份的报文不该发出去", len(stub.calls))
			}
			if len(repo.failedCalls) != 1 || len(repo.pendingCalls) != 0 {
				t.Fatalf("failed=%d pending=%d, want exactly one failed write and no pending write",
					len(repo.failedCalls), len(repo.pendingCalls))
			}
			if got := repo.failedCalls[0].FailureCode; got != failureCodeWalletIdentityMissing {
				t.Errorf("落库的 failure_code = %q, want %q", got, failureCodeWalletIdentityMissing)
			}
		})
	}
}

// TestCreatePaymentWithoutAWalletIdentityIsOnlyBlockedForNativePay 钉住这条守卫的边界：
// 它只挡 native_pay。
//
// 目录里其余几条都不需要付款人身份，一刀切会把没绑微信的用户从 H5 收银台上一起挡在门外
// ——而他本来是能付款的。
func TestCreatePaymentWithoutAWalletIdentityIsOnlyBlockedForNativePay(t *testing.T) {
	for _, code := range []string{
		catalog.CodeUMSH5Alipay, catalog.CodeUMSH5Wechat, catalog.CodeUMSH5Upqr, catalog.CodeUMSH5WechatMinipay,
	} {
		t.Run(code, func(t *testing.T) {
			stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}
			repo := &fakeRepository{}
			svc := newTestService(t, repo, stub, code)
			in := validCreateRequestFor(code)
			in.WalletOpenID = ""

			result, err := svc.CreatePayment(context.Background(), in)
			if err != nil {
				t.Fatalf("CreatePayment: %v", err)
			}
			if len(stub.calls) != 1 {
				t.Fatalf("provider calls = %d, want 1（这条方式不需要付款人身份）", len(stub.calls))
			}
			if result.Status != model.PaymentPending {
				t.Fatalf("status = %q, want %q", result.Status, model.PaymentPending)
			}
		})
	}
}

// TestCreatePaymentUncertainResultLeavesPaymentInCreated 结果不明时**什么都不改**。
//
// 这是整个发起支付里最反直觉的一条，也是最重要的：超时或结果未知时渠道侧可能已经有一张
// 能付的预支付单。把本地单标成 failed，就会出现「用户真把那笔钱付了，而我们这边是一张
// 已失败的单」。所以支付单停在 created，由超时关单扫描收走。
func TestCreatePaymentUncertainResultLeavesPaymentInCreated(t *testing.T) {
	for _, result := range []provider.Result{provider.ResultTimeout, provider.ResultUnknown, ""} {
		t.Run(string(result)+"", func(t *testing.T) {
			stub := &stubProvider{result: provider.CreateResult{Result: result}}
			repo := &fakeRepository{}
			svc := newTestService(t, repo, stub, testMethodCode)

			_, err := svc.CreatePayment(context.Background(), validCreateRequest())
			if !errors.Is(err, ErrProviderResultUncertain) {
				t.Fatalf("error = %v, want ErrProviderResultUncertain", err)
			}
			if len(repo.failedCalls) != 0 || len(repo.pendingCalls) != 0 {
				t.Fatalf("failed=%d pending=%d, want the payment left untouched in created",
					len(repo.failedCalls), len(repo.pendingCalls))
			}
			// 流水仍然要记：它是事后判断「到底发生了什么」的唯一依据。
			if len(repo.providerCall) != 1 {
				t.Fatalf("provider calls = %d, want 1", len(repo.providerCall))
			}
		})
	}
}

// TestCreatePaymentProviderCallFailureStaysInCreated 调用根本没发出去时同样停在 created。
//
// 与「结果不明」走同一条收尾，但错误不同：这是我们自己的问题（密钥没配、参数拼不出来），
// 不是一个渠道结论。
func TestCreatePaymentProviderCallFailureStaysInCreated(t *testing.T) {
	stub := &stubProvider{err: errors.New("secret is missing")}
	repo := &fakeRepository{}
	svc := newTestService(t, repo, stub, testMethodCode)

	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("a provider call that never went out must surface an error")
	}
	if len(repo.failedCalls) != 0 || len(repo.pendingCalls) != 0 {
		t.Fatalf("failed=%d pending=%d, want the payment left untouched in created",
			len(repo.failedCalls), len(repo.pendingCalls))
	}
}

// TestCreatePaymentReplaysRecordedSnapshot 同一个 request_id 再调时原样回放上次的结论。
//
// 成功与失败的回放走同一条路：客户端第二次点支付拿到的必须是同一个答复，而不是「上次被拒、
// 这次又去问了一遍渠道」。
func TestCreatePaymentReplaysRecordedSnapshot(t *testing.T) {
	previous := CreateResult{
		PaymentNo: "PAY20260914120000000001", Status: model.PaymentFailed,
		FailureCode: "RISK_REJECTED", FailureMessage: "风控拒绝",
	}
	snapshot, err := json.Marshal(previous)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	stub := &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}
	repo := &fakeRepository{beginHit: true, beginSnap: snapshot}
	svc := newTestService(t, repo, stub, testMethodCode)

	result, err := svc.CreatePayment(context.Background(), validCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	// 用 DeepEqual 而不是 ==：CreateResult 里有 PayParams 这个 map，结构体不可比较。
	if !reflect.DeepEqual(*result, previous) {
		t.Fatalf("replayed result = %+v, want %+v", *result, previous)
	}
	// 最重要的一条：回放**不去碰渠道**。否则「重试」就变成真的又发起了一笔支付。
	if len(stub.calls) != 0 {
		t.Fatalf("the provider was called %d times during a replay, want 0", len(stub.calls))
	}
	if len(repo.pendingCalls) != 0 || len(repo.failedCalls) != 0 {
		t.Error("a replay must not write any payment state")
	}
}

// TestCreatePaymentEmptySnapshotIsAnError 幂等行在但快照是空的：报错让人来查。
//
// 只可能是一行被标成 succeeded 却没写 response 的脏数据。在这里猜一个支付参数给客户端
// 是最坏的选择——客户端会拿一份凭空造出来的参数去调起支付。
func TestCreatePaymentEmptySnapshotIsAnError(t *testing.T) {
	repo := &fakeRepository{beginHit: true, beginSnap: nil}
	svc := newTestService(t, repo, &stubProvider{}, testMethodCode)
	if _, err := svc.CreatePayment(context.Background(), validCreateRequest()); err == nil {
		t.Fatal("an idempotency row without a recorded response must be an error")
	}
}

// TestDivisionForBuildsTheChannelInstruction 是「本地算好的分账计划」走到「报文里那三个键」
// 中间的那一步。
//
// 它单拎出来测，是因为它的两种错法**都不会报错**：
//
//   - 把 Mid 写成 AccountID：报文照样发得出去，只是钱会分给一个渠道不认识的号。渠道那边要么
//     拒掉整笔支付，要么按它自己的兜底处理，而本地从任务到流水一行异象都没有。
//   - 把「没有接收方」翻成一个空子单数组：渠道收到的是 `divisionFlag=true` + 空数组，规范说
//     这是非法请求，拒的是**整笔支付**——而分账本来是可有可无的那一半。
//
// 所以这里的判据是**整份指令的逐字段相等**，不是「有几条子单」。
func TestDivisionForBuildsTheChannelInstruction(t *testing.T) {
	cases := []struct {
		name string
		plan *repository.SettlementPlan
		want *provider.DivisionInstruction
	}{
		{
			// 账户出资（咖啡豆）那条路：钱不从渠道走，压根没有可分的那一份。
			name: "没有计划",
			plan: nil,
			want: nil,
		},
		{
			// 没命中规则：任务照样建（整单归平台），但渠道那边没有子单可分。**不能**返回一个
			// PlatformAmount 等于全额的指令——规范说 divisionFlag=true 时子单不能为空，而
			// 「整单归平台」本来就等于不分账。
			name: "计划在、没有接收方",
			plan: &repository.SettlementPlan{PlatformAmount: 12800},
			want: nil,
		},
		{
			// AccountID 与 ReceiverID 特意取不同的值：谁写反了这条都必须红。
			name: "多个接收方",
			plan: &repository.SettlementPlan{
				PlatformAmount: 300,
				Receivers: []repository.SettlementReceiverLine{
					{AccountID: "acct-1", ReceiverID: "MID-1", Amount: 12000},
					{AccountID: "acct-2", ReceiverID: "MID-2", Amount: 500},
				},
			},
			want: &provider.DivisionInstruction{
				PlatformAmount: 300,
				SubOrders: []provider.DivisionSubOrder{
					{Mid: "MID-1", Amount: 12000},
					{Mid: "MID-2", Amount: 500},
				},
			},
		},
		{
			// 整单都分出去（比例全给了接收方）是配得出来的：平台那一份是 0，而它到这里必须还是
			// 一个**有值的 0**——报文里那个键能不能留住 0，由 ums 那一侧的用例守。
			name: "平台那一份是 0",
			plan: &repository.SettlementPlan{
				Receivers: []repository.SettlementReceiverLine{
					{AccountID: "acct-1", ReceiverID: "MID-1", Amount: 12800},
				},
			},
			want: &provider.DivisionInstruction{
				SubOrders: []provider.DivisionSubOrder{{Mid: "MID-1", Amount: 12800}},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := divisionFor(testCase.plan)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("divisionFor() = %+v，期望 %+v", got, testCase.want)
			}
		})
	}
}

// TestCreateRequestHashIgnoresPresentationFields 幂等哈希只认业务字段。
//
// 判据是「改了它，还算不算同一笔支付」：subject 与 attach 是展示与渠道附加数据，改了它们
// 不该让一次重试变成「同一个 key 换了请求体」的冲突；而金额变了**必须**冲突。
func TestCreateRequestHashIgnoresPresentationFields(t *testing.T) {
	base := validCreateRequest()

	presentational := base
	presentational.Subject = "换了描述"
	presentational.Attach = map[string]string{"deviceNo": "D-1"}
	presentational.WalletOpenID = "openid-2"
	presentational.TraceID = "trace-2"
	if createRequestHash(base) != createRequestHash(presentational) {
		t.Error("subject / attach / walletOpenId / traceId must not change the idempotency hash")
	}

	business := base
	business.Amount = 1981
	if createRequestHash(base) == createRequestHash(business) {
		t.Error("a different amount must produce a different idempotency hash")
	}
}
