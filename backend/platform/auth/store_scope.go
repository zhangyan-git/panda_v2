package auth

import (
	"context"
	"net/http"
)

// 商户账号数据范围的三个档位，与 merchant_users.scope_type 的取值逐字一致
// （见 migrations/004_brands_stores.sql 的列注释）。
//
// 常量只此一处。中间件、gRPC 应答、各服务的过滤器都引用它们，任何一处手写字符串
// 都会在将来加档时悄悄漏掉。
const (
	// ScopeTypeMerchant 是本商户全部点位。
	ScopeTypeMerchant = "merchant"
	// ScopeTypeBrand 是指定品牌旗下的点位。
	ScopeTypeBrand = "brand"
	// ScopeTypeStore 是单个点位。
	ScopeTypeStore = "store"
)

// StoreScope 是一个商户请求的数据边界：它能看到哪些点位。
//
// 它与同一个包里的 Scope 是两个东西，**故意不合并**：
//
//   - Scope 是 JWT claim。它的类型词表是 all/custom/self，随令牌签发、生命周期
//     等于令牌；写它的地方只有 SignGrant。
//   - StoreScope 是**每个请求实时解析出来的结果**，词表是 merchant/brand/store，
//     生命周期等于一次请求；写它的地方只有 authz.MerchantMiddleware。
//
// 两者合并会让「这个边界是签出来的还是刚查出来的」变得看不出来，而那正是撤权是否
// 及时生效的全部区别。
//
// 只有一组点位的形状，是因为设备表和订单表都只持有 store_id，都不持有 merchant_id
// （见方案 §5.2.3：不为实现通用过滤而向所有业务表添加商户字段）。品牌档与商户档
// 在这里展开成点位集合，而不是作为谓词下推，这样三个消费方共用同一条过滤路径。
type StoreScope struct {
	MerchantID string
	ScopeType  string // merchant | brand | store
	// ScopeID 是品牌或门店 id；商户档为空。
	ScopeID string
	// StoreIDs 是展开后的授权点位，**永远非 nil**。不变式见 WithStoreScope。
	StoreIDs []string
}

// AuthorizedStoreIDs 返回这一组授权点位，并保证不是 nil。
//
// 不变式本来由 WithStoreScope 建立，这个方法不是它的替代品，而是消费端在把切片交给
// SQL 之前的最后一道：过滤器判的是 nil 而不是长度，nil 落到 SQL 就是 NULL，而任何读成
// 「不过滤」的谓词都会把「一个点位都没授权」变成「看全平台」。多一次长度判断，换掉的是
// 这条链路上唯一能把越权伪装成正常返回的取值。
//
// 返回的就是切片本身，没有复制：它是只读边界，调用方只该拿它去过滤。
func (s StoreScope) AuthorizedStoreIDs() []string {
	if s.StoreIDs == nil {
		return []string{}
	}
	return s.StoreIDs
}

type storeScopeContextKey struct{}

// WithStoreScope 把数据边界放进请求上下文。
//
// 它顺带把 nil 的 StoreIDs 归一成空切片，因为这条不变式是安全性的分界：下游的过滤
// 条件是 `store_id = ANY($n)`，而 pgx 把 nil 切片编码成 SQL NULL。任何写成
// 「NULL 就不过滤」的谓词都会把「这个账号一个点位都没授权」读成「看全平台」。
// 空切片编码成 '{}'，ANY('{}') 恒假，命中零行 —— 这才是没授权该有的结果。
//
// 归一放在这里而不是留给调用方：这是进入上下文的唯一入口，在此处收口，下游就没有
// 机会构造出 nil 并把边界变成全量。
func WithStoreScope(ctx context.Context, scope StoreScope) context.Context {
	if scope.StoreIDs == nil {
		scope.StoreIDs = []string{}
	}
	return context.WithValue(ctx, storeScopeContextKey{}, scope)
}

// StoreScopeFromContext 取出上下文里的数据边界。
func StoreScopeFromContext(ctx context.Context) (StoreScope, bool) {
	if ctx == nil {
		return StoreScope{}, false
	}
	scope, ok := ctx.Value(storeScopeContextKey{}).(StoreScope)
	return scope, ok
}

// StoreScopeFromRequest 取出请求上的数据边界，第二个返回值表示请求是否确实经过了
// 商户鉴权中间件。
//
// **调用方必须把 ok=false 当成拒绝，而不是当成「没有边界」。** 这是本文件唯一
// fail-closed 的地方，也是刻意如此：忘记取边界的处理器应当什么都不返回，而不是返回
// 全部。若把 false 当作「不过滤」，一条新加的路由漏挂中间件就变成了跨商户越权，
// 而且不会以任何形式报错。
func StoreScopeFromRequest(r *http.Request) (StoreScope, bool) {
	if r == nil {
		return StoreScope{}, false
	}
	scope, ok := StoreScopeFromContext(r.Context())
	if !ok {
		return StoreScope{}, false
	}
	// 中间件之外可能就是漏洞所在：没有商户身份或没有边界时一律不认。
	if scope.MerchantID == "" || scope.ScopeType == "" {
		return StoreScope{}, false
	}
	return scope, true
}
