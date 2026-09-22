// Package secret 用一把主密钥把渠道凭据加密后存进库，并保证**密钥值只在两个端点之间
// 以明文存在**：写请求进来时、以及适配器要用它的那一刻。
//
// # 为什么要有这个包
//
// 在此之前的约定是「库里只存键名，值住在环境变量里」（payment_channels.secret_ref 的
// 注释、dto 的说明、后台页面上那句「密钥不是密钥」）。那条约定挡住的是「把密钥写进会
// 回显给浏览器的 config 列」，这一点是对的，但代价是**加一个渠道仍要改部署配置 + 重启
// 服务**——而「配置完直接能用」恰恰是这一批要解决的问题。真接微信还躲不过去：它的商户
// 私钥是多行 PEM、平台证书会轮换，这两样都不适合摆在环境变量里。
//
// 所以密钥落到自己的列上（payment_channels.secrets），**密文**，一次一密。
// secret_ref 保留，作为环境变量兜底；解析顺序是库里的密文 → 环境变量 → 空串。
//
// # 信封
//
// 每个槽是一个 Envelope：nonce + 密文 + kind。kind 只区分用途（单行文本 / PEM 多行），
// 让后台按槽渲染出对的控件，它不参与密码学。
//
// # 三条不能松的线
//
//   - **空值不封**。Seal 拒绝空串：槽要么不存在，要么装着东西。「槽在、值是空的」是一种
//     只有 bug 才造得出来的状态，而它和「没有这个槽」在解析那一层意思正好相反
//     （前者会让调用方以为已配置），杜绝它比解释它便宜。
//   - **AAD 绑住槽名与 kind**。密文从一个槽搬到另一个槽、或者把 kind 改掉，解密都会失败。
//     拿库写权限的人换不出一个「用 A 的密钥当 B 用」的局面。
//   - **错误里不带明文，也不带密钥**。整个包的错误串里只有槽名，别的什么都没有——
//     错误会进日志、会被包进上层错误，明文跟进去就前功尽弃了。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// keySize 是 AES-256 的密钥长度。没有别的选项：这把密钥保护的是能直接换钱的东西，
// 不留「短一点也能用」的口子。
const keySize = 32

// nonceSize 是 GCM 的标准 nonce 长度。每个信封现取一次随机值，**绝不复用**——
// 同一把密钥下复用 nonce 会同时毁掉机密性和完整性。
const nonceSize = 12

// aadPrefix 是附加认证数据的前缀，带一个版本号。
//
// AAD 不落库（它是算出来的），所以这个前缀是**格式的一部分**：将来换存储格式时改它，
// 老密文会在解密时直接失败，而不是被新代码用错误的假设解释。
const aadPrefix = "panda-secret-v1"

var (
	// ErrMasterKeyMissing：装配处拿到的这一族主密钥是空的（今天只有 partner-service 在用：
	// PARTNER_SECRET_KEY，见 platform/config。payment-service 曾经也有一把，随着渠道凭据
	// 不再入库一起没了）。
	ErrMasterKeyMissing = errors.New("secret master key is not configured")
	// ErrMasterKeyInvalid：主密钥不是 32 字节。
	//
	// 报错文案里带生成命令，因为这几乎总是**第一次配这个变量**的人：他不知道要多长、
	// 也不知道该编码成什么样子，而「格式不对」这句话本身帮不上忙。
	ErrMasterKeyInvalid = errors.New("secret master key must be 32 bytes, base64 (openssl rand -base64 32) or 64 hex characters")
	// ErrKindUnknown：kind 不在 text / pem 里。
	ErrKindUnknown = errors.New("secret kind is not one of text or pem")
	// ErrPlaintextEmpty：要封的明文是空的（见包注释「空值不封」）。
	ErrPlaintextEmpty = errors.New("secret value is empty; remove the slot instead of storing a blank one")
	// ErrCiphertextMalformed：信封本身就不成形（nonce 不是 12 字节、base64 解不开）。
	//
	// 与 ErrDecryptFailed 分开：这一条说明**行里的数据坏了**，那一条说明**它被人动过、
	// 或者主密钥换了**。两者的排查方向完全不同。
	ErrCiphertextMalformed = errors.New("sealed secret is malformed")
	// ErrDecryptFailed：认证失败——密文被改过、槽名对不上、或者主密钥不是当初那一把。
	//
	// 不区分「哪一把钥匙不对」是有意的：能把库改掉的人知道了也没有更多办法，而区分开
	// 会让错误串带着更多能拿去试探的信息。
	ErrDecryptFailed = errors.New("sealed secret could not be decrypted")
)

// Kind 是槽的用途，只影响后台渲染成什么控件。
type Kind string

const (
	// KindText：单行文本（签名密钥、APIv3 密钥、AppKey）。
	KindText Kind = "text"
	// KindPEM：多行 PEM（商户私钥、平台证书）。
	KindPEM Kind = "pem"
)

// ParseKind 把配置里的字符串翻成 Kind。大小写与空白不敏感，理由同 signing.ParseAlgorithm：
// 这个名字是人手填进表单的。
func ParseKind(name string) (Kind, error) {
	switch Kind(strings.ToLower(strings.TrimSpace(name))) {
	case KindText:
		return KindText, nil
	case KindPEM:
		return KindPEM, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrKindUnknown, name)
	}
}

// Envelope 是一个槽在库里的样子，整个结构进 secrets 那一列。
//
// 三个字段全是 base64 或短枚举，所以这一列**可以直接给人看**：psql 里看到的不该是明文，
// 也不该是没法读的二进制。
type Envelope struct {
	// Kind 是用途，见 Kind。它是**被认证的**的一部分（进 AAD），改它会解不开。
	Kind Kind `json:"kind"`
	// Nonce 是这一次加密用的随机数，base64。
	Nonce string `json:"nonce"`
	// Ciphertext 是 GCM 输出的密文加认证标签，base64。
	Ciphertext string `json:"ciphertext"`
}

// Slots 是一整列：槽名 → 信封。
//
// 用 map 而不是结构体，是因为**有哪些槽由协议族决定**，而协议族是可以在后台新建的
// （见 provider 的 config 规格）。写死成结构体的代价是「加一家渠道要改一次这个类型」，
// 那正是这一批要消灭的东西。
type Slots map[string]Envelope

// Names 返回槽名，已排序。
//
// 这是**唯一**可以把 slots 交给上层的地方——回显、审计、日志都只该看到名字
// （见包注释第三条）。给调用方一个 map 是没用的，它要的就是「有哪些槽」。
func (s Slots) Names() []string {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Keyring 持有主密钥，负责封与解。
//
// 它是**无状态**的（除了那把密钥），因此可以并发用：一个实例建一次，装配处传下去。
type Keyring struct {
	aead cipher.AEAD
}

// New 用一把 32 字节的主密钥建 Keyring。
func New(masterKey []byte) (*Keyring, error) {
	if len(masterKey) != keySize {
		return nil, ErrMasterKeyInvalid
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		// 长度已经查过，这里只可能是环境层面的失败，不该把密钥内容带进错误。
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build gcm: %w", err)
	}
	return &Keyring{aead: aead}, nil
}

// Parse 把环境变量里那一串翻成 Keyring。
//
// 认两种写法：base64（`openssl rand -base64 32` 的输出，44 字符）与 64 位 hex。
// **不认**「32 个 ASCII 字符的短语」——那看着像密钥，实际熵远低于 256 位，允许它就等于
// 把「必须随机」这条规矩变成一句建议。两种写法的长度不同，不存在歧义。
func Parse(raw string) (*Keyring, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, ErrMasterKeyMissing
	}
	// 先试 hex：它只有一种长度，试错代价小，且 64 位 hex 不会被 base64 解成 48 字节
	// （长度对不上，所以两条路不会互相误伤）。
	if len(trimmed) == keySize*2 {
		if decoded, err := hex.DecodeString(trimmed); err == nil {
			return New(decoded)
		}
	}
	// Standard 与 Raw 两种 base64 都试：`openssl rand -base64 32` 带 "="，而有些密钥
	// 管理系统存的是去掉了填充的那种。
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.DecodeString(trimmed); err == nil && len(decoded) == keySize {
			return New(decoded)
		}
	}
	return nil, ErrMasterKeyInvalid
}

// Seal 把一个明文封进信封。
//
// slot 是槽名（"signKey" / "merchantPrivateKey" …），它进 AAD，所以密文换个槽放就解不开。
func (k *Keyring) Seal(slot string, kind Kind, plaintext string) (Envelope, error) {
	if plaintext == "" {
		return Envelope{}, fmt.Errorf("%w (slot %q)", ErrPlaintextEmpty, slot)
	}
	if _, err := ParseKind(string(kind)); err != nil {
		return Envelope{}, err
	}
	if k == nil || k.aead == nil {
		return Envelope{}, ErrMasterKeyMissing
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, fmt.Errorf("read nonce: %w", err)
	}
	sealed := k.aead.Seal(nil, nonce, []byte(plaintext), aad(slot, kind))
	return Envelope{
		Kind:       kind,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
	}, nil
}

// Open 解开一个信封，返回明文。
//
// 失败一律是 ErrDecryptFailed 或 ErrCiphertextMalformed，**错误里没有明文也没有密钥**。
// 调用方拿到错误后唯一该做的事是拒绝（验不了签就不放行），不是退回明文或空串继续。
func (k *Keyring) Open(slot string, envelope Envelope) (string, error) {
	if _, err := ParseKind(string(envelope.Kind)); err != nil {
		return "", err
	}
	if k == nil || k.aead == nil {
		return "", ErrMasterKeyMissing
	}

	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envelope.Nonce))
	if err != nil || len(nonce) != nonceSize {
		return "", fmt.Errorf("%w (slot %q)", ErrCiphertextMalformed, slot)
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envelope.Ciphertext))
	if err != nil {
		return "", fmt.Errorf("%w (slot %q)", ErrCiphertextMalformed, slot)
	}
	plaintext, err := k.aead.Open(nil, nonce, sealed, aad(slot, envelope.Kind))
	if err != nil {
		// GCM 的 Open 失败就是认证失败。**不要把 err 包进来**：它是 crypto 包内部的
		// 错误串，除了「没有更多信息」之外没有别的含义。
		return "", fmt.Errorf("%w (slot %q)", ErrDecryptFailed, slot)
	}
	return string(plaintext), nil
}

// aad 拼出附加认证数据：格式版本 + 槽名 + kind。三者任一被动过，解密都会失败。
//
// 用 NUL 分隔而不是别的字符：槽名与 kind 都是配置里手填的短名，NUL 不可能出现在里面，
// 所以 "a" + "b" 与 "ab" 撞不到一起。
func aad(slot string, kind Kind) []byte {
	return []byte(aadPrefix + "\x00" + slot + "\x00" + string(kind))
}
