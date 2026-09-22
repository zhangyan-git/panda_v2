package wechatpay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 编译期把契约钉住：少一个方法、或者签名改了，这里就编不过。
//
// 这一条比它看上去值钱：AgreementKeeper 是**可选接口**，探不到它的后果是运行期的一句
// 「这个渠道不支持签约」（见 provider 包注释里那段取舍），而那要到用户点「开通连续包月」
// 的时候才暴露。放在这里，它在编译时就暴露。
var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.SecretSlotter   = (*Provider)(nil)
	_ provider.HeaderRecorder  = (*Provider)(nil)
	_ provider.AgreementKeeper = (*Provider)(nil)
)

// testMethod 拼一条与 catalog 里那条 channel 同形的 Method。
//
// 它抄的是 catalog.wechatPayChannel 拼出来的那棵树（字段名与槽名都要对得上，否则适配器
// 在测试里能跑、在真部署里 Parse 就失败）。
func testMethod(overrides map[string]any) provider.Method {
	config := provider.Config{
		"appId":                testAppID,
		"mchId":                testMchID,
		"signMiniProgramAppId": testSignMiniProgramAppID,
		"sign":                 map[string]any{"secretRef": "apiV2Key"},
	}
	for key, value := range overrides {
		config[key] = value
	}
	return provider.Method{
		Code:          "wechat_papay",
		Action:        provider.ActionJumpMiniapp,
		ChannelCode:   "wechat_pay",
		Provider:      Name,
		ChannelConfig: config,
	}
}

func testSecrets() provider.Credentials {
	return provider.Credentials{"apiV2Key": testSecret}
}

// TestSignHandsTheClientEverythingItNeedsToJump 钉住适配器交给客户端的那张表：11 个键
// ——协议那 8 个（含 request_serial）+ sign + 跳转目标的两个值。
//
// 跳转目标**必须**在这里：客户端只 switch Action、不 switch 渠道（见 catalog.Method.Action），
// 所以「跳去哪个小程序」这件事只能由适配器带出来。少一个键，客户端就跳不去。
func TestSignHandsTheClientEverythingItNeedsToJump(t *testing.T) {
	result, err := New().Sign(context.Background(), provider.AgreementSignRequest{
		AgreementNo:  "AG2026090001",
		ContractCode: "TESTCONTRACT0001",
		PlanID:       "214488",
		UserID:       "u_1",
		WalletOpenID: "oTestOpenId1234567890",
		NotifyURL:    "https://pay.example.com/v1/payments/agreement-notify/wechat_pay",
		Method:       testMethod(nil),
		Secrets:      testSecrets(),
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if result.ContractCode != "TESTCONTRACT0001" {
		t.Fatalf("contract code = %q, want it echoed back", result.ContractCode)
	}
	if len(result.PayParams) != 11 {
		t.Fatalf("pay params = %v, want 11 keys", result.PayParams)
	}
	if result.PayParams["mini_program_appid"] != testSignMiniProgramAppID {
		t.Fatalf("mini_program_appid = %q, want the configured signing miniapp %q",
			result.PayParams["mini_program_appid"], testSignMiniProgramAppID)
	}
	if result.PayParams["mini_program_path"] != SignMiniProgramPath {
		t.Fatalf("mini_program_path = %q", result.PayParams["mini_program_path"])
	}
	if result.PayParams["sign"] == "" || result.PayParams["request_serial"] == "" {
		t.Fatalf("the client cannot sign the agreement with these params: %v", result.PayParams)
	}
	// 摘要是给流水看的：它能定位到这份协议，但**不含签名**（签名的原件与结果都不进流水）。
	if result.ResponseSummary["contractCode"] != "TESTCONTRACT0001" {
		t.Fatalf("summary = %v, want the correlation values", result.ResponseSummary)
	}
	if _, leaked := result.ResponseSummary["sign"]; leaked {
		t.Fatalf("the summary carries the signature: %v", result.ResponseSummary)
	}
}

// TestSignRefusesWithoutASecret：取不到密钥就拒绝，绝不降级成空签名——空签名在微信那边是
// 一句 SIGN_ERROR，看上去像我们的算法写错了。
func TestSignRefusesWithoutASecret(t *testing.T) {
	_, err := New().Sign(context.Background(), provider.AgreementSignRequest{
		ContractCode: "TESTCONTRACT0001",
		PlanID:       "214488",
		Method:       testMethod(nil),
		Secrets:      provider.Credentials{},
	})
	if !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("err = %v, want ErrSecretNotConfigured", err)
	}
}

// TestSecretSlotsReportTheOneKeyWeHave：这一族只有一把凭据（签名与验签共用），而**证书不在
// 槽里**——槽是给「一把字符串密钥」的，证书是文件路径。
func TestSecretSlotsReportTheOneKeyWeHave(t *testing.T) {
	slots := New().SecretSlots(testMethod(nil))
	if len(slots) != 1 || slots[0] != "apiV2Key" {
		t.Fatalf("slots = %v, want exactly the APIv2 key", slots)
	}

	// 配置坏掉时返回空切片（没有错误可返回），装配处会退回渠道行的 secret_ref；
	// 真正的报错在紧随其后的 Sign 里。
	if slots := New().SecretSlots(provider.Method{ChannelConfig: provider.Config{}}); len(slots) != 0 {
		t.Fatalf("slots = %v, want none for an unparsable config", slots)
	}
}

// TestAckFollowsTheXMLConvention 钉住应答的形状：微信只认 return_code 的那两个词，而**失败
// 也要回 200**（那是它的重推信号），并且应答体里不写失败原因。
func TestAckFollowsTheXMLConvention(t *testing.T) {
	accepted := New().Ack(testMethod(nil), true)
	if accepted.Status != 200 || !strings.Contains(string(accepted.Body), "<return_code>SUCCESS</return_code>") {
		t.Fatalf("accepted ack = %+v", accepted)
	}

	rejected := New().Ack(testMethod(nil), false)
	if rejected.Status != 200 || !strings.Contains(string(rejected.Body), "<return_code>FAIL</return_code>") {
		t.Fatalf("rejected ack = %+v", rejected)
	}
	if strings.Contains(string(rejected.Body), "return_msg") {
		t.Fatalf("the ack leaks an internal reason: %s", rejected.Body)
	}
}

// TestThisChannelRefusesToBeAPaymentChannel 钉住那两条边界声明。
//
// 它们不是在防「有人写错代码」，是在防「有人拿 catalog 里的 wechat_papay 去发起支付」——
// 那时他要得到一句「这条通道不做收款」，而不是一张永远等不到回调的支付单。
func TestThisChannelRefusesToBeAPaymentChannel(t *testing.T) {
	method := testMethod(nil)

	result, err := New().Create(context.Background(), provider.CreateRequest{
		PaymentNo: "P2026090001",
		Method:    method,
		Secrets:   testSecrets(),
	})
	if !errors.Is(err, ErrNotAPaymentChannel) {
		t.Fatalf("Create: err = %v, want ErrNotAPaymentChannel", err)
	}
	if result.Result != provider.ResultFailed {
		t.Fatalf("Create result = %q, want failed (契约要求 err 非 nil 时也要填 Result)", result.Result)
	}

	if _, err := New().Verify(context.Background(), provider.NotificationRequest{
		ChannelCode: "wechat_pay",
	}); !errors.Is(err, ErrNotAPaymentNotification) {
		t.Fatalf("Verify: err = %v, want ErrNotAPaymentNotification", err)
	}
}
