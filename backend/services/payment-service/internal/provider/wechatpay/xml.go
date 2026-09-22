package wechatpay

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/signing"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/provider"
)

// signSpec 是微信 APIv2 的签名规则，逐条对齐老系统的 generateSign
// （wechat_entrust_service.go:766）：
//
//	按 key 字典序 → 跳过空值与 sign → "k=v" 用 "&" 连 → 末尾 "&key=<密钥>" → MD5 大写
//
// 四个旋钮里的每一个都对应老系统代码里的一行，且**都与别的渠道不同**——尤其是
// KeyName="key"（微信用 key，丰选万联用 Key）与 MD5Upper（首创饭卡用大写、丰选万联用小写）。
//
// **不做 URL 编码**：老系统那四份实现里一处 url.QueryEscape 都没有，而 CANONICAL 这一层
// 本来也不编码（见 signing.Canonical.Build）。待签串是一串原样的 k=v。
var signSpec = signing.Spec{
	Canonical: signing.Canonical{
		SkipEmpty: true,
		Sort:      true,
		// 大小写敏感，且只剔 "sign"：微信 APIv2 的字段名是小写下划线，没有第二个写法。
		Omit: []string{"sign"},
		// 拼进串的密钥键名。空则密钥不参与拼串（HMAC 类才是那样），这一族必须拼。
		KeyName: "key",
	},
	Algorithm: signing.MD5Upper,
}

// maxXMLDepth 是解析回调报文时允许的最大嵌套深度。
//
// 微信的报文是**一层**（`<xml>` 下面一排标量），两层以上说明这份报文不是它的。设这个上限
// 是为了让「把一份深度一万的 XML 喂给我们」不至于在 elementStack 上吃内存——报文本身有
// 64 KiB 上限，但那一层挡不住深嵌套。
const maxXMLDepth = 32

// signParams 按微信的口径算一次签名。
//
// 导出成签名函数而不是只留内部实现：联调脚本与单测要能对同一条报文算出同一个值。密钥为空时
// 返回错误——**绝不降级成空签名**（空签名在微信那边是一句 SIGNERROR，而在这里它会掩盖
// 「密钥没配」这件运维事实）。
func signParams(params map[string]string, secret string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("%w: wechat pay APIv2 key is not configured", provider.ErrSecretNotConfigured)
	}
	signature, err := signing.Sign(params, signSpec, signing.Key{Secret: secret})
	if err != nil {
		return "", fmt.Errorf("sign wechat pay request: %w", err)
	}
	return signature, nil
}

// verifySign 校验一份回调报文的签名。
//
// 与出站签名**同一把密钥、同一套规则**（微信 APIv2 的回调就是这么签的），所以它只是
// signing.Verify 的一层包装。失败一律返回 ErrSignatureMismatch，包括「报文里根本没有 sign」
// ——一条没有签名的通知不存在一份我们该认下来的签名。
//
// 「一律」是**真的要包一层**：signing.Verify 自己那个 mismatch 是另一族的错误值（它还要服务
// 别的签名口径），直接把它透出去的话，调用方拿 provider.ErrSignatureMismatch 去判会判不中
// ——而判不中的后果是把「验签不过」当成一次普通的内部错误（见 ums 那条路，它也是逐条包着
// provider.ErrSignatureMismatch 返回的）。内层那句话留在串里：它说的是哪个字段对不上。
func verifySign(params map[string]string, secret string) error {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("%w: wechat pay APIv2 key is not configured", provider.ErrSecretNotConfigured)
	}
	provided := strings.TrimSpace(params["sign"])
	if provided == "" {
		return fmt.Errorf("%w: notification has no sign field", provider.ErrSignatureMismatch)
	}
	if err := signing.Verify(params, signSpec, signing.Key{Secret: secret}, provided); err != nil {
		return fmt.Errorf("%w: %v", provider.ErrSignatureMismatch, err)
	}
	return nil
}

// preSign 就地填上 sign 字段。
//
// 顺序是**先签名后拼报文**：sign 自己不进待签串（见 signSpec 的 Omit），所以先算后填与
// 先填后算在数学上等价——但只有「先算」这一种写法不会在有人误删 Omit 时自指。这个函数是
// 那个顺序的唯一出口。
func preSign(params map[string]string, secret string) error {
	signature, err := signParams(params, secret)
	if err != nil {
		return err
	}
	params["sign"] = signature
	return nil
}

// nonceStr 生成一个随机串，逐字照抄老系统的 generateNonceStr：`md5(纳秒+秒)` 的 **hex 小写**。
//
// 它不用于签名（nonce_str 自己也在待签串里，但值随我们定），只是报文的去重与防重放。用
// 时间戳派生而不是 crypto/rand 是照抄：微信并不要求它不可预测，而两边用同一个算法意味着
// 联调时对得上老系统那边的报文形状。
func nonceStr() string {
	sum := md5.Sum([]byte(strconv.FormatInt(time.Now().UnixNano(), 10) + strconv.FormatInt(time.Now().Unix(), 10)))
	return hex.EncodeToString(sum[:])
}

// requestSerial 是纯签约的请求序列号，逐字照抄老系统（`UnixNano` 的十进制串）。
//
// 它进待签串，微信侧用它区分同一条 contract_code 的多次签约请求。我们不做校验、只做透传。
func requestSerial() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}

// encodeXML 把一组参数拼成微信 APIv2 的报文。
//
// 形状照抄老系统的 mapToXML：**每个值都无条件包 CDATA**，包括空值。空值必须进报文（哪怕
// 它不进待签串）：`contract_termination_remark` 这类字段在微信那边是「有这个键、值为空」与
// 「没有这个键」两种不同的报文。
//
// 与老系统的两点差别，都不是协议上的：
//
//   - **键按字典序输出**。老系统遍历 map，顺序是随机的。XML 的字段顺序对微信无意义（它按
//     名字取值），而确定的顺序让测试能逐字节断言——否则这个函数的单测只能断言「包含了」。
//   - **值里的 "]]>" 会被拆开**。CDATA 段里不能出现 "]]>"，原样写进去的报文不是合法 XML，
//     微信会回一句 PARSE ERROR，而那看上去像「对面挂了」。拆成两段 CDATA 是标准做法，
//     对不含 "]]>" 的值（今天全部字段）是恒等变换。
func encodeXML(params map[string]string) []byte {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	builder.WriteString("<xml>")
	for _, key := range keys {
		builder.WriteString("<")
		builder.WriteString(key)
		builder.WriteString(">")
		writeCDATA(&builder, params[key])
		builder.WriteString("</")
		builder.WriteString(key)
		builder.WriteString(">")
	}
	builder.WriteString("</xml>")
	return []byte(builder.String())
}

// cdataCloser 是 CDATA 段的结束标记。它不能出现在段内容里，见 encodeXML 的注释。
const cdataCloser = "]]>"

func writeCDATA(builder *strings.Builder, value string) {
	builder.WriteString("<![CDATA[")
	builder.WriteString(strings.ReplaceAll(value, cdataCloser, "]]]]><![CDATA[>"))
	builder.WriteString(cdataCloser)
}

// decodeXML 把一份应答报文解成扁平的键值表。
//
// **宽松解析，不依赖结构体**：微信对同一条路径可能回不同形状的报文（成功一份、失败一份、
// 某些错误码下少几个字段），而按结构体解的话「少一个字段」会变成一次解析失败——那时我们
// 拿到的是「报文读不懂」，而不是它本来要告诉我们的那句 err_code_des。
//
// 只取 **`<xml>` 下那一层**的标量：嵌套结构在 APIv2 里不存在，而把嵌套里的值一并吸上来
// 会让「这份报文里有这个字段」变成一个我们在猜的事实。
//
// **键在、值为空**会被保留成空串（不省略键）：调用方读 `return_code` 时，「没有这个键」
// 与「键在但是空的」都要被当成「这不是一份我们能认的报文」，而把后者读成缺键要绕一层。
func decodeXML(body []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	out := make(map[string]string)
	var stack []string

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse wechat pay xml response: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			stack = append(stack, element.Name.Local)
			if len(stack) > maxXMLDepth {
				return nil, fmt.Errorf("parse wechat pay xml response: nested deeper than %d levels", maxXMLDepth)
			}
			if len(stack) == 2 {
				// 先登记键，值由后面的 CharData 累加（CDATA 在 Go 里也是 CharData）。
				out[element.Name.Local] = ""
			}
		case xml.CharData:
			if len(stack) == 2 {
				out[stack[1]] += string(element)
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	if len(stack) != 0 {
		// 上面的循环只有在 EOF 时才退出，而一个正常闭合的报文到 EOF 时栈是空的。
		// 非空说明报文被截断了——那时下面的 err_code 判断会指向一个根本没读全的报文。
		return nil, errors.New("parse wechat pay xml response: document ended inside an element")
	}
	if len(out) == 0 {
		return nil, errors.New("parse wechat pay xml response: no field found")
	}
	return out, nil
}
