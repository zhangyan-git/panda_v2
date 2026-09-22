package wechatpay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 测试里那几个钉死的值，**全部是编的**。
//
// 尤其是商户号：它一度用的是老系统那一个（真值现在只在 .env 里，见 WECHAT_PAY_MCH_ID），
// 理由是「真值能让签名算错这件事在联调时对得上，而商户号公开在收银台上」。这条理由与
// 「跳转小程序的 appid 是微信侧的固定值」是同一条错误的推理——**取值只有一个**推不出
// **它可以进仓库**。它们都是能定位到具体主体的标识，进了仓库就是发给了每一个拿到仓库的人
// （GitHub 的密钥扫描按同一判据判了那个 appid）。
//
// 换成编的值不损失任何东西：这几条用例验的是签名口径（参数怎么排、空值怎么剔、MD5 怎么编），
// 与商户号是什么无关，也没有一条断言依赖真值。
const (
	testAppID                = "wxtestappid0000001"
	testMchID                = "1900000000"
	testSignMiniProgramAppID = "wxtestsignappid0001"
	testSecret               = "testkeytestkeytestkey12abcdef"
)

// mustProtocol 拼一份能跑的协议声明。overrides 覆盖默认值（例如把 baseURL 指向假服务端）。
func mustProtocol(t *testing.T, overrides map[string]any) *Protocol {
	t.Helper()
	config := map[string]any{
		"appId":                testAppID,
		"mchId":                testMchID,
		"signMiniProgramAppId": testSignMiniProgramAppID,
		"sign":                 map[string]any{"secretRef": "apiV2Key"},
	}
	for key, value := range overrides {
		config[key] = value
	}
	protocol, err := Parse(config)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return protocol
}

// TestSignParamsMatchesTheAPIv2Vector 是这一族最要紧的一条测试：签名口径。
//
// 期望值**不是这里算出来的**——它是按微信 APIv2 的规则（按 key 字典序、剔空值与 sign、末尾拼
// &key=<密钥>、MD5 后转大写）用另一份独立实现算出来的。同一个算法写两遍当然都对不上才算新闻，
// 所以要防的不是「我们算错了」而是「我们把口径改了一点」：这条会在改 spec 时立刻红。
//
// 参数集也是断言的一部分：**恰好 8 个字段 + sign**。多一个 nonce_str 微信会回 SIGN_ERROR
// （老系统那行注释），而那是这条链路上最难查的一种错——报文看上去完全正常。
//
// 期望值随夹具的 mch_id 换过一次（老系统那个真值 → 现在这个编的，理由见上面那组常量的注释）。
// 换的方法是**用另一份独立实现重算**，不是跑一遍这里的代码把输出抄回来：先在独立实现里
// 用旧 mch_id 复算出原来的摘要 EE92B5B3D8C0FC61E01E2DB945278593（对得上，说明那份实现与
// 这条向量记的是同一套口径），再换上新 mch_id 取值。抄输出会把这条测试变成同义反复。
func TestSignParamsMatchesTheAPIv2Vector(t *testing.T) {
	client := NewClient(mustProtocol(t, nil))

	got, err := client.SignParams(SignRequest{
		PlanID:                 "214488",
		ContractCode:           "TESTCONTRACT0001",
		ContractDisplayAccount: "oTestOpenId1234567890",
		NotifyURL:              "https://pay.example.com/v1/payments/agreement-notify/wechat_pay",
		Timestamp:              1700000000,
		RequestSerial:          "1700000000000000000",
	}, testSecret)
	if err != nil {
		t.Fatalf("SignParams: %v", err)
	}

	if got.Params["sign"] != "EF7FCC6212301D29DE4DAFC1C3B1897C" {
		t.Fatalf("sign = %q, want the APIv2 vector (params: %v)", got.Params["sign"], got.Params)
	}

	wantKeys := []string{
		"appid", "contract_code", "contract_display_account", "mch_id",
		"notify_url", "plan_id", "request_serial", "sign", "timestamp",
	}
	if len(got.Params) != len(wantKeys) {
		t.Fatalf("params = %v, want exactly %d keys", got.Params, len(wantKeys))
	}
	for _, key := range wantKeys {
		if _, ok := got.Params[key]; !ok {
			t.Fatalf("params is missing %q: %v", key, got.Params)
		}
	}
	for _, forbidden := range []string{"nonce_str", "version"} {
		if _, ok := got.Params[forbidden]; ok {
			t.Fatalf("pure sign must not carry %q (WeChat answers SIGN_ERROR): %v", forbidden, got.Params)
		}
	}
	if got.RequestSerial != "1700000000000000000" {
		t.Fatalf("request serial = %q, want it echoed back", got.RequestSerial)
	}
}

// TestSignParamsRefusesIncompleteInput 把两件「我们自己的错」分开钉住：密钥没配、套餐/协议号
// 没填。两者都不能变成一次「微信拒了」——那会让调用方去改协议状态，而问题在配置或调用方。
func TestSignParamsRefusesIncompleteInput(t *testing.T) {
	client := NewClient(mustProtocol(t, nil))

	if _, err := client.SignParams(SignRequest{
		PlanID: "214488", ContractCode: "TESTCONTRACT0001",
	}, ""); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("empty secret: err = %v, want ErrSecretNotConfigured", err)
	}

	if _, err := client.SignParams(SignRequest{PlanID: "214488"}, testSecret); !errors.Is(err, provider.ErrInvalidNotification) {
		t.Fatalf("missing contract_code: err = %v, want ErrInvalidNotification", err)
	}
}

// TestXMLRoundTrip 覆盖报文的两个形状事实：**空值也要有一个键**（微信对「键在、值为空」与
// 「没有这个键」是两种报文），以及值里的 "]]>" 会被拆开（CDATA 段里不能出现它）。
func TestXMLRoundTrip(t *testing.T) {
	params := map[string]string{
		"appid":       testAppID,
		"remark":      "",
		"des":         "a]]>b",
		"notify_url":  "https://pay.example.com/cb?x=1&y=2",
		"contract_id": "1234567890",
	}

	decoded, err := decodeXML(encodeXML(params))
	if err != nil {
		t.Fatalf("decodeXML: %v", err)
	}
	if len(decoded) != len(params) {
		t.Fatalf("decoded = %v, want the same %d keys", decoded, len(params))
	}
	for key, want := range params {
		if got := decoded[key]; got != want {
			t.Fatalf("decoded[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestDecodeXMLRefusesUnreadableBodies：读不懂的报文必须报错，绝不能解出半个空 map 让调用方
// 以为「渠道回了个空」——那条路上 verdict 会把空 return_code 当成失败，还算安全；但
// QueryContract 会因此拿到一个空的 contract_state，而它的处置是「读不懂」，两边行为要一致。
func TestDecodeXMLRefusesUnreadableBodies(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"truncated":     "<xml><return_code><![CDATA[SUCCESS]]>",
		"not xml":       "return_code=SUCCESS",
		"empty element": "<xml></xml>",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeXML([]byte(body)); err == nil {
				t.Fatalf("decodeXML(%q) = nil error, want a parse failure", body)
			}
		})
	}
}

// newFakeChannel 起一个假微信，把请求体记下来，按 respond 回一份报文。
func newFakeChannel(t *testing.T, respond func(map[string]string) string) (*Client, *[]map[string]string) {
	t.Helper()
	var received []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		params, err := decodeXML(raw)
		if err != nil {
			t.Errorf("the request body is not a wechat xml document: %v (%s)", err, raw)
			return
		}
		received = append(received, params)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(respond(params)))
	}))
	t.Cleanup(server.Close)
	return NewClient(mustProtocol(t, map[string]any{"baseURL": server.URL})), &received
}

// TestQueryContractAsksWithoutNonceAndMapsEveryAnswer 一次覆盖三个形状事实：查约那一套参数
// （**要 version、不要 nonce_str**）、成功应答里 contract_state 的映射，以及三类「等于没签」
// 的错误码。
//
// 三张应答放在一张表里而不是三个测试：它们共用同一份请求，分开写会把「请求长什么样」这件事
// 断言三遍，而它只该被钉一遍。
func TestQueryContractAsksWithoutNonceAndMapsEveryAnswer(t *testing.T) {
	tests := []struct {
		name      string
		response  string
		wantState ContractState
		wantID    string
		wantErr   bool
	}{
		{
			name: "signed",
			response: `<xml><return_code><![CDATA[SUCCESS]]></return_code>` +
				`<result_code><![CDATA[SUCCESS]]></result_code>` +
				`<contract_id><![CDATA[1234567890]]></contract_id>` +
				`<contract_state><![CDATA[0]]></contract_state></xml>`,
			wantState: ContractActive,
			wantID:    "1234567890",
		},
		{
			// 用户从没签过、已经解约、查无此约——微信对这三件事回的是业务失败而不是 state=1。
			name: "never signed",
			response: `<xml><return_code><![CDATA[SUCCESS]]></return_code>` +
				`<result_code><![CDATA[FAIL]]></result_code>` +
				`<err_code><![CDATA[NOTENROLLED]]></err_code></xml>`,
			wantState: ContractTerminated,
		},
		{
			// -25 RESULT NULL 是「还没结论」，不是解约：把它当成解约会把刚签完的用户标死。
			name: "pending",
			response: `<xml><return_code><![CDATA[SUCCESS]]></return_code>` +
				`<result_code><![CDATA[FAIL]]></result_code>` +
				`<err_code><![CDATA[-25]]></err_code>` +
				`<err_code_des><![CDATA[RESULT NULL]]></err_code_des></xml>`,
			wantState: ContractPending,
		},
		{
			name: "an error we do not understand",
			response: `<xml><return_code><![CDATA[SUCCESS]]></return_code>` +
				`<result_code><![CDATA[FAIL]]></result_code>` +
				`<err_code><![CDATA[SYSTEMERROR]]></err_code></xml>`,
			wantErr: true,
		},
		{
			name: "success without a state",
			response: `<xml><return_code><![CDATA[SUCCESS]]></return_code>` +
				`<result_code><![CDATA[SUCCESS]]></result_code></xml>`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, received := newFakeChannel(t, func(map[string]string) string { return tt.response })

			got, call, err := client.QueryContract(context.Background(), testSecret, ContractQuery{
				PlanID:       "214488",
				ContractCode: "TESTCONTRACT0001",
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.name == "an error we do not understand" {
				var business *BusinessError
				if !errors.As(err, &business) || business.ErrCode != "SYSTEMERROR" {
					t.Fatalf("err = %v, want a BusinessError carrying the channel's code", err)
				}
			}
			if err != nil {
				return
			}
			if got.State != tt.wantState || got.ProviderContractID != tt.wantID {
				t.Fatalf("result = %+v, want state %q and contract id %q", got, tt.wantState, tt.wantID)
			}

			sent := (*received)[0]
			if sent["version"] != "1.0" {
				t.Fatalf("version = %q, want 1.0 (querycontract carries it)", sent["version"])
			}
			if _, ok := sent["nonce_str"]; ok {
				t.Fatalf("querycontract must not carry nonce_str: %v", sent)
			}
			if sent["plan_id"] != "214488" || sent["contract_code"] != "TESTCONTRACT0001" {
				t.Fatalf("the correlation pair did not make it into the request: %v", sent)
			}
			if call.HTTPStatus != http.StatusOK {
				t.Fatalf("HTTP status = %d, want 200", call.HTTPStatus)
			}
		})
	}
}

// TestPreNotifyCarriesNoCallbackAddress 钉住预扣费通知那一套参数：它**没有 notify_url、也没有
// spbill_create_ip**（那条通知是单向告知，没有结果要回给我们），而且它不需要商户证书——这条
// 走的就是那条不带证书的路，所以假服务端能把它整条跑通。
func TestPreNotifyCarriesNoCallbackAddress(t *testing.T) {
	client, received := newFakeChannel(t, func(map[string]string) string {
		return `<xml><return_code><![CDATA[SUCCESS]]></return_code>` +
			`<result_code><![CDATA[SUCCESS]]></result_code></xml>`
	})

	if _, err := client.PreNotify(context.Background(), testSecret, PreNotifyRequest{
		ProviderContractID: "1234567890",
		OutTradeNo:         "MEMBERSHIP2026090001",
		Body:               "会员续费",
		TotalFee:           990,
		DeductDate:         "20260922",
	}); err != nil {
		t.Fatalf("PreNotify: %v", err)
	}

	sent := (*received)[0]
	if sent["deduct_date"] != "20260922" {
		t.Fatalf("deduct_date = %q, want the yyyyMMdd we passed", sent["deduct_date"])
	}
	if sent["nonce_str"] == "" {
		t.Fatalf("nonce_str is required on this endpoint: %v", sent)
	}
	for _, forbidden := range []string{"notify_url", "spbill_create_ip"} {
		if _, ok := sent[forbidden]; ok {
			t.Fatalf("pap_pay_apply must not carry %q: %v", forbidden, sent)
		}
	}
}

// TestChargingRefusesWithoutTheMerchantCertificate 钉住 fail closed：证书没配时**一个字节都
// 不发出去**。
//
// 这条比它看上去重要：降级成不带证书发出去的话，微信可能受理、也可能回一句 401，而那笔钱
// 到底扣没扣我们再也说不清——这正是「不配就不发」要防的那件事。
func TestChargingRefusesWithoutTheMerchantCertificate(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { attempts++ }))
	t.Cleanup(server.Close)
	client := NewClient(mustProtocol(t, map[string]any{"baseURL": server.URL}))

	_, _, err := client.Charge(context.Background(), testSecret, ChargeRequest{
		ProviderContractID: "1234567890",
		OutTradeNo:         "MEMBERSHIP2026090001",
		Body:               "会员续费",
		TotalFee:           990,
	})
	if !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("Charge without a certificate: err = %v, want ErrSecretNotConfigured", err)
	}

	if _, err := client.TerminateContract(context.Background(), testSecret, TerminateContract{
		ProviderContractID: "1234567890",
	}); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("TerminateContract without a certificate: err = %v, want ErrSecretNotConfigured", err)
	}
	if attempts != 0 {
		t.Fatalf("%d requests reached the channel, want none", attempts)
	}
}

// TestCertificateLoadingIsCachedOnSuccessOnly 钉住证书那一层缓存的两面：装好一次之后复用
// （每次代扣重开一个连接池是浪费），而**加载失败不进缓存**——运维刚把证书拷进去时必须立刻
// 生效，不该被启动时的一次失败钉死。
func TestCertificateLoadingIsCachedOnSuccessOnly(t *testing.T) {
	certs := &certClients{}

	if _, err := certs.get(mustProtocol(t, nil)); !errors.Is(err, provider.ErrSecretNotConfigured) {
		t.Fatalf("no certificate configured: err = %v, want ErrSecretNotConfigured", err)
	}

	missing := mustProtocol(t, map[string]any{
		"certFile": filepath.Join(t.TempDir(), "apiclient_cert.pem"),
		"keyFile":  filepath.Join(t.TempDir(), "apiclient_key.pem"),
	})
	if _, err := certs.get(missing); err == nil {
		t.Fatal("a certificate path that does not exist loaded successfully")
	}
	if len(certs.clients) != 0 {
		t.Fatalf("a failed load was cached: %v", certs.clients)
	}

	// 证书就位之后同一条路径必须能装上——这就是「失败不缓存」要保证的那件事。
	certFile, keyFile := writeTestCertificate(t, filepath.Dir(missing.certFile))
	fixed := mustProtocol(t, map[string]any{"certFile": certFile, "keyFile": keyFile})
	first, err := certs.get(fixed)
	if err != nil {
		t.Fatalf("load the certificate that is now in place: %v", err)
	}
	second, err := certs.get(fixed)
	if err != nil {
		t.Fatalf("read the cached client: %v", err)
	}
	if first != second {
		t.Fatal("the certificate was loaded twice, want the client cached")
	}
}

// writeTestCertificate 当场生成一对自签证书并落盘，返回两个路径。
//
// 为什么生成而不是往 testdata 里放一份固定 PEM：一对**真的**商户私钥进仓库，哪怕它是假的，
// 也会让密钥扫描器（和读代码的人）在一堆噪声里多花一次判断。生成它多二十行，但那二十行里
// 没有一行是秘密。
func writeTestCertificate(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "panda-test-merchant"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the certificate: %v", err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}

	certFile := filepath.Join(dir, "apiclient_cert.pem")
	keyFile := filepath.Join(dir, "apiclient_key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", encodedKey)
	return certFile, keyFile
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
