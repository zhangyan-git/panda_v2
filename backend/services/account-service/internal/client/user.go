// Package client 持有 account-service 的出网 gRPC 客户端。跨服务调用放在这里而不是
// controller 里，controller 才能只做「HTTP 适配层」这一件事。
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

// AdminAccessResolver 按请求问 user-service「这个管理员能做什么」。
//
// 为什么不信 token 里的权限快照：那是登录时签的，会一直过期到 token 失效（当前 TTL 24
// 小时）——一次被撤销的授权、一个被降级的管理员，在剩下的一天里照旧能用。后台查福卡
// 流水是要权限码的（account:read），撤销必须在下一个请求上就生效。
//
// 这条 RPC 用调用方自己的 access token 认证，所以本客户端不需要服务令牌：服务令牌是给
// 「代表基础设施而不是某个用户」的 RPC 用的。
type AdminAccessResolver struct {
	users userv1.AdminAccessServiceClient
}

func NewAdminAccessResolver(conn grpc.ClientConnInterface) *AdminAccessResolver {
	return &AdminAccessResolver{users: userv1.NewAdminAccessServiceClient(conn)}
}

// Resolve 实现 authz.Resolver。被拒绝的调用方回哨兵错误（中间件翻成 401/403），
// 其余失败保持普通错误——那会变成一次 fail-closed 的 503。
func (r *AdminAccessResolver) Resolve(ctx context.Context, accessToken string) (authz.Grants, error) {
	if r == nil || r.users == nil {
		return authz.Grants{}, errors.New("admin access client is not configured")
	}
	// token 走 metadata，绝不放进请求字段：user-service 会重新验它，放进字段就等于
	// 允许调用方替别人声明身份。
	resp, err := r.users.GetAdminAccess(auth.WithAccessToken(ctx, accessToken), &userv1.GetAdminAccessRequest{})
	if err != nil {
		switch status.Code(err) {
		case codes.Unauthenticated:
			return authz.Grants{}, authz.ErrUnauthenticated
		case codes.PermissionDenied:
			return authz.Grants{}, authz.ErrForbidden
		default:
			return authz.Grants{}, err
		}
	}
	return authz.Grants{
		UserID:      resp.GetUserId(),
		Roles:       resp.GetRoles(),
		Permissions: resp.GetPermissions(),
	}, nil
}
