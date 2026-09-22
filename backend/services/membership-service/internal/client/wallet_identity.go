package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrWalletIdentityUnavailable：问不到用户的微信身份（身份服务没答上来、内部错误、应答是坏的）。
//
// 它是**故障不是结论**，与「这个人没有绑定微信」分开：后者是 `found=false`，调用方可以拿它去
// 做决定（告诉用户先用别的方式）；前者重发一次可能就好了。混成一个会让一次下游抖动表现成
// 「你的微信号没绑上」，用户会跑去重新登录，而他真正该做的是再点一次。
var ErrWalletIdentityUnavailable = errors.New("wallet identity service is unavailable")

// wechatAppMiniapp 是微信身份的应用类型，与 user_wechat_identities.app_type 的 CHECK 一致。
//
// 签约只要小程序这一个：微信委托代扣的 contract_code 是**在小程序里**签的，公众号下的
// openid 是另一个值，拿它去签约渠道只会回一句看不懂的错（order-service 的
// WalletIdentityReader 里是同一个常量、同一条理由）。
const wechatAppMiniapp = "miniapp"

// WalletIdentityReader 取用户绑定在微信上的身份，供发起签约时填渠道要的 openid。
//
// **openid 只能这么来**：让小程序把它放进请求体，等于把「用谁的身份签这份代扣协议」交给调用方
// ——那是一个人下个月会不会被扣款的凭据。所以它是服务间的事实查询，这是它唯一的来路。
//
// 与 Client 包里另外两个类型是同一张连接上的三种令牌，判据是「这次调用代表谁」：
//
//   - AdminAccessResolver 代表**调用者自己**（转发他的 access token），回答「他能做什么」；
//   - StoreClient 代表**会员域**去问一个商户事实（共享服务令牌），回答「这家店在不在」；
//   - 这里同样是会员域去问一个用户事实，回答「这个人的 openid 是什么」。
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
// **没有绑定不是错误**：它是一个事实，怎么收场由签约那条路决定（告诉用户先去小程序里登录一次），
// 在这里一刀切报错会让「问不到」与「没绑定」又混成一句一样的话。真正要 fail-closed 的是
// 「问不到」，见 ErrWalletIdentityUnavailable。
//
// 空串的 openid 按 found=false 处理：一个空的 openid 不是身份，把它当成身份传下去正是**静默
// 降级**——渠道会拒，而我们会以为那是一次渠道故障。
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
