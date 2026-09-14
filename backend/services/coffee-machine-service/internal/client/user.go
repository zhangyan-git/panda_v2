// Package client 是 coffee-machine-service 的出站 gRPC 客户端。跨服务调用放在这里
// 而不是控制器里，控制器才能保持成业务层之上的一个 HTTP 适配器。
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

// AdminAccessResolver 回答「这个管理员现在能做什么」，且每个请求都问一次
// user-service。
//
// 与 coupon-service 的同名客户端一致：授权不能读 token 里的声明，那是登录时签发的
// 快照，在过期前不会更新——被撤销的权限或降级的管理员会继续生效一整天（当前 TTL
// 24 小时）。这个 RPC 校验的是调用方自己的 access token，所以不需要服务令牌：服务
// 令牌是给「代表基础设施而不是代表用户」的那类 RPC 用的。
type AdminAccessResolver struct {
	users userv1.AdminAccessServiceClient
}

func NewAdminAccessResolver(conn grpc.ClientConnInterface) *AdminAccessResolver {
	return &AdminAccessResolver{users: userv1.NewAdminAccessServiceClient(conn)}
}

// Resolve 实现 authz.Resolver。调用方被拒绝时返回中间件认识的两个哨兵错误，
// 其余失败原样返回，由中间件失败关闭（503）。
func (r *AdminAccessResolver) Resolve(ctx context.Context, accessToken string) (authz.Grants, error) {
	if r == nil || r.users == nil {
		return authz.Grants{}, errors.New("admin access client is not configured")
	}
	// token 走 metadata，不走请求字段：user-service 会重新校验它，调用方因此无法
	// 声明成别人。
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
