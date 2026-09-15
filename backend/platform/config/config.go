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
	DatabaseURL, RedisAddress                                                      string
	RedisPassword                                                                  string
	RedisDB                                                                        int
	JWTSecret, JWTIssuer                                                           string
	AccountServiceURL, UserServiceURL, MerchantServiceURL, CoffeeMachineServiceURL string
	// UserDatabaseURL and MerchantDatabaseURL are the per-service databases
	// introduced by the split. Each falls back to DatabaseURL, so a stack that
	// still runs on one database starts unchanged.
	UserDatabaseURL, MerchantDatabaseURL string
	// OrderDatabaseURL is order-service's database, kept alongside the two above
	// for the callers that read a specific service's database rather than their
	// own. A service that owns a database reads ServiceDatabaseURL.
	OrderDatabaseURL string
	// ServiceDatabaseURL is the database this service owns: user-service reads
	// UserDatabaseURL, merchant-service reads MerchantDatabaseURL, order-service
	// reads OrderDatabaseURL, and every other service (including the gateway,
	// which owns none) reads DatabaseURL.
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
	// PaymentNotifyBaseURL 是渠道回调 payment-service 的公开基址，拼在
	// /v1/payments/callback/{channelCode} 前面交给渠道。缺了它 payment-service 拒绝启动：
	// 空串会拼出一个相对路径，而相对路径发给渠道的后果是**回调永远到不了**——钱收了、
	// 支付单停在 pending，直到超时关单，用户被扣了款而订单被关掉。这个错误不会在任何
	// 一次本地测试里露头，只会在第一次上真渠道时炸，所以在这里就拦住。
	PaymentNotifyBaseURL string
	// AccountGRPCAddress 是 account-service 的 gRPC 地址。今天有两个方向的调用方：
	// 抽奖问它扣福卡/读余额（lottery 还没建，所以那个方向还没有代码），以及 payment-service
	// 在账户出资（纯咖啡豆）时问它扣豆——后者已经在跑，也是 payment-service 拒绝空地址的
	// 那一条（见下面 required-address 那一段）。
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
	// 调用（今天没有调用方，但契约已经在 fortune_card.proto 里定了）。
	//
	// 发送方和服务方一个标准：服务方（merchant-service、coffee-machine-service、
	// payment-service、account-service）拒绝短于 32 字节的配置，所以任何一个能通过
	// 校验的令牌都至少是 32 字节。发送方这边配短了不是「一个更弱的令牌」——它压根
	// 匹配不上，每一次调用都会被判 401。在启动时拦下来，比让每一单饮品都回 503 好。
	if (service == "merchant-service" || service == "coffee-machine-service" || service == "order-service" || service == "payment-service" || service == "account-service") && len([]byte(merchantToken)) < 32 {
		return Config{}, fmt.Errorf("MERCHANT_INTERNAL_TOKEN must be at least 32 bytes for %s", service)
	}
	userGRPCAddr := strings.TrimSpace(os.Getenv("USER_GRPC_ADDR"))
	merchantGRPCAddr := strings.TrimSpace(os.Getenv("MERCHANT_GRPC_ADDR"))
	coffeeMachineGRPCAddr := strings.TrimSpace(os.Getenv("COFFEE_MACHINE_GRPC_ADDR"))
	paymentGRPCAddr := strings.TrimSpace(os.Getenv("PAYMENT_GRPC_ADDR"))
	accountGRPCAddr := strings.TrimSpace(os.Getenv("ACCOUNT_GRPC_ADDR"))
	paymentNotifyBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PAYMENT_NOTIFY_BASE_URL")), "/")
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
	if (service == "merchant-service" || service == "coupon-service" || service == "coffee-machine-service" || service == "order-service" || service == "account-service") && userGRPCAddr == "" {
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
	// payment-service 要的是自己的**回调基址**而不是谁的地址：它得把这个 URL 交给渠道，
	// 渠道照着它回调。缺了它支付单建得出来、钱也可能收得到，但结果永远回不到我们这边
	// （见 PaymentNotifyBaseURL 的注释），所以在这里拒绝启动。
	if service == "payment-service" && paymentNotifyBaseURL == "" {
		return Config{}, fmt.Errorf("PAYMENT_NOTIFY_BASE_URL is required for payment-service")
	}
	// payment-service 还要 account-service：账户出资（纯咖啡豆）那条路要当场扣余额，扣不了
	// 就不能把支付单推进成功。地址缺席不是「少个可选依赖」——空地址会在**用户选了豆支付
	// 那一刻**才炸（连不上账户域，5xx），而不是在启动时；那正是 account-service 对
	// USER_GRPC_ADDR 的那条理由，所以同样在这里拒绝启动。
	if service == "payment-service" && accountGRPCAddr == "" {
		return Config{}, fmt.Errorf("ACCOUNT_GRPC_ADDR is required for payment-service")
	}
	// coffee-machine-service 还要 merchant-service：设备挂点位之前，这个点位存不存在、
	// 还能不能用，只有商户服务说了算（见 client.StoreResolver）。地址缺席不是「少个
	// 可选依赖」——校验会整条失效，所以在这里就拒绝启动，而不是等到第一次保存设备。
	if service == "coffee-machine-service" && merchantGRPCAddr == "" {
		return Config{}, fmt.Errorf("MERCHANT_GRPC_ADDR is required for coffee-machine-service")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	userDatabaseURL := os.Getenv("USER_DATABASE_URL")
	merchantDatabaseURL := os.Getenv("MERCHANT_DATABASE_URL")
	couponDatabaseURL := os.Getenv("COUPON_DATABASE_URL")
	coffeeMachineDatabaseURL := os.Getenv("COFFEE_MACHINE_DATABASE_URL")
	orderDatabaseURL := os.Getenv("ORDER_DATABASE_URL")
	paymentDatabaseURL := os.Getenv("PAYMENT_DATABASE_URL")
	accountDatabaseURL := os.Getenv("ACCOUNT_DATABASE_URL")
	return Config{
		ServiceName:                service,
		Version:                    version,
		Environment:                env,
		HTTPAddress:                addr,
		GRPCAddress:                grpcAddr,
		RegistryEndpoint:           registryEndpoint,
		DatabaseURL:                databaseURL,
		UserDatabaseURL:            userDatabaseURL,
		MerchantDatabaseURL:        merchantDatabaseURL,
		OrderDatabaseURL:           orderDatabaseURL,
		ServiceDatabaseURL:         resolveDatabase(service, databaseURL, userDatabaseURL, merchantDatabaseURL, couponDatabaseURL, coffeeMachineDatabaseURL, orderDatabaseURL, paymentDatabaseURL, accountDatabaseURL),
		MigrateOnStart:             parseBoolEnv("DB_MIGRATE_ON_START"),
		RedisAddress:               os.Getenv("REDIS_ADDR"),
		RedisPassword:              os.Getenv("REDIS_PASSWORD"),
		RedisDB:                    redisDB,
		JWTSecret:                  os.Getenv("JWT_SECRET"),
		JWTIssuer:                  os.Getenv("JWT_ISSUER"),
		AccountServiceURL:          os.Getenv("ACCOUNT_SERVICE_URL"),
		UserServiceURL:             os.Getenv("USER_SERVICE_URL"),
		MerchantServiceURL:         os.Getenv("MERCHANT_SERVICE_URL"),
		UserGRPCAddress:            userGRPCAddr,
		MerchantGRPCAddress:        merchantGRPCAddr,
		CoffeeMachineGRPCAddress:   coffeeMachineGRPCAddr,
		PaymentGRPCAddress:         paymentGRPCAddr,
		PaymentNotifyBaseURL:       paymentNotifyBaseURL,
		AccountGRPCAddress:         strings.TrimSpace(os.Getenv("ACCOUNT_GRPC_ADDR")),
		MerchantInternalToken:      merchantToken,
		MerchantOwnershipTimeoutMS: ownershipTimeout,
		AuthorizationTimeoutMS:     authorizationTimeout,
		HTTPTimeoutMS:              httpTimeoutMS,
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

// resolveDatabase picks the database a service owns after the split. Each
// per-service variable falls back to the shared DATABASE_URL, so a stack that
// still runs on one database keeps working; user-service, merchant-service,
// coupon-service, coffee-machine-service, order-service, payment-service and
// account-service have an owned database today.
//
// A service missing from this switch silently reads DATABASE_URL — that is the
// identity database in the dev stack, so the failure looks like working code
// writing to the wrong database, not like a misconfiguration. Add the case in
// the same change that adds the service.
func resolveDatabase(service, shared, user, merchant, coupon, coffeeMachine, order, payment, account string) string {
	var owned string
	switch service {
	case "user-service":
		owned = user
	case "merchant-service":
		owned = merchant
	case "coupon-service":
		owned = coupon
	case "coffee-machine-service":
		owned = coffeeMachine
	case "order-service":
		owned = order
	case "payment-service":
		owned = payment
	case "account-service":
		owned = account
	}
	if strings.TrimSpace(owned) == "" {
		return shared
	}
	return owned
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
