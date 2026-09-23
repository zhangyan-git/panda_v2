package config

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultUploadMaxFileSize 是 UPLOAD_MAX_FILE_SIZE 的缺省值（10MB）。
// 上限不在这里：那是上传子系统自己的规则，归 platform/upload 管。
const defaultUploadMaxFileSize = 10 << 20

type Config struct {
	ServiceName, Version, Environment                                              string
	HTTPAddress, GRPCAddress                                                       string
	RegistryEndpoint                                                               string
	RedisAddress                                                                   string
	RedisPassword                                                                  string
	RedisDB                                                                        int
	JWTSecret, JWTIssuer                                                           string
	AccountServiceURL, UserServiceURL, MerchantServiceURL, CoffeeMachineServiceURL string
	// ServiceDatabaseURL is the database this service owns, and it is required:
	// each owning service reads its own variable (see databaseEnvNames). A service
	// whose variable is missing fails Load rather than falling back to anything —
	// see resolveDatabase for why. Only a service that owns no database at all
	// (gateway-service) leaves this empty.
	//
	// **这里刻意没有** DatabaseURL / UserDatabaseURL / MerchantDatabaseURL /
	// OrderDatabaseURL 那四个字段。它们曾经存在，但从来没有一个调用方读过：服务一律读
	// 这一份，而那四个是「反正都在同一个库上」时代的残留。留着它们最坏的地方不是占内存，
	// 而是它们让「共享 DATABASE_URL」看起来像一份约定——回退的入口正是从那儿长出来的。
	// DATABASE_URL 这个**环境变量**仍然要留着：user-service/cmd/verify、cmd/seed 与
	// merchant-service/cmd/backfill-region 直接读它连身份库，它们不是服务，不在这条规则里。
	ServiceDatabaseURL string
	// MigrateOnStart applies this service's migration set during startup. It is
	// off by default and stays off in production, where the release process
	// migrates out of band; it exists for local stacks and new environments.
	MigrateOnStart bool
	// UserGRPCAddress and MerchantGRPCAddress are the static internal gRPC
	// endpoints. They are used until service discovery is enabled, and remain the
	// fallback afterwards.
	UserGRPCAddress, MerchantGRPCAddress string
	// CoffeeMachineGRPCAddress 是 order-service 问设备事实的地址（下单时校验设备状态，
	// 方案 5.8）。缺了它 order-service 无法启动：那一版里它是唯一一条服务端校验，
	// 没有它，「这台机器能不能出杯」就只剩调用方说了算。
	CoffeeMachineGRPCAddress string
	// PaymentGRPCAddress 是 order-service 发起支付时问 payment-service 的地址。
	// 创建支付不能走 MQ（客户端要立刻拿到支付参数，方案 7.2），所以它是一条同步
	// RPC；缺了它 order-service 就没法把订单金额交给支付服务，下单之后只能停在
	// pending_payment。
	PaymentGRPCAddress string
	// MembershipGRPCAddress 是 order-service 下单时问会员套餐的地址。带会员行的那一单，价格、
	// 时长、会员价路径都从会员域现取再冻进订单行（见 service.createOrder）：这些事实只有会员域
	// 有，收客户端填的那一份等于让客户端定价——一份 originalUnitPrice=1、快照写着年卡的请求
	// 能花一分钱开一年。缺了它 order-service 拒绝启动：那要到用户点「立即购买」才暴露。
	MembershipGRPCAddress string
	// OrderGRPCAddress 是 partner-service 记设备刷卡订单时问 order-service 的地址
	// （线下刷卡机，方案 §四）。
	//
	// 缺了它 partner-service 拒绝启动：设备回调是**先收钱、后建单**——钱已经在机器上收过
	// 了，只有把订单记下来这一步。地址缺席不是「少个可选依赖」，是那条回调会验完签之后
	// 建不出单，而合作方看到的是一句 5xx，它会一直重投。
	//
	// 反向不需要：order-service **不调** partner-service。设备回调的 HTTP 入口在
	// partner-service（/v1/openapi 那一棵树只有它一个上游），它验完签再走这条 RPC。
	OrderGRPCAddress string
	// PaymentNotifyBaseURL 是渠道回调 payment-service 的公开基址，拼在
	// /v1/payments/callback/{channelCode} 前面交给渠道。缺了它 payment-service 拒绝启动：
	// 空串会拼出一个相对路径，而相对路径发给渠道的后果是**回调永远到不了**——钱收了、
	// 支付单停在 pending，直到超时关单，用户被扣了款而订单被关掉。这个错误不会在任何
	// 一次本地测试里露头，只会在第一次上真渠道时炸，所以在这里就拦住。
	PaymentNotifyBaseURL string
	// PaymentReturnPageURL 是 H5 支付完成后我们**再送用户一程**的那个结果页地址，可选。
	//
	// 它是上面那条基址的兄弟，但两件事必须分开说清楚：
	//
	//   - 基址（NotifyBaseURL）拼出的是**我们自己的**回跳入口
	//     `<base>/v1/payments/return/{channelCode}`，那一段由代码拼、不进配置。
	//   - 这一项是**再往后一跳**：用户在我们这里验完签之后被送去哪儿。它是一张页面，
	//     通常不在 API 域名上（前端域名与接口域名是两个），所以没有共用基址这种取法。
	//
	// 里面的 `{merOrderId}` 占位符会被换成支付单号（见 service.returnPageURL）。
	//
	// **缺了它不拒绝启动**，与 PaymentNotifyBaseURL 的处置相反：那条缺了会静默丢钱
	// （回调永远到不了），这条缺了只是用户看完一句话自己退出去。把一条只影响观感的东西
	// 做成启动即失败，只会让本地和 CI 多一个非填不可的变量。
	PaymentReturnPageURL string
	// PaymentUMS* 是银联商务全民付那条渠道的**账户值**：根地址、应用标识、商户号、终端号、
	// 来源编号、消息来源域名。
	//
	// 它们曾经住在 payment_channels 那张表里，运营在后台一栏一栏地填（老系统对应
	// configs/config.yaml 的 `umspay:` 段）。那是为「同时接五家第三方渠道」准备的形状，
	// 而这个仓库只有银联商务一家、两条协议，这些值又是**这家公司在渠道侧登记的标识**、
	// 不是运营每天要动的数据——所以它们跟着部署走，跟着代码走。V2 没有 YAML 这一层，
	// 唯一的配置文件是 .env（见 loadDotEnv），所以它们读自 .env。
	//
	// # 为什么不在这里拒绝启动
	//
	// 一个只跑咖啡豆支付的部署根本用不到这一族。把「没接银联商务」做成「服务起不来」，
	// 代价大于收益。真正的拦截点在**用户选中银联商务那一刻**：那时报一句点名到环境变量的
	// 错，比启动时盲报一句「配置不全」有用得多（见 payment-service 的 internal/catalog）。
	//
	// # 为什么这组里没有两把密钥
	//
	// appKey（出站签名）与 commKey（入站验签）**刻意不进这个结构体**：它会被打印、会被
	// %+v 到日志里，密钥进去之后离泄漏只差一次调试。装配处只持有**变量名**，签名那一刻
	// 才 os.Getenv——与 PAYMENT_MANUAL_SECRET 走的是同一条路（见 payment-service 的
	// internal/provider/ums）。
	PaymentUMSBaseURL    string
	PaymentUMSAppID      string
	PaymentUMSMID        string
	PaymentUMSTID        string
	PaymentUMSSourceCode string
	PaymentUMSDomainName string
	// PaymentUMSSceneType / PaymentUMSMerAppName / PaymentUMSMerAppID 是**微信 H5** 那条路
	// 上的三项应用标识（应用类型 / 应用名称 / 应用标识），渠道文档标为必填。
	//
	// 它们是**这个应用自己的**标识，与上面那组渠道给的账户值不是一回事。三条留空是合法的：
	// 只有微信 H5 与「微信 H5 转小程序」两条路用得上，另外两条（支付宝、云闪付）带了反而是
	// 噪声——文档对支付宝那条明确写着「参数无效」。
	PaymentUMSSceneType  string
	PaymentUMSMerAppName string
	PaymentUMSMerAppID   string
	// WeChatPay* 是微信**直连**委托代扣（连续包月签约）那条通道的账户值：
	// 小程序 appid、商户号、根地址，以及商户证书与私钥的 PEM 路径。
	//
	// 它与上面那组 PAYMENT_UMS_* **不是一回事**：银联商务是收单通道（用户当场付钱），
	// 这一族是签约通道（用户授权以后每个月自动扣），两套商户号、两套密钥、两套签名口径，
	// 在 payment-service 里是两个适配器（见 internal/provider/wechatpay 的包注释）。
	// 用户已拍板：**签约也走微信直连，复用老系统那套商户号与实现方式**。
	//
	// # 为什么 appid 单独一行、不复用 WECHAT_MINIAPP_APP_ID
	//
	// 两个值在部署上今天是同一个（都是 V2 小程序），但**它们回答的问题不同**：上面那个回答
	// 「哪个小程序能拿用户身份换登录态」，这里回答「签约请求以哪个小程序的名义发出」。复用
	// 会让 payment-service 悄悄依赖 user-service 的一行凭据——有人轮换登录 appid 的那一刻，
	// 签约请求里的 appid 跟着变，而症状是微信回一句 SIGN_ERROR（看上去像签名算错了）。
	// 多一行必须填成同一个值的配置，比一个跨越两个服务的隐式耦合便宜。
	//
	// # 为什么这组里没有 APIv2 密钥
	//
	// 与上面那两把银联商务密钥同一条规矩：这个结构体会被打印、会被 %+v 到日志里。密钥
	// 只留**变量名**（见 catalog.Channel.SecretEnv），签名那一刻才 os.Getenv。
	//
	// # 为什么留空不拒绝启动
	//
	// 同 PaymentUMSBaseURL：一个不卖连续包月的部署不该因为没接微信而起不来。拦截点在
	// **选中这条通道那一刻**——签约请求会点名缺的是哪个变量（见 catalog 的 wechatPayChannel）。
	WeChatPayBaseURL  string
	WeChatPayAppID    string
	WeChatPayMchID    string
	WeChatPayCertPath string
	WeChatPayKeyPath  string
	// WeChatPaySignMiniProgramAppID 是**微信官方的签约小程序**的 appid —— 用户在它里面点
	// 「同意并签约」，签完跳回我们的小程序。它与 WeChatPayAppID 是两回事：那个是我们自己的
	// 小程序，待签串以它的名义发出；这个只是跳转目标。
	//
	// 它曾经写死在适配器里（wechatpay.SignMiniProgramAppID），理由是「微信侧的固定值，不是
	// 部署事实」。那条理由站不住：它是一串**能定位到具体主体的标识**，与商户号同一性质，写进
	// 源码就是把它发到了每一个拿到仓库的人手上（GitHub 的密钥扫描正是这么判的）。协议固定
	// 不等于可以公开——取值仍然只有那一个，但它归部署配置管。
	WeChatPaySignMiniProgramAppID string
	// PartnerSecretKey 是加密合作方签名密钥的主密钥（AES-256-GCM，见 platform/secret）。
	// 只有 partner-service 读它，且**读不到就拒绝启动**（见下面那条校验）。
	//
	// 库里一旦有密文，缺了它就再也解不开，
	// 而解不开的表现是**每一个合作方的每一次调用都验签失败**——日志里只有「签名不对」，
	// 排查会从合作方那边找起，而问题在我们自己的配置里。格式（32 字节，base64 或 64 位
	// hex）同样由 platform/secret 的 Parse 在装配处校验，本包只判非空。
	PartnerSecretKey string
	// PartnerTrustedProxyCIDRs 是 partner-service 认的**可信代理网段**，逗号分隔的 CIDR
	// （或裸地址）列表。它只回答一个问题：判定合作方 IP 白名单时，能不能读
	// X-Forwarded-For。
	//
	// 为什么 partner-service 需要它而别的服务不需要：合作方永远是从网关进来的，所以
	// RemoteAddr 是网关自己；不配这一项时白名单里写的任何地址都匹配不上，结果是**全拒**
	// （失败关闭，见 migrations/partner 与 internal/ingress/auth.go）。反过来，把 XFF
	// 无条件下信也不行——那是请求头，谁都能写，白名单会变成一句空话。
	//
	// 留空是**合法**的：它等于「没有可信代理，只看 RemoteAddr」，也就是本地直连与
	// 「网关就是唯一入口且用 RemoteAddr 判」这两种拓扑的正确取值。
	PartnerTrustedProxyCIDRs string
	// AccountGRPCAddress 是 account-service 的 gRPC 地址。三个调用方：lottery-service 问它
	// 扣福卡/冲正/读余额，payment-service 在账户出资（纯咖啡豆）时问它扣豆，order-service
	// 在受理退款申请前问它「这一单的福卡现在冻得上吗」。三个都在下面 required-address
	// 那一段里拒绝空地址。
	AccountGRPCAddress         string
	MerchantInternalToken      string
	MerchantOwnershipTimeoutMS int
	// AuthorizationTimeoutMS bounds one live authorization lookup. It is a
	// per-request budget, not a cache TTL: services that use it re-check the
	// caller's grants on every admin request.
	AuthorizationTimeoutMS                   int
	DevAccountInitEnabled                    bool
	DevAdminUsername, DevAdminPassword       string
	DevMerchantUsername, DevMerchantPassword string
	// HTTPTimeoutMS 是 kratos HTTP server 的整请求超时。kratos 在 mux filter 里
	// 装的是一个包住整个请求（含读 body）的 context deadline，所以它同时是
	// 一次上传的总预算，而不是某个处理步骤的预算。
	HTTPTimeoutMS int
	// 以下五项是图片上传到 OSS 的配置。键名沿用旧后端 panda_serve，
	// 「把旧配置搬过来」因此就是原样抄四行。只有 merchant-service 用得上，
	// 其余服务读到空值也无所谓：Load 不会因为它们缺席而失败。
	OSSAccessKey, OSSSecretKey, OSSEndpoint, OSSBucket, OSSCNAME string
	UploadPath                                                   string
	UploadMaxFileSize                                            int64
	UploadUseMD5                                                 bool
	// 以下三项是微信小程序登录凭据，只有 user-service 用得上。
	// 缺席不是错误：dev 栈没有小程序账号也能起，微信登录接口会回 503 并说明
	// 缺的是哪一项——比启动失败好，因为其余接口都还能用。
	WechatMiniappAppID, WechatMiniappSecret, WechatMiniappAPIBase string
	// SmsDevLogCodes 用「把验证码写进日志」顶替真实短信通道，只在 PANDA_ENV=dev
	// 下被接受（见 user-service 的 main.go）。默认关闭：宁可接口回 503，也不要
	// 任何一个没配短信通道的环境悄悄把验证码写进日志。
	SmsDevLogCodes bool
}

func Load(service string) (Config, error) {
	if err := loadDotEnv(); err != nil {
		return Config{}, err
	}
	env := strings.TrimSpace(os.Getenv("PANDA_ENV"))
	if env == "" {
		return Config{}, fmt.Errorf("PANDA_ENV is required")
	}
	version := os.Getenv("SERVICE_VERSION")
	if version == "" {
		version = "0.1.0"
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	grpcAddr := os.Getenv("GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9090"
	}
	registryEndpoint, err := registryEndpointFromEnv()
	if err != nil {
		return Config{}, err
	}
	redisDB, err := parseRedisDB(os.Getenv("REDIS_DB"))
	if err != nil {
		return Config{}, err
	}
	ownershipTimeout := 5000
	if service == "user-service" {
		ownershipTimeout, err = parseIntEnv("MERCHANT_OWNERSHIP_TIMEOUT_MS", 5000)
		if err != nil {
			return Config{}, err
		}
		if ownershipTimeout < 1000 || ownershipTimeout > 30000 {
			return Config{}, fmt.Errorf("MERCHANT_OWNERSHIP_TIMEOUT_MS must be between 1000 and 30000")
		}
	}
	// 实时鉴权的单次查询预算。默认 2 秒：一次本地 gRPC 往返的正常耗时是毫秒级，
	// 2 秒已经足够容忍抖动，又短到不至于让一个卡住的请求长时间占住连接。超过这个
	// 预算即失败关闭（503），不会退回 token 里的权限快照。
	authorizationTimeout := 2000
	if service == "coupon-service" {
		authorizationTimeout, err = parseIntEnv("AUTHZ_TIMEOUT_MS", 2000)
		if err != nil {
			return Config{}, err
		}
		if authorizationTimeout < 500 || authorizationTimeout > 30000 {
			return Config{}, fmt.Errorf("AUTHZ_TIMEOUT_MS must be between 500 and 30000")
		}
	}
	// 整请求超时按服务名给代码默认值：5 秒够普通 JSON 接口，装不下一次 10MB 的
	// 图片上传。默认值必须写在代码里而不是 .env —— loadDotEnv 从 CWD 向上只找
	// 一个 .env，一个变量表达不了「merchant 90 秒、其余 5 秒」；本机 .env 里没这个
	// 键时，开发者会拿到 5 秒，然后报「上传坏了」。
	//
	// 放宽 merchant-service 会连着放宽它所有路由，包括实时鉴权那次 RPC——kratos
	// 没有按路由设 deadline 的办法。90 秒是三层预算的中间层：
	// 网关 120s > 服务端 90s > OSS 单次读写 60s。
	httpTimeoutMS := 5000
	if service == "merchant-service" {
		httpTimeoutMS = 90000
	}
	httpTimeoutMS, err = parseIntEnv("HTTP_TIMEOUT_MS", httpTimeoutMS)
	if err != nil {
		return Config{}, err
	}
	if httpTimeoutMS < 1000 || httpTimeoutMS > 300000 {
		return Config{}, fmt.Errorf("HTTP_TIMEOUT_MS must be between 1000 and 300000")
	}
	uploadMaxFileSize, err := parseIntEnv("UPLOAD_MAX_FILE_SIZE", defaultUploadMaxFileSize)
	if err != nil {
		return Config{}, err
	}
	// UPLOAD_USE_MD5 留着只为兼容旧配置：上传代码始终用内容寻址的 md5 键，
	// 设成 false 不会有任何效果，只会在启动时换来一条警告（见 platform/upload）。
	uploadUseMD5, err := parseBoolEnvWithDefault("UPLOAD_USE_MD5", true)
	if err != nil {
		return Config{}, err
	}
	merchantToken := os.Getenv("MERCHANT_INTERNAL_TOKEN")
	// merchant-service 自己要用它验内部调用；coffee-machine-service 要用它去问点位；
	// order-service 要用它去读设备（下单时校验设备状态）；payment-service 要用它验
	// order-service 发起支付的那次调用；account-service 要用它验将来扣减福卡的那次
	// 调用（那个调用方现在是 lottery-service，见下）。
	//
	// 发送方和服务方一个标准：服务方（merchant-service、coffee-machine-service、
	// payment-service、account-service）拒绝短于 32 字节的配置，所以任何一个能通过
	// 校验的令牌都至少是 32 字节。发送方这边配短了不是「一个更弱的令牌」——它压根
	// 匹配不上，每一次调用都会被判 401。在启动时拦下来，比让每一单饮品都回 503 好。
	// lottery-service 也在发送方这一列：它带着这个令牌去调 account-service 扣福卡。
	//
	// membership-service 在**服务方**那一列：它的 gRPC 面只有 order-service 与
	// coffee-machine-service 会用服务令牌来问「这个用户现在算不算会员价」。令牌缺席或太短
	// 时 UnaryServerInterceptor 一律回 UNAUTHENTICATED（它不区分「没配」与「配错了」），
	// 于是每一次资格查询都失败——下游看到的是 5xx，而本服务日志里只有一行 401，隔了一层
	// 很难对上。本服务**不**带这个令牌出向调用任何东西，服务方与发送方在这里是两件事，
	// 而这一条管的是前者。
	// partner-service 也在发送方这一列：它带着这个令牌去问 membership-service「这个用户
	// 此刻算不算会员价」。那条 RPC 的服务端（membership-service/rpc）第一行就是
	// auth.RequireService，缺席或太短的令牌在那里一律 UNAUTHENTICATED，而调用方看到的是
	// 一次 5xx——正是本段开头那条「启动时拦下来」的理由。
	if (service == "merchant-service" || service == "coffee-machine-service" || service == "order-service" || service == "payment-service" || service == "account-service" || service == "lottery-service" || service == "membership-service" || service == "partner-service") && len([]byte(merchantToken)) < 32 {
		return Config{}, fmt.Errorf("MERCHANT_INTERNAL_TOKEN must be at least 32 bytes for %s", service)
	}
	userGRPCAddr := strings.TrimSpace(os.Getenv("USER_GRPC_ADDR"))
	merchantGRPCAddr := strings.TrimSpace(os.Getenv("MERCHANT_GRPC_ADDR"))
	coffeeMachineGRPCAddr := strings.TrimSpace(os.Getenv("COFFEE_MACHINE_GRPC_ADDR"))
	paymentGRPCAddr := strings.TrimSpace(os.Getenv("PAYMENT_GRPC_ADDR"))
	membershipGRPCAddr := strings.TrimSpace(os.Getenv("MEMBERSHIP_GRPC_ADDR"))
	orderGRPCAddr := strings.TrimSpace(os.Getenv("ORDER_GRPC_ADDR"))
	accountGRPCAddr := strings.TrimSpace(os.Getenv("ACCOUNT_GRPC_ADDR"))
	paymentNotifyBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PAYMENT_NOTIFY_BASE_URL")), "/")
	// 只 TrimSpace、**不 TrimRight "/"**：这一项是一整条地址（可能带查询串，比如
	// `https://h5.example.com/pay/result?no={merOrderId}`），结尾那个斜杠是它自己的形状。
	// 与上面那条相反——那条是拿来做前缀拼接的，两边都带斜杠会拼出 `//v1/...`。
	paymentReturnPageURL := strings.TrimSpace(os.Getenv("PAYMENT_RETURN_PAGE_URL"))
	// 银联商务那组账户值与三项应用标识。**只读不校验**：它们全是可选的，理由见
	// PaymentUMSBaseURL 的注释（没接这一族的部署不该起不来）。baseURL 为空时由 ums 包
	// 回落到生产地址——那个默认值属于适配器，不属于部署配置。
	paymentUMSBaseURL := strings.TrimSpace(os.Getenv("PAYMENT_UMS_BASE_URL"))
	paymentUMSAppID := strings.TrimSpace(os.Getenv("PAYMENT_UMS_APP_ID"))
	paymentUMSMID := strings.TrimSpace(os.Getenv("PAYMENT_UMS_MID"))
	paymentUMSTID := strings.TrimSpace(os.Getenv("PAYMENT_UMS_TID"))
	paymentUMSSourceCode := strings.TrimSpace(os.Getenv("PAYMENT_UMS_SOURCE_CODE"))
	paymentUMSDomainName := strings.TrimSpace(os.Getenv("PAYMENT_UMS_DOMAIN_NAME"))
	paymentUMSSceneType := strings.TrimSpace(os.Getenv("PAYMENT_UMS_SCENE_TYPE"))
	paymentUMSMerAppName := strings.TrimSpace(os.Getenv("PAYMENT_UMS_MER_APP_NAME"))
	paymentUMSMerAppID := strings.TrimSpace(os.Getenv("PAYMENT_UMS_MER_APP_ID"))
	// 微信直连委托代扣那组（见 Config.WeChatPayBaseURL）。同样**只读不校验**。
	// 证书两项是**文件路径**：读不到文件要到真正发起签约/解约那一刻才知道，这里只搬路径。
	weChatPayBaseURL := strings.TrimSpace(os.Getenv("WECHAT_PAY_BASE_URL"))
	weChatPayAppID := strings.TrimSpace(os.Getenv("WECHAT_PAY_APP_ID"))
	weChatPayMchID := strings.TrimSpace(os.Getenv("WECHAT_PAY_MCH_ID"))
	weChatPayCertPath := strings.TrimSpace(os.Getenv("WECHAT_PAY_CERT_PATH"))
	weChatPayKeyPath := strings.TrimSpace(os.Getenv("WECHAT_PAY_KEY_PATH"))
	weChatPaySignMiniProgramAppID := strings.TrimSpace(os.Getenv("WECHAT_PAY_SIGN_MINI_PROGRAM_APP_ID"))
	// Each service now reaches its peer over gRPC, so it needs that peer's
	// address rather than its own. MERCHANT_SERVICE_URL is deliberately no longer
	// required here: user-service stopped calling merchant-service over HTTP, and
	// the variable now belongs to the gateway alone.
	if service == "user-service" {
		if merchantGRPCAddr == "" {
			return Config{}, fmt.Errorf("MERCHANT_GRPC_ADDR is required for user-service")
		}
		if len([]byte(merchantToken)) < 32 {
			return Config{}, fmt.Errorf("MERCHANT_INTERNAL_TOKEN must be at least 32 bytes for user-service")
		}
	}
	// 这几个服务都按请求调用 user-service 取实时授权，所以都要地址。
	// account-service 在其中：后台查福卡账户与流水是要权限码的（account:read），
	// 授权在每个请求上现取，没有缓存可依赖。
	// membership-service 也在其中（membership:read / manage / adjust 三枚）。
	// payment-service 同理（payment:read 一枚，见 migrations/identity 与 routes/admin.go）。
	// 它也是**最后一个**接进来的服务——在此之前它的后台面根本不存在，所以它不在名单上不是
	// 因为「不需要」，是因为「还没有」。
	// partner-service 同理（partner:read / partner:manage 两枚，见 migrations/identity
	// 与 internal/routes/admin.go）。它同时也在上面那条令牌发送方链里——但两条链的理由不同：
	// 这条是「后台要现取授权」，那条是「它要去调别人的 gRPC 面」。
	if (service == "merchant-service" || service == "coupon-service" || service == "coffee-machine-service" || service == "order-service" || service == "account-service" || service == "lottery-service" || service == "membership-service" || service == "payment-service" || service == "partner-service") && userGRPCAddr == "" {
		return Config{}, fmt.Errorf("USER_GRPC_ADDR is required for %s", service)
	}
	// order-service 还要 coffee-machine-service：下单时校验设备（在不在、启没启用、
	// 点位对不对，方案 5.8）。和上面那条同样的理由——地址缺席不是「少个可选依赖」，
	// 校验会整条失效，所以在这里拒绝启动，而不是等到第一次下单。
	if service == "order-service" && coffeeMachineGRPCAddr == "" {
		return Config{}, fmt.Errorf("COFFEE_MACHINE_GRPC_ADDR is required for order-service")
	}
	// order-service 还要 payment-service：发起支付那条链路（方案 7.2）是它编排的，
	// 金额与归属在订单侧校验完再交给支付服务建单。地址缺席不是「少个可选依赖」——
	// 缺了它订单建得出来却付不了款，只能停在 pending_payment，所以在这里拒绝启动。
	if service == "order-service" && paymentGRPCAddr == "" {
		return Config{}, fmt.Errorf("PAYMENT_GRPC_ADDR is required for order-service")
	}
	// order-service 还要 membership-service：带会员行的那一单，价格、时长与会员价路径**只能**
	// 从会员域取（下单选套餐就是它在卖的东西）。地址缺席不是「少个可选依赖」——缺了它，
	// 客户端说自己买的是什么就是什么，一份 originalUnitPrice=1、快照写着年卡的请求能花一分钱
	// 开一年会员，所以在这里拒绝启动。
	if service == "order-service" && membershipGRPCAddr == "" {
		return Config{}, fmt.Errorf("MEMBERSHIP_GRPC_ADDR is required for order-service")
	}
	// order-service 还要 account-service：受理退款申请之前要问一句「这一单赠送的福卡现在还
	// 冻得上吗」（一张都没被用过才允许申请），问不到就不能受理。地址缺席不是「少个可选依赖」
	// ——把问不到当成「那就放行」等于这条规则在账户域抖动时静默失效，而它挡的是「卡已经抽掉
	// 了还想退钱」，失效的代价是把追不回来的卡退出去。所以在这里拒绝启动，与上面几条同一个
	// 理由：空地址不会在启动时说话，会在用户点「申请退款」那一刻变成一个 5xx。
	if service == "order-service" && accountGRPCAddr == "" {
		return Config{}, fmt.Errorf("ACCOUNT_GRPC_ADDR is required for order-service")
	}
	// payment-service 要的是自己的**回调基址**而不是谁的地址：它得把这个 URL 交给渠道，
	// 渠道照着它回调。缺了它支付单建得出来、钱也可能收得到，但结果永远回不到我们这边
	// （见 PaymentNotifyBaseURL 的注释），所以在这里拒绝启动。
	if service == "payment-service" && paymentNotifyBaseURL == "" {
		return Config{}, fmt.Errorf("PAYMENT_NOTIFY_BASE_URL is required for payment-service")
	}
	// **没有 PAYMENT_SECRET_KEY 这一项**：支付渠道的凭据不再入库。从前它们是密文存在
	// payment_channels.config 里的，于是需要一个主密钥来解；那张表连同「渠道是数据」这件事
	// 一起没了，凭据改成按名字读环境变量（见 payment-service 的 catalog.Channel.SecretEnv），
	// 主密钥因此成了一件不需要存在的东西。它的用途只到这里为止，别的服务本来就不读它。
	// payment-service 还要 account-service：账户出资（纯咖啡豆）那条路要当场扣余额，扣不了
	// 就不能把支付单推进成功。地址缺席不是「少个可选依赖」——空地址会在**用户选了豆支付
	// 那一刻**才炸（连不上账户域，5xx），而不是在启动时；那正是 account-service 对
	// USER_GRPC_ADDR 的那条理由，所以同样在这里拒绝启动。
	if service == "payment-service" && accountGRPCAddr == "" {
		return Config{}, fmt.Errorf("ACCOUNT_GRPC_ADDR is required for payment-service")
	}
	// partner-service 还要 membership-service：开放接口里那条「会员权益查询」只是一层转发，
	// 事实全在会员域。与上面几条同一个理由——空地址不会在启动时说话，会在**合作方第一次
	// 调这个接口**时变成一个 5xx，而那时排查的人手上只有合作方的一个工单。
	//
	// 另两条开放接口（订单查询 / 优惠券发放）**今天没有对应的 RPC**，所以不要求
	// ORDER_GRPC_ADDR / COUPON_GRPC_ADDR：要求一个用不上的地址，只会让部署配置多一行
	// 没人知道为什么在那里的变量。它们落地时在这里补。
	if service == "partner-service" && membershipGRPCAddr == "" {
		return Config{}, fmt.Errorf("MEMBERSHIP_GRPC_ADDR is required for partner-service")
	}
	// partner-service 还要 order-service：设备刷卡回调（方案 §四）在它这里验完签，再走这条
	// RPC 把订单记下来。地址缺席不是「少个可选依赖」——那条回调是**先收钱、后建单**，钱已经
	// 在机器上收过了，建不出单就是一笔收了钱却没有单的交易，而合作方看到 5xx 会一直重投。
	if service == "partner-service" && orderGRPCAddr == "" {
		return Config{}, fmt.Errorf("ORDER_GRPC_ADDR is required for partner-service")
	}
	partnerSecretKey := strings.TrimSpace(os.Getenv("PARTNER_SECRET_KEY"))
	// partner-service 还要加密签名密钥的主密钥。理由见 PartnerSecretKey 的注释：库里一旦
	// 有密文，缺了它就解不开，而表现是每个合作方的每一次调用都验签失败——与「合作方算错了
	// 签名」无从区分。这里只判非空，格式由 platform/secret 在装配处校验。
	if service == "partner-service" && partnerSecretKey == "" {
		return Config{}, fmt.Errorf("PARTNER_SECRET_KEY is required for partner-service")
	}
	// lottery-service 还要 account-service：一次参与就是当场扣一张福卡，扣不了就不能把这次
	// 参与计进期次。地址缺席不是「少个可选依赖」——空地址会在**用户点「参与抽奖」那一刻**
	// 才炸（连不上账户域，5xx），而不是在启动时；这正是上面 account-service 对 USER_GRPC_ADDR、
	// payment-service 对它自己的那份理由，所以同样在这里拒绝启动。
	if service == "lottery-service" && accountGRPCAddr == "" {
		return Config{}, fmt.Errorf("ACCOUNT_GRPC_ADDR is required for lottery-service")
	}
	// coffee-machine-service 还要 merchant-service：设备挂点位之前，这个点位存不存在、
	// 还能不能用，只有商户服务说了算（见 client.StoreResolver）。地址缺席不是「少个
	// 可选依赖」——校验会整条失效，所以在这里就拒绝启动，而不是等到第一次保存设备。
	if service == "coffee-machine-service" && merchantGRPCAddr == "" {
		return Config{}, fmt.Errorf("MERCHANT_GRPC_ADDR is required for coffee-machine-service")
	}
	// lottery-service 也要 merchant-service，而且是两条路都要：开通前问一次「这家店存在吗」
	// （问不出来就不受理，否则会留下一条永远查无此店的开通记录），以及每次读列表时解一页
	// 门店名——抽奖库只存门店 id（migrations/lottery），名字是现场问来的。地址缺席不是
	// 「少个可选依赖」：前者会让校验整条失效，后者会让每个列表页的门店名都是空的。
	if service == "lottery-service" && merchantGRPCAddr == "" {
		return Config{}, fmt.Errorf("MERCHANT_GRPC_ADDR is required for lottery-service")
	}
	// membership-service 也要 merchant-service，同样是两条路：后台开通会员前问一次「这家店
	// 存在吗」（问不出来就不开通，否则会留下一条查无此店的**归属门店**），以及每次读会员列表
	// 与详情时解一页门店名——会员库只存门店 id（migrations/membership），名字是现场问来
	// 的。地址缺席不是「少个可选依赖」：前者会让那次校验整条失效，后者会让归属门店一列全是空。
	if service == "membership-service" && merchantGRPCAddr == "" {
		return Config{}, fmt.Errorf("MERCHANT_GRPC_ADDR is required for membership-service")
	}
	// membership-service 还要 payment-service：连续包月的签约（发起、回渠道核、解约）全在那一侧，
	// 本服务只决定「谁、什么时候、签哪一款套餐」。地址缺席不是「少个可选依赖」——签约那条路整条
	// 会不可用，而**没有签约就扣不了款**：开一条永远扣不到钱的订阅比没有订阅更糟，用户会以为续上了。
	if service == "membership-service" && paymentGRPCAddr == "" {
		return Config{}, fmt.Errorf("PAYMENT_GRPC_ADDR is required for membership-service")
	}
	// membership-service 还要 order-service：一期代扣**成功之后**要先把这一期记成一张订单
	// （订单管理里看得见、下游 coupon-service 拿订单号才发会员价券），再在本库续这一期会员。
	// 地址缺席不是「少个可选依赖」——钱已经在渠道那边扣走了，而这一期记不成账：那条事件会一直
	// 重投到死信，用户看到的是「钱扣了、券没发、订单里也没有这笔」。与上面 payment 那条同一个
	// 理由，只是换了一个域。
	//
	// 顺带说明**另一条**用得上它的路：后台订阅详情里「首月支付信息」那一段（first_payment_order_id
	// → 订单 → 支付单 → 渠道流水）。那一条是只读展示，坏了只让那一块空着；真正要求这个地址的
	// 是上面那条写路径。
	if service == "membership-service" && orderGRPCAddr == "" {
		return Config{}, fmt.Errorf("ORDER_GRPC_ADDR is required for membership-service")
	}
	// 自有库在这里定下来。**不回退**：读不到就拒绝启动，而不是悄悄连上别人那个库。
	// 因此这里不再逐个读那五个 *_DATABASE_URL：谁读哪一份由 resolveDatabase 那张
	// 表说了算，多一处平行读取就多一处能跟它走偏的地方。
	serviceDatabaseURL, err := resolveDatabase(service)
	if err != nil {
		return Config{}, err
	}
	return Config{
		ServiceName:                   service,
		Version:                       version,
		Environment:                   env,
		HTTPAddress:                   addr,
		GRPCAddress:                   grpcAddr,
		RegistryEndpoint:              registryEndpoint,
		ServiceDatabaseURL:            serviceDatabaseURL,
		MigrateOnStart:                parseBoolEnv("DB_MIGRATE_ON_START"),
		RedisAddress:                  os.Getenv("REDIS_ADDR"),
		RedisPassword:                 os.Getenv("REDIS_PASSWORD"),
		RedisDB:                       redisDB,
		JWTSecret:                     os.Getenv("JWT_SECRET"),
		JWTIssuer:                     os.Getenv("JWT_ISSUER"),
		AccountServiceURL:             os.Getenv("ACCOUNT_SERVICE_URL"),
		UserServiceURL:                os.Getenv("USER_SERVICE_URL"),
		MerchantServiceURL:            os.Getenv("MERCHANT_SERVICE_URL"),
		UserGRPCAddress:               userGRPCAddr,
		MerchantGRPCAddress:           merchantGRPCAddr,
		CoffeeMachineGRPCAddress:      coffeeMachineGRPCAddr,
		PaymentGRPCAddress:            paymentGRPCAddr,
		MembershipGRPCAddress:         membershipGRPCAddr,
		OrderGRPCAddress:              orderGRPCAddr,
		PaymentNotifyBaseURL:          paymentNotifyBaseURL,
		PaymentReturnPageURL:          paymentReturnPageURL,
		PaymentUMSBaseURL:             paymentUMSBaseURL,
		PaymentUMSAppID:               paymentUMSAppID,
		PaymentUMSMID:                 paymentUMSMID,
		PaymentUMSTID:                 paymentUMSTID,
		PaymentUMSSourceCode:          paymentUMSSourceCode,
		PaymentUMSDomainName:          paymentUMSDomainName,
		PaymentUMSSceneType:           paymentUMSSceneType,
		PaymentUMSMerAppName:          paymentUMSMerAppName,
		PaymentUMSMerAppID:            paymentUMSMerAppID,
		WeChatPayBaseURL:              weChatPayBaseURL,
		WeChatPayAppID:                weChatPayAppID,
		WeChatPayMchID:                weChatPayMchID,
		WeChatPayCertPath:             weChatPayCertPath,
		WeChatPayKeyPath:              weChatPayKeyPath,
		WeChatPaySignMiniProgramAppID: weChatPaySignMiniProgramAppID,
		PartnerSecretKey:              partnerSecretKey,
		PartnerTrustedProxyCIDRs:      os.Getenv("PARTNER_TRUSTED_PROXY_CIDRS"),
		AccountGRPCAddress:            strings.TrimSpace(os.Getenv("ACCOUNT_GRPC_ADDR")),
		MerchantInternalToken:         merchantToken,
		MerchantOwnershipTimeoutMS:    ownershipTimeout,
		AuthorizationTimeoutMS:        authorizationTimeout,
		HTTPTimeoutMS:                 httpTimeoutMS,
		// 裸值透传：UPLOAD_PATH 为空时由 platform/upload 用它的 DefaultPrefix
		// 兜底，默认值只有一处定义。OSSCNAME 留空则从 OSS_ENDPOINT 推公开基址。
		OSSAccessKey:            os.Getenv("OSS_ACCESS_KEY"),
		OSSSecretKey:            os.Getenv("OSS_SECRET_KEY"),
		OSSEndpoint:             os.Getenv("OSS_ENDPOINT"),
		OSSBucket:               os.Getenv("OSS_BUCKET"),
		OSSCNAME:                os.Getenv("OSS_CNAME"),
		UploadPath:              strings.TrimSpace(os.Getenv("UPLOAD_PATH")),
		UploadMaxFileSize:       int64(uploadMaxFileSize),
		UploadUseMD5:            uploadUseMD5,
		WechatMiniappAppID:      strings.TrimSpace(os.Getenv("WECHAT_MINIAPP_APP_ID")),
		WechatMiniappSecret:     strings.TrimSpace(os.Getenv("WECHAT_MINIAPP_APP_SECRET")),
		WechatMiniappAPIBase:    strings.TrimSpace(os.Getenv("WECHAT_MINIAPP_API_BASE")),
		SmsDevLogCodes:          parseBoolEnv("SMS_DEV_LOG_CODES"),
		CoffeeMachineServiceURL: os.Getenv("COFFEE_MACHINE_SERVICE_URL"),
		DevAccountInitEnabled:   parseBoolEnv("DEV_ACCOUNT_INIT_ENABLED"),
		DevAdminUsername:        os.Getenv("DEV_ADMIN_USERNAME"),
		DevAdminPassword:        os.Getenv("DEV_ADMIN_PASSWORD"),
		DevMerchantUsername:     os.Getenv("DEV_MERCHANT_USERNAME"),
		DevMerchantPassword:     os.Getenv("DEV_MERCHANT_PASSWORD"),
	}, nil
}

// databaseEnvNames maps a service to the environment variable holding the
// database it owns. user-service owns the identity database, which is why its
// entry points at USER_DATABASE_URL and not at something named "identity".
//
// The table is exhaustive on purpose: a service that is not in it and not in
// servicesWithoutDatabase fails to start. It used to fall back to the shared
// DATABASE_URL instead, and in the dev stack that is the identity database —
// so the failure looked like working code writing to the wrong database rather
// than like a misconfiguration. That is the shape of bug this table exists to
// make impossible; register the service in the same change that adds it.
var databaseEnvNames = map[string]string{
	"user-service":           "USER_DATABASE_URL",
	"merchant-service":       "MERCHANT_DATABASE_URL",
	"coupon-service":         "COUPON_DATABASE_URL",
	"coffee-machine-service": "COFFEE_MACHINE_DATABASE_URL",
	"order-service":          "ORDER_DATABASE_URL",
	"payment-service":        "PAYMENT_DATABASE_URL",
	"account-service":        "ACCOUNT_DATABASE_URL",
	"lottery-service":        "LOTTERY_DATABASE_URL",
	"membership-service":     "MEMBERSHIP_DATABASE_URL",
	"partner-service":        "PARTNER_DATABASE_URL",
}

// servicesWithoutDatabase is the explicit allowlist of services that own no
// database, so leaving ServiceDatabaseURL empty for them is the correct answer
// and not a missing configuration.
//
// Deliberately an allowlist rather than "absent from databaseEnvNames means no
// database": the latter passes silently the moment someone adds a service and
// forgets to register it, which is exactly the failure being fixed here.
var servicesWithoutDatabase = map[string]bool{
	"gateway-service": true,
}

// resolveDatabase returns the database this service owns: the value of its own
// variable, or the empty string for a service that owns no database.
//
// There is no fallback. A missing or blank variable is an error rather than a
// shared default, because a service silently pointed at another service's
// database does not fail — it migrates its tables into the wrong database and
// serves reads and writes from it, and nothing surfaces until someone notices
// the data is in the wrong place. Refusing to start turns that into a message
// naming the variable, at the only moment it is cheap to fix.
func resolveDatabase(service string) (string, error) {
	if servicesWithoutDatabase[service] {
		return "", nil
	}
	envName, ok := databaseEnvNames[service]
	if !ok {
		return "", fmt.Errorf("unknown service %q: register its database in databaseEnvNames", service)
	}
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return "", fmt.Errorf("%s is required for %s", envName, service)
	}
	return value, nil
}

// loadDotEnv loads the first .env found from the current directory upward.
// Existing process variables always take precedence over values in the file.
func loadDotEnv() error {
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("find .env: %w", err)
	}
	for {
		path := filepath.Join(dir, ".env")
		if _, err := os.Stat(path); err == nil {
			return parseDotEnv(path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read .env: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func parseDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open .env: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return fmt.Errorf("invalid .env entry on line %d", lineNumber)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value, err = strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf("invalid .env value on line %d: %w", lineNumber, err)
			}
		} else if index := strings.Index(value, " #"); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set .env variable %q: %w", key, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	return nil
}

// registryEndpointFromEnv reads the etcd endpoints from REGISTRY_ENDPOINT,
// falling back to the older ETCD_ENDPOINTS name.
//
// The two names disagreeing was a real defect, not a cosmetic one: the deploy
// template shipped ETCD_ENDPOINTS while the code only ever read
// REGISTRY_ENDPOINT, so a stack that configured etcd correctly still ran with
// the no-op registry — services never registered and discovery never resolved.
// Both names are honoured now, and the legacy one is still accepted so existing
// deployments do not break, but it warns: a silently ignored setting is exactly
// the failure this function exists to end.
func registryEndpointFromEnv() (string, error) {
	if endpoint := strings.TrimSpace(os.Getenv("REGISTRY_ENDPOINT")); endpoint != "" {
		return endpoint, nil
	}
	legacy := strings.TrimSpace(os.Getenv("ETCD_ENDPOINTS"))
	if legacy == "" {
		return "", nil
	}
	slog.Warn("ETCD_ENDPOINTS is deprecated and was moved to REGISTRY_ENDPOINT; rename the setting to silence this warning",
		"deprecated", "ETCD_ENDPOINTS", "replacement", "REGISTRY_ENDPOINT")
	return legacy, nil
}

func parseBoolEnv(key string) bool {
	value, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && parsed
}

// parseBoolEnvWithDefault 与 parseBoolEnv 相同，区别是缺省值可指定，
// 并且把写错的值当成错误而不是静默当成 false —— 一个拼错的开关被当成
// 「关」是最难查的一类问题。
func parseBoolEnvWithDefault(key string, defaultValue bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return parsed, nil
}

func parseIntEnv(key string, defaultValue int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func parseRedisDB(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	db, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid REDIS_DB %q: %w", value, err)
	}
	if db < 0 {
		return 0, fmt.Errorf("REDIS_DB must not be negative: %d", db)
	}
	return db, nil
}
