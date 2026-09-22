package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrWalletIdentityUnavailable：问不到用户的微信身份（身份服务没答上来、内部错误、应答是坏的）。
//
// 它是**故障不是结论**，与「这个人没有绑定微信」分开：后者是一个事实，调用方可以拿它去做
// 决定（换一种支付方式）；前者重发一次可能就好了。混成一个会让一次下游抖动表现成
// 「你的微信号没绑上」，用户会跑去重新登录，而不是重试支付。
var ErrWalletIdentityUnavailable = errors.New("wallet identity service is unavailable")

// 微信身份的应用类型，与 user_wechat_identities.app_type 的 CHECK 一致。
//
// 支付链路只要小程序这一个：微信小程序支付认的 payer.openid 就是小程序下的 openid，
// 公众号那个是另一个值，拿它去发起小程序支付渠道只会回一句看不懂的错。
const wechatAppMiniapp = "miniapp"

// WalletIdentityReader 取用户绑定在微信上的身份，供发起支付时填渠道要的 payer.openid。
//
// 与上面的 AdminAccessResolver 是同一张连接上的两种令牌，判据是「这次调用代表谁」：
//
//   - AdminAccessResolver 代表**调用者自己**（转发他的 access token），回答的是「他能做什么」；
//   - 这里代表**订单域**去问一个用户事实（共享服务令牌），回答的是「付款人的 openid 是什么」。
//
// openid 不能从请求体拿：让客户端自称 openid 等于把「用谁的身份发起支付」交给调用方
// （见 dto.PayOrderRequest 的说明）。所以它是服务间的事实查询，这是它唯一的来路。
type WalletIdentityReader struct {
	users   userv1.UserServiceClient
	token   string
	timeout time.Duration
}

// NewWalletIdentityReader 复用调用方那条连接：main 只拨一次，与鉴权解析器共享同一个
// *grpc.ClientConn——它们问的是同一个服务的两个 RPC。
func NewWalletIdentityReader(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*WalletIdentityReader, error) {
	if conn == nil {
		return nil, errors.New("user service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("user service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("user service timeout must be positive")
	}
	return &WalletIdentityReader{users: userv1.NewUserServiceClient(conn), token: token, timeout: timeout}, nil
}

// MiniappOpenID 问用户在小程序下的 openid。found 为 false 表示这个人没有绑定小程序身份。
//
// **没有绑定不是错误**：它是一个事实，能不能收场由支付侧决定——订单这边不知道用户选的那个
// 支付方式是不是微信 JSAPI（qrcode、h5、咖啡豆都不需要 openid），在这里一刀切拒掉会让
// 没绑微信的用户连豆都花不出去。真正要 fail-closed 的是「问不到」，见
// ErrWalletIdentityUnavailable。
//
// 空串的 openid 按 found=false 处理：一个空的 payer.openid 不是身份，把它当成身份传下去
// 正是「静默降级」——渠道会拒，而我们会以为那是一次渠道故障。
func (r *WalletIdentityReader) MiniappOpenID(ctx context.Context, userID string) (string, bool, error) {
	if r == nil || r.users == nil {
		return "", false, errors.New("wallet identity reader is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()

	resp, err := r.users.GetWechatIdentity(ctx, &userv1.GetWechatIdentityRequest{
		UserId:  userID,
		AppType: wechatAppMiniapp,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// 身份服务明确回答「这个人没绑小程序」。这是有结论的一次回答。
			return "", false, nil
		}
		return "", false, ErrWalletIdentityUnavailable
	}
	openID := strings.TrimSpace(resp.GetOpenid())
	if openID == "" {
		return "", false, nil
	}
	return openID, true, nil
}

// AdminAccessResolver 用「问 user-service 这个后台账号现在能做什么」来回答后台的授权
// 问题，每个请求问一次。
//
// 为什么不读令牌里的 claims：那是登录时签发的，在过期前一直是旧值（当前 access token
// 是 24 小时）。一次被撤销的授权、一个被降级的管理员，会在剩下的整个有效期内继续生效。
//
// admin access RPC 验的是调用者自己的 access token，所以这个客户端不带服务令牌：
// 服务令牌是给「代表基础设施去操作」的 RPC 用的（见本包 coffee_machine.go）。
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
