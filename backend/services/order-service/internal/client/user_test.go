package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeUserConn 与 payment_test.go 的 fakePaymentConn 同一个做法：只实现 Invoke，内嵌接口
// 让流式方法留在「没实现」的状态。
type fakeUserConn struct {
	grpc.ClientConnInterface
	reply *userv1.GetWechatIdentityResponse
	err   error
	// token 记下这次调用带出去的服务令牌；request 记下入参，两者都是这条链路的契约。
	token   []string
	request *userv1.GetWechatIdentityRequest
}

func (c *fakeUserConn) Invoke(ctx context.Context, _ string, req, reply any, _ ...grpc.CallOption) error {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		c.token = md.Get(auth.MetadataServiceToken)
	}
	if in, ok := req.(*userv1.GetWechatIdentityRequest); ok {
		c.request = in
	}
	if c.err != nil {
		return c.err
	}
	if c.reply == nil {
		return nil
	}
	// 不能按值拷：protobuf 消息里带 MessageState，赋值会被 vet 拦下（也真的会拷锁）。
	out, ok := reply.(proto.Message)
	if !ok {
		return status.Errorf(codes.Internal, "fakeUserConn: unexpected reply type %T", reply)
	}
	proto.Merge(out, c.reply)
	return nil
}

func newWalletReader(t *testing.T, conn grpc.ClientConnInterface) *WalletIdentityReader {
	t.Helper()
	r, err := NewWalletIdentityReader(conn, "0123456789012345678901234567890123456789", time.Second)
	if err != nil {
		t.Fatalf("NewWalletIdentityReader = %v", err)
	}
	return r
}

// TestMiniappOpenIDSendsThePinnedQuestion 钉住这次调用的三个契约：带服务令牌、
// 问的是**付款人**的 id、要的是**小程序**下的身份。
//
// app_type 写错的症状不是查不到，而是查到另一个值（公众号的 openid 与小程序的不一样），
// 而它会一路走到渠道报文里，换来一句与本意无关的错。
func TestMiniappOpenIDSendsThePinnedQuestion(t *testing.T) {
	conn := &fakeUserConn{reply: &userv1.GetWechatIdentityResponse{Openid: "o_miniapp_1"}}
	reader := newWalletReader(t, conn)

	openID, found, err := reader.MiniappOpenID(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("MiniappOpenID: %v", err)
	}
	if !found || openID != "o_miniapp_1" {
		t.Fatalf("openID = %q, found = %v; want o_miniapp_1/true", openID, found)
	}
	if len(conn.token) != 1 || conn.token[0] != "0123456789012345678901234567890123456789" {
		t.Fatalf("service token = %v; 这一次调用代表订单域，必须带共享服务令牌", conn.token)
	}
	if conn.request.GetUserId() != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("user_id = %q; want the payer", conn.request.GetUserId())
	}
	if conn.request.GetAppType() != wechatAppMiniapp {
		t.Fatalf("app_type = %q; want %q", conn.request.GetAppType(), wechatAppMiniapp)
	}
}

// TestMiniappOpenIDSeparatesUnboundFromUnavailable 守住这条路上唯一的那个分界：
// 「他没绑微信」（有结论，返回 found=false）与「我们没问到」（故障，返回错误）。
//
// 混成一个的代价是双向的：把故障说成没绑定，用户会去重新登录微信；把没绑定说成故障，
// 用户会反复重试一个永远不会成的请求。
func TestMiniappOpenIDSeparatesUnboundFromUnavailable(t *testing.T) {
	cases := []struct {
		name      string
		reply     *userv1.GetWechatIdentityResponse
		err       error
		wantFound bool
		wantErr   error
	}{
		{
			name:      "identity not found",
			err:       status.Error(codes.NotFound, "wechat identity not found"),
			wantFound: false,
		},
		{
			// 身份行在、openid 是空串：这不是一个身份，按没绑定处理，不能当成 found 传下去。
			name:      "empty openid",
			reply:     &userv1.GetWechatIdentityResponse{Openid: ""},
			wantFound: false,
		},
		{
			name:      "blank openid",
			reply:     &userv1.GetWechatIdentityResponse{Openid: "   "},
			wantFound: false,
		},
		{
			name:    "service unavailable",
			err:     status.Error(codes.Unavailable, "connection refused"),
			wantErr: ErrWalletIdentityUnavailable,
		},
		{
			name:    "internal error",
			err:     status.Error(codes.Internal, "query wechat identity"),
			wantErr: ErrWalletIdentityUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newWalletReader(t, &fakeUserConn{reply: tc.reply, err: tc.err})
			openID, found, err := reader.MiniappOpenID(context.Background(), "user-1")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v; want %v", err, tc.wantErr)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v; want %v", found, tc.wantFound)
			}
			if openID != "" {
				t.Fatalf("openID = %q; 这两种结局都不该带出一个 openid", openID)
			}
		})
	}
}

// TestNewWalletIdentityReaderValidatesItsInputs：与同包其它读端一样的构造校验。
// 一个空地址的连接会在第一次发起支付时才炸，而那一次请求正卡在用户的收银台上。
func TestNewWalletIdentityReaderValidatesItsInputs(t *testing.T) {
	conn := &fakeUserConn{}
	if _, err := NewWalletIdentityReader(nil, "token", time.Second); err == nil {
		t.Error("nil connection must be rejected")
	}
	if _, err := NewWalletIdentityReader(conn, "   ", time.Second); err == nil {
		t.Error("blank token must be rejected")
	}
	if _, err := NewWalletIdentityReader(conn, "token", 0); err == nil {
		t.Error("non-positive timeout must be rejected")
	}
}
