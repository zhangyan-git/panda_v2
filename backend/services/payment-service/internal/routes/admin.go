package routes

import (
	"net/http"

	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/controller"
)

// 权限码，与 migrations/identity/026_payment_admin.sql 逐字一致。
//
// 今天只有 read 这一枚：支付方式与渠道收成代码里的常量之后，后台不再有「改配置」这件事
// ——那枚 manage（027_payment_config_admin.sql 种的）在代码里已经没有引用点了。它留在身份库里
// 不碍事，也**不要**顺手用它去管别的接口：这个服务对钱的写入口一个都没有（见下面那两行的说明）。
//
// 「为什么这枚也走实时授权、不读令牌里的旧 claims」写在 internal/client/admin_access.go。
const permRead = "payment:read"

// 分账那两枚权限码，与 migrations/identity/035_settlement_admin.sql 逐字一致。
//
// 它们**绝不并进 payment:read / payment:manage**：008 的文件头（第 11–13 行）把分账的授权
// 单独列了一段——「谁能看支付单」与「谁能改分账配置」是两件事，前者是所有客服都要的，后者
// 决定了钱分给谁。并进去之后，一次「给他开个支付单查询吧」会连带把分账的写权限一起给出去。
//
// payout 那一枚今天**没有**：分账是随支付一次下发、支付成功即成功的，没有一个「打款」动作
// 可以授权。它属于结算单与打款那一层，而那一层本轮没做（见 docs/architecture.md）。
const (
	permSettlementRead   = "settlement:read"
	permSettlementManage = "settlement:manage"
)

// methodRoute 把一个 HTTP 方法绑到它自己的权限码和处理器上。
//
// 这张表是**唯一**声明「这条路径允许什么方法」的地方，省掉之后「只注册 GET」这件事就只剩
// handler 里的一句 if，而那是可以被顺手删掉的一句。留着它，加写方法的人必须先在这里加一行、
// 再想一遍自己该挂哪一枚码——写方法挂错成 permRead 的那一天，这个服务的只读保证就没了。
type methodRoute struct {
	permission string
	handler    http.HandlerFunc
}

// RegisterAdmin 挂载后台的支付路由。鉴权由调用方套在最外层，这样一个漏传校验器的调用
// 不可能变成失败开放。
//
// 传 nil 的 authenticate 时**所有**路由都回 401（见 protect），不是只跳过权限检查——
// 「忘了装配」必须表现为「谁都进不去」。
//
// # 与回调那棵树的边界
//
// 本服务有**两棵互不相干的 HTTP 树**：这棵（/v1/admin/*，挂身份与权限）与
// routes.RegisterPayment 那棵（/v1/payments/*，故意不挂认证——渠道带的是签名不是我们的
// 令牌）。它们是两次独立的 Register 调用，**绝不能合并成一个函数**：
// 合成一个之后，那个「authenticate 为 nil 就全 401」的保证会被回调树的存在稀释，而回调
// 树恰恰需要 authenticate 为 nil 也能正常工作。见 controller/doc.go。
func RegisterAdmin(
	r *runtime.HTTPRouter,
	handler *controller.AdminPaymentController,
	settlement *controller.AdminSettlementController,
	authenticate func(...string) func(http.Handler) http.Handler,
) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"errorCode":"UNAUTHORIZED","errorMessage":"unauthorized"}`))
	})
	protect := func(permission string, next http.Handler) http.Handler {
		if authenticate == nil {
			return unauthorized
		}
		return authenticate(permission)(next)
	}
	// routeFor 在建表时就把每个方法各自包上自己的权限中间件，请求来时只按方法查表。
	routeFor := func(byMethod map[string]methodRoute) http.HandlerFunc {
		protected := make(map[string]http.HandlerFunc, len(byMethod))
		for method, route := range byMethod {
			protected[method] = protect(route.permission, route.handler).ServeHTTP
		}
		return func(w http.ResponseWriter, r *http.Request) {
			h, ok := protected[r.Method]
			if !ok {
				// 路径存在但方法不对，按 kratos 的既有做法回 404 而不是 405：
				// 这套路由表里没有声明 Allow 的地方，回 405 还得自己拼头。
				//
				// 这条分支仍然是**有内容的**，不是摆设：这棵树上一条写路由都没有，而
				// 验收时会拿它当「支付单仍然只读」的证据（POST / PUT / DELETE 回 404 而不是 200）。
				http.NotFound(w, r)
				return
			}
			h(w, r)
		}
	}

	// —— 支付单（只读） ——
	//
	// 列表与详情是同一个 handler：路径参数从 r.URL.Path 里裁出来（见 controller.Payments），
	// 与库存那边入库单列表 / 详情同一个形状。**没有 PUT / PATCH / DELETE**——支付单是钱的
	// 既成事实，要改它得先有退款与对账的语义，而它们本轮没做。
	//
	// 「用户点的是哪个支付方式、这笔走在谁的通道上」在列表与详情里照旧带得出来（code + 名字），
	// 但那是**代码里的目录**补出来的展示列，不是可增删改的配置行——所以这棵树上再没有
	// payment-methods / payment-channels 那几条路径了。
	r.HandleFunc("/v1/admin/payments", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Payments},
	}))
	r.HandleFunc("/v1/admin/payments/{paymentNo}", routeFor(map[string]methodRoute{
		http.MethodGet: {permRead, handler.Payments},
	}))

	// —— 分账：规则与账户可改，任务只读 ——
	//
	// # 为什么规则能改而支付单不能
	//
	// 上面那段说的是「支付单是钱的既成事实」——它不能被改，因为改它等于改写历史。分账规则与
	// 账户是**将来怎么分钱**的配置，与「这笔钱发生了什么」是两件事：改一条规则不影响已经建好
	// 的任务（它们存的是当初那份快照），所以它像支付方式与渠道那样能动。区别是那一页今天没有
	// 了（方式与渠道收成了代码常量），而分账的配置还得有人来填。
	//
	// # 权限三分
	//
	// 任务那两条挂 read（与支付单同一枚：能看支付单的人本来就该能看这笔的分账明细）；规则与
	// 账户的四条 GET 挂 read，六条写挂 manage。**没有一条与 payment:* 共用**，理由见上面
	// 那两枚常量的说明。
	//
	// 规则项不单独开路由：它们是规则的一部分，「比例合计不超过 1」这类约束是跨行的，拆成子
	// 资源就没有任何一个时刻能看见完整的一套项（见 service/admin_settlement.go）。
	r.HandleFunc("/v1/admin/settlement/rules", routeFor(map[string]methodRoute{
		http.MethodGet:  {permSettlementRead, settlement.Rules},
		http.MethodPost: {permSettlementManage, settlement.Rules},
	}))
	r.HandleFunc("/v1/admin/settlement/rules/{id}", routeFor(map[string]methodRoute{
		http.MethodGet:    {permSettlementRead, settlement.Rules},
		http.MethodPut:    {permSettlementManage, settlement.Rules},
		http.MethodDelete: {permSettlementManage, settlement.Rules},
	}))
	r.HandleFunc("/v1/admin/settlement/accounts", routeFor(map[string]methodRoute{
		http.MethodGet:  {permSettlementRead, settlement.Accounts},
		http.MethodPost: {permSettlementManage, settlement.Accounts},
	}))
	r.HandleFunc("/v1/admin/settlement/accounts/{id}", routeFor(map[string]methodRoute{
		http.MethodGet:    {permSettlementRead, settlement.Accounts},
		http.MethodPut:    {permSettlementManage, settlement.Accounts},
		http.MethodDelete: {permSettlementManage, settlement.Accounts},
	}))
	r.HandleFunc("/v1/admin/settlement/tasks", routeFor(map[string]methodRoute{
		http.MethodGet: {permSettlementRead, settlement.Tasks},
	}))
	r.HandleFunc("/v1/admin/settlement/tasks/{id}", routeFor(map[string]methodRoute{
		http.MethodGet: {permSettlementRead, settlement.Tasks},
	}))
	r.HandleFunc("/v1/admin/settlement/channels", routeFor(map[string]methodRoute{
		http.MethodGet: {permSettlementRead, settlement.Channels},
	}))
}
