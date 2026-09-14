package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StoreResolver 回答「这个点位现在能不能挂设备」。
//
// 与 AdminAccessResolver 不同，这条调用用的是**服务令牌**：它代表基础设施去问
// 商户服务的事实，不代表某个用户。user-service 查账号归属走的是同一条路。
type StoreResolver struct {
	stores  merchantv1.MerchantServiceClient
	token   string
	timeout time.Duration
}

// NewStoreResolver 复用调用方那条连接：main 只拨一次，多个调用点共享同一个
// *grpc.ClientConn。
func NewStoreResolver(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*StoreResolver, error) {
	if conn == nil {
		return nil, errors.New("merchant service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("merchant service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("merchant service timeout must be positive")
	}
	return &StoreResolver{stores: merchantv1.NewMerchantServiceClient(conn), token: token, timeout: timeout}, nil
}

// Resolve 返回点位状态。found 为 false 表示商户服务里没有这个 id。
//
// 「不存在」和「问不到」必须分得开，否则调用方只能二选一，两边都错：把两者都当成
// 不存在，商户服务抖一下就会拒掉一次合法保存；都当成可用，校验就形同虚设。所以
// 前者用 found 表达，后者用 err 表达，不合并成一个错误。
// 返回值不取名：名为 status 的返回值会遮住 google.golang.org/grpc/status，而下面
// 正是靠它把 NotFound 和传输错误分开的。
func (r *StoreResolver) Resolve(ctx context.Context, storeID string) (string, bool, error) {
	if r == nil || r.stores == nil {
		return "", false, errors.New("store resolver is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.stores.GetStore(ctx, &merchantv1.GetStoreRequest{StoreId: storeID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", false, nil
		}
		return "", false, err
	}
	store := resp.GetStore()
	if store == nil {
		// 应答里没有门店又不算 NotFound：这不可能是一次正常回答，当成问不到，
		// 别让一个空应答被读成「点位存在且状态为空」。
		return "", false, errors.New("merchant service returned an empty store")
	}
	// 字符串和枚举两种形态里取有值的那个，理由同 user-service 读 merchantStatus：
	// 上游可能只填其中一种，读的人不该挑一种去信。
	if value := store.GetStatus(); value != "" {
		return value, true, nil
	}
	switch store.GetStatusCode() {
	case merchantv1.ResourceStatus_RESOURCE_STATUS_ACTIVE:
		return "active", true, nil
	case merchantv1.ResourceStatus_RESOURCE_STATUS_DISABLED:
		return "disabled", true, nil
	default:
		// 枚举有值但不是这两个之一：上游加了新状态，而这一版不认识它。既不能说「停用」
		// （那是我们证不出来的断言，会让运维去查一个根本没停用的点位），也不能说
		// 「不存在」（更错）。当成「这次没问出结果」，让调用方拒绝写入并回 503。
		//
		// 注意 found 只能传 false：这一路带 err，调用方按 err 分支走，found 会被忽略；
		// 传 true 只会给后来读代码的人一个「存在但状态未知」的假象。
		return "", false, errors.New("merchant service returned an unknown store status")
	}
}
