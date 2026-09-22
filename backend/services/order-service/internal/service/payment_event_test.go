package service

import (
	"context"
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/dto"
	"github.com/panda-dev/panda-v2/backend/services/order-service/internal/repository"
)

// fakeSettleRepo 只做一件事：把落单参数接住，好让用例看清我们交给仓储的是哪个值。
//
// 内嵌 Repository（一个接口）而不是自己实现全部方法：这个用例只走到 SettlePayment，
// 别的方法被调到就 panic——那正是「这条路上不该有别的调用」的断言。
type fakeSettleRepo struct {
	Repository
	settled *repository.SettlePaymentParams
}

func (f *fakeSettleRepo) SettlePayment(_ context.Context, p repository.SettlePaymentParams) (*repository.OrderPaymentResult, bool, error) {
	f.settled = &p
	return &repository.OrderPaymentResult{OrderNo: p.OrderNo, Status: "paid"}, true, nil
}

func settleThroughEvent(t *testing.T, body string) *repository.SettlePaymentParams {
	t.Helper()
	repo := &fakeSettleRepo{}
	orderService := New(repo, nil, nil, nil, nil, nil, Options{})
	if err := orderService.HandlePaymentEvent(t.Context(), messaging.Envelope{
		EventType: dto.EventPaymentSucceeded, Payload: []byte(body),
	}); err != nil {
		t.Fatalf("HandlePaymentEvent() error = %v", err)
	}
	if repo.settled == nil {
		t.Fatal("事件被 ack 掉了，没有落单")
	}
	return repo.settled
}

// TestPaymentEventCarriesOneMethodValue 钉住订单侧只认事件里的**一个**支付方式值。
//
// 这个值是 catalog 的 code（如 ums_h5_alipay），写两处：orders.payment_method（后台订单
// 列表展示的那一列）与 order_payment_lines.line_type。
//
// 从前这里是两个字段、两套词表：展示读 code，line_type 读「出资渠道」（wechat /
// unionpay / coffee_bean / wallet / other）。写反或写串的表现：后台订单列表上的「支付方式」
// 永远是「其他」（支付宝在那套词表里没有对应值），或者更糟——拿一个词表外的值去插
// line_type，整条落单事务回滚，而钱在支付侧已经收了。
func TestPaymentEventCarriesOneMethodValue(t *testing.T) {
	params := settleThroughEvent(t, `{"orderNo":"ORD1","paymentNo":"PAY1","amount":1980,
		"paymentMethod":"ums_h5_alipay",
		"fundings":[{"lineType":"ums_h5_alipay","amount":1980,"paymentNo":"PAY1","accountEntryId":null}],
		"providerTransactionId":"T1","paidAtUnix":0,"failureCode":"","failureMessage":""}`)

	if params.PaymentMethod != "ums_h5_alipay" {
		t.Errorf("落库的支付方式 = %q，期望 ums_h5_alipay", params.PaymentMethod)
	}
	if len(params.Fundings) != 1 || params.Fundings[0].LineType != "ums_h5_alipay" {
		t.Errorf("出资行 = %+v，期望一行 line_type = ums_h5_alipay（与 orders.payment_method 同值）", params.Fundings)
	}
}

// TestPaymentEventPassesTheMethodThroughUntouched 钉住服务层**不加料**。
//
// 事件不带分摊时补一行是仓储层的事（见 repository.SettlePayment），服务层只管把事件里的值
// 原样交下去。这一条盯住的就是那个边界：服务层既不补行、也不改写这个值。
func TestPaymentEventPassesTheMethodThroughUntouched(t *testing.T) {
	params := settleThroughEvent(t, `{"orderNo":"ORD1","paymentNo":"PAY1","amount":1980,
		"paymentMethod":"ums_h5_wechat","fundings":[],"providerTransactionId":"T1",
		"paidAtUnix":0,"failureCode":"","failureMessage":""}`)

	if params.PaymentMethod != "ums_h5_wechat" {
		t.Errorf("支付方式 = %q，期望 ums_h5_wechat", params.PaymentMethod)
	}
	if len(params.Fundings) != 0 {
		t.Errorf("出资行 = %+v，期望原样传空——补行是仓储层的事", params.Fundings)
	}
}

// TestPaymentEventDoesNotInventAMethod 钉住「事件没带支付方式」那一格：**服务层不兜底**。
//
// 从前这里有两条兜底：支付方式为空时回落成 `other`。`other` 是那套已退场的出资渠道词表里的
// 兜底值，今天不存在了；而更要紧的是那句话本身——凭空写一个值等于替用户编一句「这笔钱从哪
// 出」。所以服务层原样把空值交下去，由仓储层整条报错（见 repository.SettlePayment），
// 消息进死信由人来看。
//
// 这条路径本该走不到：payments.payment_method 是 NOT NULL，正常发不出不带方式的事件。
func TestPaymentEventDoesNotInventAMethod(t *testing.T) {
	params := settleThroughEvent(t, `{"orderNo":"ORD1","paymentNo":"PAY1","amount":1980,
		"paymentMethod":"","fundings":[],"providerTransactionId":"T1",
		"paidAtUnix":0,"failureCode":"","failureMessage":""}`)

	if params.PaymentMethod != "" {
		t.Errorf("支付方式 = %q，期望原样传空——服务层不该替用户编一个", params.PaymentMethod)
	}
	if len(params.Fundings) != 0 {
		t.Errorf("出资行 = %+v，期望一行都不补", params.Fundings)
	}
}
