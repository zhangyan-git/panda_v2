package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/catalog"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 这一组测试覆盖退款那两条路里**不碰渠道网络**的部分：可退余额与金额形状、出资分摊、
// 三种渠道结论（成功 / 拒绝 / 不确定）各自对退款单做什么、以及退款查询任务怎么推那一批
// 停在 processing 的单。
//
// 与 create_test.go 同一个取舍：渠道网络用假适配器顶替，真仓储换成一个记账用的假的。
// 这一层最容易写错的是**判断**——哪几行不该发去渠道、结果不明时该不该落结论、一笔问不出
// 结果的退款要不要推到队尾——而不是 SQL 本身。
//
// 渠道协议（报文怎么签、SUCCESS/FAIL 怎么读出来）由 provider/ums 的测试守，这里用的是已经
// 翻好的 provider.OperationResult。

// refundablePayment 是一张**可退**的支付单：已收妥、走了带渠道的那条支付方式。
//
// 每个用例都要一张，而其中任何一个字段错了都会让用例在离本意很远的地方失败：状态不是
// succeeded 会撞 ErrPaymentNotRefundable，支付方式不在目录里会撞 ErrPaymentMethodNotFound。
func refundablePayment() *model.Payment {
	return &model.Payment{
		ID: "payment-1", PaymentNo: "PAY20260914120000000001", OrderNo: testOrderNo,
		UserID: testUserID, Amount: 10000, PaymentMethod: testMethodCode,
		Status: model.PaymentSucceeded, Provider: stubProviderName, RequestID: testRequest,
	}
}

// refundFundingsOf 造一组退款出资行。line_no 按入参顺序给，因为「第几行」是它唯一的身份。
func refundFundingsOf(lines ...model.RefundFunding) []*model.RefundFunding {
	fundings := make([]*model.RefundFunding, 0, len(lines))
	for i := range lines {
		line := lines[i]
		line.LineNo = i + 1
		line.RefundID = "refund-1"
		fundings = append(fundings, &line)
	}
	return fundings
}

func validRefundRequest() RefundRequest {
	return RefundRequest{
		AfterSaleNo: "AS20260914000001", PaymentNo: refundablePayment().PaymentNo,
		Amount: 10000, Reason: "用户申请", RequestID: testRequest, TraceID: "trace-1",
	}
}

// stubOperator 是一个可编排的渠道操作适配器：Execute 原样返回预设结果，并记下请求。
//
// 它**嵌着 stubProvider**，因为装配处是按 provider.Provider 注册适配器的，而退款这条路
// 要的是同一个适配器再实现 Operator。用一个类型同时顶这两件事，就不会出现「注册进去的
// 是一个、被断言成 Operator 的是另一个」那种只有测试里才存在的错配。
//
// 结果按调用次序取：第 n 次调用拿 results[n]。给一个长度的切片就能编排「第一次不确定、
// 第二次问出成功」——退款查询那条路正是这个形状。
type stubOperator struct {
	stubProvider
	results []provider.OperationResult
	errs    []error
	calls   []provider.OperationRequest
}

func (s *stubOperator) Execute(_ context.Context, req provider.OperationRequest) (provider.OperationResult, error) {
	i := len(s.calls)
	s.calls = append(s.calls, req)
	var result provider.OperationResult
	if i < len(s.results) {
		result = s.results[i]
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return result, err
}

// —— 假仓储里的退款那一段（见 repository/refund.go）——
//
// 三个 Mark* 就地改那一张退款单，FindRefundByNo 读它。用「一张会变状态的对象」而不是
// 「每次调用记一笔」，是因为退款那条链的形状就是「同一张单被推着走」。

func (f *fakeRepository) BeginRefund(_ context.Context, p repository.BeginRefundParams) (*model.Refund, []*model.RefundFunding, bool, error) {
	f.beginRefundCalls = append(f.beginRefundCalls, p)
	if f.beginRefundErr != nil {
		return nil, nil, false, f.beginRefundErr
	}
	if f.beginRefundHit {
		// 命中幂等：拿回的是上次那张单，连同它当时算好的那几行。**不重新算可退余额**——
		// 上一次退款已经把它占掉了（见 BeginRefund 的注释）。
		return f.refund, f.refundFundings, true, nil
	}
	f.refund = &model.Refund{
		ID: "refund-1", RefundNo: p.RefundNo, PaymentNo: p.PaymentNo,
		OrderNo: testOrderNo, UserID: testUserID, AfterSaleNo: p.AfterSaleNo,
		Amount: p.Amount, Reason: p.Reason, Status: model.RefundPending,
		RequestID: p.RequestID,
	}
	return f.refund, f.refundFundings, false, nil
}

func (f *fakeRepository) MarkRefundSucceeded(_ context.Context, p repository.MarkRefundSucceededParams) (*model.Refund, error) {
	f.refundSettles = append(f.refundSettles, p)
	if f.refund == nil {
		return nil, repository.ErrRefundNotFound
	}
	f.refund.Status = model.RefundSucceeded
	f.refund.ProviderRefundID = p.ProviderRefundID
	return f.refund, nil
}

func (f *fakeRepository) MarkRefundFailed(_ context.Context, p repository.MarkRefundFailedParams) (*model.Refund, error) {
	f.refundFailures = append(f.refundFailures, p)
	if f.refund == nil {
		return nil, repository.ErrRefundNotFound
	}
	f.refund.Status = model.RefundFailed
	f.refund.FailureCode = p.FailureCode
	f.refund.FailureMessage = p.FailureMessage
	return f.refund, nil
}

func (f *fakeRepository) MarkRefundProcessing(_ context.Context, p repository.MarkRefundProcessingParams) (*model.Refund, error) {
	f.refundProcessing = append(f.refundProcessing, p)
	if f.refund == nil {
		return nil, repository.ErrRefundNotFound
	}
	f.refund.Status = model.RefundProcessing
	f.refund.ProviderRefundID = p.ProviderRefundID
	return f.refund, nil
}

func (f *fakeRepository) FindRefundByNo(_ context.Context, refundNo string) (*model.Refund, []*model.RefundFunding, error) {
	if f.refundByNoErr != nil {
		return nil, nil, f.refundByNoErr
	}
	if f.refund == nil || f.refund.RefundNo != refundNo {
		return nil, nil, repository.ErrRefundNotFound
	}
	return f.refund, f.refundFundings, nil
}

func (f *fakeRepository) ListStaleProcessingRefunds(_ context.Context, staleBefore time.Time, limit int) ([]model.Refund, error) {
	f.refundQueries = append(f.refundQueries, staleQuery{staleBefore: staleBefore, limit: limit})
	if f.staleRefundErr != nil {
		return nil, f.staleRefundErr
	}
	return f.processingRefunds, nil
}

func (f *fakeRepository) TouchRefund(_ context.Context, refundID string) error {
	if f.touchErr != nil {
		return f.touchErr
	}
	f.touchedRefunds = append(f.touchedRefunds, refundID)
	return nil
}

// newRefundService 组装一条带渠道操作的退款业务层，并交出那个假适配器。
//
// 单独一个辅助函数是因为退款那几条用例都要「拿到 stub 看它收到什么」——把它们都写成
// newServiceWith(t, repo, stub, ...) 再自己留一份 stub 的引用，就是每条用例重复一遍同一件事。
func newRefundService(t *testing.T, repo *fakeRepository, operator *stubOperator) *PaymentService {
	t.Helper()
	return newServiceWith(t, repo, operator, testMethodCode, nil,
		func(*catalog.Channel, string) string { return "secret" })
}

// TestRefundNoFitsTheChannelLengthBudget 钉住退款单号的长度。
//
// 这不是风格问题，是**渠道的硬约束**：这个号会被编成 refundOrderId 发出去，规范给的上限是
// 「总长度大于 6 位、小于 28 位」，含渠道分配的 4 位来源编号（见 provider/ums/orderid.go）。
// 4 + 23 = 27 卡在上限之内；多加一位（比如照抄支付单号那三个毫秒位）就会被渠道挡在门外，
// 而那个错误在本地跑多少遍都不会出现——只有真发到渠道才会。
func TestRefundNoFitsTheChannelLengthBudget(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	no := refundNo(now)

	if len(no) != 23 {
		t.Fatalf("退款单号 %q 长度 = %d，期望 23（4 位来源编号加进来要小于 28）", no, len(no))
	}
	if !strings.HasPrefix(no, "REF20260914120000") {
		t.Errorf("退款单号 %q 的前缀不是 REF + 时间戳", no)
	}
	// 同一秒内两笔退款必须拿到不同的号（撞上是概率问题，但**必须**是概率问题而不是必然：
	// 后六位要是写死成 0，所有同秒退款都会撞在 payment_refunds_refund_no_key 上）。
	if again := refundNo(now); again == no {
		t.Errorf("同一秒内的两次生成拿到了同一个号 %q", no)
	}
}

// TestCreateRefundValidation 校验是形状层面的：一条都不能走到仓储。
//
// 「没走到仓储」是断言的一部分：一条建不了单的请求在库上留下任何痕迹（哪怕只是一次
// SELECT）都是错的，而 SELECT 恰恰不会留下痕迹——所以这里看的是 beginRefundCalls。
func TestCreateRefundValidation(t *testing.T) {
	valid := validRefundRequest()
	cases := []struct {
		name   string
		mutate func(*RefundRequest)
		want   error
	}{
		{"没有售后单号", func(r *RefundRequest) { r.AfterSaleNo = "  " }, ErrAfterSaleNoRequired},
		{"没有支付单号", func(r *RefundRequest) { r.PaymentNo = "" }, ErrPaymentNoRequired},
		{"金额为零", func(r *RefundRequest) { r.Amount = 0 }, ErrRefundAmountNotPositive},
		{"金额为负", func(r *RefundRequest) { r.Amount = -1 }, ErrRefundAmountNotPositive},
		{"行号不是 uuid", func(r *RefundRequest) { r.OrderLineID = "line-1" }, ErrOrderLineIDInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)
			repo := &fakeRepository{}
			// 支付单能给出来：这样「拒了」只可能是形状校验拒的，不会是「查不到这张支付单」。
			repo.paymentByNo = refundablePayment()

			_, err := newRefundService(t, repo, &stubOperator{}).CreateRefund(context.Background(), in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateRefund = %v，期望 %v", err, tc.want)
			}
			if len(repo.beginRefundCalls) != 0 {
				t.Fatalf("形状就不对的请求走到了仓储：%+v", repo.beginRefundCalls)
			}
		})
	}

	t.Run("整单退（空行号）放行", func(t *testing.T) {
		repo := &fakeRepository{}
		repo.paymentByNo = refundablePayment()
		repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
		operator := &stubOperator{results: []provider.OperationResult{{Result: provider.ResultSuccess}}}

		in := valid
		in.OrderLineID = ""
		if _, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), in); err != nil {
			t.Fatalf("CreateRefund: %v", err)
		}
		if len(operator.calls) != 1 {
			t.Fatalf("渠道调用 = %d 次，期望 1", len(operator.calls))
		}
	})
}

// TestCreateRefundOnlyRefundsSettledPayments 钉住「只有收妥的钱能退」。
//
// 这条挡在渠道之前是有理由的：对一张还没收妥的单发起退款，渠道的回答会是一句与本意无关的
// 「查无此单」，而那句话会盖住真正的原因。
func TestCreateRefundOnlyRefundsSettledPayments(t *testing.T) {
	for _, status := range []string{model.PaymentCreated, model.PaymentPending, model.PaymentFailed, model.PaymentClosed, model.PaymentExpired} {
		t.Run(status, func(t *testing.T) {
			payment := refundablePayment()
			payment.Status = status
			repo := &fakeRepository{paymentByNo: payment}
			operator := &stubOperator{}

			_, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
			if !errors.Is(err, repository.ErrPaymentNotRefundable) {
				t.Fatalf("CreateRefund = %v，期望 ErrPaymentNotRefundable", err)
			}
			if len(repo.beginRefundCalls) != 0 || len(operator.calls) != 0 {
				t.Fatalf("不可退的支付单走到了建单或渠道：begin=%d calls=%d",
					len(repo.beginRefundCalls), len(operator.calls))
			}
		})
	}
}

// TestCreateRefundSendsTheChannelOnlyItsShare 是这一组里最重要的一条：**退给渠道的金额是
// 走渠道那一部分，不是退款单的总额**。
//
// 混合出资（一条渠道 + 一条咖啡豆）今天还落不了地，但退款这条链从上到下都是按多行写的，
// 而这个值只在 dispatchRefund 里算一次。真到那一天，这里要是发的是总额，表现是**多退钱**
// ——渠道那边会照着总额退，而豆那部分已经在账户域退过一次了。
func TestCreateRefundSendsTheChannelOnlyItsShare(t *testing.T) {
	repo := &fakeRepository{}
	repo.paymentByNo = refundablePayment()
	repo.refundFundings = refundFundingsOf(
		model.RefundFunding{LineType: "ums_h5_wechat", Amount: 8000},
		model.RefundFunding{LineType: fundingLineTypeCoffeeBean, Amount: 2000},
	)
	operator := &stubOperator{results: []provider.OperationResult{
		{Result: provider.ResultSuccess, ProviderRefundID: "UMS-REFUND-1"},
	}}

	in := validRefundRequest()
	in.Amount = 10000
	result, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}

	if len(operator.calls) != 1 {
		t.Fatalf("渠道调用 = %d 次，期望 1", len(operator.calls))
	}
	sent := operator.calls[0]
	if sent.Amount != 8000 {
		t.Errorf("发给渠道的金额 = %d，期望 8000（豆那 2000 不该发去渠道）", sent.Amount)
	}
	if sent.Operation != provider.OperationRefund {
		t.Errorf("发出去的操作 = %q，期望 %q", sent.Operation, provider.OperationRefund)
	}
	if sent.RefundNo != result.RefundNo {
		t.Errorf("发给渠道的退款单号 = %q，与对外那个 %q 不一致", sent.RefundNo, result.RefundNo)
	}
	if sent.RequestID != testRequest {
		t.Errorf("发给渠道的请求号 = %q，期望 %q", sent.RequestID, testRequest)
	}

	if len(repo.refundSettles) != 1 {
		t.Fatalf("结算调用 = %d 次，期望 1", len(repo.refundSettles))
	}
	settled := repo.refundSettles[0]
	if settled.ProviderRefundID != "UMS-REFUND-1" {
		t.Errorf("落库的渠道退款单号 = %q，期望 UMS-REFUND-1", settled.ProviderRefundID)
	}
	// 豆那一行**不走渠道**，所以它必须以 NoOpLineTypes 传下去——漏了它的表现是那一行留在
	// pending，而整张退款单已经是 succeeded 了。
	if len(settled.NoOpLineTypes) != 1 || settled.NoOpLineTypes[0] != fundingLineTypeCoffeeBean {
		t.Errorf("不走渠道的出资行 = %v，期望只有 %s", settled.NoOpLineTypes, fundingLineTypeCoffeeBean)
	}
	if result.Status != model.RefundSucceeded {
		t.Errorf("对外状态 = %q，期望 %q", result.Status, model.RefundSucceeded)
	}
	if len(repo.refundFailures)+len(repo.refundProcessing) != 0 {
		t.Errorf("一次成功的退款落下了别的结论：failed=%d processing=%d",
			len(repo.refundFailures), len(repo.refundProcessing))
	}
}

// TestCreateRefundChannelOnlyPassesAnEmptyExcludeList 钉住「整张退款单都走渠道」时传给结算的
// 排除表是**空切片而不是 nil**。
//
// 两者在 Go 里看着一样（都是「一个名字都没有」），到了 SQL 里不是：pgx 把 nil 切片编成 NULL，
// 而 `NOT (line_type = ANY(NULL))` 是 NULL——走渠道的那几行一行都匹配不上，退款单成了
// succeeded、它的出资行却永远停在 pending。这条链靠 Go 侧的测试看不出来（假仓储不认 SQL），
// 所以这里断言的是那个**参数**本身：只要它是 nil，真仓储上就一定不生效。
func TestCreateRefundChannelOnlyPassesAnEmptyExcludeList(t *testing.T) {
	repo := &fakeRepository{}
	repo.paymentByNo = refundablePayment()
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
	operator := &stubOperator{results: []provider.OperationResult{
		{Result: provider.ResultSuccess, ProviderRefundID: "UMS-REFUND-1"},
	}}

	if _, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest()); err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(repo.refundSettles) != 1 {
		t.Fatalf("结算调用 = %d 次，期望 1", len(repo.refundSettles))
	}
	settled := repo.refundSettles[0]
	if settled.NoOpLineTypes == nil {
		t.Fatal("不走渠道的出资行是 nil（SQL 里会变成 NULL，走渠道的行一行都不会被标成功）")
	}
	if len(settled.NoOpLineTypes) != 0 {
		t.Errorf("不走渠道的出资行 = %v，期望为空", settled.NoOpLineTypes)
	}
}

// TestCreateRefundSettlesAccountFundedRefundsLocally 钉住账户出资那条路**整条都不碰渠道**。
//
// 这不是「渠道不可用时的兜底」，是那条路**正常**的形状：豆在售后审核通过那一刻已经被
// account-service 冲正了（幂等键 after_sale:{afterSaleNo}），这里没有任何第三方可问。
// 真去发一次渠道请求的话，会拿着一个没有渠道的支付方式撞在 resolveMethod 上。
func TestCreateRefundSettlesAccountFundedRefundsLocally(t *testing.T) {
	repo := &fakeRepository{}
	payment := refundablePayment()
	payment.PaymentMethod = catalog.CodeCoffeeBean
	payment.Amount = 500
	repo.paymentByNo = payment
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: fundingLineTypeCoffeeBean, Amount: 500})
	// 这个适配器**根本不实现 Operator**：那一行要是真发了渠道请求，撞上的是
	// ErrProviderOperationUnsupported，而不是一个安静的通过。
	operator := &stubOperator{}

	in := validRefundRequest()
	in.Amount = 500
	result, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(operator.calls) != 0 {
		t.Fatalf("账户出资的退款发了渠道请求：%+v", operator.calls)
	}

	if len(repo.refundSettles) != 1 {
		t.Fatalf("结算调用 = %d 次，期望 1", len(repo.refundSettles))
	}
	settled := repo.refundSettles[0]
	if settled.ProviderRefundID != "" {
		t.Errorf("账户出资那一路不该有渠道退款单号，拿到 %q", settled.ProviderRefundID)
	}
	if len(settled.NoOpLineTypes) != 1 || settled.NoOpLineTypes[0] != fundingLineTypeCoffeeBean {
		t.Errorf("不走渠道的出资行 = %v，期望只有 %s", settled.NoOpLineTypes, fundingLineTypeCoffeeBean)
	}
	if result.Status != model.RefundSucceeded {
		t.Errorf("对外状态 = %q，期望 %q", result.Status, model.RefundSucceeded)
	}
}

// TestCreateRefundDeclineIsAResultNotAnError 钉住「渠道明确拒绝是一个结论，不是服务端故障」。
//
// 返回 error 会让调用方（order-service 的审核流程）把一次正常的业务拒绝当成故障去重试，
// 而重试只会拿到同一个拒绝。
func TestCreateRefundDeclineIsAResultNotAnError(t *testing.T) {
	repo := &fakeRepository{}
	repo.paymentByNo = refundablePayment()
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
	operator := &stubOperator{results: []provider.OperationResult{{
		Result: provider.ResultFailed, FailureCode: "REFUND_NOT_ALLOWED", FailureMessage: "该笔交易不支持退款",
	}}}

	result, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
	if err != nil {
		t.Fatalf("渠道拒绝不该是一个 error，拿到 %v", err)
	}
	if result.Status != model.RefundFailed {
		t.Errorf("对外状态 = %q，期望 %q", result.Status, model.RefundFailed)
	}
	if result.FailureCode != "REFUND_NOT_ALLOWED" {
		t.Errorf("失败码 = %q，期望 REFUND_NOT_ALLOWED", result.FailureCode)
	}
	if len(repo.refundFailures) != 1 {
		t.Fatalf("失败落库 = %d 次，期望 1", len(repo.refundFailures))
	}
	if repo.refundFailures[0].From != model.RefundPending {
		t.Errorf("失败是从 %q 出发的，期望 %q（发起那条路）", repo.refundFailures[0].From, model.RefundPending)
	}
	if len(repo.refundSettles) != 0 {
		t.Errorf("失败的那笔还落了成功：%+v", repo.refundSettles)
	}
}

// TestCreateRefundDeclineWithoutCodeGetsOne 钉住「渠道没说为什么也要给一个非空的码」。
//
// 空的 failure_code 在后台列表里看起来像「没失败」，而这一笔确实失败了——售后单会落 failed
// 却什么都没显示，客服只能猜。
func TestCreateRefundDeclineWithoutCodeGetsOne(t *testing.T) {
	repo := &fakeRepository{}
	repo.paymentByNo = refundablePayment()
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
	operator := &stubOperator{results: []provider.OperationResult{{Result: provider.ResultFailed}}}

	result, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if result.FailureCode == "" {
		t.Fatal("渠道没给失败码，落库的也是空的")
	}
	if result.Status != model.RefundFailed {
		t.Errorf("对外状态 = %q，期望 %q", result.Status, model.RefundFailed)
	}
}

// TestCreateRefundUncertainResultLeavesItProcessing 钉住「没有结论就停在 processing，
// 而且不发任何事件」。
//
// processing 不是终态，它是交给退款查询 worker 的**交接点**：规范里没有退款回调，渠道不会
// 主动来告诉我们结果，所以这一笔必须留下一个能被扫到的状态。落 succeeded 是这条路上唯一
// 不能犯的错——那会告诉订单侧「钱退回去了」，而其实什么都没确认。
func TestCreateRefundUncertainResultLeavesItProcessing(t *testing.T) {
	cases := []struct {
		name   string
		result provider.OperationResult
	}{
		{"应答是 PROCESSING", provider.OperationResult{Result: provider.ResultUnknown, ProviderRefundID: "UMS-REFUND-9"}},
		{"调用超时", provider.OperationResult{Result: provider.ResultTimeout}},
		{"适配器漏填了结果", provider.OperationResult{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{}
			repo.paymentByNo = refundablePayment()
			repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
			operator := &stubOperator{results: []provider.OperationResult{tc.result}}

			out, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
			if err != nil {
				t.Fatalf("没有结论不是 error，拿到 %v", err)
			}
			if out.Status != model.RefundProcessing {
				t.Errorf("对外状态 = %q，期望 %q", out.Status, model.RefundProcessing)
			}
			if len(repo.refundProcessing) != 1 {
				t.Fatalf("processing 落库 = %d 次，期望 1", len(repo.refundProcessing))
			}
			if repo.refundProcessing[0].ProviderRefundID != tc.result.ProviderRefundID {
				t.Errorf("落下的渠道退款单号 = %q，期望 %q",
					repo.refundProcessing[0].ProviderRefundID, tc.result.ProviderRefundID)
			}
			if len(repo.refundSettles)+len(repo.refundFailures) != 0 {
				t.Errorf("没有结论却落了别的结论：succeeded=%d failed=%d",
					len(repo.refundSettles), len(repo.refundFailures))
			}
		})
	}
}

// TestCreateRefundTransportFailureStaysPending 钉住「请求根本没发出去时一个结论都不落」。
//
// err 非 nil 的契约是「这次调用没有离开本机」（见 provider.Operator）。落 processing 会让
// 退款查询 worker 去问一笔渠道**根本没收到**的退款——那边答「查无此单」，而那会被读成
// 「这笔退款失败了」，把一次网络抖动变成一次终态判断。停在 pending 才是对的：重发同一个
// 售后单号会接着走。
func TestCreateRefundTransportFailureStaysPending(t *testing.T) {
	repo := &fakeRepository{}
	repo.paymentByNo = refundablePayment()
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
	operator := &stubOperator{errs: []error{errors.New("dial tcp: connection refused")}}

	_, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
	if err == nil {
		t.Fatal("请求没发出去，调用方却拿到了成功")
	}
	if len(repo.refundSettles)+len(repo.refundFailures)+len(repo.refundProcessing) != 0 {
		t.Errorf("没发出去的那次调用落了结论：succeeded=%d failed=%d processing=%d",
			len(repo.refundSettles), len(repo.refundFailures), len(repo.refundProcessing))
	}
	// 渠道调用流水照样要记：一次没发出去的回调如果不留痕，运维那边看到的是「这笔退款从来
	// 没被处理过」。
	if len(repo.providerCall) != 1 {
		t.Errorf("渠道调用流水 = %d 条，期望 1", len(repo.providerCall))
	}
}

// TestCreateRefundReplaysAFinishedRefund 钉住幂等的形状：同一个售后单号重发，拿回上次那张单，
// **不重发渠道请求**。
//
// 不重发的理由是硬的：processing 的那一笔正由退款查询 worker 跟着，再发一次是在两张单上
// 问同一件事；而 succeeded/failed 的更是已经从终态出发，没有任何可推进的了。
func TestCreateRefundReplaysAFinishedRefund(t *testing.T) {
	for _, status := range []string{model.RefundSucceeded, model.RefundProcessing, model.RefundFailed} {
		t.Run(status, func(t *testing.T) {
			repo := &fakeRepository{beginRefundHit: true}
			repo.paymentByNo = refundablePayment()
			repo.refund = &model.Refund{
				ID: "refund-1", RefundNo: "REF20260914120000000001", PaymentNo: refundablePayment().PaymentNo,
				AfterSaleNo: validRefundRequest().AfterSaleNo, Amount: 10000, Status: status,
				ProviderRefundID: "UMS-REFUND-1", FailureCode: "SOMETHING",
			}
			operator := &stubOperator{results: []provider.OperationResult{{Result: provider.ResultSuccess}}}

			result, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
			if err != nil {
				t.Fatalf("CreateRefund: %v", err)
			}
			if len(operator.calls) != 0 {
				t.Fatalf("重发同一个售后单号又发了一次渠道请求：%+v", operator.calls)
			}
			if result.Status != status {
				t.Errorf("拿回的状态 = %q，期望 %q", result.Status, status)
			}
			if result.RefundNo != "REF20260914120000000001" {
				t.Errorf("拿回的退款单号 = %q，期望是上次那一张", result.RefundNo)
			}
		})
	}
}

// TestCreateRefundRetriesAPendingRefund 钉住另一个方向：上次**建完单就没了**（进程死在发起
// 之前），重发要把渠道请求补上。
//
// 这与上面那条是一对：幂等挡的是「同一件事做两次」，不是「没做完的那件事别做了」。重发是
// 安全的——渠道按 (商户订单号, 退款单号) 判幂等，同号回的是上一次那张退货单的结果。
func TestCreateRefundRetriesAPendingRefund(t *testing.T) {
	repo := &fakeRepository{beginRefundHit: true}
	repo.paymentByNo = refundablePayment()
	repo.refund = &model.Refund{
		ID: "refund-1", RefundNo: "REF20260914120000000001", PaymentNo: refundablePayment().PaymentNo,
		AfterSaleNo: validRefundRequest().AfterSaleNo, Amount: 10000, Status: model.RefundPending,
	}
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
	operator := &stubOperator{results: []provider.OperationResult{{Result: provider.ResultSuccess}}}

	result, err := newRefundService(t, repo, operator).CreateRefund(context.Background(), validRefundRequest())
	if err != nil {
		t.Fatalf("CreateRefund: %v", err)
	}
	if len(operator.calls) != 1 {
		t.Fatalf("渠道调用 = %d 次，期望 1（停在 pending 的那张要把请求补上）", len(operator.calls))
	}
	if result.Status != model.RefundSucceeded {
		t.Errorf("对外状态 = %q，期望 %q", result.Status, model.RefundSucceeded)
	}
}

// TestCreateRefundWithoutAnOperatorIsExplicit 钉住「适配器没有退款能力」那条路。
//
// 它是**配置问题**，不是「钱退不回去」：把退款单标成 failed 会让售后单落到一个终态，而其实
// 换条路就能退。所以这里要的是一个 error，且退款单停在 pending。
func TestCreateRefundWithoutAnOperatorIsExplicit(t *testing.T) {
	repo := &fakeRepository{}
	repo.paymentByNo = refundablePayment()
	repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})

	// 一个**只**实现 provider.Provider 的适配器，没有退款能力。
	plain := &stubProvider{result: provider.CreateResult{Result: provider.ResultSuccess}}
	svc := newServiceWith(t, repo, plain, testMethodCode, nil,
		func(*catalog.Channel, string) string { return "secret" })

	_, err := svc.CreateRefund(context.Background(), validRefundRequest())
	if !errors.Is(err, ErrProviderOperationUnsupported) {
		t.Fatalf("CreateRefund = %v，期望 ErrProviderOperationUnsupported", err)
	}
	if len(repo.refundSettles)+len(repo.refundFailures)+len(repo.refundProcessing) != 0 {
		t.Errorf("没有退款能力的渠道却落了结论：succeeded=%d failed=%d processing=%d",
			len(repo.refundSettles), len(repo.refundFailures), len(repo.refundProcessing))
	}
}

// —— 退款查询（见 refund_reconcile.go / worker/refund.go）——

// processingRefund 造一张停在 processing 的退款单，形状与库里的那一行一致。
func processingRefund(no string) model.Refund {
	return model.Refund{
		ID: "refund-" + no, RefundNo: no, PaymentNo: refundablePayment().PaymentNo,
		OrderNo: testOrderNo, UserID: testUserID, AfterSaleNo: "AS20260914000001",
		Amount: 10000, Status: model.RefundProcessing, RequestID: testRequest,
	}
}

// TestReconcileProcessingRefundsSettlesTheOnesWithAnAnswer 覆盖查询那条路能给出的两个结论。
//
// 它是这条链上**唯一**能把 processing 推向终态的东西：规范里没有退款回调。所以这一条同时
// 也是「退款不会永远停在处理中」这件事的全部证据。
func TestReconcileProcessingRefundsSettlesTheOnesWithAnAnswer(t *testing.T) {
	cases := []struct {
		name       string
		result     provider.OperationResult
		wantStatus string
	}{
		{"问出成功", provider.OperationResult{Result: provider.ResultSuccess, ProviderRefundID: "UMS-REFUND-2"}, model.RefundSucceeded},
		{"问出失败", provider.OperationResult{Result: provider.ResultFailed, FailureCode: "REFUND_FAILED"}, model.RefundFailed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{processingRefunds: []model.Refund{processingRefund("REF20260914120000000002")}}
			repo.paymentByNo = refundablePayment()
			repo.refund = &processingRefundHolder
			operator := &stubOperator{results: []provider.OperationResult{tc.result}}

			stats, err := newRefundService(t, repo, operator).ReconcileProcessingRefunds(
				context.Background(), time.Now(), 20)
			if err != nil {
				t.Fatalf("ReconcileProcessingRefunds: %v", err)
			}
			if stats.Scanned != 1 {
				t.Errorf("扫描 = %d，期望 1", stats.Scanned)
			}
			if stats.Skipped != 0 || stats.Inconclusive != 0 {
				t.Errorf("有结论的一笔被记成了问不到：skipped=%d inconclusive=%d", stats.Skipped, stats.Inconclusive)
			}

			// 问的是**退款查询**，不是再发起一次退款——发错了会在渠道那边多退一次钱。
			if len(operator.calls) != 1 {
				t.Fatalf("渠道调用 = %d 次，期望 1", len(operator.calls))
			}
			if operator.calls[0].Operation != provider.OperationQueryRefund {
				t.Errorf("发出去的操作 = %q，期望 %q", operator.calls[0].Operation, provider.OperationQueryRefund)
			}
			if operator.calls[0].RefundNo != "REF20260914120000000002" {
				t.Errorf("问的退款单号 = %q，不对", operator.calls[0].RefundNo)
			}
			// 查询不该带金额：渠道按退款单号认这一笔，带上去只会让人以为那是「要退多少」。
			if operator.calls[0].Amount != 0 {
				t.Errorf("查询带上了金额 %d", operator.calls[0].Amount)
			}

			// 有结论的一笔**不推队尾**：它已经走了，推它等于让后面那些排队的多等一轮。
			if len(repo.touchedRefunds) != 0 {
				t.Errorf("有结论的退款单被推到了队尾：%v", repo.touchedRefunds)
			}

			switch tc.wantStatus {
			case model.RefundSucceeded:
				if stats.Succeeded != 1 {
					t.Errorf("succeeded 计数 = %d，期望 1", stats.Succeeded)
				}
				if len(repo.refundSettles) != 1 {
					t.Fatalf("结算 = %d 次，期望 1", len(repo.refundSettles))
				}
				if repo.refundSettles[0].ProviderRefundID != tc.result.ProviderRefundID {
					t.Errorf("落下的渠道退款单号 = %q，期望 %q",
						repo.refundSettles[0].ProviderRefundID, tc.result.ProviderRefundID)
				}
			case model.RefundFailed:
				if stats.Failed != 1 {
					t.Errorf("failed 计数 = %d，期望 1", stats.Failed)
				}
				if len(repo.refundFailures) != 1 {
					t.Fatalf("失败落库 = %d 次，期望 1", len(repo.refundFailures))
				}
				// 这一条失败是从 processing 出发的，不是 pending——状态流水上那一行要记对。
				if repo.refundFailures[0].From != model.RefundProcessing {
					t.Errorf("失败是从 %q 出发的，期望 %q（查询那条路）", repo.refundFailures[0].From, model.RefundProcessing)
				}
			}
		})
	}
}

// processingRefundHolder 让假仓储里那一张退款单与 ListStaleProcessingRefunds 回答的是同一条。
//
// 做成包级变量而不是每条用例各写一遍，是因为「扫出来的那一条」与「被推进的那一条」必须是
// 同一张——两张不同的单会让断言在状态上看着对、实际改的是别人。
var processingRefundHolder = processingRefund("REF20260914120000000002")

// TestReconcileProcessingRefundsPushesTheInconclusiveToTheBack 钉住排队那条规矩。
//
// 一笔托管退款在渠道那边可以挂几天，每一轮都会被扫到。不推队尾的话，一批 20 笔全卡在最早
// 的那几笔上，后面那些**其实一次就能问出结果**的退款永远排不到——而那正是「用户的钱退了
// 但售后单一直不动」这个症状的来源。代价是那一笔下次晚一个周期再问，它本来就还不知道。
func TestReconcileProcessingRefundsPushesTheInconclusiveToTheBack(t *testing.T) {
	cases := []struct {
		name   string
		result provider.OperationResult
	}{
		{"还是 PROCESSING", provider.OperationResult{Result: provider.ResultUnknown}},
		{"读不懂的应答", provider.OperationResult{Result: "something_else"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepository{processingRefunds: []model.Refund{processingRefundHolder}}
			repo.paymentByNo = refundablePayment()
			repo.refund = &processingRefundHolder
			operator := &stubOperator{results: []provider.OperationResult{tc.result}}

			stats, err := newRefundService(t, repo, operator).ReconcileProcessingRefunds(
				context.Background(), time.Now(), 20)
			if err != nil {
				t.Fatalf("ReconcileProcessingRefunds: %v", err)
			}
			if stats.Inconclusive != 1 {
				t.Errorf("inconclusive = %d，期望 1", stats.Inconclusive)
			}
			if stats.Succeeded+stats.Failed != 0 {
				t.Errorf("没有结论却记成了有结论：succeeded=%d failed=%d", stats.Succeeded, stats.Failed)
			}
			if len(repo.touchedRefunds) != 1 || repo.touchedRefunds[0] != processingRefundHolder.ID {
				t.Errorf("被推到队尾的退款单 = %v，期望 [%s]", repo.touchedRefunds, processingRefundHolder.ID)
			}
			// 推进用的是 MarkRefundProcessing（把它从 processing 再写一次 processing）：这一格
			// 会顺带更新 updated_at 之外什么都没变，但**不改出资行、不发事件**。
			if len(repo.refundSettles)+len(repo.refundFailures) != 0 {
				t.Errorf("没有结论却落了终态：succeeded=%d failed=%d",
					len(repo.refundSettles), len(repo.refundFailures))
			}
		})
	}
}

// TestReconcileProcessingRefundsKeepsGoingAfterABadOne 钉住「一笔坏的不打断这一轮」。
//
// 三种坏法各自计进 Skipped：支付单不见了（数据异常）、渠道没适配器、出资行读不出来。它们
// 的原因逐笔记在日志里，而这一轮里**后面那些好的照样要被问**——批里第一笔就中断的话，
// 一笔坏数据能让整个退款队列停摆。
func TestReconcileProcessingRefundsKeepsGoingAfterABadOne(t *testing.T) {
	t.Run("支付单不见了", func(t *testing.T) {
		repo := &fakeRepository{processingRefunds: []model.Refund{processingRefundHolder}}
		// paymentByNo 零值 = 查不到。这张退款单指向的支付单没了，没有任何东西能推它往前走。
		operator := &stubOperator{}

		stats, err := newRefundService(t, repo, operator).ReconcileProcessingRefunds(
			context.Background(), time.Now(), 20)
		if err != nil {
			t.Fatalf("ReconcileProcessingRefunds: %v", err)
		}
		if stats.Skipped != 1 || stats.Scanned != 1 {
			t.Errorf("scanned=%d skipped=%d，期望 1/1", stats.Scanned, stats.Skipped)
		}
		if len(operator.calls) != 0 {
			t.Errorf("支付单都读不出来还发了渠道请求：%+v", operator.calls)
		}
		// 没发出去的那一笔也不该被推队尾：它下一轮会被再捞出来，原因会被再记一次，而推队尾
		// 会让它沉到后面——数据异常要一直浮在面上，直到有人来看。
		if len(repo.touchedRefunds) != 0 {
			t.Errorf("数据异常的那笔被推到了队尾：%v", repo.touchedRefunds)
		}
	})

	t.Run("一笔坏的不挡住后面的", func(t *testing.T) {
		bad := processingRefund("REF-BAD")
		good := processingRefund("REF-GOOD")
		repo := &fakeRepository{processingRefunds: []model.Refund{bad, good}}
		repo.paymentByNo = refundablePayment()
		// 假仓储只认得出 good 那一张的出资行（真仓储按 refund_no 逐张查，两张都查得到）。
		// 于是第一笔在「出资行读不出来」那一步被计进 Skipped，第二笔必须照样走到渠道。
		goodHolder := good
		repo.refund = &goodHolder
		repo.refundFundings = refundFundingsOf(model.RefundFunding{LineType: "ums_h5_wechat", Amount: 10000})
		operator := &stubOperator{results: []provider.OperationResult{
			{Result: provider.ResultSuccess, ProviderRefundID: "UMS-REFUND-3"},
		}}

		stats, err := newRefundService(t, repo, operator).ReconcileProcessingRefunds(
			context.Background(), time.Now(), 20)
		if err != nil {
			t.Fatalf("ReconcileProcessingRefunds: %v", err)
		}
		if stats.Scanned != 2 {
			t.Errorf("scanned = %d，期望 2（第一笔坏掉不能中断这一轮）", stats.Scanned)
		}
		if stats.Skipped != 1 {
			t.Errorf("skipped = %d，期望 1（只有第一笔坏）", stats.Skipped)
		}
		if stats.Succeeded != 1 {
			t.Errorf("succeeded = %d，期望 1（第二笔要真的被问出结论）", stats.Succeeded)
		}
		if len(operator.calls) != 1 || operator.calls[0].RefundNo != "REF-GOOD" {
			t.Errorf("问到渠道的 = %+v，期望只有 REF-GOOD", operator.calls)
		}
	})

	t.Run("扫描本身失败", func(t *testing.T) {
		repo := &fakeRepository{staleRefundErr: errors.New("connection refused")}
		stats, err := newRefundService(t, repo, &stubOperator{}).ReconcileProcessingRefunds(
			context.Background(), time.Now(), 20)
		if err == nil {
			t.Fatal("库读不出来却报成功")
		}
		if stats != (RefundReconcileStats{}) {
			t.Errorf("失败的扫描还带了计数：%+v", stats)
		}
	})
}

// TestReconcileProcessingRefundsPassesTheQueryWindowThrough 钉住扫描的实参是调用方给的。
//
// 「多久没动过才算该问一句、一次最多问几笔」是 worker 的决定（见 worker/refund.go 里那两
// 个常量），假仓储不替它做——但得能断言它真的传下来了。传错的表现是**每一轮都把全部积压
// 重问一遍**，而出网次数翻倍在日志里是看不出来的。
func TestReconcileProcessingRefundsPassesTheQueryWindowThrough(t *testing.T) {
	repo := &fakeRepository{}
	staleBefore := time.Date(2026, 9, 14, 11, 55, 0, 0, time.UTC)

	if _, err := newRefundService(t, repo, &stubOperator{}).ReconcileProcessingRefunds(
		context.Background(), staleBefore, 7); err != nil {
		t.Fatalf("ReconcileProcessingRefunds: %v", err)
	}
	if len(repo.refundQueries) != 1 {
		t.Fatalf("扫描 = %d 次，期望 1", len(repo.refundQueries))
	}
	got := repo.refundQueries[0]
	if !got.staleBefore.Equal(staleBefore) {
		t.Errorf("staleBefore = %v，期望 %v", got.staleBefore, staleBefore)
	}
	if got.limit != 7 {
		t.Errorf("limit = %d，期望 7", got.limit)
	}
}

// TestRefundStateMachine 钉住退款那两张状态机表的形状。
//
// 它是给「有人在代码里直接 UPDATE 状态」这类改动兜底的：canTransition 对**未知状态一律
// 拒绝**（见 state.go），所以这里也要验一个不认识的词不会被放行——线上出现一个没人认识的
// 状态时，宁可让操作失败让人来查。
func TestRefundStateMachine(t *testing.T) {
	legal := []struct{ from, to string }{
		{model.RefundPending, model.RefundProcessing},
		{model.RefundPending, model.RefundSucceeded},
		{model.RefundPending, model.RefundFailed},
		{model.RefundProcessing, model.RefundSucceeded},
		{model.RefundProcessing, model.RefundFailed},
	}
	for _, edge := range legal {
		if !CanRefundTransition(edge.from, edge.to) {
			t.Errorf("%s → %s 被判成非法，但它是一条真实存在的边", edge.from, edge.to)
		}
	}

	illegal := []struct{ from, to string }{
		// 两个终态都没有出边：钱退回去了、或者退不了。重试是**同一张售后单**重发
		// CreateRefund，那会命中幂等拿回同一张单，而不是把这张改回去。
		{model.RefundSucceeded, model.RefundFailed},
		{model.RefundSucceeded, model.RefundProcessing},
		{model.RefundFailed, model.RefundSucceeded},
		{model.RefundFailed, model.RefundProcessing},
		// processing 回不到 pending：它已经从渠道那里拿到过一个含糊的应答了。
		{model.RefundProcessing, model.RefundPending},
		// 自己到自己不是一条边。
		{model.RefundPending, model.RefundPending},
		// 不认识的词一律拒。
		{"refunded", model.RefundSucceeded},
		{model.RefundPending, "refunded"},
	}
	for _, edge := range illegal {
		if CanRefundTransition(edge.from, edge.to) {
			t.Errorf("%s → %s 被判成合法，但这条边不该存在", edge.from, edge.to)
		}
	}

	// 退款出资行比出资本身短得多：没有预占那一步，一行要么还没退、要么退了、要么退不成。
	if !CanRefundFundingTransition(model.RefundFundingPending, model.RefundFundingSucceeded) {
		t.Error("退款出资行 pending → succeeded 被判成非法")
	}
	if !CanRefundFundingTransition(model.RefundFundingPending, model.RefundFundingFailed) {
		t.Error("退款出资行 pending → failed 被判成非法")
	}
	for _, from := range []string{model.RefundFundingSucceeded, model.RefundFundingFailed} {
		for _, to := range []string{model.RefundFundingPending, model.RefundFundingSucceeded, model.RefundFundingFailed} {
			if CanRefundFundingTransition(from, to) {
				t.Errorf("退款出资行 %s → %s 被判成合法，但它是终态", from, to)
			}
		}
	}

	// 退款那几个值都是合法的退款状态。
	for _, state := range []string{model.RefundPending, model.RefundProcessing, model.RefundSucceeded, model.RefundFailed, model.RefundCancelled} {
		if !model.IsRefundStatus(state) {
			t.Errorf("%q 不是一个合法的退款状态", state)
		}
	}

	// **processing 是退款这条链独有的**：支付单没有这个状态，而它的存在正是「退款不改
	// payments.status」那句注释的可执行版本（见 paymentTransitions 那一段）。这条断言只能
	// 靠 processing 来写——pending / succeeded / failed 这几个词两边同名，值相同但**不是
	// 同一张表上的东西**，用它们去比只会证明字符串相等。
	if model.IsPaymentStatus(model.RefundProcessing) {
		t.Errorf("退款独有状态 %q 被当成了一个合法的支付单状态", model.RefundProcessing)
	}
	if !model.IsPaymentStatus(model.PaymentSucceeded) {
		t.Error("succeeded 不是一个合法的支付单状态")
	}
}
