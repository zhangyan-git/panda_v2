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

// AdminAccessResolver 用「问 user-service 这个后台账号现在能做什么」来回答后台的授权
// 问题，每个请求问一次。
//
// 与 order-service / coffee-machine-service 里那几个同名类型是同一份实现，各服务各留一份：
// 它是**平台契约的适配器**（把 user-service 的 RPC 翻译成 authz.Resolver），把它抽成共享包
// 会让 platform 依赖 contracts 里某个具体的服务版本，而那正是 platform 不该知道的事。
//
// 为什么不读令牌里的 claims：那是登录时签发的，在过期前一直是旧值（access token 是 24
// 小时）。一次被撤销的授权、一个被降级的管理员，会在剩下的整个有效期内继续生效——而
// lottery:draw 这一枚管的正是「谁能决定中奖名单」，一小时的延迟都不能接受。
//
// admin access RPC 验的是调用者自己的 access token，所以这个客户端**不带服务令牌**：
// 服务令牌是给「代表基础设施去操作」的 RPC 用的（见本包 account.go）。
type AdminAccessResolver struct {
	users userv1.AdminAccessServiceClient
}

func NewAdminAccessResolver(conn grpc.ClientConnInterface) *AdminAccessResolver {
	return &AdminAccessResolver{users: userv1.NewAdminAccessServiceClient(conn)}
}

// Resolve 实现 authz.Resolver。「调用者被拒」翻译成中间件认得的哨兵错误（401/403），
// 其余失败原样返回，由中间件 fail-closed 成 503——「他没权限」和「没问到用户服务」
// 必须分开，后者被当成前者会让一次下游抖动表现为全员被踢下线。
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
