package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	defaultRequestTimeout = 10 * time.Second
	// defaultUploadTimeout 是上传路径的预算。它比其余路由大一个量级，因为它覆盖
	// 的是「客户端上行 + 上游写进 OSS」整段，而不只是一次服务端处理：10MB 走
	// 1Mbps 上行约 80 秒，给 60 秒会稳定产出 504。
	defaultUploadTimeout = 120 * time.Second
)

// Config contains the upstream addresses used by the compatibility facade.
//
// The gateway holds no service token: internal calls moved to gRPC, where the
// caller authenticates itself with metadata, so the facade no longer vouches for
// anything it forwards.
type Config struct {
	MerchantServiceURL string
	UserServiceURL     string
	// CouponServiceURL 可留空：优惠券服务尚未上线时，/v1/admin/coupons 保持
	// 404，与「没有这个上游」的语义一致。填了才注册该路由。
	CouponServiceURL string
	// CoffeeMachineServiceURL 同上：可留空，留空时 /v1/admin/coffee-machines 保持
	// 404。设备域是后加的服务，不填不该让网关起不来。
	CoffeeMachineServiceURL string
	// OrderServiceURL 同上：可留空，留空时订单域的四条路径都保持 404（订单域是后加的
	// 服务）。注意它接的是订单与售后两个资源、两端各一棵：/v1/admin/orders、
	// /v1/admin/after-sales、/v1/miniapp/orders、/v1/miniapp/after-sales，后两个落在
	// user-service 的 /v1/miniapp 前缀里——顺序和「留空时不许落到 user-service」
	// 这两件事都在 NewHandler 的那条 case 上，见那里的注释。
	OrderServiceURL string
	// PaymentServiceURL 同上：可留空，留空时支付域的两条路径前缀都保持 404。
	//
	// 支付域与上面几个不一样的地方是：它那两条回调路径（支付结果与协议变更）**必须**能从
	// 公网到达。渠道的回调是从公网打进来的（它签得了名，但进不了内网），所以这一条不是
	// 「给前端用的转发」，而是支付域对渠道唯一的入口。两条都挂在同一个 /v1/payments 前缀下，
	// 网关不需要分别认识它们。发起支付走的是另一条路（order-service 同步 gRPC 调支付域），
	// 过不了网关，也不需要过。
	//
	// 后台那几条（/v1/admin/payments 支付单，以及 /v1/admin/settlement/* 分账三页）走的是
	// 同一条上游，与别的域的 /v1/admin/* 一样，只是给 admin-web 转发。原来还有
	// /v1/admin/payment-methods 与 /v1/admin/payment-channels 两条：收钱那四种（咖啡豆 /
	// 银联商务小程序 / 银联商务 H5 / 取货码）现在是支付服务里的一张常量表，后台没有可配的
	// 东西，那两页连同两条前缀一起删了。
	PaymentServiceURL string
	// AccountServiceURL 同上：可留空，留空时资产账户域的路径保持 404（资产账户域是
	// 最后加的服务）。它接的是福卡与咖啡豆两棵树：/v1/admin/{fortune-cards,coffee-beans}
	// 与 /v1/miniapp/{fortune-cards,coffee-beans}——后两条落在 user-service 的
	// /v1/miniapp 前缀里，顺序见 NewHandler 的那条 case。
	AccountServiceURL string
	// LotteryServiceURL 同上：可留空，留空时抽奖域的路径保持 404（抽奖域是最后加的
	// 服务）。它接的是两条 /v1/miniapp/lottery/*（抽奖中心与我的中奖记录），都落在
	// user-service 的 /v1/miniapp 前缀里——顺序见 NewHandler 的那条 case。
	LotteryServiceURL string
	// MembershipServiceURL 同上：可留空，留空时会员域的路径保持 404（会员域是最后加的服务）。
	//
	// 它与其他几个不同的地方是**两端各有一棵树**，而且小程序那半棵必须抢在
	// /v1/miniapp → user-service 前面：/v1/miniapp/membership 落在那个前缀里，写在它后面就
	// 永远轮不到，小程序拿到的会是一句「用户服务没有这个接口」。后台那半棵（四条：
	// membership-plans / membership-subscriptions / membership-campaigns / memberships）与
	// user-service 接的 /v1/admin/* 都不相交。
	//
	// ⚠️ 这五条前缀与 membership-service 自己的路由表（internal/routes 里的
	// miniappPath 与 admin.go）**必须逐字对得上**：那边注册的是
	// /v1/miniapp/membership{,/plans,/auto-renew,/campaigns/claim} 与后台那四棵树，
	// 这边只写前两段前缀。少一条就是一个静默的 404，而两侧都「看起来没问题」。
	//
	// **这不是假设，是踩过的**：包月订阅与店铺码活动两页的接口（membership-subscriptions
	// 与 membership-campaigns）加进来时，这边只跟着改了服务里的路由表，后台点开这两页是
	// 网关自己回的 404——页面把 404 显示成「保存失败：可能是码参数已被占用」，查的人先去
	// 怀疑数据。新加后台资源时，**这边与 proxy_test.go 里那份路径清单要同批改**。
	MembershipServiceURL string
	// PartnerServiceURL 同上：可留空，留空时合作方域的三条路径前缀都保持 404（开放平台是
	// 最后加的服务）。
	//
	// 它接的是**两棵性质相反的树**：后台那两条（/v1/admin/partners、/v1/admin/partner-call-logs）
	// 是给 admin-web 的，与别的域的 /v1/admin/* 一样；而 /v1/openapi 那一棵是**合作方从公网
	// 打进来的**——那条路上的凭据是合作方自己算的签名（X-API-Key + X-Signature），不是我们的
	// 令牌。它与支付域的回调是同一种性质：网关是它唯一的入口，所以认证在服务里做，网关这一层
	// 只负责把它转过去（限流在 newMux 那条链上，别把它绕过去）。
	//
	// **设备同步也在这一棵下面**（线下刷卡机，POST /v1/openapi/device/sync-order）：它与
	// 其余开放接口走同一个上游、同一套验签与 nonce 去重，所以**不另开 case**，这条前缀已经
	// 把它包含了。订单落在订单域，但那是合作方服务验完签之后 gRPC 调过去的，网关这一层不多
	// 一跳。⚠️ 正因为包含关系成立，再补一条更细的 `/v1/openapi/...` case 会**吃掉**整个子树
	// 并把它转给别的上游——见 NewHandler 里那条 case 上的说明。
	//
	// **这份清单必须与 NewHandler 里那条 case 逐条对得上**：漏一条就是一个静默的 404
	// （请求落到兜底、没人报错）。
	PartnerServiceURL string
	RequestTimeout    time.Duration
	// UploadTimeout replaces RequestTimeout for the upload path only, so one slow
	// route does not buy every other route a two-minute hang.
	UploadTimeout time.Duration
	HTTPClient    *http.Client
}

// NewHandler returns a thin HTTP facade for browser API paths.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.RequestTimeout < 0 {
		return nil, fmt.Errorf("request timeout must not be negative")
	}
	if cfg.UploadTimeout == 0 {
		cfg.UploadTimeout = defaultUploadTimeout
	}
	if cfg.UploadTimeout < 0 {
		return nil, fmt.Errorf("upload timeout must not be negative")
	}
	merchant, err := newProxy(cfg.MerchantServiceURL, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("merchant service URL: %w", err)
	}
	user, err := newProxy(cfg.UserServiceURL, cfg.HTTPClient)
	if err != nil {
		return nil, fmt.Errorf("user service URL: %w", err)
	}
	// 只有配置了上游才构造，未配置时 coupons 走 default 分支的 404。
	var coupon http.Handler
	if strings.TrimSpace(cfg.CouponServiceURL) != "" {
		coupon, err = newProxy(cfg.CouponServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("coupon service URL: %w", err)
		}
	}
	// 同上，设备域也是一个后加的服务。
	var coffeeMachine http.Handler
	if strings.TrimSpace(cfg.CoffeeMachineServiceURL) != "" {
		coffeeMachine, err = newProxy(cfg.CoffeeMachineServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("coffee machine service URL: %w", err)
		}
	}
	// 订单域同样后加。
	var order http.Handler
	if strings.TrimSpace(cfg.OrderServiceURL) != "" {
		order, err = newProxy(cfg.OrderServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("order service URL: %w", err)
		}
	}
	// 支付域同样后加。它挂在 /v1/payments 这个**独立前缀**上，与订单域刻意分开：渠道回调
	// 不该长在小程序的路径树下（那个前缀已经整个转给订单域与 user-service 了）。
	var payment http.Handler
	if strings.TrimSpace(cfg.PaymentServiceURL) != "" {
		payment, err = newProxy(cfg.PaymentServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("payment service URL: %w", err)
		}
	}
	// 资产账户域（福卡 + 咖啡豆两棵树）同样后加。它接的两条 /v1/miniapp/* 都落在
	// user-service 的 /v1/miniapp 前缀里，所以构造完之后还要看下面那条 case 的位置。
	var account http.Handler
	if strings.TrimSpace(cfg.AccountServiceURL) != "" {
		account, err = newProxy(cfg.AccountServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("account service URL: %w", err)
		}
	}
	// 抽奖域同样后加。它接的两条 /v1/miniapp/lottery/* 也落在 user-service 的
	// /v1/miniapp 前缀里，构造完之后同样要看下面那条 case 的位置。
	var lottery http.Handler
	if strings.TrimSpace(cfg.LotteryServiceURL) != "" {
		lottery, err = newProxy(cfg.LotteryServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("lottery service URL: %w", err)
		}
	}
	// 会员域同样后加，而且与订单/账户/抽奖一样**两端各有一棵**：小程序那半棵
	// （/v1/miniapp/membership）落在 user-service 的前缀里，所以下面那条 case 的位置与它们
	// 同样要命。后台那半棵（/v1/admin/membership-plans 与 /v1/admin/memberships）与
	// user-service 接的 /v1/admin/* 都不相交。
	var membership http.Handler
	if strings.TrimSpace(cfg.MembershipServiceURL) != "" {
		membership, err = newProxy(cfg.MembershipServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("membership service URL: %w", err)
		}
	}
	// 合作方（开放平台）域同样后加。它接的两棵树性质相反：后台那两条是给 admin-web 的，
	// /v1/openapi 那一棵是合作方从公网打进来的——**网关是它唯一的入口**，所以这条上游必须
	// 配成合作方能到达的地址（生产是公网域名，本地是网关自己的地址），与支付域的回调同理。
	var partner http.Handler
	if strings.TrimSpace(cfg.PartnerServiceURL) != "" {
		partner, err = newProxy(cfg.PartnerServiceURL, cfg.HTTPClient)
		if err != nil {
			return nil, fmt.Errorf("partner service URL: %w", err)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := normalizePath(r.URL.Path)
		r.URL.Path = path
		// RawPath is intentionally retained (after the same /api removal) so
		// encoded path segments are not decoded and re-escaped differently.
		r.URL.RawPath = normalizeRawPath(r.URL.RawPath)
		var upstream http.Handler
		timeout := cfg.RequestTimeout
		switch {
		// 资产账户域的两棵树的四条路径：福卡与咖啡豆。它们必须排在下面那条
		// /v1/miniapp → user-service 之前，理由与订单域一样：/v1/miniapp/fortune-cards
		// 与 /v1/miniapp/coffee-beans 都落在那个前缀里，写在它后面就永远轮不到，
		// 小程序拿到的会是一句「用户服务没有这个接口」。
		//
		// 咖啡豆那两条是后补的，补之前它们分别落在这里的两处坏地方：/v1/admin/coffee-beans
		// 落到 default 是网关自己的 404，/v1/miniapp/coffee-beans 被转给 user-service。
		// 两者在界面上都是「接口不存在」，而真正的原因是**网关不认识这条路径**——
		// account-service 里那几条路由一直是好的。
		//
		// 两条后台路径也与 user-service 的 /v1/admin/* 都不冲突，放同一条 case 只是因为
		// 它们属于同一个上游。写前缀而不是写整条路径：一棵树上有多条子路径
		// （/coffee-beans/entries、/coffee-beans/{userId}/adjustments），逐个列举漏一个
		// 就是一个静默的 404。
		//
		// 与 order/coupon 那几条一样，这里**不把 account == nil 合进 case 条件**：
		// 没配上游时落到 /v1/miniapp，同样会被转给 user-service，而真正的原因是账户
		// 服务没接上。写在这里显式 404，客户端才不会把「账户域没部署」读成「用户服务
		// 没这个接口」。
		case hasPathPrefix(path, "/v1/admin/fortune-cards"),
			hasPathPrefix(path, "/v1/admin/coffee-beans"),
			hasPathPrefix(path, "/v1/miniapp/fortune-cards"),
			hasPathPrefix(path, "/v1/miniapp/coffee-beans"):
			if account == nil {
				http.NotFound(w, r)
				return
			}
			upstream = account
		// 抽奖域的两条小程序路径，理由与账户域逐字相同：/v1/miniapp/lottery/campaigns
		// 与 /v1/miniapp/lottery/wins 都落在下面的 /v1/miniapp → user-service 前缀里，
		// 写在它后面就永远轮不到，小程序拿到的会是一句「用户服务没有这个接口」。
		//
		// 后台那一条（/v1/admin/lottery/*）本可以放到后面去——/v1/admin/lottery 与
		// user-service 接的 /v1/admin/* 都不冲突——但两端同属一个上游，拆成两条 case
		// 只是让「抽奖域接哪几条路径」这个问题有两个答案。写前缀而不是整条路径：
		// 一棵树上有多条子路径（campaigns/{id}、rounds/{id}/draw），逐个列举漏一个
		// 就是一个静默的 404。
		//
		// 与 account/order 那几条一样，这里**不把 lottery == nil 合进 case 条件**：
		// 没配上游时落到 /v1/miniapp 会被转给 user-service，而真正的原因是抽奖服务
		// 没接上。写在这里显式 404，客户端才不会把「抽奖域没部署」读成「用户服务
		// 没这个接口」。
		case hasPathPrefix(path, "/v1/admin/lottery"),
			hasPathPrefix(path, "/v1/miniapp/lottery"):
			if lottery == nil {
				http.NotFound(w, r)
				return
			}
			upstream = lottery
		// 订单域的四条路径必须先于 user-service 那一条：/v1/miniapp/orders 与
		// /v1/miniapp/after-sales 同时落在「/v1/miniapp →user-service」这个前缀里，
		// 而 Go 的 switch 取第一个成立的 case，写在它后面就永远轮不到。放到前面的
		// 代价只是多几次前缀比较。
		//
		// 售后是订单域的第二棵树（订单与售后各自一棵），所以四个前缀都写出来，而不是
		// 只写一个 /v1/ 的宽前缀：后台与小程序是两棵不同的端树，订单与售后是两个不同的
		// 资源，将来任何一端或任何一个挪走，都不该把其余的带走。
		//
		// 漏掉售后那两条的后果不是 404 而是一句更难查的话：/v1/admin/after-sales 落到
		// default 直接 404，/v1/miniapp/after-sales 落到下面的 /v1/miniapp 被转给
		// user-service，客户端拿到的 404 看着像「用户服务没有这个接口」。
		//
		// 这里刻意不把 order == nil 合进 case 条件：那样没配上游时这些路径会落到
		// 下面的 /v1/miniapp，被转给 user-service，客户端拿到的 404 看着像「用户
		// 服务没有这个接口」。真正的原因是订单服务没接上，就该在这里 404——与优惠券、
		// 设备域「没配上游就 404」是同一条约定。
		case hasPathPrefix(path, "/v1/admin/orders"),
			hasPathPrefix(path, "/v1/admin/after-sales"),
			hasPathPrefix(path, "/v1/miniapp/orders"),
			hasPathPrefix(path, "/v1/miniapp/after-sales"),
			// 商户端的订单列表与详情。售后**不在**这一棵里：商户端这一轮只有只读的
			// 订单列表与详情，没有 /v1/merchant/after-sales（多写一条就是凭空多出
			// 一个入口），所以这里也不为它留前缀。
			hasPathPrefix(path, "/v1/merchant/orders"):
			if order == nil {
				http.NotFound(w, r)
				return
			}
			upstream = order
		// 会员域的两棵树。小程序那半棵（/v1/miniapp/membership 以及它下面的 /plans 与
		// /auto-renew）必须排在下面那条 /v1/miniapp → user-service 之前，理由与订单/账户/
		// 抽奖逐字相同：写在它后面就永远轮不到，小程序拿到的会是一句「用户服务没有这个接口」，
		// 而 membership-service 里那三条路由一直是好的。
		//
		// 后台那半棵本可以放到后面去（/v1/admin/membership-plans 与 /v1/admin/memberships 都
		// 不与 user-service 接的 /v1/admin/* 冲突），但两端同属一个上游，拆成两条 case 只是让
		// 「会员域接哪几条路径」这个问题有两个答案。
		//
		// **后台这四条前缀要分开写**：hasPathPrefix 按路径段比较，"/v1/admin/membership" 匹配不上
		// "/v1/admin/membership-plans"（那不是一个路径段），"/v1/admin/memberships" 同理也匹配
		// 不上 "/v1/admin/membership-subscriptions"。写成一条通配会一条都接不到，而症状只是
		// 后台的会员页 404。四条 = admin.go 里那四棵树，缺一条那一页就整片打不开。
		//
		// 与 account/order/lottery 那几条一样，这里**不把 membership == nil 合进 case 条件**：
		// 没配上游时落到 /v1/miniapp 会被转给 user-service，而真正的原因是会员服务没接上。
		// 写在这里显式 404，客户端才不会把「会员域没部署」读成「用户服务没这个接口」。
		case hasPathPrefix(path, "/v1/admin/membership-plans"),
			hasPathPrefix(path, "/v1/admin/membership-subscriptions"),
			hasPathPrefix(path, "/v1/admin/membership-campaigns"),
			hasPathPrefix(path, "/v1/admin/memberships"),
			hasPathPrefix(path, "/v1/miniapp/membership"):
			if membership == nil {
				http.NotFound(w, r)
				return
			}
			upstream = membership
		case isNestedMerchantUsersPath(path),
			// 小程序用户管理。单独一条，是因为 /v1/admin/users 的前缀匹配
			// （下一行）管不到它——hasPathPrefix 按路径段比较，不按字符串前缀。
			hasPathPrefix(path, "/v1/admin/miniapp-users"),
			// 操作日志。身份库是这张表的归属方（审计消费者就住在 user-service），
			// 所以查询也走它，不另起上游。
			hasPathPrefix(path, "/v1/admin/operation-logs"),
			hasPathPrefix(path, "/v1/admin/merchant-users"),
			hasPathPrefix(path, "/v1/admin/roles"),
			hasPathPrefix(path, "/v1/admin/permissions"),
			hasPathPrefix(path, "/v1/admin/menus"),
			hasPathPrefix(path, "/v1/admin/accounts"),
			hasPathPrefix(path, "/v1/admin/auth"),
			hasPathPrefix(path, "/v1/admin/users"),
			hasPathPrefix(path, "/v1/merchant/auth"),
			hasPathPrefix(path, "/v1/merchant/users"),
			hasPathPrefix(path, "/v1/miniapp"):
			upstream = user
		// The upload path goes to the same upstream as the rest of the admin API but
		// gets its own, larger budget. Raising RequestTimeout instead would hold a
		// gateway goroutine and an upstream connection for two minutes on ANY stuck
		// merchant route, and would quietly change the latency contract the other
		// routes are pinned to.
		case hasPathPrefix(path, "/v1/admin/uploads"):
			upstream, timeout = merchant, cfg.UploadTimeout
		// 商户端那一棵与后台那一棵接同一个上游（merchant-service），但**必须分开列**：
		// 它们是两棵前缀不同的树，而 hasPathPrefix 按路径段比较。三处商户端前缀
		// （stores / devices / orders）分别落在三个上游上，各自写在自己那一条 case 里；
		// 只有这一条是 merchant-service 的。
		//
		// 与 /v1/merchant/auth、/v1/merchant/users（那两条在 user-service 的 case 里）
		// 不是同一棵树：那两条是登录与「我是谁」，两条都不带数据范围，所以留在身份服务。
		case hasPathPrefix(path, "/v1/admin/merchants"),
			hasPathPrefix(path, "/v1/admin/brands"),
			hasPathPrefix(path, "/v1/admin/stores"),
			hasPathPrefix(path, "/v1/merchant/stores"):
			upstream = merchant
		case coupon != nil && hasPathPrefix(path, "/v1/admin/coupons"):
			upstream = coupon
		// 设备域（厂商 / 咖啡机设备 / 饮品）。它和上面几条都没有共同前缀，落到
		// default 就是 404，而 404 在页面上表现为「接口不存在」，看着像后端没部署。
		case hasPathPrefix(path, "/v1/admin/coffee-machines"),
			// 商户端的设备列表与详情。与上面那条同属设备域，所以合在一条 case 里：
			// 拆开只会让「设备域接哪几条路径」这个问题有两个答案。饮品**不在**这一棵里
			// （商户端不做饮品），后台那条 /v1/admin/coffee-machines/drinks 照旧。
			//
			// 这里刻意**不把 coffeeMachine == nil 合进 case 条件**：没配上游时落到 default
			// 也是一句 404，但那样「设备域没部署」与「网关不认识这条路径」就分不开了。
			// 与订单、优惠券、支付域是同一条约定。
			hasPathPrefix(path, "/v1/merchant/devices"):
			if coffeeMachine == nil {
				http.NotFound(w, r)
				return
			}
			upstream = coffeeMachine
		// 支付域：渠道打进来的三条回调（支付结果 POST /v1/payments/callback/{provider}、
		// 协议变更 POST /v1/payments/agreement-notify/{provider}、扣款结果
		// POST /v1/payments/agreement-charge-notify/{provider}）加后台那一条只读路径。
		//
		// 三条回调**同属 /v1/payments 这一个前缀**，所以新加一条回调不需要在这里加 case
		// ——这也正是「回调路径长什么样」不该由网关决定的意思：它是支付服务自己的路由。
		//
		// 两条前缀同属一个上游，所以合成一条 case 而不是拆成两条——拆开只是让「支付域接哪
		// 几条路径」这个问题有两个答案（与上面 membership 那段的理由相同）。
		//
		// **后台那两条是单列的，不是为了好看**：hasPathPrefix 按路径段比较，
		// "/v1/admin/payments" 匹配不上 "/v1/admin/payment-methods"（那是另一个路径段），
		// 也匹配不上 "/v1/admin/settlement"（分账那三页，与支付单是**两个**路径段）。
		// methods / channels 那两条前缀已经随「支付方式与渠道」那一页一起删了（支付方式现在
		// 是支付服务里的一张常量表），但这条规矩留着：后台哪天再冒出一个 /v1/admin/payment-*
		// 的路径段，少列一条的症状仍然只是那一页 404——与上面 membership 那条注释是同一个坑。
		// 分账这三条就是这么漏过一次的：接口在支付服务里已经在（直连 :18085 回 401），
		// 后台点开分账三页却整页 404，而 404 在页面上表现为「接口不存在」。
		//
		// 与上面几条不同，这里**不把 payment == nil 合进 case 条件**：没配上游时落到
		// default 也是一句 404，但那样通道号、签名这些都没了上下文，看着像「渠道调错了
		// 地址」。在这里显式 404 与订单、优惠券、设备域是同一条约定。
		//
		// 回调那条路径**不挂认证是有意的**（渠道的凭据是它自己的签名，验签在支付服务里做），
		// 所以网关这一层就是它唯一的把关：限流在 newMux 那条链上，别把它绕过去。
		// 后台那一条的认证在支付服务里（认证 → 平台账号闸门 → 实时授权 → payment:read），
		// 网关不重复判一遍——两处各判一次的结果迟早会不一样。
		case hasPathPrefix(path, "/v1/payments"),
			hasPathPrefix(path, "/v1/admin/payments"),
			// 后台分账那三页（规则 / 账户 / 明细）走同一个上游。**必须单独列一条**：
			// "/v1/admin/settlement" 与 "/v1/admin/payments" 没有共同前缀。
			hasPathPrefix(path, "/v1/admin/settlement"):
			if payment == nil {
				http.NotFound(w, r)
				return
			}
			upstream = payment
		// 合作方域（开放平台）：后台两条加开放接口那一棵。
		//
		// **三条前缀必须分开写**：hasPathPrefix 按路径段比较，"partners" 与
		// "partner-call-logs" 是两个不同的段（写成 "/v1/admin/partner" 一条通配会一条都接不到），
		// 而 /v1/openapi 根本不在 /v1/admin 下面。与上面 membership / payment 那几段是同一个坑。
		//
		// ⚠️ **别再给 /v1/openapi 下面加更细的 case**（尤其不要为设备加一条）。
		//
		// 两条设备接口（POST /v1/openapi/device/sync-order 刷卡机回执、POST /v1/openapi/device/pickup
		// 取货码）与其余开放接口**同一个上游**
		// ——就是本条 case。设备回调与别的开放接口共用同一套 X-API-Key/X-Timestamp/X-Nonce/
		// X-Signature 验签、同一套 nonce 去重与限流（见 partner-service 的 internal/ingress），
		// 而且设备密钥**每个合作方一把**、就存在合作方服务的凭据表里；网关这一层没有任何
		// 理由把它和别的开放接口分开。订单确实落在订单域，但那是**验签之后**由合作方服务
		// gRPC 调过去的——HTTP 入口这一步不跨上游。
		//
		// switch 按书写顺序取第一个匹配的 case，所以一旦有人为设备补一条
		// `case hasPathPrefix(path, "/v1/openapi/device")`，它会**吃掉**整条设备子树并把它
		// 转给另一个上游——而那个上游没有验签、没有 nonce 去重、也拿不到密钥表，于是设备
		// 回调要么直接 404，要么（更糟）绕开验签建出订单。两种错法看上去都像「对面没实现
		// 这个接口」，与「网关把它送错了上游」这个真实原因完全对不上。
		//
		// 与上面几条一样，这里**不把 partner == nil 合进 case 条件**：没配上游时落到 default
		// 也是一句 404，但那样「合作方域没部署」与「网关不认识这条路径」就分不开了。写在这里
		// 显式 404，与订单、优惠券、设备、支付域是同一条约定。
		//
		// 开放接口那一棵的认证在合作方服务里（验签 → 时间窗 → nonce → 启停 → 白名单 → 限流，
		// 见 partner-service 的 internal/ingress）：网关**不重复判一遍**，也不给它挂任何我们
		// 自己的令牌——合作方没有我们的令牌。
		case hasPathPrefix(path, "/v1/admin/partners"),
			hasPathPrefix(path, "/v1/admin/partner-call-logs"),
			hasPathPrefix(path, "/v1/openapi"):
			if partner == nil {
				http.NotFound(w, r)
				return
			}
			upstream = partner
		default:
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		upstream.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func newProxy(rawURL string, client *http.Client) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(rawURL, "#") {
		return nil, fmt.Errorf("must be an absolute HTTP URL without credentials, query, or fragment")
	}
	if target.Path != "" && target.Path != "/" {
		return nil, fmt.Errorf("upstream base path must be empty or root")
	}
	if client == nil {
		client = http.DefaultClient
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(req *httputil.ProxyRequest) {
			path, rawPath := req.In.URL.Path, req.In.URL.RawPath
			req.SetURL(target)
			req.Out.URL.Path, req.Out.URL.RawPath = path, rawPath
			req.Out.URL.RawQuery = req.In.URL.RawQuery
			// Strip every identity-bearing header, including the retired service
			// token and the forwarding chain: nothing downstream trusts these any
			// more, and a client must not be able to smuggle one through the facade.
			for header := range req.Out.Header {
				name := strings.ToLower(header)
				if strings.HasPrefix(name, "x-user") || strings.HasPrefix(name, "x-tenant") || name == "x-roles" || name == "x-service-token" || strings.HasPrefix(name, "x-forwarded") {
					delete(req.Out.Header, header)
				}
			}
			setXForwarded(req)
		},
		Transport: client.Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status := http.StatusBadGateway
			var netErr net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
				status = http.StatusGatewayTimeout
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			code := "BAD_GATEWAY"
			if status == http.StatusGatewayTimeout {
				code = "GATEWAY_TIMEOUT"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "errorCode": code, "errorMessage": http.StatusText(status),
			})
		},
	}
	if proxy.Transport == nil {
		proxy.Transport = http.DefaultTransport
	}
	return proxy, nil
}

// setXForwarded 重写转发链，而不是沿用 httputil.ProxyRequest.SetXForwarded。
//
// 后者是**追加**语义：它把入站的 X-Forwarded-For 原样接在新地址前面。客户端
// 随便写一个值，下游看到的链首就是伪造的地址。网关在边缘，本机 RemoteAddr
// 就是真实来源，所以这里整个替换掉；调用前 x-forwarded-* 已被清空。
//
// 如果将来网关前面还有一层可信代理，正确做法是按配置的信任网段取链尾，
// 而不是退回追加——那等于把来源交回给客户端决定。
func setXForwarded(req *httputil.ProxyRequest) {
	clientIP, _, err := net.SplitHostPort(req.In.RemoteAddr)
	if err != nil {
		// 分不出端口就整个丢掉，宁可不给来源，也不给一个可能是伪造的值。
		req.Out.Header.Del("X-Forwarded-For")
	} else {
		req.Out.Header.Set("X-Forwarded-For", clientIP)
	}
	req.Out.Header.Set("X-Forwarded-Host", req.In.Host)
	if req.In.TLS == nil {
		req.Out.Header.Set("X-Forwarded-Proto", "http")
	} else {
		req.Out.Header.Set("X-Forwarded-Proto", "https")
	}
}

func isNestedMerchantUsersPath(path string) bool {
	parts := strings.Split(path, "/")
	return len(parts) == 6 && parts[0] == "" && parts[1] == "v1" && parts[2] == "admin" && parts[3] == "merchants" && parts[4] != "" && parts[5] == "users"
}
func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
func normalizePath(path string) string {
	if path == "/api" {
		return "/"
	}
	if strings.HasPrefix(path, "/api/") {
		return strings.TrimPrefix(path, "/api")
	}
	return path
}
func normalizeRawPath(path string) string {
	if path == "/api" {
		return "/"
	}
	if strings.HasPrefix(path, "/api/") {
		return strings.TrimPrefix(path, "/api")
	}
	return path
}
