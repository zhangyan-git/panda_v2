package wechatpay

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// notifyBody 拼一份签好名的协议变更通知。
//
// 签名走的是**出站那条路用的同一个函数**（preSign → signParams），不是这里重写一遍的算式：
// 这条用例要答的是「我们认得自己签出去的报文吗」，自己再实现一遍签名只会验出「两次实现一样」，
// 而真正会出的事是「读的那一半口径与写的那一半不一致」——那正是共用同一个函数能挡住的。
//
// secret 为空时不签名（造「密钥没配」那一条）；afterSign 在签名算完之后改字段，用来造
// 「报文在路上被改过」——签名真正要挡的就是这件事。
func notifyBody(t *testing.T, params map[string]string, secret string, afterSign func(map[string]string)) []byte {
	t.Helper()
	signed := make(map[string]string, len(params)+1)
	maps.Copy(signed, params)
	// 空格串与空串一样是「没有密钥」（signParams 先 TrimSpace），那时签不了名——用例要造的
	// 正是「密钥没配时收到一条带签名的报文」，所以这里跳过签名而不是让辅助函数报错。
	if strings.TrimSpace(secret) != "" {
		if err := preSign(signed, secret); err != nil {
			t.Fatalf("preSign: %v", err)
		}
	}
	if afterSign != nil {
		afterSign(signed)
	}
	return encodeXML(signed)
}

// addNotify 是一份**签约成功**通知的字段（微信的 ADD 报文）。
func addNotify() map[string]string {
	return map[string]string{
		"return_code":    "SUCCESS",
		"result_code":    "SUCCESS",
		"change_type":    changeTypeAdd,
		"contract_code":  "AG2026090001",
		"contract_id":    "1234567890",
		"contract_state": string(ContractActive),
	}
}

// deleteNotify 是一份**解约**通知的字段。注意它**没有 contract_code**——微信的解约报文里只有
// contract_id，这是这条路上最要紧的一个形状事实（按协议号找回那份协议对它是走不通的）。
func deleteNotify() map[string]string {
	return map[string]string{
		"return_code": "SUCCESS",
		"result_code": "SUCCESS",
		"change_type": changeTypeDelete,
		"contract_id": "1234567890",
	}
}

// TestParseAgreementNotifyMapsBothChangeTypes 钉住「报文里的两个字翻成我们的两个状态」。
//
// 两条都在这一张表里，是因为它们的**必填字段不同**：ADD 要有 contract_code（按它找回协议），
// DELETE 没有。分开写会把「两个状态怎么翻」验两遍，而两处真正的差别是字段那一列。
func TestParseAgreementNotifyMapsBothChangeTypes(t *testing.T) {
	cases := []struct {
		name             string
		params           map[string]string
		wantState        provider.AgreementState
		wantContractCode string
		wantContractID   string
		wantProviderWord string
	}{
		{
			name: "签约成功", params: addNotify(),
			wantState: provider.AgreementSigned,
			// contract_code 是我们自己的协议号，contract_id 是微信侧的——
			// 扣款与解约用的是后者，两者都要原样带出去（见 provider.AgreementNotification）。
			wantContractCode: "AG2026090001", wantContractID: "1234567890",
			wantProviderWord: string(ContractActive),
		},
		{
			name: "解约", params: deleteNotify(),
			wantState: provider.AgreementTerminated,
			// 报文没写 contract_code：**留空而不是猜一个**。调用方必须能按 contract_id 找协议。
			wantContractCode: "", wantContractID: "1234567890",
			// 解约通知更是不带 contract_state——「报文没写」与「写的是 0」是两件事，
			// 所以这里要的是空串，而不是 ContractTerminated。
			wantProviderWord: "",
		},
		{
			name: "解约但带了协议号", params: func() map[string]string {
				params := deleteNotify()
				params["contract_code"] = "AG2026090001"
				return params
			}(),
			wantState:        provider.AgreementTerminated,
			wantContractCode: "AG2026090001", wantContractID: "1234567890",
			wantProviderWord: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notify, _, err := ParseAgreementNotify(notifyBody(t, tc.params, testSecret, nil), testSecret)
			if err != nil {
				t.Fatalf("ParseAgreementNotify: %v", err)
			}
			if got := notify.State(); got != tc.wantState {
				t.Errorf("State() = %q，想要 %q", got, tc.wantState)
			}
			if notify.ContractCode != tc.wantContractCode {
				t.Errorf("ContractCode = %q，想要 %q", notify.ContractCode, tc.wantContractCode)
			}
			if notify.ProviderContractID != tc.wantContractID {
				t.Errorf("ProviderContractID = %q，想要 %q", notify.ProviderContractID, tc.wantContractID)
			}
			if got := string(notify.ProviderState); got != tc.wantProviderWord {
				t.Errorf("ProviderState = %q，想要 %q（渠道的原话，没有就是空）", got, tc.wantProviderWord)
			}
		})
	}
}

// TestParseAgreementNotifyRefusesBeforeReadingAnyField 把三类拒绝分开钉住，并守住它们的**顺序**。
//
// 顺序是这条用例的重点：验签必须在读任何字段之前（报文里的 contract_code 是不可信输入，拿它去
// 查库等于让攻击者决定我们去看哪一行）。所以签名这一类失败的期望是「notify 一个字段都没填」，
// 而不只是「报了个错」——先填后验的写法能通过后一个断言，通不过前一个。
//
// 三类分别是「报文不是我们签的」「报文是我们签的但内容不对」「报文对但这条路上没有可做的事」，
// 排查方向完全不同，所以各自要一个能 errors.Is 的错值。
func TestParseAgreementNotifyRefusesBeforeReadingAnyField(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]string
		secret  string
		mutate  func(map[string]string)
		wantErr error
		// wantBlank 表示「报错时 notify 一个字段都没填」——只有签名那几种失败该是 true。
		wantBlank bool
	}{
		{
			name: "签名不对", params: addNotify(),
			mutate:    func(p map[string]string) { p["sign"] = "00000000000000000000000000000000" },
			wantErr:   provider.ErrSignatureMismatch,
			wantBlank: true,
		},
		{
			// 报文在路上被改过：签名还是那一份正确的签名，盖不住改后的内容。这是签名唯一
			// 真正要挡的场景（改的正是 contract_code——被改对了方向就是「把别人的协议记到我们头上」）。
			name: "报文被改过", params: addNotify(),
			mutate:    func(p map[string]string) { p["contract_code"] = "AG2026019999" },
			wantErr:   provider.ErrSignatureMismatch,
			wantBlank: true,
		},
		{
			name: "报文没有签名", params: addNotify(),
			mutate:    func(p map[string]string) { delete(p, "sign") },
			wantErr:   provider.ErrSignatureMismatch,
			wantBlank: true,
		},
		{
			// 密钥没配：**不能降级成空签名**再去比，否则运维看到的是一句「验签失败」，
			// 排查方向（配置）与实际原因完全对不上。
			name: "密钥没配", params: addNotify(), secret: " ",
			wantErr:   provider.ErrSecretNotConfigured,
			wantBlank: true,
		},
		{
			name: "签约通知缺协议号", params: func() map[string]string {
				params := addNotify()
				delete(params, "contract_code")
				return params
			}(),
			wantErr: provider.ErrInvalidNotification,
		},
		{
			name: "签约通知缺渠道协议号", params: func() map[string]string {
				params := addNotify()
				delete(params, "contract_id")
				return params
			}(),
			wantErr: provider.ErrInvalidNotification,
		},
		{
			// 解约通知没有 contract_code 是正常的（见 deleteNotify），没有 contract_id 不是：
			// 那条报文里两个都没有，我们没有任何东西能定位到一份协议。
			name: "解约通知缺渠道协议号", params: func() map[string]string {
				params := deleteNotify()
				delete(params, "contract_id")
				return params
			}(),
			wantErr: provider.ErrInvalidNotification,
		},
		{
			// 微信自己在通知里说了这次操作没成。签名是真的，但这不是一份「协议变了」的通知。
			name: "微信说这次没成", params: func() map[string]string {
				params := addNotify()
				params["return_code"] = "FAIL"
				params["return_msg"] = "SYSTEMERROR"
				return params
			}(),
			wantErr: provider.ErrNotificationNotActionable,
		},
		{
			// 没有 change_type = 支付中签约的付款结果通知（下单与签约合成一次请求那个入口的
			// 回传）。它说的是钱不是协议，本刀不做——**回失败应答而不是静默 ack**：
			// 静默 ack 的前提是「我们知道它是什么」，而这里不是。
			name: "没有变更类型（支付中签约的付款结果）", params: func() map[string]string {
				params := addNotify()
				delete(params, "change_type")
				return params
			}(),
			wantErr: provider.ErrNotificationNotActionable,
		},
		{
			name: "不认识的变更类型", params: func() map[string]string {
				params := addNotify()
				params["change_type"] = "UPDATE"
				return params
			}(),
			wantErr: provider.ErrNotificationNotActionable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secret := tc.secret
			if secret == "" {
				secret = testSecret
			}
			notify, _, err := ParseAgreementNotify(notifyBody(t, tc.params, secret, tc.mutate), secret)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
			}
			if !tc.wantBlank {
				return
			}
			if notify != (AgreementNotify{}) {
				t.Fatalf("验签没过却已经读了字段：%+v", notify)
			}
		})
	}
}

// chargeNotifyParams 是一份**扣款成功**通知的字段（微信的 pay/pappayapply 回调）。
func chargeNotifyParams() map[string]string {
	return map[string]string{
		"return_code":    "SUCCESS",
		"result_code":    "SUCCESS",
		"out_trade_no":   "CHG202609141200000012345678",
		"transaction_id": "4200001234202609140000000001",
		"contract_id":    "1234567890",
		"total_fee":      "1990",
	}
}

// TestParseChargeNotifyMapsBothResults 钉住这条路上唯一要翻的东西：**钱到了没有**。
//
// 两层码的分工是这条用例的重点：return_code 是「这条通知送成没有」（不是结论），result_code
// 才是「这一期扣到没有」（是结论）。老系统正是在这里栽的——它把同步应答的 result_code=SUCCESS
// 当成了扣款成功，于是在余额不足的风控单上照样给用户续了会员。
func TestParseChargeNotifyMapsBothResults(t *testing.T) {
	cases := []struct {
		name        string
		params      map[string]string
		wantResult  provider.Result
		wantTxnID   string
		wantAmount  int64
		wantFailure string
	}{
		{
			name: "扣到了", params: chargeNotifyParams(),
			wantResult: provider.ResultSuccess,
			// 它是用户在微信账单里看到的那个号，也是通知重投时对账的旁证。
			wantTxnID: "4200001234202609140000000001", wantAmount: 1990,
		},
		{
			name: "没扣到", params: func() map[string]string {
				params := chargeNotifyParams()
				params["result_code"] = "FAIL"
				params["err_code"] = "NOTENOUGH"
				params["err_code_des"] = "余额不足"
				// 渠道拒一笔时 transaction_id 是**空的**：钱没动过，自然没有单号。
				delete(params, "transaction_id")
				return params
			}(),
			wantResult: provider.ResultFailed, wantAmount: 1990, wantFailure: "NOTENOUGH",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notify, _, err := ParseChargeNotify(notifyBody(t, tc.params, testSecret, nil), testSecret)
			if err != nil {
				t.Fatalf("ParseChargeNotify: %v", err)
			}
			if notify.Result != tc.wantResult {
				t.Errorf("Result = %q，想要 %q", notify.Result, tc.wantResult)
			}
			if notify.OutTradeNo != "CHG202609141200000012345678" {
				t.Errorf("OutTradeNo = %q，想要报文里那个（它是定位这一期唯一的键）", notify.OutTradeNo)
			}
			if notify.ProviderTransactionID != tc.wantTxnID {
				t.Errorf("ProviderTransactionID = %q，想要 %q", notify.ProviderTransactionID, tc.wantTxnID)
			}
			if notify.Amount != tc.wantAmount {
				t.Errorf("Amount = %d，想要 %d（调用方要拿它跟本地那一期对账）", notify.Amount, tc.wantAmount)
			}
			if notify.FailureCode != tc.wantFailure {
				t.Errorf("FailureCode = %q，想要 %q", notify.FailureCode, tc.wantFailure)
			}
		})
	}
}

// TestParseChargeNotifyRefusesBeforeReadingAnyField 与协议通知那条同一个形状（顺序同样是重点）。
//
// 除了「验签在读字段之前」，这一条还多钉两处**本路特有**的拒绝：
//
//   - 没有 out_trade_no：报文里没有协议号、没有期次，别的字段都定位不到那一期，留着它等于留
//     一条什么都做不了的通知；
//   - total_fee 读不成数：**不按 0 处理**。按 0 走下去会以一次「金额对不上」收场，那句话说的是
//     「渠道报了一个不同的数」，而事实是我们根本没读懂它——两种排查方向完全不同。
func TestParseChargeNotifyRefusesBeforeReadingAnyField(t *testing.T) {
	cases := []struct {
		name      string
		params    map[string]string
		secret    string
		mutate    func(map[string]string)
		wantErr   error
		wantBlank bool
	}{
		{
			name: "签名不对", params: chargeNotifyParams(),
			mutate:    func(p map[string]string) { p["total_fee"] = "1" },
			wantErr:   provider.ErrSignatureMismatch,
			wantBlank: true,
		},
		{
			name: "报文没有签名", params: chargeNotifyParams(),
			mutate:    func(p map[string]string) { delete(p, "sign") },
			wantErr:   provider.ErrSignatureMismatch,
			wantBlank: true,
		},
		{
			name: "密钥没配", params: chargeNotifyParams(), secret: " ",
			wantErr:   provider.ErrSecretNotConfigured,
			wantBlank: true,
		},
		{
			name: "没有商户单号", params: func() map[string]string {
				params := chargeNotifyParams()
				delete(params, "out_trade_no")
				return params
			}(),
			wantErr: provider.ErrInvalidNotification,
		},
		{
			name: "金额读不成数", params: func() map[string]string {
				params := chargeNotifyParams()
				params["total_fee"] = "19.9"
				return params
			}(),
			wantErr: provider.ErrInvalidNotification,
		},
		{
			// 微信自己在通知里说了这次通知没送成：签名是真的，但它不是一句结论。
			name: "微信说这条通知没成", params: func() map[string]string {
				params := chargeNotifyParams()
				params["return_code"] = "FAIL"
				params["return_msg"] = "SYSTEMERROR"
				return params
			}(),
			wantErr: provider.ErrNotificationNotActionable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secret := tc.secret
			if secret == "" {
				secret = testSecret
			}
			notify, _, err := ParseChargeNotify(notifyBody(t, tc.params, secret, tc.mutate), secret)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，想要 %v", err, tc.wantErr)
			}
			if !tc.wantBlank {
				return
			}
			if notify != (ChargeNotify{}) {
				t.Fatalf("验签没过却已经读了字段：%+v", notify)
			}
		})
	}
}

// TestProviderVerifyAgreementChargeNotification 与协议那条同构：解析与验签在 ParseChargeNotify
// 里，这一层只该做「按协议声明的槽取密钥」。
//
// 摘要那一条同样值钱：total_fee **要在白名单里**——它是调用方拿去与本地那一期对账的数，摘要里
// 缺了它，排查的人就只能回到报文原文里去找。
func TestProviderVerifyAgreementChargeNotification(t *testing.T) {
	body := notifyBody(t, chargeNotifyParams(), testSecret, nil)

	got, err := New().VerifyAgreementChargeNotification(context.Background(), provider.NotificationRequest{
		ChannelCode: "wechat_pay",
		Body:        body,
		Method:      testMethod(nil),
		Secrets:     testSecrets(),
	})
	if err != nil {
		t.Fatalf("VerifyAgreementChargeNotification: %v", err)
	}
	if got.Result != provider.ResultSuccess || got.Amount != 1990 {
		t.Fatalf("结果 = %q / 金额 = %d，想要报文里的那两个", got.Result, got.Amount)
	}
	if got.OutTradeNo != "CHG202609141200000012345678" || got.ProviderContractID != "1234567890" {
		t.Fatalf("单号 = %q / 协议号 = %q，想要报文里的那两个", got.OutTradeNo, got.ProviderContractID)
	}
	if got.ResponseSummary["total_fee"] != "1990" {
		t.Fatalf("摘要 = %v，想要带上实付金额（对账时要看的就是它）", got.ResponseSummary)
	}
	if _, leaked := got.ResponseSummary["sign"]; leaked {
		t.Fatalf("摘要里带了签名：%v", got.ResponseSummary)
	}

	// 取不到密钥就地拒绝：与另外两条路同一条规矩，绝不拿空密钥去验一份报文。
	if _, err := New().VerifyAgreementChargeNotification(context.Background(), provider.NotificationRequest{
		ChannelCode: "wechat_pay", Body: body, Method: testMethod(nil),
		Secrets: provider.Credentials{},
	}); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("没有密钥时 err = %v，想要 ErrSecretNotConfigured", err)
	}
}

// TestProviderVerifyAgreementNotification 守适配器那一层多做的那一件事：**把凭据取出来**。
//
// 报文解析与验签在 ParseAgreementNotify 里（上面两条用例管着），这一层只该做「按协议声明的槽
// 取密钥」，而它错了的表现是「所有通知都验不过」——那时没人会想到是这一行取错了槽。
//
// 摘要那一条同样值钱：报文原文只允许进 payment_notifications.body（方案 11.5），摘要走白名单，
// 签名原件不许跟着进流水。
func TestProviderVerifyAgreementNotification(t *testing.T) {
	body := notifyBody(t, addNotify(), testSecret, nil)

	got, err := New().VerifyAgreementNotification(context.Background(), provider.NotificationRequest{
		ChannelCode: "wechat_pay",
		Body:        body,
		Method:      testMethod(nil),
		Secrets:     testSecrets(),
	})
	if err != nil {
		t.Fatalf("VerifyAgreementNotification: %v", err)
	}
	if got.AgreementNo != "AG2026090001" || got.ContractNo != "1234567890" {
		t.Fatalf("协议号 = %q / %q，想要报文里的那两个", got.AgreementNo, got.ContractNo)
	}
	if got.State != provider.AgreementSigned {
		t.Fatalf("State = %q，想要 %q", got.State, provider.AgreementSigned)
	}
	if got.ProviderState != string(ContractActive) {
		t.Fatalf("ProviderState = %q，想要渠道的原话 %q", got.ProviderState, ContractActive)
	}
	// 摘要是**原样**的那几个白名单键（微信的字段名，不做驼峰转换）：换成 contractCode 之
	// 类的写法就等于在流水里另起一套词表，而排查的人手上只有渠道报文。
	if got.ResponseSummary["contract_code"] != "AG2026090001" {
		t.Fatalf("摘要 = %v，想要能定位到这份协议的字段", got.ResponseSummary)
	}
	if _, leaked := got.ResponseSummary["sign"]; leaked {
		t.Fatalf("摘要里带了签名：%v", got.ResponseSummary)
	}

	// 取不到密钥就地拒绝：与 Sign / QueryAgreement 同一条规矩，绝不拿空密钥去验一份报文。
	if _, err := New().VerifyAgreementNotification(context.Background(), provider.NotificationRequest{
		ChannelCode: "wechat_pay", Body: body, Method: testMethod(nil),
		Secrets: provider.Credentials{},
	}); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("没有密钥时 err = %v，想要 ErrSecretNotConfigured", err)
	}
}
