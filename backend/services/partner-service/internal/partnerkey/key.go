// Package partnerkey 生成并掩码合作方的 API 凭据。
//
// 两件东西，两个不同的安全等级，所以它们不共用一套规则：
//
//   - api_key 是**公开标识**（每次请求写在 X-API-Key 里），明文入库、明文可读。
//   - secret 是**签名密钥**，只有拿得到它的人才能签出对得上的签名；它以密文入库
//     （platform/secret 的信封），并且**只在签发响应里出现一次**。
//
// 两者都掩码成「前 4 + … + 后 4」存一份在库里（api_key_mask / secret_mask），读接口只回
// 掩码。这样「读接口不回明文」就不是一句承诺，而是**结构上的**事实：读路径上一次解密都
// 不做，明文根本不在它手里。
//
// 掩码为什么留头尾各 4 位而不是只留末 4 位：运营手上拿到的是一串随机字符，只留末 4 位时
// 他一眼看不出「这是他上周发的那把还是刚刚重发的那把」。头 4 位加上末 4 位在列表里足够
// 分辨，而 8 位随机字符（约 47 bit）也不构成可用的暴力面——真正的凭据是那 32 位全串。
package partnerkey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
)

const (
	// length 是凭据长度。老系统也是 32 位（models/merchant_api.go 自助生成的那一串），
	// 保持一致：合作方那边拿去做联调的手册、示例代码都不用改。
	length = 32
	// alphabet 是 base62。刻意不含 "-" / "_" / "+" / "/"：这串东西会被贴进 URL 查询串、
	// shell 命令、YAML 配置与 HTTP 头，每一种场合对特殊字符的转义规则都不同，而随机串的
	// 字符集完全不必去踩那些坑。
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	// maskVisible 是掩码两端各留的字符数。
	maskVisible = 4
	// maskEllipsis 是掩码中间那一颗省略号。用单字符 U+2026 而不是 "..."，是因为掩码会
	// 出现在窄表格里（8 位可见字符 + 一颗省略号 = 9 个字符宽）。
	maskEllipsis = "…"
)

// ErrMaskTooShort：要掩码的串太短，掩不了。
//
// 它不是运行时可能出现的错误（调用方传的都是刚生成或刚从库里读出来的 32 位串），而是给
// 「把某个自定义长度的值也拿来掩码」这种改动的一道闸。短的串掩完等于没掩——前 4 加后 4
// 就会把整串原样显示出来，而调用方会以为它是安全的。
var ErrMaskTooShort = errors.New("value is too short to mask")

// Generate 生成一个全新的 32 位随机凭据。
//
// 用 crypto/rand 而不是 math/rand：这一串是我们唯一的凭据，可预测的随机数等于没有凭据。
// rand.Read 在这里不会失败（Go 的 crypto/rand 在失败时会 panic 而不是返回错误），但错误
// 仍然向上返回，免得装配处以为「它不会失败」而在别处省掉判断。
func Generate() (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate partner credential: %w", err)
	}
	// 逐字节取模会有偏（256 % 62 ≠ 0）。这里的取舍是**接受这点偏差**：base62 的偏差
	// 相对 256/62≈4.13 只有约 1.6% 的不均匀，而 32 个字符的熵仍有 ~190 bit。用拒绝采样
	// 换那 1.6% 的均匀度，代价是一个可能不终止的循环——在签发密钥这条路径上不值得。
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// Mask 把一串凭据掩成「前 4 + … + 后 4」。
//
// 长度不足 2*maskVisible 时返回 ErrMaskTooShort 而不是硬掩：那种情况下掩码会把整串暴露
// 出来，而调用方还以为自己拿到的是安全的展示值。
func Mask(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 2*maskVisible {
		return "", fmt.Errorf("%w: need at least %d characters, got %d", ErrMaskTooShort, 2*maskVisible, len(trimmed))
	}
	return trimmed[:maskVisible] + maskEllipsis + trimmed[len(trimmed)-maskVisible:], nil
}

// Issued 是一次凭据签发的产物：**同时**带着明文与掩码，只在写请求那一条路径上存在。
//
// 单独一个类型而不是两个返回值，是为了让「明文与掩码必须一起产生」这件事在类型上成立：
// 它们来自同一个随机串，掩码算错一位就会出现「列表里显示的掩码与手上那把钥匙对不上」，
// 而那种错在库里是查不出来的（掩码就是唯一线索）。
type Issued struct {
	// APIKey 是公开标识，明文，入库。
	APIKey string
	// Secret 是签名密钥，**只在创建响应里出现这一次**；入库的是它的密文。
	Secret string
	// APIKeyMask / SecretMask 是两者各自的掩码，入库并用于所有读取。
	APIKeyMask string
	SecretMask string
}

// Issue 生成一对新凭据：一个 api_key + 一个签名密钥，附带各自的掩码。
//
// 两次独立取随机数，**不是从 api_key 派生出 secret**：派生会让「见过 api_key」的人只差一个
// 未知的派生函数就能算出密钥，而两次独立取值让两者在信息上无关。
func Issue() (Issued, error) {
	apiKey, err := Generate()
	if err != nil {
		return Issued{}, err
	}
	secret, err := Generate()
	if err != nil {
		return Issued{}, err
	}
	keyMask, err := Mask(apiKey)
	if err != nil {
		return Issued{}, err
	}
	secretMask, err := Mask(secret)
	if err != nil {
		return Issued{}, err
	}
	return Issued{APIKey: apiKey, Secret: secret, APIKeyMask: keyMask, SecretMask: secretMask}, nil
}
