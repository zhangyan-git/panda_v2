package client

import (
	"context"
	"errors"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MerchantAccessResolver 回答「这个商户账号现在能看见哪些点位」，每个请求问一次
// user-service。
//
// 与 AdminAccessResolver 成对，且**故意不合并**：管理员那边回的是角色与权限码，商户
// 这边回的是一组点位，一条路由只能是其中一种。合并之后「这是哪一类调用方」就变成答案
// 的一个运行时属性，而它本该校在路由上。
//
// 同样的理由也适用于「为什么不再查一次令牌里的 scope」：access token 的有效期是 24
// 小时，签进去的范围要等一天才会跟着改，而改范围与停用账号都必须立刻生效。
type MerchantAccessResolver struct {
	users userv1.MerchantAccessServiceClient
}

func NewMerchantAccessResolver(conn grpc.ClientConnInterface) *MerchantAccessResolver {
	return &MerchantAccessResolver{users: userv1.NewMerchantAccessServiceClient(conn)}
}

// Resolve 实现 authz.MerchantResolver。调用方被拒绝时返回中间件认识的两个哨兵错误，
// 其余失败原样返回，由中间件失败关闭（503）。
//
// 尤其注意 Unavailable 这一路：边界取不到就是取不到，既不回落成空集（界面会显示成
// 「你没有订单」），也不放宽成全量。
func (r *MerchantAccessResolver) Resolve(ctx context.Context, accessToken string) (authz.MerchantGrants, error) {
	if r == nil || r.users == nil {
		return authz.MerchantGrants{}, errors.New("merchant access client is not configured")
	}
	// token 走 metadata，不走请求字段：user-service 会重新校验它，调用方因此无法
	// 声明成别人。
	resp, err := r.users.GetMerchantAccess(auth.WithAccessToken(ctx, accessToken), &userv1.GetMerchantAccessRequest{})
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated:
			return authz.MerchantGrants{}, authz.ErrUnauthenticated
		case codes.PermissionDenied:
			return authz.MerchantGrants{}, authz.ErrForbidden
		default:
			return authz.MerchantGrants{}, err
		}
	}
	return authz.MerchantGrants{
		MerchantID: resp.GetMerchantId(),
		ScopeType:  resp.GetScopeType(),
		ScopeID:    resp.GetScopeId(),
		StoreIDs:   resp.GetStoreIds(),
	}, nil
}
