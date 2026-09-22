package ums

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// 这一份把 key 拼接签名的判据从「我们自己照规范复刻的一遍」换成**规范自己印出来的向量**。
//
// 为什么要换：keyappend_test.go 里那个写死的向量，是我们用自己的 Python 按同一套规则算出来的。
// 它钉住的是「排序、分隔符、密钥拼接方式、摘要种类与大小写都没被误改」，钉不住**规则本身读错**
// 这件事——规则读错的话，实现和那个向量会一起错，测试照样全绿。这一族恰恰最怕这个：入站签名是
// 渠道算、我们验，读错规则的后果是**一条真实收款通知都收不下**，而日志里只有「签名不对」。
//
// 向量出处：《全民付移动支付 H5支付》v20221020
//
//   - §1.9.4 示例报文 —— 一条 `bills.getQRCode` 的请求，规范把待签串、MD5 签名、SHA256 签名
//     三样都印全了，并给出了算它用的通讯密钥（示例值，非我们的）；
//   - §1.10 支付结果通知 —— 示例里给了一条**完整的通知 URL**，带 `sign` 与 `signType=SHA256`。
//
// 这两条覆盖的正是这一族的两条路：一条走 JSON 体（出站那种形状的待签串），一条走 URL 查询串、
// 值是百分号编码的（入站回跳那种形状）。第二条尤其值钱：它是**渠道真实的报文形状**，而不是
// 我们自己拼出来的。
//
// 仍未验到的只剩一件事：**我们账户那把通讯密钥**。算法这一层，从这一份起有厂商的向量背书。

// specExampleSecret 是规范 §1.9.3 印出来的示例通讯密钥。它是公开的示例值，不是任何真实账户的
// 凭据——写在这里不会漏任何东西，而它让这一份测试不需要任何环境变量就能跑。
const specExampleSecret = "fcAmtnx7MwismjWNhNKdHC44mNXtnEQeJkRrhKJwyrW2ysRR"

// TestKeyAppendSignatureMatchesTheSpecsPublishedVectors 是这一族最硬的一条：规范印出来的待签串
// 与签名，我们逐字算出来必须一样。
//
// 两条向量各钉一支摘要：MD5 与 SHA256。少钉一支的话，`keyAppendSignature` 里把两个分支接反
// （比如 MD5 那支写成 sha256.Sum256）也不会被发现。
func TestKeyAppendSignatureMatchesTheSpecsPublishedVectors(t *testing.T) {
	// 规范 §1.9.4。goods 的值是一个 JSON 串——里面的引号与方括号**不做转义**：规范说
	// 「值里有特殊字符要 URLEncode，但签名用原始值」，待签串用的是原始值。
	goods := `[{"body":"微信二维码测试","price":"1","goodsName":"微信二维码测试",` +
		`"goodsId":"1","quantity":"1","goodsCategory":"TEST"}]`
	params := map[string]string{
		"billDate":         "2017-06-26",
		"billNo":           "31940000201700002",
		"goods":            goods,
		"instMid":          "QRPAYDEFAULT",
		"mid":              "898340149000005",
		"msgSrc":           "WWW.TEST.COM",
		"msgType":          "bills.getQRCode",
		"requestTimestamp": "2017-06-26 17:28:02",
		"tid":              "88880001",
		"totalAmount":      "1",
		"walletOption":     "SINGLE",
	}

	const wantMD5 = "57f81baf8e3bae1190b26d6c733038af"
	const wantSHA256 = "a9eced8dd8425d1fc4047cf94e672c69ed1073557ee831c51287341cfab0b21f"

	if got := keyAppendSignature(params, specExampleSecret, signTypeMD5); got != wantMD5 {
		t.Fatalf("MD5 与规范印出来的不同：\n got %s\nwant %s", got, wantMD5)
	}
	if got := keyAppendSignature(params, specExampleSecret, signTypeSHA256); got != wantSHA256 {
		t.Fatalf("SHA256 与规范印出来的不同：\n got %s\nwant %s", got, wantSHA256)
	}

	// 待签串本身也钉一遍。只比摘要有一种漏法：实现与规范在**两处**各错一点、恰好在摘要层面
	// 抵消——不可能，但把原文钉出来能让上面那条失败时一眼看出错在哪一段（多一个 `&`？
	// 少一个参数？密钥前面多了分隔符？）。
	wantPayload := "billDate=2017-06-26&billNo=31940000201700002&goods=" + goods +
		"&instMid=QRPAYDEFAULT&mid=898340149000005&msgSrc=WWW.TEST.COM&msgType=bills.getQRCode" +
		"&requestTimestamp=2017-06-26 17:28:02&tid=88880001&totalAmount=1&walletOption=SINGLE"
	if got := keyAppendPayload(params, specExampleSecret); got != wantPayload+specExampleSecret {
		t.Fatalf("待签串与规范印出来的不同：\n got %s\nwant %s", got, wantPayload+specExampleSecret)
	}
}

// specNotificationURL 是规范 §1.10 支付结果通知给出的示例。
//
// 三个细节与真实报文一致，值得留意：`payTime` 与 `createTime` 里的空格在传输时是 `+`、
// 冒号是 `%3A`；中文（商户名、订单描述、资金渠道）全是百分号编码；`signType` 是参数之一，
// 照常参与排序与拼接（只有 `sign` 自己被摘掉）。
const specNotificationURL = "https://www.baidu.com/?payTime=2022-06-21+17%3A13%3A04&ns=cyuT" +
	"&connectSys=OPENCHANNEL" +
	"&sign=714DAF2ACAA090E79B80F843829ED4251652742DDC44BF596EEEA6732D154C62" +
	"&merName=%E6%B5%8B%E8%AF%95%E9%80%80%E8%B4%A75%281111%29&mid=898340149000005" +
	"&invoiceAmount=1&settleDate=2022-06-21" +
	"&billFunds=%E7%8E%B0%E9%87%91%E6%94%AF%E4%BB%980.01%E5%85%83%E3%80%82" +
	"&buyerId=o8wNP0RtDiUq4NzMZyAGK5psp6hs&mchntUuid=4aa8728a06a04f7385869df8b659cd01" +
	"&tid=88880001&instMid=YUEDANDEFAULT&receiptAmount=1" +
	"&targetOrderId=4200001471202206211141496123&signType=SHA256" +
	"&orderDesc=%E6%B5%8B%E8%AF%95%E9%80%80%E8%B4%A75%281111%29&seqId=01120706230N" +
	"&merOrderId=3194278460076848369664&targetSys=WXPay&totalAmount=1" +
	"&createTime=2022-06-21+17%3A12%3A53&buyerPayAmount=1" +
	"&notifyId=e82a0275-f767-4f77-90c2-614c91222275&subInst=000100&status=TRADE_SUCCESS"

// TestVerifyAcceptsTheSpecsPublishedNotification 把规范那条通知**原样喂给 Verify**。
//
// 它比上一条更接近真实：走的是完整的入站路径（取查询串 → 按内容解参数 → 挑 signType →
// 摘签名参数 → 排序拼串 → 附密钥 → 摘要 → 折大小写比较），而不是直接调那两个函数。上一条钉的
// 是算法，这一条钉的是**接线**——比如 inboundParams 事先替我们做过一次 URL 解码。
//
// 注意它验的是「渠道真实报文的形状」：值是编码过的、键里混着 `signType`、参数比我们文档里列的
// 多得多。这一族此前所有入站用例的参数集都是我们自己造的。
func TestVerifyAcceptsTheSpecsPublishedNotification(t *testing.T) {
	query, err := url.ParseQuery(strings.TrimPrefix(specNotificationURL, "https://www.baidu.com/?"))
	if err != nil {
		t.Fatalf("规范的示例 URL 解不开：%v", err)
	}

	config := withConfig(channelConfig("https://api-mop.chinaums.com"), "sign", map[string]any{
		"secretRef":     "appKey",
		"commSecretRef": "commKey",
	})

	notification, err := New().Verify(context.Background(), provider.NotificationRequest{
		ChannelCode: "ums",
		Headers:     http.Header{},
		HTTPMethod:  http.MethodGet,
		RequestPath: "/v1/payments/return/ums",
		Query:       query,
		Method:      channelMethod(config),
		// 密钥槽装的是规范印出来的那把示例密钥——这条测的是算法与接线，不是我们账户的凭据。
		Secrets: provider.Credentials{"appKey": testSecret, "commKey": specExampleSecret},
	})
	if err != nil {
		t.Fatalf("规范印出来的那条通知没被认下来：%v", err)
	}
	if notification.EventType != eventReturn {
		t.Fatalf("EventType = %q，want %q", notification.EventType, eventReturn)
	}
	if notification.PaymentNo != "3194278460076848369664" {
		t.Fatalf("PaymentNo = %q，want 渠道示例里的商户订单号", notification.PaymentNo)
	}

	// 改动其中**任意一个**参数都必须验不过——包括只动一个中文字段。这一条守的是「签名真的盖住了
	// 整份报文」：如果实现漏掉了某个参数（比如按文档列的固定字段名裁剪过），改它就不会被发现。
	for _, name := range []string{"totalAmount", "merName", "notifyId", "ns"} {
		t.Run("改了 "+name, func(t *testing.T) {
			damaged := url.Values{}
			maps.Copy(damaged, query)
			damaged.Set(name, "TAMPERED")

			_, err := New().Verify(context.Background(), provider.NotificationRequest{
				ChannelCode: "ums",
				Headers:     http.Header{},
				HTTPMethod:  http.MethodGet,
				RequestPath: "/v1/payments/return/ums",
				Query:       damaged,
				Method:      channelMethod(config),
				Secrets:     provider.Credentials{"appKey": testSecret, "commKey": specExampleSecret},
			})
			if err == nil {
				t.Fatalf("把 %s 改掉之后仍然验过了：签名没有盖住这个参数", name)
			}
		})
	}
}
