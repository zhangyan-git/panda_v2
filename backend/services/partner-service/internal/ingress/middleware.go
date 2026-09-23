package ingress

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/ratelimit"
	"github.com/panda-dev/panda-v2/backend/platform/secret"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/model"
)

// 拒绝时回给调用方的那几句话。**所有凭据类失败共用第一句**（见 doc.go）：签名不对、密钥
// 查不到、过期、停用、来源不在白名单、nonce 重放——对外的差别只有 HTTP 状态码 401 一个字，
// 别的什么都没有。老系统把「API密钥无效」与「签名验证失败」原样发回去，等于给攻击者一台
// 枚举机，让他知道猜到了哪一步。
const (
	unauthorizedBody = `{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`
	// unavailableBody 是我们自己的故障（库连不上、Redis 连不上、密钥解不开）。它与 401 分开
	// 不是泄漏：这几条与「调用方是谁」无关，任何请求在那一刻都会得到同一句。
	//
	// 合并进 401 的代价很具体：库挂了的时候所有合作方会去查自己的签名，而真正的原因在我们这边。
	unavailableBody = `{"success":false,"errorCode":"SERVICE_UNAVAILABLE","errorMessage":"service unavailable"}`
	// badRequestBody 只在读报文失败时报（客户端中途断开、报文不是我们读得下来的东西）。它
	// 发生在碰任何凭据之前，所以不泄漏调用方的任何信息。
	badRequestBody = `{"success":false,"errorCode":"INVALID_REQUEST","errorMessage":"invalid request body"}`
	// tooLargeBody 与 badRequestBody 同一条理由，发生在凭据之前。
	tooLargeBody = `{"success":false,"errorCode":"INVALID_REQUEST","errorMessage":"request body too large"}`
	// tooManyBody 与 401 分开是因为客户端**必须**能分辨：这一条要等，不是要改签名。为此还带
	// 上 Retry-After（见 reject）。
	tooManyBody = `{"success":false,"errorCode":"TOO_MANY_REQUESTS","errorMessage":"too many requests"}`
)

// 内部错误码。它们**只进 partner_call_logs.error_code**，绝不回给调用方——这是「真实原因
// 只有我们看得到」那句话的落点，也是排查时唯一能分清「对方签名写错了」与「对方在重放」的
// 地方（两者对外都是同一句 401）。
const (
	codeMissingCredentials  = "MISSING_CREDENTIALS"
	codeTimestampOutOfRange = "TIMESTAMP_OUT_OF_WINDOW"
	codeCredentialLookup    = "CREDENTIAL_LOOKUP_FAILED"
	codeUnknownAPIKey       = "UNKNOWN_API_KEY"
	codePartnerDisabled     = "PARTNER_DISABLED"
	codePartnerExpired      = "PARTNER_EXPIRED"
	codeAPIKeyDisabled      = "API_KEY_DISABLED"
	codeAPIKeyExpired       = "API_KEY_EXPIRED"
	codeIPNotAllowed        = "IP_NOT_ALLOWED"
	codeSecretUnreadable    = "SECRET_UNREADABLE"
	codeSignatureMismatch   = "SIGNATURE_MISMATCH"
	codeNonceUnavailable    = "NONCE_STORE_UNAVAILABLE"
	codeNonceReplayed       = "NONCE_REPLAYED"
	codeRateLimited         = "RATE_LIMITED"
	// codeRateLimitDegraded 是「这次没挡，但限流器没建起来」。
	//
	// 它与 RATE_LIMITED 分开，是因为它会**出现在一条 200 的记录上**（限流那一步失败是放行的，
	// 见下面那段的取舍）。共用 RATE_LIMITED 的代价很具体：调用日志里会出现 status=200 且
	// error_code=RATE_LIMITED 的行，读的人会以为「这次调用被限流了」，而它其实成功了。
	//
	// 同理，它也不能被当成「这次调用失败了」：RecordCallLog 用「error_code 为空」判断验签是
	// 否通过（见 calllog.go），所以写上它意味着这一行不更新密钥的 last_used_at。
	codeRateLimitDegraded = "RATE_LIMIT_DEGRADED"
	codeBodyUnreadable    = "BODY_UNREADABLE"
	codeBodyTooLarge      = "BODY_TOO_LARGE"
)

// errorCodes 是上面那一组常量的清单。**它是这份词表唯一的可遍历形态**。
//
// 加一个码时只改上面、忘了这里，表现是后台按新码筛日志筛不出来（列表里有那些行，筛选框里
// 没有这个选项）；反过来，这里多一个不存在的码，表现是筛出来永远是空。测试盯着这一条
// （TestErrorCodesCoverEveryConstant）。
var errorCodes = []string{
	codeMissingCredentials,
	codeTimestampOutOfRange,
	codeCredentialLookup,
	codeUnknownAPIKey,
	codePartnerDisabled,
	codePartnerExpired,
	codeAPIKeyDisabled,
	codeAPIKeyExpired,
	codeIPNotAllowed,
	codeSecretUnreadable,
	codeSignatureMismatch,
	codeNonceUnavailable,
	codeNonceReplayed,
	codeRateLimited,
	codeRateLimitDegraded,
	codeBodyUnreadable,
	codeBodyTooLarge,
}

// ErrorCodes 返回全部内部错误码，已排序。
//
// 给后台的筛选器用：它必须与中间件真正会写的那一套是同一份。返回副本，调用方排不了序也改不了
// 这里的顺序（那是 map 泄漏之外最容易出的一类事故）。
func ErrorCodes() []string {
	out := slices.Clone(errorCodes)
	slices.Sort(out)
	return out
}

// IsErrorCode 判一个串是不是我们写过的内部错误码。
//
// 筛选用它挡拼错的值：调用日志的 error_code 是一套**封闭**词表（只有本包会写这一列），
// 所以「不认识的码」一定是一次输错，而不是一个「将来会有的码」——让它返回空列表等于对运营
// 说「这段时间没有这类失败」。
func IsErrorCode(code string) bool {
	return slices.Contains(errorCodes, strings.TrimSpace(code))
}

const (
	// maxLoggedBodyBytes 是进调用日志的报文上限（请求与响应各自计）。
	//
	// 8 KiB 是**日志**的上限，不是接口的上限（那个是 maxBodyBytes = 1 MiB）。取舍在这里：
	// 排查一次失败要看的是报文的前几个字段与错误响应全文，而不是一份几百 KB 的列表响应；
	// 把整份存进去的后果是 partner_call_logs 这张**只增不删**的表按流量膨胀，而它今天还没有
	// 保留期清理（见 migrations/partner 文末）。
	//
	// 截断时留一个显式的标记（truncate 里那段），不然「报文就只有这么长」与「被我们截了」
	// 在日志里分不开——后者会让一次排查停在一个不存在的问题上。
	maxLoggedBodyBytes = 8 << 10
	// maxLoggedQueryBytes 是 query 串的日志上限。它比正文小得多：待签串里 query 是逐键取
	// **第一个**值进的，一个几百 KB 的 query 与本服务的行为无关，只值得留一截线索。
	maxLoggedQueryBytes = 2 << 10
	// callLogTimeout 限制写一条调用日志的时间。
	//
	// 它走的是**独立的** context（context.WithoutCancel）：请求自己的 context 在客户端断开
	// 时就取消了，而"有人调了但我们没答上"恰恰是这张表最该留下的记录。超时不为零是为了让
	// 库真的挂掉时请求不跟着一起卡住。
	callLogTimeout = 3 * time.Second
)

// truncateMarker 是截断标记。用一句英文短句而不是「…」：它会出现在日志检索里，而中英文
// 混排的标记没法用 grep 精确匹配。
const truncateMarker = "\n...[truncated]"

// CallLogWriter 是 ingress 需要仓储提供的一件事：把这次调用写进去。**只写，不改，不读**。
//
// 命名与 repository 上的方法逐字一致，装配处直接把仓储传进来（见 cmd/main.go），不写适配器。
type CallLogWriter interface {
	RecordCallLog(ctx context.Context, entry model.CallLog) error
}

// Options 是 Guard 的全部依赖。
type Options struct {
	// Lookup 按 X-API-Key 取密钥行与合作方（一条 JOIN）。
	Lookup CredentialLookup
	// Keyring 解签名密钥的密文信封。
	Keyring *secret.Keyring
	// Nonces 是 Redis 上的 nonce 去重器，**构造期已经确认过它真的连得上**（见 nonce.go）。
	Nonces NonceStore
	// Limiters 按额度缓存限流器。
	Limiters *Limiters
	// CallLogs 写调用日志。
	CallLogs CallLogWriter
	// TrustedProxies 决定「谁是真的客户端」，来自 PARTNER_TRUSTED_PROXY_CIDRS。可以为 nil
	// （那等于只看 RemoteAddr）。
	TrustedProxies *ratelimit.ProxyTrust
	// Now 可注入，只为测试能造出超窗的时间戳。为零值时为 time.Now。
	Now func() time.Time
}

// Guard 是开放接口树最外层的那一段中间件。一个进程建一个。
type Guard struct {
	lookup   CredentialLookup
	keyring  *secret.Keyring
	nonces   NonceStore
	limiters *Limiters
	logs     CallLogWriter
	trust    *ratelimit.ProxyTrust
	now      func() time.Time
}

// New 构造 Guard。**任何一项依赖缺失都返回错误**，不做「缺了就跳过那一步」的降级：
// 一个少了 nonce 检查的 Guard 能跑、能返回 200，而它防重放的那一层是空的——那种服务比
// 起不来的服务危险得多。
func New(opts Options) (*Guard, error) {
	switch {
	case opts.Lookup == nil:
		return nil, errors.New("partner ingress: credential lookup is required")
	case opts.Keyring == nil:
		return nil, errors.New("partner ingress: secret keyring is required")
	case opts.Nonces == nil:
		return nil, errors.New("partner ingress: nonce store is required")
	case opts.Limiters == nil:
		return nil, errors.New("partner ingress: rate limiters are required")
	case opts.CallLogs == nil:
		return nil, errors.New("partner ingress: call log writer is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Guard{
		lookup:   opts.Lookup,
		keyring:  opts.Keyring,
		nonces:   opts.Nonces,
		limiters: opts.Limiters,
		logs:     opts.CallLogs,
		trust:    opts.TrustedProxies,
		now:      now,
	}, nil
}

// Handler 包住开放接口那棵树。
//
// # 顺序，以及每一步为什么在这里
//
//  1. **头齐不齐**（最便宜）。缺哪一个都是 401，且不查库、不读报文——一次没有凭据的请求
//     不该让我们付读报文或查库的代价。
//  2. **时间窗**。纯计算。放在查库之前：一个时钟差一小时的客户端，它的密钥对不对根本不重要。
//  3. **查密钥行**（一次 JOIN，拿密钥与合作方）。查不到就是 UNKNOWN_API_KEY。
//  4. **启停与过期**（合作方与密钥各判一次，取与）。这两个判断用的是**刚读回来的那一行**，
//     没有缓存——所以「停用立即生效」是结构上的事实，不是一句承诺。缓存会把它变成「最多
//     一分钟后生效」，而那一分钟里被停用的合作方照样能发券。
//  5. **IP 白名单**。它要读密钥行上的 TEXT[]，所以只能在第 3 步之后。
//  6. **解密钥并验签**。放在限流与 nonce 之前是**硬要求**：那两步都要往 Redis 写东西，而
//     把写放在验签之前，等于让任何一个没有密钥的人都能往我们的 Redis 里塞任意多的键
//     （nonce 由调用方给，TTL 五分钟）——一次不需要凭据的内存放大。
//  7. **限流**。在 nonce 之前：它是对「这家合作方整体打了多少」的上限，先按总账挡一道，
//     再谈这一次是不是重复。同时成立时（重放 + 超限）报的是 RATE_LIMITED——两个都是拒，
//     但先说是为了让「刷接口」这件事在写 Redis 之前就有上限。
//  8. **nonce 去重**。最后一个，因为它是唯一一步「拒绝的理由来自我们这边的一个可变状态」，
//     前面任何一步失败都不该在 Redis 里留下痕迹。
//  9. **进 handler**，并把身份放进请求上下文（见 Caller）。
//
// # 日志
//
// 一次调用只写一条，用 defer 挂在最外层：拒绝的那几条路在写响应时就 return 了，成功的那条
// 路走完 handler 才 return，两种情况的 status / 响应体 / 耗时都在同一个地方取。分开写
// （拒绝处一条、成功处一条）的代价是「有人在中间加了一条 return，日志就漏了」——而漏掉的
// 恰恰会是一类没见过的失败。
func (g *Guard) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &responseRecorder{ResponseWriter: w}
		exchange := &exchange{
			guard:    g,
			started:  g.now(),
			recorder: recorder,
			entry: model.CallLog{
				// 认不出调用方时先写成全零 UUID，认出来之后覆盖（见 model.UnknownPartnerID：
				// 这一行本身必须留下，所以不能用 NULL 表达「不知道」）。
				PartnerID:  model.UnknownPartnerID,
				APIKeyID:   model.UnknownPartnerID,
				Method:     r.Method,
				Path:       r.URL.Path,
				Query:      truncate(r.URL.RawQuery, maxLoggedQueryBytes),
				RequestIP:  g.clientIP(r),
				StatusCode: http.StatusOK,
			},
		}
		defer exchange.writeCallLog(r.Context())

		g.serve(exchange, recorder, next, r)
	})
}

// serve 是 Handler 去掉日志外壳之后的本体。单独一个函数只是为了 defer 的位置清楚。
func (g *Guard) serve(ex *exchange, w http.ResponseWriter, next http.Handler, r *http.Request) {
	apiKeyValue := strings.TrimSpace(r.Header.Get(HeaderAPIKey))
	timestamp := strings.TrimSpace(r.Header.Get(HeaderTimestamp))
	nonce := strings.TrimSpace(r.Header.Get(HeaderNonce))
	signature := strings.TrimSpace(r.Header.Get(HeaderSignature))
	if apiKeyValue == "" || timestamp == "" || nonce == "" || signature == "" {
		ex.reject(w, http.StatusUnauthorized, codeMissingCredentials, unauthorizedBody)
		return
	}

	// 时间戳与 nonce 都会进待签串（见 spec.go），所以这里**原样**使用头里的那两串，不做
	// 任何归一：合作方签的是它发出来的字节，我们改一个字节就等于签不上。
	now := g.now()
	if err := TimestampWithinWindow(timestamp, now); err != nil {
		ex.reject(w, http.StatusUnauthorized, codeTimestampOutOfRange, unauthorizedBody)
		return
	}

	key, partner, err := g.lookup(r.Context(), apiKeyValue)
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			ex.reject(w, http.StatusUnauthorized, codeUnknownAPIKey, unauthorizedBody)
			return
		}
		// 库不可用 / 查询出错。不是调用方的错，也不是「凭据无效」——回 503，让合作方看见
		// 「对面有问题」而不是去改自己的签名。原因只进日志。
		slog.ErrorContext(r.Context(), "partner credential lookup failed", "error", err)
		ex.reject(w, http.StatusServiceUnavailable, codeCredentialLookup, unavailableBody)
		return
	}
	ex.entry.PartnerID = key.PartnerID
	ex.entry.APIKeyID = key.ID
	ex.entry.APIKeyMask = key.APIKeyMask

	if !partner.AllowsAt(now) {
		ex.reject(w, http.StatusUnauthorized, partnerRejectionCode(partner, now), unauthorizedBody)
		return
	}
	if !key.AllowsAt(now) {
		ex.reject(w, http.StatusUnauthorized, keyRejectionCode(key, now), unauthorizedBody)
		return
	}

	// 白名单的解析放在这里（而不是读密钥行时一起做）：一个存坏了的白名单值只该影响**这一把**
	// 密钥的调用，而不该让「读这一行」这个动作失败——那会让运营在页面上看不到这行，也就修不了它。
	allowList, err := ParseIPAllowList(key.IPWhitelist)
	if err != nil {
		slog.ErrorContext(r.Context(), "partner ip whitelist is malformed",
			"error", err, "api_key_id", key.ID)
		ex.reject(w, http.StatusUnauthorized, codeIPNotAllowed, unauthorizedBody)
		return
	}
	if !allowList.Allows(ex.entry.RequestIP) {
		ex.reject(w, http.StatusUnauthorized, codeIPNotAllowed, unauthorizedBody)
		return
	}

	// 报文在这一步才读：上面任何一条拒绝都不需要它，而读进来就要把 1 MiB 放在内存里。
	body, ok := ex.readBody(w, r)
	if !ok {
		return
	}
	// 报文原样进调用日志，**但凭据类字段先抹掉值**（见 redact.go）：这张表后台读得到、有它
	// 自己的保留期，而取货码那条路的报文里带着这台设备扣款的凭据。
	ex.entry.RequestBody = truncate(string(redactSecretFields(body)), maxLoggedBodyBytes)

	plaintext, err := g.keyring.Open(model.SecretSlot, key.Secret)
	if err != nil {
		// 解不开只可能是数据坏了或主密钥换了（platform/secret 的错误串里没有明文，见那个包）。
		// 回 503 而不是 401：这不是「你签名写错了」，是我们这边的一行数据坏了。
		slog.ErrorContext(r.Context(), "partner signing secret cannot be opened",
			"error", err, "api_key_id", key.ID)
		ex.reject(w, http.StatusServiceUnavailable, codeSecretUnreadable, unavailableBody)
		return
	}
	if err := Verify(Params(r, body), plaintext, signature); err != nil {
		ex.reject(w, http.StatusUnauthorized, codeSignatureMismatch, unauthorizedBody)
		return
	}

	limiter, err := g.limiters.For(key.RateLimitPerMinute)
	if err != nil {
		// 额度值不合法只可能来自手工改库（库上有 CHECK）。当成配额为默认值继续，比让这一把
		// 密钥彻底不能用好，也让这次调用留在日志里（error_code 会说明发生过什么）。
		slog.ErrorContext(r.Context(), "partner rate limiter could not be built",
			"error", err, "api_key_id", key.ID, "limit", key.RateLimitPerMinute)
		ex.entry.ErrorCode = codeRateLimitDegraded
	} else {
		allowed, lerr := limiter.Allow(r.Context(), rateLimitKey(key.ID))
		switch {
		case lerr != nil:
			// platform/ratelimit 的判定失败会自己退化成进程内计数（见那个包的 withFallback），
			// 所以走到这里说明连退化的那条路都失败了。这里的取舍跟着 platform 走：**放行**。
			// 限流挡的是流量，不是身份——让它自己出问题的时候变成「所有人被挡在外面」，
			// 比额度短时偏松更糟（note 与限流不同：那一条失败必须拒绝，见下）。
			slog.WarnContext(r.Context(), "partner rate limit check failed, allowing the request",
				"error", lerr, "api_key_id", key.ID)
		case !allowed:
			ex.reject(w, http.StatusTooManyRequests, codeRateLimited, tooManyBody)
			return
		}
	}

	claimed, err := g.nonces.Claim(r.Context(), nonceKey(key.ID, nonce), Window)
	if err != nil {
		// **失败关闭**：去重做不了的时候拒绝，不是放行。理由见 nonce.go 与方案 1.4 第 1 条
		// ——一个静默失效的防重放比没有防重放更危险，它让人以为自己有。
		slog.ErrorContext(r.Context(), "partner nonce store is unavailable",
			"error", err, "api_key_id", key.ID)
		ex.reject(w, http.StatusServiceUnavailable, codeNonceUnavailable, unavailableBody)
		return
	}
	if !claimed {
		ex.reject(w, http.StatusUnauthorized, codeNonceReplayed, unauthorizedBody)
		return
	}

	// 身份进上下文。走到这里意味着上面九道全过了（见 Caller 的注释）。
	ctx := WithCaller(r.Context(), Caller{
		PartnerID:   key.PartnerID,
		PartnerCode: partner.Code,
		APIKeyID:    key.ID,
		APIKeyMask:  key.APIKeyMask,
	})
	next.ServeHTTP(w, r.WithContext(ctx))
}

// clientIP 取判定用的客户端地址。
//
// **不要直接读 r.RemoteAddr**：本服务永远跑在网关后面，RemoteAddr 是网关自己，于是「配了
// 白名单的合作方全部被拒」——失败关闭，但看上去像对接方填错了 IP。
//
// nil 的 trust 等同于「什么都不信」，也就是只看 RemoteAddr（platform/ratelimit 的
// ProxyTrust.ClientIP 自己处理 nil 接收者）：那是本地直连与服务间直连的正确取值。配了
// PARTNER_TRUSTED_PROXY_CIDRS 之后，只有来自可信网段的请求才会去读 X-Forwarded-For，
// 且从右往左走到第一个不可信的跳为止——客户端自己伪造的 XFF 会被可信代理解析在真实地址
// 左边，走不到。
func (g *Guard) clientIP(r *http.Request) string { return g.trust.ClientIP(r) }

// partnerRejectionCode 把「合作方为什么不能用」分成两个码。
//
// 分开是因为排查方向相反：停用是运营有意关掉的（去问运营），过期是合同到期（去续期）。
// 合成一个 "PARTNER_INVALID" 会让这两种在日志里长得一样。
func partnerRejectionCode(partner *model.PartnerAccount, now time.Time) string {
	if partner.Status != model.StatusEnabled {
		return codePartnerDisabled
	}
	if partner.ExpiresAt != nil && !now.Before(*partner.ExpiresAt) {
		return codePartnerExpired
	}
	// 走到这里说明 AllowsAt 拒了但两个具体理由都不是——只有 nil 接收者会这样，而调用点
	// 已经用 lookup 的返回值保证它不是 nil。留着这句是为了让新增的判定条件不会静默地
	// 落进一个错误的码。
	return codePartnerDisabled
}

// keyRejectionCode 同上，密钥那一侧。
func keyRejectionCode(key *model.APIKey, now time.Time) string {
	if key.Status != model.StatusEnabled {
		return codeAPIKeyDisabled
	}
	if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
		return codeAPIKeyExpired
	}
	return codeAPIKeyDisabled
}

// exchange 攒这一次调用的日志，并在最后写出去。
type exchange struct {
	guard    *Guard
	started  time.Time
	recorder *responseRecorder
	entry    model.CallLog
}

// reject 写一个拒绝响应，并记下内部原因。
//
// **响应体是调用方给的常量**，不是这里拼出来的：任何时候都不要把 err.Error() 或者别的内部
// 串写进响应体（那正是"泄漏内部原因"的实现方式）。真实原因进 error_code，只落在日志里。
func (ex *exchange) reject(w http.ResponseWriter, status int, code, body string) {
	ex.entry.ErrorCode = code
	if status == http.StatusTooManyRequests {
		// 让客户端知道该等多久。取 60 秒（窗口长度）而不是剩余时间：剩余时间要读限流器内部
		// 状态，而这里只需要一个不比真实等待短的数——报短了会让客户端提前重试，又吃一个 429。
		w.Header().Set("Retry-After", strconv.Itoa(int(rateWindow.Seconds())))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// readBody 读报文，超限时已经把 413 写好了（第二个返回值为 false）。
//
// 上限之外**多读一个字节**：只读 maxBodyBytes 的话，「正好等于上限」与「远超上限」都会读到
// 同样多的字节，判断不出后者。多读这一个字节是判断「超了没有」最省的做法。
//
// 读完把 r.Body 换成一份可重放的副本：验签要读它，handler 还要再读一次（controller 解析
// JSON）。不换的话第二次读拿到的是 EOF，症状是每个 POST 都报「请求体不能为空」。
func (ex *exchange) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		ex.reject(w, http.StatusBadRequest, codeBodyUnreadable, badRequestBody)
		return nil, false
	}
	if len(body) > maxBodyBytes {
		ex.reject(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, tooLargeBody)
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// writeCallLog 把这一次调用落库。
//
// 用 context.WithoutCancel：请求自己的 ctx 在客户端挂断时就取消了，而那张表最该留下的恰恰
// 是「有人调了但我们没答上」。带上值（trace id 在里面）而不带取消，再加一个自己的超时——
// 库真的挂掉时不能让请求跟着一起等。
func (ex *exchange) writeCallLog(ctx context.Context) {
	ex.entry.StatusCode = ex.recorder.statusOrDefault()
	ex.entry.DurationMS = time.Since(ex.started).Milliseconds()
	ex.entry.ResponseBody = ex.recorder.captured()

	logCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callLogTimeout)
	defer cancel()
	if err := ex.guard.logs.RecordCallLog(logCtx, ex.entry); err != nil {
		// 写日志失败**不改变已经发出去的响应**（它此刻已经到客户端手上了），只记一条。
		// 不记的话，这张表开始缺行时我们没有任何线索。
		slog.Error("partner call log could not be recorded",
			"error", err, "path", ex.entry.Path, "error_code", ex.entry.ErrorCode,
			"api_key_id", ex.entry.APIKeyID)
	}
}

// responseRecorder 一边把响应发给客户端，一边留一份（截断后的）副本给调用日志。
//
// 它只实现 WriteHeader / Write / Unwrap：**不自己实现 Flush / Hijack**，而是把 Unwrap 交给
// Go 的 http.ResponseController 去发现——手写那几个方法就得把每个接口的语义再实现一遍，
// 而其中 Hijack 的实现错了会把一个连接搞坏。今天这批接口全是普通 JSON，用不到它们；这一段
// 是为了让将来某个 handler 想用 ResponseController 时不会失败得莫名其妙。
type responseRecorder struct {
	http.ResponseWriter
	status   int
	body     bytes.Buffer
	overflow bool
}

// Unwrap 让 http.ResponseController 能找到底层的 ResponseWriter。
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		// 没显式调过 WriteHeader 的首次 Write 等价于 200（net/http 的约定）。
		r.status = http.StatusOK
	}
	if !r.overflow {
		remaining := maxLoggedBodyBytes - r.body.Len()
		if remaining <= 0 {
			r.overflow = true
		} else if len(p) > remaining {
			r.body.Write(p[:remaining])
			r.overflow = true
		} else {
			r.body.Write(p)
		}
	}
	return r.ResponseWriter.Write(p)
}

// statusOrDefault 返回实际写出去的状态码；一个字节都没写时是 200（net/http 的约定）。
func (r *responseRecorder) statusOrDefault() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// captured 返回留档的响应体，被截断过时带上显式标记。
func (r *responseRecorder) captured() string {
	text := r.body.String()
	if r.overflow {
		return text + truncateMarker
	}
	return text
}

// truncate 按字节截断一个串，超出时追加标记。
//
// 按**字节**而不是按字符：这两个上限本来就是内存与行宽的量级，而按字符截断要在每个调用点
// 决定「一个中文字算几个」——那是个没有正确答案的问题（UTF-8 里 1 到 4 个字节）。
// 截断点可能落在一个多字节字符中间，所以这个函数只用于**日志**，绝不用于进待签串或
// 回给调用方的任何东西。
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + truncateMarker
}
