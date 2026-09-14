package service

import (
	"context"
	"errors"
)

// 微信与短信通道的失败词汇定义在这里，而不是在实现它们的 client 包里：
// 接口由使用方定义，它的错误也归使用方。这样 handler 只认识 service 的错误，
// 不需要知道「微信 errcode」这种传输层概念；client 反过来 import 本包来用这些
// 值——本包永远不 import client，方向不会成环。
var (
	// ErrWechatAuthFailed 微信明确拒绝了这次授权：code 无效、已用过、已过期。
	// 这是用户这一侧的问题，对应 401。
	ErrWechatAuthFailed = errors.New("微信授权失败")
	// ErrWechatUnavailable 够不着微信，或者 appid/secret 没配。这是部署问题，
	// 对应 503。和上面分开是必须的：混成一个 401 的话，「secret 配错了」会在
	// 用户侧表现为「你的登录已失效」，两边都查不出来。
	ErrWechatUnavailable = errors.New("微信登录暂不可用")
	// ErrSmsUnavailable 没有可用的短信通道（还没接服务商，或本机没开日志开关）。
	// 也是部署问题，对应 503。
	ErrSmsUnavailable = errors.New("短信服务暂不可用")
)

// MerchantAccessPort exposes the small set of merchant facts needed by
// merchant accounts. It is implemented by the remote merchant-service client.
type MerchantAccessPort interface {
	FindStatus(ctx context.Context, merchantID string) (string, error)
	FindName(ctx context.Context, merchantID string) (string, error)
}

// WechatGateway 是小程序登录需要的微信开放接口。定义成接口而不是直接调客户端，
// 是为了让登录链路可测：这两次调用都要往返微信服务器，单测里不可能拿真凭据跑，
// 有了接口就能拿 httptest 顶上去。
//
// 返回值刻意用裸串而不是一个结构体：这样 client 包不需要反过来 import service，
// 和 MerchantAccessPort 保持同一个方向。
type WechatGateway interface {
	// Code2Session 用 wx.login 拿到的 code 换 openid 和 unionid。
	// unionid 可能为空——未绑定微信开放平台的小程序拿不到它。
	Code2Session(ctx context.Context, code string) (openID, unionID string, err error)
	// ResolvePhone 用 getPhoneNumber 返回的 code 换手机号。
	//
	// 老系统把 session_key 发给前端、由前端解出手机号再回传，那等于让客户端
	// 自己证明手机号是自己的。这里整个解密都在服务端做，session_key 不出网。
	ResolvePhone(ctx context.Context, code string) (phone string, err error)
}

// SmsSender 把验证码发给用户。
//
// 生产实现是短信服务商；本机开发没有服务商，用日志实现把验证码打进日志，
// 否则本地根本走不通登录。两者的选择在 NewSmsSender 里，且生产环境不会退化成
// 日志实现——把验证码写进日志等于把它交给了所有能看日志的人。
type SmsSender interface {
	Send(ctx context.Context, phone, code string) error
}
