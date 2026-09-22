package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// 这一层管的是**请求的形状**：范围与行自不自洽、理由有没有、图片是不是 URL、审核人从哪来。
// 金额、状态、互斥（同一行有没有别的售后单在跑）都由仓储在锁内判，所以那些用例在
// repository 的集成测试里，不在这里——在没有数据库的地方假装它们已经验过了，比不测更糟。
//
// 内嵌 Repository 是为了只实现本文件真正走到的方法：走到别的会 nil panic，那是故意的，
// 它说明用例越界了，不该悄悄返回个零值把事情糊过去。

type fakeAfterSaleRepo struct {
	Repository

	applyParams *repository.ApplyAfterSaleParams
	applyRow    *repository.AfterSaleRow
	applyReplay bool
	applyErr    error

	// gatePlan / gateCheckable 是受理之前那次只读的「这一单要冻住哪几笔发放、一共几张」。
	// 默认 checkable=false：绝大多数用例造的单没承诺福卡，那道闸根本不该开——真开了的话
	// 桩上的 quoter 是 nil，会当场炸，这本身就是一条断言。
	gatePlan      repository.FortuneCardFreezePlan
	gateCheckable bool
	gateErr       error

	reviewParams *repository.ReviewAfterSaleParams
	reviewRow    *repository.AfterSaleRow
	reviewErr    error

	cancelParams *repository.CancelAfterSaleParams
	cancelRow    *repository.AfterSaleRow
	cancelErr    error

	// —— 退款（见 refund.go）——
	findRow *repository.AfterSaleRow
	findErr error

	startParams *repository.StartRefundParams
	startRow    *repository.AfterSaleRow
	startReplay bool
	startErr    error

	advanceParams *repository.AdvanceRefundParams
	advanceRow    *repository.AfterSaleRow
	advanceReplay bool
	advanceErr    error
}

func (f *fakeAfterSaleRepo) ApplyAfterSale(_ context.Context, p repository.ApplyAfterSaleParams) (*repository.AfterSaleRow, bool, error) {
	f.applyParams = &p
	if f.applyErr != nil {
		return nil, false, f.applyErr
	}
	return f.applyRow, f.applyReplay, nil
}

func (f *fakeAfterSaleRepo) FortuneCardFreezeGate(_ context.Context, _, _, _ string) (repository.FortuneCardFreezePlan, bool, error) {
	if f.gateErr != nil {
		return repository.FortuneCardFreezePlan{}, false, f.gateErr
	}
	return f.gatePlan, f.gateCheckable, nil
}

func (f *fakeAfterSaleRepo) ReviewAfterSale(_ context.Context, p repository.ReviewAfterSaleParams) (*repository.AfterSaleRow, error) {
	f.reviewParams = &p
	if f.reviewErr != nil {
		return nil, f.reviewErr
	}
	return f.reviewRow, nil
}

func (f *fakeAfterSaleRepo) CancelAfterSale(_ context.Context, p repository.CancelAfterSaleParams) (*repository.AfterSaleRow, error) {
	f.cancelParams = &p
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	return f.cancelRow, nil
}

func (f *fakeAfterSaleRepo) FindAfterSaleByNo(_ context.Context, afterSaleNo string) (*repository.AfterSaleRow, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.findRow, nil
}

func (f *fakeAfterSaleRepo) StartRefund(_ context.Context, p repository.StartRefundParams) (*repository.AfterSaleRow, bool, error) {
	f.startParams = &p
	if f.startErr != nil {
		return nil, false, f.startErr
	}
	return f.startRow, f.startReplay, nil
}

func (f *fakeAfterSaleRepo) AdvanceRefund(_ context.Context, p repository.AdvanceRefundParams) (*repository.AfterSaleRow, bool, error) {
	f.advanceParams = &p
	if f.advanceErr != nil {
		return nil, false, f.advanceErr
	}
	return f.advanceRow, f.advanceReplay, nil
}

func newAfterSaleService(repo *fakeAfterSaleRepo) *OrderService {
	// wallets 为 nil：售后这条链路不发起支付，也不该去问身份域。
	// payments 也为 nil——绝大多数用例走的是**审核之前**的那几道校验，压根到不了退款那一步。
	// 真要走到的用例用下面那个带桩的构造（见 TestReviewAfterSaleStartsTheRefund）。
	return newAfterSaleServiceWith(repo, nil)
}

func newAfterSaleServiceWith(repo *fakeAfterSaleRepo, payments PaymentCreator) *OrderService {
	return New(repo, nil, payments, nil, nil, nil, Options{})
}

// fakeFortuneCardQuoter 冒充账户域那一次冻结预览。它只回答两个数，不写任何东西——
// 真实实现（client.FortuneCardQuoter）的职责就是这两句，网络那一层由它自己的测试盯。
type fakeFortuneCardQuoter struct {
	params *client.FreezeQuoteInput
	quote  *client.FortuneCardFreezeQuote
	err    error
}

func (f *fakeFortuneCardQuoter) FreezeQuote(_ context.Context, in client.FreezeQuoteInput) (*client.FortuneCardFreezeQuote, error) {
	f.params = &in
	if f.err != nil {
		return nil, f.err
	}
	return f.quote, nil
}

// newAfterSaleServiceWithQuoter 造一个接了账户域的 service，只给「福卡没用过才受理」那条
// 规则用。其余读端一律 nil：这条链不碰设备、会员、支付、身份。
func newAfterSaleServiceWithQuoter(repo *fakeAfterSaleRepo, fortuneCards FortuneCardQuoter) *OrderService {
	return New(repo, nil, nil, nil, nil, fortuneCards, Options{})
}

// promisedCards 造一份「这一单承诺了 3 张福卡」的闸门答案：两个键（饮品那笔 + 加购那笔），
// 一共 3 张。键的内容不重要，重要的是它被原样交给账户域——用例断言的就是这件事。
func promisedCards() repository.FortuneCardFreezePlan {
	return repository.FortuneCardFreezePlan{
		EntryKeys: []string{
			"order:00000000-0000-0000-0000-000000000001:base",
			"order:00000000-0000-0000-0000-000000000001:bonus:C1",
		},
		Cards: 3,
	}
}

// refundableRow 是一张「已经审核通过、可以推去退款」的售后单。
//
// PaymentNo 必须在这一行上（它来自 orders.payment_no，见 AfterSaleRow 的注释）：
// 没有它就走到 ErrOrderHasNoPayment，那是设备单的结论，不是这里要测的东西。
func refundableRow(afterSaleNo, status string) *repository.AfterSaleRow {
	line := "00000000-0000-0000-0000-000000000003"
	return &repository.AfterSaleRow{
		AfterSale: &model.OrderAfterSale{
			AfterSaleNo:  afterSaleNo,
			OrderNo:      "ORD20260914000000000001",
			Status:       status,
			Scope:        model.AfterSaleScopeDrink,
			OrderLineID:  &line,
			Reason:       "少冰做成了正常冰",
			RefundAmount: 1800,
		},
		PaymentNo: "PAY20260914000000000001",
	}
}

// validApply 是一份能过形状校验的申请，用例各自改它关心的那一栏。
func validApply() ApplyAfterSaleInput {
	return ApplyAfterSaleInput{
		OrderID:        "00000000-0000-0000-0000-000000000001",
		UserID:         "00000000-0000-0000-0000-000000000002",
		IdempotencyKey: "key-1",
		Scope:          model.AfterSaleScopeAll,
		Reason:         "买错了",
	}
}

// applyRow 是仓储申请成功后返回的那一行。只要单号有值就够用例断言「服务层原样透传」——
// 其余的列由仓储的集成测试负责，在这个没有数据库的地方假装它们有内容反而会骗人。
func applyRow(afterSaleNo string) *repository.AfterSaleRow {
	return &repository.AfterSaleRow{AfterSale: &model.OrderAfterSale{AfterSaleNo: afterSaleNo}}
}

func TestApplyAfterSaleShapeValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ApplyAfterSaleInput)
		want   error
		// notShape：这条不是「字段填错了」而是「查不到这张单」，controller 回 404 而不是
		// 400（同 CancelOrder 对空订单 id 的处理）。
		notShape bool
	}{
		{"没有订单 id", func(in *ApplyAfterSaleInput) { in.OrderID = "  " }, ErrOrderNotFound, true},
		{"没有幂等键", func(in *ApplyAfterSaleInput) { in.IdempotencyKey = "" }, ErrIdempotencyKeyRequired, false},
		{"没有用户", func(in *ApplyAfterSaleInput) { in.UserID = "" }, ErrUserRequired, false},
		{"范围不认识", func(in *ApplyAfterSaleInput) { in.Scope = "everything" }, ErrAfterSaleScopeInvalid, false},
		{"范围是会员", func(in *ApplyAfterSaleInput) { in.Scope = model.AfterSaleScopeMembership }, ErrAfterSaleMembershipUnsupported, false},
		{"没有理由", func(in *ApplyAfterSaleInput) { in.Reason = "   " }, ErrAfterSaleReasonRequired, false},
		{"整单退带了行", func(in *ApplyAfterSaleInput) {
			line := "00000000-0000-0000-0000-000000000003"
			in.OrderLineID = &line
		}, ErrAfterSaleLineNotAllowed, false},
		{"按行退没带行", func(in *ApplyAfterSaleInput) { in.Scope = model.AfterSaleScopeDrink }, ErrAfterSaleLineRequired, false},
		{"按行退带了空行", func(in *ApplyAfterSaleInput) {
			in.Scope = model.AfterSaleScopeDrink
			blank := "  "
			in.OrderLineID = &blank
		}, ErrAfterSaleLineRequired, false},
		{"图片不是 URL", func(in *ApplyAfterSaleInput) { in.Images = []string{"javascript:alert(1)"} }, ErrAfterSaleImagesInvalid, false},
		{"图片缺 host", func(in *ApplyAfterSaleInput) { in.Images = []string{"http://"} }, ErrAfterSaleImagesInvalid, false},
		{"图片太多", func(in *ApplyAfterSaleInput) {
			for i := 0; i <= maxAfterSaleImages; i++ {
				in.Images = append(in.Images, "https://example.com/a.jpg")
			}
		}, ErrAfterSaleImagesInvalid, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{}
			in := validApply()
			tc.mutate(&in)
			_, _, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ApplyAfterSale() = %v, want %v", err, tc.want)
			}
			if repo.applyParams != nil {
				// 校验没过就不该走到仓储：走到那一步意味着请求已经落库过一次了。
				t.Fatalf("形状校验失败却调用了仓储: %+v", repo.applyParams)
			}
			if tc.notShape {
				return
			}
			if !IsValidationError(err) {
				t.Fatalf("校验错误没登记进 ValidationErrors，controller 会回 500: %v", err)
			}
		})
	}
}

// TestApplyAfterSalePassesTrimmedShape 盯的是「服务层替仓储收拾干净了什么」：单号在这里
// 生成、图片在这里序列化、范围与行的等价关系在这里定死。这些在返回值上看不出来，只能看
// 落库前那一份入参。
func TestApplyAfterSalePassesTrimmedShape(t *testing.T) {
	line := "00000000-0000-0000-0000-000000000003"
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), applyReplay: true}
	in := validApply()
	in.Scope = " " + model.AfterSaleScopeDrink + " "
	in.OrderLineID = &line
	in.Reason = " 少冰做成了正常冰 "
	in.Images = []string{" https://example.com/a.jpg "}

	row, replayed, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), in)
	if err != nil {
		t.Fatalf("ApplyAfterSale() error = %v", err)
	}
	if row.AfterSale.AfterSaleNo != "REF1" {
		t.Fatalf("结果没有原样返回仓储的返回: %+v", row)
	}
	// replayed 必须原样透出来：controller 靠它决定回 200 还是 201，服务层吞掉的话
	// 每一次重放都会被当成一次新的申请。
	if !replayed {
		t.Fatal("仓储说是重放，服务层却回 false：controller 会给重放回 201")
	}
	got := repo.applyParams
	if got.Scope != model.AfterSaleScopeDrink {
		t.Fatalf("scope = %q, want %q", got.Scope, model.AfterSaleScopeDrink)
	}
	if got.OrderLineID == nil || *got.OrderLineID != line {
		t.Fatalf("orderLineId = %v, want %s", got.OrderLineID, line)
	}
	if got.Reason != "少冰做成了正常冰" {
		t.Fatalf("reason = %q, 没有去空白", got.Reason)
	}
	if got.ActorType != model.ActorUser {
		t.Fatalf("actorType = %q, want %q", got.ActorType, model.ActorUser)
	}
	if !strings.HasPrefix(got.AfterSaleNo, "REF") {
		t.Fatalf("售后单号 %q 不以 REF 开头", got.AfterSaleNo)
	}
	// 图片以 JSON 数组落库（列类型是 JSONB），URL 的前后空白也要去掉。
	var images []string
	if err := json.Unmarshal(got.Images, &images); err != nil {
		t.Fatalf("images 不是合法 JSON 数组: %v (%s)", err, got.Images)
	}
	if len(images) != 1 || images[0] != "https://example.com/a.jpg" {
		t.Fatalf("images = %v", images)
	}
}

// TestApplyAfterSaleWithoutImages 确认「没传图」落成 []，不是 null：列的默认值就是 []，
// 两种空在库里长得一样，读出来才不用判两次。
func TestApplyAfterSaleWithoutImages(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1")}
	if _, _, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), validApply()); err != nil {
		t.Fatalf("ApplyAfterSale() error = %v", err)
	}
	if string(repo.applyParams.Images) != "[]" {
		t.Fatalf("images = %s, want []", repo.applyParams.Images)
	}
}

// TestApplyAfterSaleForwardsRepositoryError 确认仓储的判断原样传到调用方：这里的错误是
// 「这一行已经退过款了」这类锁内事实，服务层不该把它翻译成别的东西——controller 比的
// 是同一个值。
func TestApplyAfterSaleForwardsRepositoryError(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyErr: repository.ErrAfterSaleAlreadyRefunded}
	_, _, err := newAfterSaleService(repo).ApplyAfterSale(t.Context(), validApply())
	if !errors.Is(err, ErrAfterSaleAlreadyRefunded) {
		t.Fatalf("err = %v, want ErrAfterSaleAlreadyRefunded", err)
	}
}

// TestApplyAfterSaleRejectsAnOrderWithUsedFortuneCards 是那条业务规则的正例：一单承诺了
// 3 张福卡，账户域答「还挂着 3 张，但此刻只冻得上 1 张」——说明这片池子里已经有卡被抽走
// 了，退钱也追不回来，**不受理**。
//
// 两条断言缺一不可：错误要能被 controller 认出来（errors.Is），售后单要**根本没落库**
// （applyParams 为 nil）。只判前者的话，一个「先落库再报错」的实现照样能过——那正是这条
// 规则要根除的旧行为（照收申请、冻结时悄悄钳住）。
func TestApplyAfterSaleRejectsAnOrderWithUsedFortuneCards(t *testing.T) {
	plan := promisedCards()
	repo := &fakeAfterSaleRepo{
		applyRow:      applyRow("REF1"),
		gatePlan:      plan,
		gateCheckable: true,
	}
	quoter := &fakeFortuneCardQuoter{quote: &client.FortuneCardFreezeQuote{Granted: 3, Freezable: 1}}
	in := validApply()

	row, replayed, err := newAfterSaleServiceWithQuoter(repo, quoter).ApplyAfterSale(t.Context(), in)
	if !errors.Is(err, ErrAfterSaleFortuneCardsUsed) {
		t.Fatalf("err = %v, want ErrAfterSaleFortuneCardsUsed", err)
	}
	if row != nil || replayed {
		t.Fatalf("被拒的申请回了一个结果: row=%+v replayed=%v", row, replayed)
	}
	if repo.applyParams != nil {
		t.Fatal("福卡已经用过了，售后单还是落了库")
	}
	// 问的是这一单、这个人、这几笔发放。键算错（比如漏了 bonus）会问出一份别人的答案。
	if quoter.params == nil {
		t.Fatal("没问账户域就下了结论")
	}
	if quoter.params.UserID != in.UserID {
		t.Fatalf("问的是 %q 的账户，want %q", quoter.params.UserID, in.UserID)
	}
	if len(quoter.params.EntryKeys) != len(plan.EntryKeys) || quoter.params.EntryKeys[0] != plan.EntryKeys[0] {
		t.Fatalf("entryKeys = %v, want %v", quoter.params.EntryKeys, plan.EntryKeys)
	}
}

// TestApplyAfterSaleAllowsAnOrderWhoseCardsHaveNotLanded 钉住两个数不能合成一个：
// Granted 为 0 是**「发放还没落库」**，不是「用过了」。这一单承诺 3 张、账户域一张都还没
// 看见（先申请退款、后完成订单是设计支持的），此时 Freezable 也是 0——只看 Freezable
// 的实现会在这里给一个什么都没做错的用户一句「你的卡用过了」，把一条正常路径堵死。
func TestApplyAfterSaleAllowsAnOrderWhoseCardsHaveNotLanded(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gatePlan: promisedCards(), gateCheckable: true}
	quoter := &fakeFortuneCardQuoter{quote: &client.FortuneCardFreezeQuote{Granted: 0, Freezable: 0}}

	row, _, err := newAfterSaleServiceWithQuoter(repo, quoter).ApplyAfterSale(t.Context(), validApply())
	if err != nil {
		t.Fatalf("发放还没落库就拒了申请: %v", err)
	}
	if row == nil || repo.applyParams == nil {
		t.Fatal("申请没有落库")
	}
}

// TestApplyAfterSaleAcceptsWhenEveryCardIsStillFreezable 是对照组：3 张一张没少，放行。
// 没有它的话，一个「承诺了福卡就一律拒」的实现能通过上面那两条里的第一条以外的一切。
func TestApplyAfterSaleAcceptsWhenEveryCardIsStillFreezable(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gatePlan: promisedCards(), gateCheckable: true}
	quoter := &fakeFortuneCardQuoter{quote: &client.FortuneCardFreezeQuote{Granted: 3, Freezable: 3}}

	if _, _, err := newAfterSaleServiceWithQuoter(repo, quoter).ApplyAfterSale(t.Context(), validApply()); err != nil {
		t.Fatalf("一张没用的单被拒了: %v", err)
	}
}

// TestApplyAfterSaleTreatsAnUnansweredQuoteAsUnavailable 盯「没结论」与「有结论」的分界。
// 问不到账户域时**不能**当成放行（那会把追不回来的卡退出去），也**不能**说「卡用过了」
// （那是另一个结论，会冤枉一个什么都没做错的用户）——回的是 503 那个哨兵。
//
// 两种「问不到」都要盖：下游报错，以及压根没接账户域的部署（fortuneCards 为 nil）。
func TestApplyAfterSaleTreatsAnUnansweredQuoteAsUnavailable(t *testing.T) {
	t.Run("下游报错", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gatePlan: promisedCards(), gateCheckable: true}
		quoter := &fakeFortuneCardQuoter{err: client.ErrFortuneCardServiceUnavailable}

		_, _, err := newAfterSaleServiceWithQuoter(repo, quoter).ApplyAfterSale(t.Context(), validApply())
		if !errors.Is(err, ErrFortuneCardQuoteUnavailable) {
			t.Fatalf("err = %v, want ErrFortuneCardQuoteUnavailable", err)
		}
		if errors.Is(err, ErrAfterSaleFortuneCardsUsed) {
			t.Fatal("把「没问到」说成了「卡用过了」")
		}
		if repo.applyParams != nil {
			t.Fatal("没问到就放行了")
		}
	})
	t.Run("没接账户域", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gatePlan: promisedCards(), gateCheckable: true}

		// 这个部署的 quoter 是 nil。静默放行是这里最危险的走法：规则会整个失效，
		// 而调用方看到的是一次成功的申请。
		_, _, err := newAfterSaleServiceWithQuoter(repo, nil).ApplyAfterSale(t.Context(), validApply())
		if !errors.Is(err, ErrFortuneCardQuoteUnavailable) {
			t.Fatalf("err = %v, want ErrFortuneCardQuoteUnavailable", err)
		}
		if repo.applyParams != nil {
			t.Fatal("没接账户域就放行了")
		}
	})
}

// TestApplyAfterSaleSkipsTheGateWhenNoCardsWerePromised 确认这道闸只在「这一单真的承诺了
// 福卡」时才开：没承诺福卡的单（大部分单）一次账户域往返都不该付——所以 quoter 故意留
// nil，真去问了会当场炸。闸门自己报 checkable=false 是常见情形（订单不存在、不是本人的、
// 状态不是已支付/已完成），承诺 0 张是另一种。
func TestApplyAfterSaleSkipsTheGateWhenNoCardsWerePromised(t *testing.T) {
	cases := map[string]struct {
		checkable bool
		plan      repository.FortuneCardFreezePlan
	}{
		"这一单没承诺福卡":    {checkable: true, plan: repository.FortuneCardFreezePlan{Cards: 0}},
		"这一单根本不是他的":   {checkable: false, plan: promisedCards()},
		"这一单还不到能退的状态": {checkable: false, plan: promisedCards()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gatePlan: tc.plan, gateCheckable: tc.checkable}
			if _, _, err := newAfterSaleServiceWithQuoter(repo, nil).ApplyAfterSale(t.Context(), validApply()); err != nil {
				t.Fatalf("不该问账户域的单被拦下了: %v", err)
			}
			if repo.applyParams == nil {
				t.Fatal("申请没有落库")
			}
		})
	}
}

// TestApplyAfterSaleForwardsGateError 确认闸门自己出错时不会被吞掉：读订单失败是一次
// 故障，不是「这一单没承诺福卡」。吞掉它就等于在库抖动时静默放行。
func TestApplyAfterSaleForwardsGateError(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gateErr: errors.New("boom")}
	_, _, err := newAfterSaleServiceWithQuoter(repo, nil).ApplyAfterSale(t.Context(), validApply())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want 原样透出的闸门错误", err)
	}
	if repo.applyParams != nil {
		t.Fatal("闸门出错还是落了库")
	}
}

// TestApplyAfterSaleChecksShapeBeforeTheCards 钉住次序：形状不对的请求（scope 与行对不
// 上、没写理由）先拿到它们各自该拿的那个错，不会先撞上福卡那道闸。次序错了的话，一个
// 参数写错的用户会收到「你的福卡用过了」——一句与他的错处毫不相干的结论。
func TestApplyAfterSaleChecksShapeBeforeTheCards(t *testing.T) {
	repo := &fakeAfterSaleRepo{applyRow: applyRow("REF1"), gatePlan: promisedCards(), gateCheckable: true}
	quoter := &fakeFortuneCardQuoter{quote: &client.FortuneCardFreezeQuote{Granted: 3, Freezable: 0}}
	in := validApply()
	in.Reason = "  "

	_, _, err := newAfterSaleServiceWithQuoter(repo, quoter).ApplyAfterSale(t.Context(), in)
	if !errors.Is(err, ErrAfterSaleReasonRequired) {
		t.Fatalf("err = %v, want ErrAfterSaleReasonRequired", err)
	}
	if quoter.params != nil {
		t.Fatal("形状都没过就问账户域了")
	}
}

func TestReviewAfterSaleShapeValidation(t *testing.T) {
	cases := []struct {
		name string
		in   ReviewAfterSaleInput
		want error
	}{
		{"没有单号", ReviewAfterSaleInput{Action: model.AfterSaleActionApprove, ReviewedBy: "u1"}, ErrAfterSaleNotFound},
		{"动作不认识", ReviewAfterSaleInput{AfterSaleNo: "REF1", Action: "maybe", ReviewedBy: "u1"}, ErrAfterSaleActionInvalid},
		{"驳回没写理由", ReviewAfterSaleInput{AfterSaleNo: "REF1", Action: model.AfterSaleActionReject, ReviewedBy: "u1"}, ErrAfterSaleRemarkRequired},
		{"没有审核人", ReviewAfterSaleInput{AfterSaleNo: "REF1", Action: model.AfterSaleActionApprove}, ErrForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{}
			_, err := newAfterSaleService(repo).ReviewAfterSale(t.Context(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReviewAfterSale() = %v, want %v", err, tc.want)
			}
			if repo.reviewParams != nil {
				t.Fatalf("校验失败却调用了仓储: %+v", repo.reviewParams)
			}
		})
	}
}

// TestReviewAfterSaleCarriesFortuneCardConfirmation 盯的是福卡规则的那个开关有没有被
// 原样传下去：它决定这笔退款能不能放行，中途丢掉等于给「订单送过福卡的退款」开后门。
func TestReviewAfterSaleCarriesFortuneCardConfirmation(t *testing.T) {
	approved := refundableRow("REF1", model.AfterSaleStatusApproved)
	repo := &fakeAfterSaleRepo{reviewRow: approved, startRow: approved}
	_, err := newAfterSaleServiceWith(repo, &stubPaymentCreator{
		refundOutcome: &client.RefundOutcome{RefundNo: "RF1", Status: "processing"},
	}).ReviewAfterSale(t.Context(), ReviewAfterSaleInput{
		AfterSaleNo:                " REF1 ",
		Action:                     model.AfterSaleActionApprove,
		Remark:                     " 客服已核 ",
		FortuneCardUnusedConfirmed: true,
		ReviewedBy:                 "admin-1",
		TraceID:                    "trace-1",
	})
	if err != nil {
		t.Fatalf("ReviewAfterSale() error = %v", err)
	}
	got := repo.reviewParams
	if got.AfterSaleNo != "REF1" || got.Remark != "客服已核" {
		t.Fatalf("单号/备注没有去空白: %+v", got)
	}
	if !got.FortuneCardUnusedConfirmed {
		t.Fatal("福卡确认没有传到仓储：这笔单会被当成「没确认」拒掉")
	}
	if got.Action != model.AfterSaleActionApprove || got.ReviewedBy != "admin-1" || got.TraceID != "trace-1" {
		t.Fatalf("入参被改动: %+v", got)
	}
}

// TestReviewAfterSaleStartsTheRefund 钉住「审核通过 = 同意退 + 当场去退」这条链的形状：
// 审核落了库只是第一步，紧接着必须拿着**这张售后单的**支付单号与金额去支付域建退款单，
// 建成了才算把售后推进到 refunding。
//
// 这三段的顺序本身就是不变量：少了第三段，售后单停在 approved 而钱已经在路上；少了第二段，
// 售后单到了 refunding 而支付侧什么都没有——那两种都只能靠人来发现。
func TestReviewAfterSaleStartsTheRefund(t *testing.T) {
	approved := refundableRow("REF1", model.AfterSaleStatusApproved)
	refunding := refundableRow("REF1", model.AfterSaleStatusRefunding)
	refunding.AfterSale.RefundNo = "RF1"
	repo := &fakeAfterSaleRepo{reviewRow: approved, startRow: refunding}
	creator := &stubPaymentCreator{
		refundOutcome: &client.RefundOutcome{RefundNo: "RF1", Status: "processing"},
	}

	row, err := newAfterSaleServiceWith(repo, creator).ReviewAfterSale(t.Context(), ReviewAfterSaleInput{
		AfterSaleNo: "REF1", Action: model.AfterSaleActionApprove, ReviewedBy: "admin-1", TraceID: "trace-1",
	})
	if err != nil {
		t.Fatalf("ReviewAfterSale() error = %v", err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusRefunding {
		t.Fatalf("返回的售后单状态 = %q, want refunding", row.AfterSale.Status)
	}
	// 幂等键必须是**售后单号本身**：支付侧那张表上 after_sale_no 整表唯一，重试才不会退两次。
	if creator.refundGot.AfterSaleNo != "REF1" {
		t.Fatalf("退款请求的幂等键 = %q, want REF1", creator.refundGot.AfterSaleNo)
	}
	// 支付单号与金额只能来自这张售后单，不能是别处算出来的数。
	if creator.refundGot.PaymentNo != approved.PaymentNo || creator.refundGot.Amount != approved.AfterSale.RefundAmount {
		t.Fatalf("退款请求 = %+v, want paymentNo=%s amount=%d",
			creator.refundGot, approved.PaymentNo, approved.AfterSale.RefundAmount)
	}
	if creator.refundGot.OrderLineID != *approved.AfterSale.OrderLineID {
		t.Fatalf("退款请求没带上被退的那一行: %+v", creator.refundGot)
	}
	if repo.startParams == nil || repo.startParams.RefundNo != "RF1" {
		t.Fatalf("没有拿支付侧给的退款单号去落 refunding: %+v", repo.startParams)
	}
	if repo.startParams.ActorID != "admin-1" {
		t.Fatalf("发起退款的操作人 = %q, want admin-1（重试出口要靠它记是谁推的）", repo.startParams.ActorID)
	}
}

// TestReviewAfterSaleKeepsApprovedWhenTheRefundCannotStart 是本轮**最重要的一条**：退款单
// 建不起来时，审核**已经生效**（那张单确实批了），但钱没上路。
//
// 所以回的是 ErrRefundNotStarted 而不是随便一个 503：后台要把「批了」与「退了」分开说，
// 否则审核人会再点一次「通过」，而第二次只会拿到「不在待审核状态」——那是个死胡同，
// 正确动作是去点「发起退款」。
func TestReviewAfterSaleKeepsApprovedWhenTheRefundCannotStart(t *testing.T) {
	approved := refundableRow("REF1", model.AfterSaleStatusApproved)
	repo := &fakeAfterSaleRepo{reviewRow: approved}
	creator := &stubPaymentCreator{refundErr: client.ErrRefundServiceUnavailable}

	_, err := newAfterSaleServiceWith(repo, creator).ReviewAfterSale(t.Context(), ReviewAfterSaleInput{
		AfterSaleNo: "REF1", Action: model.AfterSaleActionApprove, ReviewedBy: "admin-1", TraceID: "trace-1",
	})
	if !errors.Is(err, ErrRefundNotStarted) {
		t.Fatalf("err = %v, want ErrRefundNotStarted", err)
	}
	// 底下那个原因要还查得到：controller 需要它决定重试是不是有意义（拒绝 vs 抖动）。
	if !errors.Is(err, ErrRefundServiceUnavailable) {
		t.Fatalf("err = %v, 没有把支付侧的原因包在里面", err)
	}
	if repo.startParams != nil {
		t.Fatal("退款单都没建起来，却把售后推进了 refunding")
	}
}

// TestReviewAfterSaleRejectionDoesNotTouchThePayment 确认驳回那条路上**一个字节的网络调用
// 都没有**：钱不退，也就不该去问支付域。桩没配退款（默认会报错），走到那里会当场暴露。
func TestReviewAfterSaleRejectionDoesNotTouchThePayment(t *testing.T) {
	rejected := refundableRow("REF1", model.AfterSaleStatusRejected)
	repo := &fakeAfterSaleRepo{reviewRow: rejected}

	row, err := newAfterSaleServiceWith(repo, &stubPaymentCreator{}).ReviewAfterSale(
		t.Context(), ReviewAfterSaleInput{
			AfterSaleNo: "REF1", Action: model.AfterSaleActionReject, Remark: "不符合规则", ReviewedBy: "admin-1",
		})
	if err != nil {
		t.Fatalf("ReviewAfterSale() error = %v", err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusRejected {
		t.Fatalf("状态 = %q, want rejected", row.AfterSale.Status)
	}
	if repo.startParams != nil {
		t.Fatalf("驳回却动了退款: %+v", repo.startParams)
	}
}

// TestStartRefundRetriesAnApprovedSale 是重试出口：审核通过之后那次发起没成，单停在
// approved，后台点「发起退款」把它推下去。它要拿到与审核那条路**逐字相同**的请求。
func TestStartRefundRetriesAnApprovedSale(t *testing.T) {
	approved := refundableRow("REF1", model.AfterSaleStatusApproved)
	refunding := refundableRow("REF1", model.AfterSaleStatusRefunding)
	refunding.AfterSale.RefundNo = "RF1"
	repo := &fakeAfterSaleRepo{findRow: approved, startRow: refunding}
	creator := &stubPaymentCreator{
		refundOutcome: &client.RefundOutcome{RefundNo: "RF1", Status: "processing"},
	}

	row, err := newAfterSaleServiceWith(repo, creator).StartRefund(t.Context(), StartRefundInput{
		AfterSaleNo: " REF1 ", ActorID: "admin-2", TraceID: "trace-2",
	})
	if err != nil {
		t.Fatalf("StartRefund() error = %v", err)
	}
	if row.AfterSale.Status != model.AfterSaleStatusRefunding {
		t.Fatalf("状态 = %q, want refunding", row.AfterSale.Status)
	}
	if creator.refundCalls != 1 {
		t.Fatalf("调了支付域 %d 次, want 1", creator.refundCalls)
	}
	if repo.startParams.ActorID != "admin-2" {
		t.Fatalf("操作人 = %q, want admin-2", repo.startParams.ActorID)
	}
}

// TestStartRefundIsANoOpWhenItIsAlreadyRunning 是「列表刷新慢了一拍、用户又点了一次」的
// 现场：那张单已经在退了。此时**不该**再去建一次退款单——虽然幂等键挡得住，但白跑一次
// 渠道调用，而且它在界面上看起来就该是「已经退了」。
func TestStartRefundIsANoOpWhenItIsAlreadyRunning(t *testing.T) {
	for _, status := range []string{model.AfterSaleStatusRefunding, model.AfterSaleStatusRefunded} {
		t.Run(status, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{findRow: refundableRow("REF1", status)}
			creator := &stubPaymentCreator{}

			row, err := newAfterSaleServiceWith(repo, creator).StartRefund(t.Context(), StartRefundInput{
				AfterSaleNo: "REF1", ActorID: "admin-2",
			})
			if err != nil {
				t.Fatalf("StartRefund() error = %v, want nil（已经在退了不是错误）", err)
			}
			if row.AfterSale.Status != status {
				t.Fatalf("状态 = %q, want %q", row.AfterSale.Status, status)
			}
			if creator.refundCalls != 0 {
				t.Fatalf("已经在退了却又建了一次退款单（%d 次）", creator.refundCalls)
			}
		})
	}
}

// TestStartRefundRefusesAnythingNotApproved：重试出口只认 approved。还没批过的单退钱出去
// 就是绕过审核；已经驳回/撤销/失败的单再退一次是同一笔钱的第二张退款单。
func TestStartRefundRefusesAnythingNotApproved(t *testing.T) {
	for _, status := range []string{
		model.AfterSaleStatusPending, model.AfterSaleStatusRejected,
		model.AfterSaleStatusCancelled, model.AfterSaleStatusFailed,
	} {
		t.Run(status, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{findRow: refundableRow("REF1", status)}
			creator := &stubPaymentCreator{}

			_, err := newAfterSaleServiceWith(repo, creator).StartRefund(t.Context(), StartRefundInput{
				AfterSaleNo: "REF1", ActorID: "admin-2",
			})
			if !errors.Is(err, ErrAfterSaleNotApproved) {
				t.Fatalf("err = %v, want ErrAfterSaleNotApproved", err)
			}
			if creator.refundCalls != 0 {
				t.Fatal("没批过的单却去建了退款单")
			}
			if repo.startParams != nil {
				t.Fatal("没批过的单却被推进了 refunding")
			}
		})
	}
}

// TestStartRefundRefusesAnOrderWithoutPayment：设备单（没有 payments 行）在**动钱之前**
// 就被挡住。走到支付域去问只会得到一句「支付单不存在」，而真正的问题是这一单根本没有
// 支付单——那句实话只有订单库说得出来。
func TestStartRefundRefusesAnOrderWithoutPayment(t *testing.T) {
	row := refundableRow("REF1", model.AfterSaleStatusApproved)
	row.PaymentNo = ""
	repo := &fakeAfterSaleRepo{findRow: row}
	creator := &stubPaymentCreator{}

	_, err := newAfterSaleServiceWith(repo, creator).StartRefund(t.Context(), StartRefundInput{
		AfterSaleNo: "REF1", ActorID: "admin-2",
	})
	if !errors.Is(err, ErrOrderHasNoPayment) {
		t.Fatalf("err = %v, want ErrOrderHasNoPayment", err)
	}
	if creator.refundCalls != 0 {
		t.Fatal("没有支付单却去问了支付域")
	}
}

// TestHandleRefundEventSettlesTheSale 是事件那一侧的入口：两个号必须都取自事件体，
// 成功那条要带上渠道给的时刻（补投旧事件时不能用 NOW()，否则「上周退的款」会记成「刚才」）。
func TestHandleRefundEventSettlesTheSale(t *testing.T) {
	settled := refundableRow("REF1", model.AfterSaleStatusRefunded)
	repo := &fakeAfterSaleRepo{advanceRow: settled}
	body := []byte(`{"afterSaleNo":"REF1","refundNo":"RF1","orderNo":"ORD1","paymentNo":"PAY1",
		"amount":1800,"fundings":[],"succeededAtUnix":1758000000,"failureCode":"","failureMessage":""}`)

	err := newAfterSaleService(repo).HandleRefundEvent(t.Context(), messaging.Envelope{
		EventType: dto.EventPaymentRefundSucceeded, Payload: body, EventID: "evt-1", TraceID: "trace-1",
	})
	if err != nil {
		t.Fatalf("HandleRefundEvent() error = %v", err)
	}
	got := repo.advanceParams
	if got == nil {
		t.Fatal("事件没有落到仓储")
	}
	if got.AfterSaleNo != "REF1" || got.RefundNo != "RF1" || !got.Succeeded {
		t.Fatalf("入参 = %+v", got)
	}
	if got.RefundedAt.Unix() != 1758000000 {
		t.Fatalf("refundedAt = %v, 没有用事件里的时刻", got.RefundedAt)
	}
	if got.RequestID != "evt-1" {
		t.Fatalf("requestID = %q, want evt-1（状态流水的关联键）", got.RequestID)
	}
	// trace 也要跟着走到仓储：退款结果那两条事件是在仓储里写 outbox 的，没有 trace_id
	// 那一行在链路上就断了线——账号域那边只能看到一条来路不明的消息。
	if got.TraceID != "trace-1" {
		t.Fatalf("traceID = %q, want trace-1", got.TraceID)
	}
}

// TestHandleRefundEventFailureCarriesTheReason 盯的是失败那条：渠道为什么拒必须落到
// failure_code 上，后台与客服就是靠它解释「这笔钱为什么没退成」的。
func TestHandleRefundEventFailureCarriesTheReason(t *testing.T) {
	repo := &fakeAfterSaleRepo{advanceRow: refundableRow("REF1", model.AfterSaleStatusFailed)}
	body := []byte(`{"afterSaleNo":"REF1","refundNo":"RF1","orderNo":"ORD1","paymentNo":"PAY1",
		"amount":1800,"fundings":[],"succeededAtUnix":0,"failureCode":"ACQ.TRADE_NOT_EXIST",
		"failureMessage":"原交易不存在"}`)

	err := newAfterSaleService(repo).HandleRefundEvent(t.Context(), messaging.Envelope{
		EventType: dto.EventPaymentRefundFailed, Payload: body, EventID: "evt-2",
	})
	if err != nil {
		t.Fatalf("HandleRefundEvent() error = %v", err)
	}
	got := repo.advanceParams
	if got.Succeeded {
		t.Fatal("失败事件被当成了成功")
	}
	if got.FailureCode != "ACQ.TRADE_NOT_EXIST" || got.FailureMessage != "原交易不存在" {
		t.Fatalf("失败原因没有传下去: %+v", got)
	}
	if !got.RefundedAt.IsZero() {
		t.Fatalf("refundedAt = %v, 失败的事件不该有退款时刻", got.RefundedAt)
	}
}

// TestHandleRefundEventRejectsMalformedPayloads：两个号缺一不可，多一个字段也不行。
//
// 它们全都回错误（= 不 ack）：退款这条路上「假装处理成功」的代价是钱的状态永远对不上，
// 而那比一条进死信队列的消息严重得多。
func TestHandleRefundEventRejectsMalformedPayloads(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"没有售后单号", `{"refundNo":"RF1"}`},
		{"没有退款单号", `{"afterSaleNo":"REF1"}`},
		{"多了不认识的字段", `{"afterSaleNo":"REF1","refundNo":"RF1","extra":1}`},
		{"根本不是 JSON", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeAfterSaleRepo{}
			err := newAfterSaleService(repo).HandleRefundEvent(t.Context(), messaging.Envelope{
				EventType: dto.EventPaymentRefundSucceeded, Payload: []byte(tc.body),
			})
			if !errors.Is(err, ErrInvalidPaymentEvent) {
				t.Fatalf("err = %v, want ErrInvalidPaymentEvent", err)
			}
			if repo.advanceParams != nil {
				t.Fatal("坏消息却落到了仓储")
			}
		})
	}
}

// TestHandlePaymentEventRoutesRefundEvents 确认两条退款事件真的会被分到退款那条路上。
//
// 分错了的表现是**静默的**：HandlePaymentEvent 遇到不认识的事件类型返回 nil（ack），
// 于是退款结果被丢掉，售后单永远停在 refunding，而日志里一个字都没有。
func TestHandlePaymentEventRoutesRefundEvents(t *testing.T) {
	repo := &fakeAfterSaleRepo{advanceRow: refundableRow("REF1", model.AfterSaleStatusRefunded)}
	body := []byte(`{"afterSaleNo":"REF1","refundNo":"RF1","orderNo":"ORD1","paymentNo":"PAY1",
		"amount":1800,"fundings":[],"succeededAtUnix":0,"failureCode":"","failureMessage":""}`)

	err := newAfterSaleService(repo).HandlePaymentEvent(t.Context(), messaging.Envelope{
		EventType: dto.EventPaymentRefundSucceeded, Payload: body,
	})
	if err != nil {
		t.Fatalf("HandlePaymentEvent() error = %v", err)
	}
	if repo.advanceParams == nil {
		t.Fatal("退款事件被 ack 掉了，没有推进售后单")
	}
}

// TestReviewAfterSaleForwardsNotPending 是并发审核那一步的现场：后到的那次看到的是
// approved，仓储回 ErrAfterSaleNotPending，服务层原样传出去（controller 回 409）。
func TestReviewAfterSaleForwardsNotPending(t *testing.T) {
	repo := &fakeAfterSaleRepo{reviewErr: repository.ErrAfterSaleNotPending}
	_, err := newAfterSaleService(repo).ReviewAfterSale(t.Context(), ReviewAfterSaleInput{
		AfterSaleNo: "REF1", Action: model.AfterSaleActionApprove, ReviewedBy: "u1",
	})
	if !errors.Is(err, ErrAfterSaleNotPending) {
		t.Fatalf("err = %v, want ErrAfterSaleNotPending", err)
	}
}

func TestCancelAfterSaleShape(t *testing.T) {
	t.Run("没有用户", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{}
		_, err := newAfterSaleService(repo).CancelAfterSale(t.Context(), CancelAfterSaleInput{AfterSaleNo: "REF1"})
		if !errors.Is(err, ErrUserRequired) {
			t.Fatalf("err = %v, want ErrUserRequired", err)
		}
		if repo.cancelParams != nil {
			t.Fatal("校验失败却调用了仓储")
		}
	})

	t.Run("没有理由时补一句默认的", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{cancelRow: &repository.AfterSaleRow{}}
		if _, err := newAfterSaleService(repo).CancelAfterSale(t.Context(), CancelAfterSaleInput{
			AfterSaleNo: "REF1", UserID: "u1",
		}); err != nil {
			t.Fatalf("CancelAfterSale() error = %v", err)
		}
		// 状态流水的 reason 空着就查不出「这张单为什么走到 cancelled」，所以补一句。
		if repo.cancelParams.Reason == "" {
			t.Fatal("撤销没有补默认理由：状态流水里会留下一条空 reason 的记录")
		}
	})

	t.Run("归属由仓储判，服务层原样传 userID", func(t *testing.T) {
		repo := &fakeAfterSaleRepo{cancelErr: repository.ErrAfterSaleNotFound}
		_, err := newAfterSaleService(repo).CancelAfterSale(t.Context(), CancelAfterSaleInput{
			AfterSaleNo: "REF1", UserID: "someone-else",
		})
		if !errors.Is(err, ErrAfterSaleNotFound) {
			t.Fatalf("err = %v, want ErrAfterSaleNotFound", err)
		}
		if repo.cancelParams.UserID != "someone-else" {
			t.Fatalf("userID = %q, 没有传下去", repo.cancelParams.UserID)
		}
	})
}

// TestAfterSaleValidationSentinelRegistered 是「新增一条校验就要在 controller 里同步加一个
// case」这句话的自动版：writeOrderError 的兜底是 500，忘了登记的校验错误会以 500 的形式
// 回给前端，前端拿 500 提示不出「你这个字段填错了」。
func TestAfterSaleValidationSentinelRegistered(t *testing.T) {
	sentinels := []error{
		ErrAfterSaleScopeInvalid, ErrAfterSaleMembershipUnsupported, ErrAfterSaleLineRequired,
		ErrAfterSaleLineNotAllowed, ErrAfterSaleReasonRequired, ErrAfterSaleImagesInvalid,
		ErrAfterSaleRemarkRequired, ErrAfterSaleActionInvalid,
	}
	for _, sentinel := range sentinels {
		if !IsValidationError(sentinel) {
			t.Errorf("%v 不在 ValidationErrors 里，controller 会回 500", sentinel)
		}
	}
}

// TestGenerateAfterSaleNo 只盯格式：前缀与长度是客服肉眼分单的依据（REF 开头一眼看得出
// 不是订单号）。唯一性靠唯一索引兜，不在这里赌随机数。
func TestGenerateAfterSaleNo(t *testing.T) {
	pattern := regexp.MustCompile(`^REF\d{14}\d{3}\d{6}$`)
	for i := 0; i < 5; i++ {
		if no := generateAfterSaleNo(time.Now()); !pattern.MatchString(no) {
			t.Fatalf("售后单号 %q 不符合 REF+YmdHis+9 位数字 的格式", no)
		}
	}
}

// TestAfterSaleStateMachine 把这张表的两条不变量钉住：这一版只能从 pending 出发，而
// 终态没有出边。表被改坏（比如给 rejected 加了出边）时，这里会先响。
func TestAfterSaleStateMachine(t *testing.T) {
	reachable := []struct{ from, to string }{
		{model.AfterSaleStatusPending, model.AfterSaleStatusApproved},
		{model.AfterSaleStatusPending, model.AfterSaleStatusRejected},
		{model.AfterSaleStatusPending, model.AfterSaleStatusCancelled},
	}
	for _, tc := range reachable {
		if !CanTransitionAfterSale(tc.from, tc.to) {
			t.Errorf("CanTransitionAfterSale(%s, %s) = false, want true", tc.from, tc.to)
		}
	}

	blocked := []struct{ from, to string }{
		// 终态没有出边：被驳回、已撤销、退款失败都意味着钱没出去，用户要退就重新申请一张。
		{model.AfterSaleStatusRejected, model.AfterSaleStatusPending},
		{model.AfterSaleStatusCancelled, model.AfterSaleStatusPending},
		{model.AfterSaleStatusRefunded, model.AfterSaleStatusApproved},
		{model.AfterSaleStatusFailed, model.AfterSaleStatusApproved},
		// 审核通过只能往下走去建退款单，不能回到 pending，也不能再驳回一次。
		{model.AfterSaleStatusApproved, model.AfterSaleStatusRejected},
		{model.AfterSaleStatusApproved, model.AfterSaleStatusPending},
		// 没听说过的状态一律拒绝：宁可挡住一次操作让人来查。
		{"whatever", model.AfterSaleStatusPending},
		{model.AfterSaleStatusPending, "whatever"},
	}
	for _, tc := range blocked {
		if CanTransitionAfterSale(tc.from, tc.to) {
			t.Errorf("CanTransitionAfterSale(%s, %s) = true, want false", tc.from, tc.to)
		}
	}
}
