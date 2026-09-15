package manual_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider/manual"
)

// 这一组测试是「验签是真的在做」的证据。适配器的 Create 可以是个模拟动作，但 Verify
// 不行：它挡着的是「伪造一条回调就让订单变成已支付」这条最要命的路，用桩函数写它等于
// 把这条路留空。
const testSecret = "manual-test-secret-at-least-32-bytes"

func notifyRequest(t *testing.T, secret string, body []byte, signature string) provider.NotificationRequest {
	t.Helper()
	headers := http.Header{}
	if signature != "" {
		headers.Set(manual.SignatureHeader, signature)
	}
	return provider.NotificationRequest{
		ChannelCode: "manual_dev",
		Body:        body,
		Headers:     headers,
		Method:      provider.Method{ChannelCode: "manual_dev", Provider: manual.Name},
		Secret:      secret,
	}
}

func succeededBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(manual.NotificationBody{
		NotificationID:        "notify-1",
		EventType:             string(provider.EventSucceeded),
		PaymentNo:             "PAY-1",
		Amount:                1234,
		ProviderTransactionID: "MANUAL-PAY-1",
		PaidAtUnix:            1700000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestVerifyAcceptsALegitimatelySignedNotification(t *testing.T) {
	body := succeededBody(t)
	notification, err := manual.New().Verify(context.Background(), notifyRequest(t, testSecret, body, manual.Sign(testSecret, body)))
	if err != nil {
		t.Fatalf("Verify error = %v", err)
	}
	if notification.NotificationID != "notify-1" || notification.PaymentNo != "PAY-1" {
		t.Fatalf("notification = %#v", notification)
	}
	if notification.EventType != provider.EventSucceeded || notification.Amount != 1234 {
		t.Fatalf("notification = %#v", notification)
	}
	// 成交时间必须是渠道给的那个，不是「现在」。对账以渠道时间为准，晚收几小时不该让
	// 这笔钱看起来晚收了几小时。
	if got := notification.PaidAt.Unix(); got != 1700000000 {
		t.Fatalf("PaidAt = %d, want 1700000000", got)
	}
}

// 签名是照着**另一份字节**算的（这里改的是金额）——这是攻击者能做到的最直接的一件事：
// 拿一条真的回调改掉金额再发一次。
func TestVerifyRejectsATamperedBody(t *testing.T) {
	body := succeededBody(t)
	signature := manual.Sign(testSecret, body)

	tampered := strings.Replace(string(body), `"amount":1234`, `"amount":999999`, 1)
	if tampered == string(body) {
		t.Fatal("test fixture did not actually change the amount")
	}

	_, err := manual.New().Verify(context.Background(), notifyRequest(t, testSecret, []byte(tampered), signature))
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("Verify error = %v, want ErrSignatureMismatch", err)
	}
}

// 用另一把密钥签的（等价于「攻击者不知道密钥」）。与上一条分开：那条改的是报文，
// 这条改的是密钥，两条走的是同一段比较但坏在两边。
func TestVerifyRejectsAForeignSecret(t *testing.T) {
	body := succeededBody(t)
	_, err := manual.New().Verify(context.Background(), notifyRequest(t, testSecret, body, manual.Sign("someone-elses-secret", body)))
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("Verify error = %v, want ErrSignatureMismatch", err)
	}
}

// 密钥没配时必须**拒绝**，不能放行。放行的后果是「验签这段代码形同不存在」：任何知道
// 回调地址的人都能把一张待支付的单变成已支付。
func TestVerifyRefusesWhenTheSecretIsMissing(t *testing.T) {
	body := succeededBody(t)
	_, err := manual.New().Verify(context.Background(), notifyRequest(t, "", body, manual.Sign("", body)))
	if !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("Verify error = %v, want ErrSecretNotConfigured", err)
	}
}

// 没带签名头。它与「签名不对」是同一个处置（都拒），但错误信息里要说得出签名头本来该在哪。
func TestVerifyRefusesWhenTheSignatureHeaderIsAbsent(t *testing.T) {
	body := succeededBody(t)
	_, err := manual.New().Verify(context.Background(), notifyRequest(t, testSecret, body, ""))
	if !errors.Is(err, provider.ErrSignatureMismatch) {
		t.Fatalf("Verify error = %v, want ErrSignatureMismatch", err)
	}
}

// 签名过了但报文不全：签名只证明「这是我们的密钥签的」，不证明「它是一份完整的成交
// 通知」。成功通知缺金额时尤其不能接受——那会让我们失去金额对账这个判据。
func TestVerifyRefusesAnIncompleteNotification(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "no notification id", body: `{"eventType":"succeeded","paymentNo":"PAY-1","amount":100}`},
		{name: "no payment no", body: `{"notificationId":"n1","eventType":"succeeded","amount":100}`},
		{name: "succeeded without amount", body: `{"notificationId":"n1","eventType":"succeeded","paymentNo":"PAY-1"}`},
		{name: "unknown event type", body: `{"notificationId":"n1","eventType":"refunded","paymentNo":"PAY-1","amount":100}`},
		{name: "not json at all", body: `not json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			_, err := manual.New().Verify(context.Background(), notifyRequest(t, testSecret, body, manual.Sign(testSecret, body)))
			if !errors.Is(err, provider.ErrInvalidNotification) {
				t.Fatalf("Verify error = %v, want ErrInvalidNotification", err)
			}
		})
	}
}

// 渠道加字段是它的权利，不该让我们拒收。这与我们自己的跨服务事件契约相反（那边多一个
// 字段必须炸）——区别在于这份报文的作者是别人，不是我们。
func TestVerifyAcceptsExtraProviderFields(t *testing.T) {
	body := []byte(`{"notificationId":"n1","eventType":"succeeded","paymentNo":"PAY-1","amount":100,"coupon":"FREE-COFFEE"}`)
	if _, err := manual.New().Verify(context.Background(), notifyRequest(t, testSecret, body, manual.Sign(testSecret, body))); err != nil {
		t.Fatalf("Verify error = %v", err)
	}
}

func TestCreateReturnsTheChannelTransactionAndClientParams(t *testing.T) {
	expires := time.Unix(1700000900, 0)
	result, err := manual.New().Create(context.Background(), provider.CreateRequest{
		PaymentNo: "PAY-1",
		OrderNo:   "ORD-1",
		Amount:    1234,
		NotifyURL: "http://localhost:18088/v1/payments/callback/manual_dev",
		ExpiresAt: expires,
		Method:    provider.Method{ID: "m1", Code: "manual_dev", Action: provider.ActionJumpMiniapp, ChannelCode: "manual_dev", Provider: manual.Name, Mode: "sandbox"},
	})
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	if result.Result != provider.ResultSuccess || !result.Pending() {
		t.Fatalf("result = %#v, want a pending success", result)
	}
	if result.ProviderTransactionID != "MANUAL-PAY-1" {
		t.Fatalf("ProviderTransactionID = %q", result.ProviderTransactionID)
	}
	// 金额回客户端必须是字符串（方案 13.1）：JS 的数字精度接不住分。
	if got := result.PayParams["amount"]; got != "1234" {
		t.Fatalf("payParams[amount] = %q, want \"1234\"", got)
	}
	if result.PayParams["notifyUrl"] == "" {
		t.Fatal("payParams did not carry the notify url")
	}
}

// 金额为 0 的单在 payments 表的 CHECK 那关本来就会挂。适配器先拦住它，是为了让「是不是
// 我传错了金额」在日志里一眼可见，而不是变成一次数据库约束报错。
func TestCreateRefusesAnEmptyPayment(t *testing.T) {
	if _, err := manual.New().Create(context.Background(), provider.CreateRequest{PaymentNo: "PAY-1"}); err == nil {
		t.Fatal("expected an error for a non-positive amount")
	}
	if _, err := manual.New().Create(context.Background(), provider.CreateRequest{Amount: 100}); err == nil {
		t.Fatal("expected an error for a missing payment number")
	}
}
