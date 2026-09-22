package ums

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 这一组测试守的是「伪造的回调一个字段都不动」这条不变量的**入口**：Verify 必须拒掉一切它
// 算不出签名的报文。
//
// 签名同样用 config_test.go 里的 legacyAuthHeader 造——老系统那段实现的复刻。用本包的 Sign
// 来造签名只会证明「我们自己签的自己认」，而真实的对手方是渠道。
//
// 但这里有一条必须先记住的前提：**老系统根本没有验签**（见 ums.go 与 notify.go 上那两段长
// 注释）。所以这一组测试证明的是「我们自己的推断实现得自洽」，**不是**「渠道的回调真是这么
// 签的」。后者必须拿真实凭据实测，那时要确认的是：回调到底带不带 Authorization 头、带的那个
// 是不是同一个算法的结果。

// callbackBody 造一份回调报文。金额单位照这一族的报文是**分**。
func callbackBody(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("造回调报文：%v", err)
	}
	return body
}

// 签名头里的那两段由用例共享：改随机串/时间戳的那两条要在**不改签名**的前提下换掉它们，
// 所以它们不能藏在 signedCallback 里面。
const (
	callbackTimestamp = "20260917102030"
	callbackNonce     = "nonce-from-the-channel"
)

// signedCallback 用老系统那套算法给一份报文签一次，返回可以直接交给 Verify 的请求。
//
// appID 由调用方给：验签的第一道就是「这个 appId 是不是本渠道的」，用例要能伪造别人的。
func signedCallback(t *testing.T, config map[string]any, appID, secret string, body []byte) provider.NotificationRequest {
	t.Helper()
	header := legacyAuthHeader(appID, secret, callbackTimestamp, callbackNonce, body)

	headers := http.Header{}
	headers.Set(headerAuthorization, header)

	return provider.NotificationRequest{
		ChannelCode: "ums_wechat",
		Body:        body,
		Headers:     headers,
		HTTPMethod:  http.MethodPost,
		RequestPath: "/v1/payments/callback/ums_wechat",
		Method:      channelMethod(config),
		Secrets:     credentialsFor(secret),
	}
}

const callbackPath = "/v1/payments/callback/ums_wechat"

// TestVerifyAcceptsALegacySignedCallback 是这一组的正面：一份用老系统那套算法签出来的回调，
// 被本包的 Verify 认下来，且每个字段都翻对了。
func TestVerifyAcceptsALegacySignedCallback(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	body := callbackBody(t, map[string]any{
		"merOrderId":    "PAY20260917000000000001",
		"status":        "TRADE_SUCCESS",
		"targetOrderId": "T-9001",
		"transactionId": "TX-1",
		"notifyId":      "N-1",
		"totalAmount":   12800,
		"payTime":       "2026-09-17 10:20:30",
	})

	notification, err := New().Verify(context.Background(), signedCallback(t, config, testAppID, testSecret, body))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if notification.EventType != provider.EventSucceeded {
		t.Fatalf("EventType = %q", notification.EventType)
	}
	if notification.PaymentNo != "PAY20260917000000000001" {
		t.Fatalf("PaymentNo = %q", notification.PaymentNo)
	}
	// 三级回落的第一级就是交易号，所以这里取的是 targetOrderId 而不是 notifyId。
	if notification.ProviderTransactionID != "T-9001" {
		t.Fatalf("ProviderTransactionID = %q", notification.ProviderTransactionID)
	}
	if notification.Amount != 12800 {
		t.Fatalf("Amount = %d, want 12800（分，不换算）", notification.Amount)
	}
	if notification.NotificationID != "PAY20260917000000000001:TRADE_SUCCESS" {
		t.Fatalf("NotificationID = %q", notification.NotificationID)
	}
	if got := notification.PaidAt.Format(requestTimestampLayout); got != "2026-09-17 10:20:30" {
		t.Fatalf("PaidAt = %q，成交时间必须取渠道给的 payTime 而不是我们收到回调的时间", got)
	}
}

// TestVerifyRejectsAnythingThatChangesTheSignedInput 是这一族最要紧的一组：**改了待签串里
// 的任何一段，签名就必须对不上**。
//
// 变体对应真实的攻击：改报文（把金额改大）、把报文搬到另一个渠道上重放、用一个我们知道
// 密钥的别人的 appId 自己签一份。
func TestVerifyRejectsAnythingThatChangesTheSignedInput(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	body := callbackBody(t, map[string]any{
		"merOrderId": "PAY20260917000000000001", "status": "TRADE_SUCCESS", "totalAmount": 100,
	})
	tampered := callbackBody(t, map[string]any{
		"merOrderId": "PAY20260917000000000001", "status": "TRADE_SUCCESS", "totalAmount": 999900,
	})

	cases := []struct {
		name   string
		change func(*provider.NotificationRequest)
	}{
		{"报文被改了一个字节", func(r *provider.NotificationRequest) { r.Body = tampered }},
		{"签名头被改了一位", func(r *provider.NotificationRequest) {
			header := r.Headers.Get(headerAuthorization)
			r.Headers.Set(headerAuthorization, header+"x")
		}},
		// 这两条**只换头里的那一段、不动签名**（不能重签：重签出来的是另一份合法签名，
		// 那验的就是别的东西了）。它们验的是「随机串与时间戳确实进了待签串」。
		{"换了随机串", func(r *provider.NotificationRequest) {
			r.Headers.Set(headerAuthorization,
				strings.Replace(r.Headers.Get(headerAuthorization), callbackNonce, "another-nonce", 1))
		}},
		{"换了时间戳", func(r *provider.NotificationRequest) {
			r.Headers.Set(headerAuthorization,
				strings.Replace(r.Headers.Get(headerAuthorization), callbackTimestamp, "20200101000000", 1))
		}},
		// 用**别人的 appId** 签：待签串以 appId 开头，所以这份报文是自洽的——除非我们先把
		// appId 与配置比一遍。少了那一步，任何知道「某个 appId 配某把密钥」的人都能伪造回调。
		{"用了别人的 appId 自己签一份", func(r *provider.NotificationRequest) {
			r.Headers.Set(headerAuthorization, legacyAuthHeader("another-app", testSecret, "20260917102030", "nonce-from-the-channel", r.Body))
		}},
		// 签名头声明的是别人的 appId，签名却是我们那把密钥算的：两道判据里至少有一道要拦下它。
		{"签名头里写了别人的 appId", func(r *provider.NotificationRequest) {
			header := r.Headers.Get(headerAuthorization)
			r.Headers.Set(headerAuthorization, strings.Replace(header, testAppID, "another-app", 1))
		}},
		{"用了另一把密钥签", func(r *provider.NotificationRequest) {
			r.Headers.Set(headerAuthorization, legacyAuthHeader(testAppID, "a-different-secret", "20260917102030", "nonce-from-the-channel", r.Body))
		}},
		{"签名头整个没了", func(r *provider.NotificationRequest) { r.Headers.Del(headerAuthorization) }},
		{"签名头是别的形状", func(r *provider.NotificationRequest) { r.Headers.Set(headerAuthorization, "Bearer xyz") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := signedCallback(t, config, testAppID, testSecret, body)
			tc.change(&request)

			_, err := New().Verify(context.Background(), request)
			if !errors.Is(err, provider.ErrSignatureMismatch) {
				t.Fatalf("want ErrSignatureMismatch, got %v", err)
			}
			// **绝不能是 ErrInvalidNotification**：那个错的意思是「签名验过了、报文不合法」，
			// 调用方会据此把 payment_notifications.signature_verified 记成 true。
			// 在这一档包上它，库里就会出现一条「签名验过了」而其实根本没验的记录。
			if errors.Is(err, provider.ErrInvalidNotification) {
				t.Fatalf("验签失败被报成了「签名过了、报文不合法」：%v", err)
			}
		})
	}
}

// TestVerifyNeverFallsBackToSkippingTheSignature：空密钥、坏配置两条路都必须拒绝。
//
// 这一条尤其要紧，因为**老系统的现状正好演示了不设防的回调入口长什么样**：ParseNotify 是
// 空壳，UMSPayNotify 连它都没调。绝不能有「解不出密钥就跳过验签」这种降级。
func TestVerifyNeverFallsBackToSkippingTheSignature(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	body := callbackBody(t, map[string]any{
		"merOrderId": "PAY20260917000000000001", "status": "TRADE_SUCCESS", "totalAmount": 100,
	})

	t.Run("密钥是空的", func(t *testing.T) {
		request := signedCallback(t, config, testAppID, testSecret, body)
		request.Secrets = credentialsFor("   ")

		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrSecretNotConfigured) {
			t.Fatalf("want ErrSecretNotConfigured, got %v", err)
		}
		if errors.Is(err, provider.ErrInvalidNotification) {
			t.Fatalf("验不了签被报成了「签名过了、报文不合法」：%v", err)
		}
	})

	t.Run("配置解不开", func(t *testing.T) {
		request := signedCallback(t, config, testAppID, testSecret, body)
		request.Method = channelMethod(map[string]any{"baseURL": "https://api-mop.chinaums.com"})

		_, err := New().Verify(context.Background(), request)
		if !errors.Is(err, provider.ErrConfigInvalid) {
			t.Fatalf("want ErrConfigInvalid, got %v", err)
		}
		// 配置解不开时签名还没验，所以**不能**声称验过了。
		if errors.Is(err, provider.ErrInvalidNotification) {
			t.Fatalf("配置解不开被报成了「签名过了、报文不合法」：%v", err)
		}
	})
}

// TestVerifyReportsSignedButMalformedBodiesAsInvalidNotification 是错误分档的另一半。
//
// 这一档的报文**签名是对的**——渠道确实发了它，只是内容不合法。调用方据此把
// payment_notifications.signature_verified 记成 true，那一列的全部意义就在这个区分上。
func TestVerifyReportsSignedButMalformedBodiesAsInvalidNotification(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")

	cases := []struct {
		name string
		body []byte
	}{
		{"报文不是 json", []byte(`<html>hello</html>`)},
		// 成功回调必须说清是哪一单：不带 merOrderId 的话调用方根本不知道该动哪一行。
		{"成功但不说是哪一单", callbackBody(t, map[string]any{"status": "TRADE_SUCCESS", "totalAmount": 100})},
		// 金额是结算时与支付单对账的凭据（repository/callback.go:216 无条件比一遍），缺了它
		// 这条回调**一定**会被拒——在这里先报出来，错误串能指着 totalAmount 说。
		{"成功但没有金额", callbackBody(t, map[string]any{"merOrderId": "PAY-1", "status": "TRADE_SUCCESS"})},
		{"成功但金额是零", callbackBody(t, map[string]any{"merOrderId": "PAY-1", "status": "TRADE_SUCCESS", "totalAmount": 0})},
		{"成功但金额读不懂", callbackBody(t, map[string]any{"merOrderId": "PAY-1", "status": "TRADE_SUCCESS", "totalAmount": "一百块"})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := signedCallback(t, config, testAppID, testSecret, tc.body)

			_, err := New().Verify(context.Background(), request)
			if !errors.Is(err, provider.ErrInvalidNotification) {
				t.Fatalf("want ErrInvalidNotification, got %v", err)
			}
		})
	}
}

// TestVerifyAcceptsAStringTypedAmount：同一个字段在两种投递方式下类型不同。
//
// 老系统的回调入口按 Content-Type 把体解成 JSON 或**表单**，表单那条路上每个值都是字符串。
// 只吃数字会让表单投递的成功回调全部被拒——而那是渠道的一种正常投递方式。
func TestVerifyAcceptsAStringTypedAmount(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")
	body := []byte(`{"merOrderId":"PAY-1","status":"TRADE_SUCCESS","totalAmount":"12800"}`)

	notification, err := New().Verify(context.Background(), signedCallback(t, config, testAppID, testSecret, body))
	if err != nil {
		t.Fatalf("表单投递的金额该被认下来：%v", err)
	}
	if notification.Amount != 12800 {
		t.Fatalf("Amount = %d, want 12800", notification.Amount)
	}
}

// TestOnlySuccessIsRecognized 钉死这一族回调处置的**形状**：只认成功，其余原样带出去。
//
// 老系统的回调只处理成功（HandlePayNotify 那个 if），其余一律「跳过处理」——没有任何失败
// 分支。这里照抄，并且**不**把它映射成 provider.EventFailed：
//
//   - 那些状态里混着 TRADE_CLOSED（确实没成）、WAIT_BUYER_PAY（还没付）、TRADE_REFUND
//     （收款**之后**的退款）三类，映射成 failed 会把最后那一类变成「一笔已经收妥的单被标成
//     失败」；
//   - 原样把渠道的词当事件类型带出去，service 记一行 ignored、回成功应答：支付状态一个字段
//     都不动，而「渠道说的到底是什么词」在库里查得到。
func TestOnlySuccessIsRecognized(t *testing.T) {
	config := channelConfig("https://api-mop.chinaums.com")

	cases := []struct {
		name       string
		status     string
		totalFen   int
		want       provider.NotificationEvent
		wantAmount int64
	}{
		{"成功", "TRADE_SUCCESS", 100, provider.EventSucceeded, 100},
		{"成功的另一种写法", "SUCCESS", 100, provider.EventSucceeded, 100},
		{"成功的第三种写法", "success", 100, provider.EventSucceeded, 100},
		// 这几个都**不是** failed，理由见上。TRADE_CLOSED 确实没成，但它也不该在这里被
		// 映射成失败：这一族的回调里没有任何一个词能确定地表示「这笔钱不会来了」，
		// 而查单那条路（status=TRADE_CLOSED）才是渠道明确说「这笔没有成」的地方。
		//
		// 它们的报文里**照样带金额**：非成功那条路根本不读这个字段，所以一条带金额的
		// TRADE_REFUND 不该让 Amount 变成非零（退款额不是支付额，带出去会让结算时
		// 与支付单的金额对不上而整条拒收）。
		{"已关闭", "TRADE_CLOSED", 100, provider.NotificationEvent("TRADE_CLOSED"), 0},
		{"还没付", "WAIT_BUYER_PAY", 100, provider.NotificationEvent("WAIT_BUYER_PAY"), 0},
		{"退款", "TRADE_REFUND", 100, provider.NotificationEvent("TRADE_REFUND"), 0},
		{"不认识的状态", "WHATEVER", 100, provider.NotificationEvent("WHATEVER"), 0},
		{"连状态都没给", "", 100, provider.NotificationEvent(""), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{
				"merOrderId":  "PAY-1",
				"status":      tc.status,
				"totalAmount": tc.totalFen,
			}
			request := signedCallback(t, config, testAppID, testSecret, callbackBody(t, fields))

			notification, err := New().Verify(context.Background(), request)
			if err != nil {
				// 非成功状态**不校验支付单号**：一条与收款无关的通知不带我们的单号是正常的，
				// 而拿一个必填校验把它变成「拒收」会让渠道一直重投。
				t.Fatalf("非成功状态该被原样带出去，而不是拒掉：%v", err)
			}
			if notification.EventType != tc.want {
				t.Fatalf("EventType = %q, want %q", notification.EventType, tc.want)
			}
			if notification.EventType == provider.EventFailed {
				t.Fatal("这一族的回调里没有任何一个词能确定地表示「这笔钱不会来了」")
			}
			// 金额只在成功那条路上读（也只有那条路要求它非零）：非成功通知里那个字段既不
			// 该被解析、也不该被带出去。
			if notification.Amount != tc.wantAmount {
				t.Fatalf("Amount = %d, want %d", notification.Amount, tc.wantAmount)
			}
		})
	}
}

// TestNotificationIDIsStableAndSeparatesStatuses 钉死去重键。
//
// 两个方向都要验：
//
//   - 同一条通知重投 → 同一个键（唯一键挡得住，不会一秒钟刷出一堆行）
//   - PENDING 之后再来一条 SUCCESS → **两个不同的键**。单用订单号做键时第二条会被当成重复
//     丢掉，于是这笔钱永远结算不了——这正是它带上 status 的理由。
//
// 而且**不用 notifyId**（渠道的通知号）：我们不知道它的粒度，若渠道是「一单一 notifyId」，
// 那个陷阱会更隐蔽。带上 status 的合成键在两种粒度下都安全。
func TestNotificationIDIsStableAndSeparatesStatuses(t *testing.T) {
	success := paymentCallback{MerOrderID: "PAY-1", Status: "TRADE_SUCCESS", NotifyID: "N-1"}
	pending := paymentCallback{MerOrderID: "PAY-1", Status: "WAIT_BUYER_PAY", NotifyID: "N-1"}

	if got := notificationID(success); got != "PAY-1:TRADE_SUCCESS" {
		t.Fatalf("NotificationID = %q", got)
	}
	if notificationID(success) != notificationID(success) {
		t.Fatal("同一份报文两次算出来的键不同：重投挡不住了")
	}
	if notificationID(success) == notificationID(pending) {
		t.Fatal("两个状态撞成同一个键：那条成功的会被当成重复丢掉，这笔钱结算不了")
	}
	// 渠道换了通知号、状态与单号都没变——**仍然是同一条通知**，键必须一样。
	sameAgain := paymentCallback{MerOrderID: "PAY-1", Status: "TRADE_SUCCESS", NotifyID: "N-2"}
	if notificationID(sameAgain) != notificationID(success) {
		t.Fatal("键里混进了 notifyId：渠道换个通知号就能绕过唯一键")
	}

	// 两段都为空时键会退化成 "<空>:<状态>"。走到那里的通知状态一定不是成功（成功那条路
	// 要求 merOrderId 非空），所以撞键的后果只是少记几行 ignored。
	if got := notificationID(paymentCallback{}); got != ":" {
		t.Fatalf("空报文的键 = %q", got)
	}
}

// TestProviderTransactionIDFallsBackThroughThreeNames：三级回落是照抄老系统的 if/else 链，
// 说明真实报文里出现过只有其中某一个的情况。
//
// **最后一级是个已知的语义瑕疵**：notifyId 是通知号不是交易号，照抄它是因为不抄的代价是
// 「一个带 notifyId 的回调拿不到交易号」——而那个号在 V2 会被写进支付单，成为对账时唯一的
// 渠道侧锚点。
func TestProviderTransactionIDFallsBackThroughThreeNames(t *testing.T) {
	cases := []struct {
		name     string
		callback paymentCallback
		want     string
	}{
		{"三个都有", paymentCallback{TargetOrderID: "T-1", TransactionID: "TX-1", NotifyID: "N-1"}, "T-1"},
		{"没有 targetOrderId", paymentCallback{TransactionID: "TX-1", NotifyID: "N-1"}, "TX-1"},
		{"只有 notifyId", paymentCallback{NotifyID: "N-1"}, "N-1"},
		{"一个都没有", paymentCallback{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerTransactionID(tc.callback); got != tc.want {
				t.Fatalf("providerTransactionID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAmountFenAcceptsBothShapes：金额解码器单独测一遍。
//
// **返回 0 与返回 false 是两件事**：0 是一个渠道明确说了的金额，false 是「这个字段我们没读懂」。
// 把两者混起来会让「渠道没给金额」与「渠道给了 0 元」在库里长得一样。
func TestAmountFenAcceptsBothShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{"JSON 数字", `12800`, 12800, true},
		{"JSON 字符串", `"12800"`, 12800, true},
		{"带小数的字符串", `"12800.00"`, 12800, true},
		{"四舍五入到分", `"12800.005"`, 12800, true},
		{"零", `0`, 0, true},
		{"字符串零", `"0"`, 0, true},
		{"空串", `""`, 0, false},
		{"null", `null`, 0, false},
		{"字段不在", ``, 0, false},
		{"带单位的字符串", `"12800分"`, 0, false},
		{"对象", `{"a":1}`, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := amountFen([]byte(tc.raw))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("amountFen(%s) = (%d, %v), want (%d, %v)", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestParsePayTime：成交通知的四种形状，认不出时回零值。
//
// 零值表示「渠道没给或给的东西看不懂」，调用方看到零值会退回 NOW()。**不能拿 0 直接建
// time.Unix(0)**：那会写成 1970 年，一条 1970 年的流水会让对账报表永远对不平。
func TestParsePayTime(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"渠道的墙上时间", "2026-09-17 10:20:30", "2026-09-17 10:20:30"},
		{"紧凑格式", "20260917102030", "2026-09-17 10:20:30"},
		{"RFC3339", "2026-09-17T10:20:30+08:00", "2026-09-17 10:20:30"},
		{"空", "", ""},
		{"看不懂", "昨天下午", ""},
		// 8 位是日期不是时间戳：当成 Unix 秒会得到一个 1970 年的日子。
		{"一个八位数", "20260917", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePayTime(tc.raw)
			if tc.want == "" {
				if !got.IsZero() {
					t.Fatalf("parsePayTime(%q) = %v, want 零值", tc.raw, got)
				}
				return
			}
			if got.IsZero() {
				t.Fatalf("parsePayTime(%q) 回了零值", tc.raw)
			}
			if formatted := got.Format(requestTimestampLayout); formatted != tc.want {
				t.Fatalf("parsePayTime(%q) = %q, want %q", tc.raw, formatted, tc.want)
			}
		})
	}
}

// TestRecordableHeadersIsEmpty：这一族**没有**任何可留档的请求头。
//
// provider.HeaderRecorder 的约定是「留时间戳与随机串，**签名头不能留**」。在这一族里这三样
// 全都装在同一个 Authorization 头里（`OPEN-BODY-SIG AppId=…, Timestamp=…, Nonce=…,
// Signature=…`），没有任何一段能单独摘出来——摘 Timestamp 就等于连 Signature 一起留档，
// 而那份签名本身是一份「这条报文有有效签名」的凭证。
//
// 所以这里返回空名单，理由与 hmacbody 那边相反但结论同源：与其留一个包含签名的头，
// 不如一个都不留（渠道侧的时间戳在调用流水里找得到，够复盘用了）。
//
// 这一条与 provider.go 上那句「银联商务同理（Timestamp + Nonce）」是**冲突**的：那句话
// 的前提是各家都把时间戳与随机串放在独立的头里（微信 APIv3 的形状）。真按它做，这里就得
// 返回 Authorization，而那会把签名一起留进库。取更安全的那个。
func TestRecordableHeadersIsEmpty(t *testing.T) {
	names := New().RecordableHeaders()
	if len(names) != 0 {
		t.Fatalf("RecordableHeaders = %v，这一族的三段签名材料共用同一个头，摘不出干净的时间戳", names)
	}
}
