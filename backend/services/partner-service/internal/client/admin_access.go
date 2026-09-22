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

// AdminAccessResolver 用「问 user-service 这个后台账号现在能做什么」来回答后台的授权问题，
// 每个请求问一次。
//
// 与 payment-service / membership-service / lottery-service / order-service /
// order-service / coffee-machine-service 里那几个同名类型是同一份实现，各服务各留一份
// （理由见包注释）。
//
// 为什么开放平台这一域也要实时取一次授权：本域的两枚码（partner:read / partner:manage）
// 里，manage 能动的是**别人调用我们的凭据**——停用一把密钥、改一条 IP 白名单、签发一把新的，
// 每一件都能立刻改变谁能打进来。这种码的生效延迟必须为零，所以它不能读令牌里那份可能已经
// 旧了一小时的 claims。
//
// admin access RPC 验的是调用者自己的 access token，所以这个客户端**不带服务令牌**：服务
// 令牌是给「代表基础设施去操作」的 RPC 用的。
type AdminAccessResolver struct {
	users userv1.AdminAccessServiceClient
}

func NewAdminAccessResolver(conn grpc.ClientConnInterface) *AdminAccessResolver {
	return &AdminAccessResolver{users: userv1.NewAdminAccessServiceClient(conn)}
}

// Resolve 实现 authz.Resolver。「调用者被拒」翻译成中间件认得的哨兵错误（401/403），其余
// 失败原样返回，由中间件 fail-closed 成 503——「他没权限」与「没问到用户服务」必须分开，
// 后者被当成前者会让一次下游抖动表现为全员被踢下线。
func (r *AdminAccessResolver) Resolve(ctx context.Context, accessToken string) (authz.Grants, error) {
	if r == nil || r.users == nil {
		return authz.Grants{}, errors.New("admin access client is not configured")
	}
	// token 走 metadata，不走请求字段：user-service 会重新校验它，所以调用方无法在这里
	// 声称自己是别人。
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
