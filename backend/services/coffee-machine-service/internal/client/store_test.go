package client

import (
	"context"
	"testing"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// fakeConn 只实现 Invoke：生成的客户端除了流式方法都走这一条。内嵌接口是为了让
// NewStream 之类的方法留在「没实现」的状态，而不是在这里编一个假的流出来。
type fakeConn struct {
	grpc.ClientConnInterface
	reply *merchantv1.GetStoreResponse
	err   error
	// token 记下这次调用带出去的服务令牌，用于验证 Resolve 确实认了证。
	token []string
}

func (c *fakeConn) Invoke(ctx context.Context, _ string, _, reply any, _ ...grpc.CallOption) error {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		c.token = md.Get(auth.MetadataServiceToken)
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
		return status.Errorf(codes.Internal, "fakeConn: unexpected reply type %T", reply)
	}
	proto.Merge(out, c.reply)
	return nil
}

func newResolver(t *testing.T, conn grpc.ClientConnInterface) *StoreResolver {
	t.Helper()
	r, err := NewStoreResolver(conn, "0123456789012345678901234567890123456789", time.Second)
	if err != nil {
		t.Fatalf("NewStoreResolver = %v", err)
	}
	return r
}

// TestStoreResolverMapping 钉住「商户服务的每一种应答，到了这边算哪一档」。
//
// 这一层是 400/503 的分水岭，之前两次判断失误都出在这里：NotFound 之外的任何错误都
// 会变成 Internal，而 Internal 不是 NotFound，于是被读成「问不到」回 503。所以
// 「不存在」「停用」「看不懂」各自落到哪一档，必须逐条写死。
func TestStoreResolverMapping(t *testing.T) {
	cases := []struct {
		name      string
		reply     *merchantv1.GetStoreResponse
		err       error
		wantErr   bool   // 非 nil 即「问不到」，调用方回 503
		wantFound bool   // 只在 wantErr 为 false 时有意义
		wantState string // 同上
	}{
		{
			name:  "商户服务说没这个点位",
			err:   status.Error(codes.NotFound, "merchant resource not found"),
			wantFound: false,
		},
		{
			name:    "传输层错误算问不到",
			err:     status.Error(codes.Unavailable, "connection refused"),
			wantErr: true,
		},
		{
			name:      "应答里没有 store",
			reply:     &merchantv1.GetStoreResponse{},
			wantErr:   true,
		},
		{
			name:      "字符串形态 active",
			reply:     storeWith("active", merchantv1.ResourceStatus_RESOURCE_STATUS_UNSPECIFIED),
			wantFound: true, wantState: "active",
		},
		{
			name:      "枚举形态 disabled",
			reply:     storeWith("", merchantv1.ResourceStatus_RESOURCE_STATUS_DISABLED),
			wantFound: true, wantState: "disabled",
		},
		{
			// 枚举有值但不是这两个之一：上游加了新状态。既不能说「停用」（我们证不出来），
			// 也不能说「不存在」，只能当成这次没问出结果。
			name:    "不认识的枚举算问不到，不能猜成停用",
			reply:   storeWith("", merchantv1.ResourceStatus(99)),
			wantErr: true,
		},
		{
			// 字符串和枚举都空：同样是没有答案，不是「存在但状态为空」。
			name:    "两种形态都为空算问不到",
			reply:   storeWith("", merchantv1.ResourceStatus_RESOURCE_STATUS_UNSPECIFIED),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeConn{reply: tc.reply, err: tc.err}
			state, found, err := newResolver(t, conn).Resolve(t.Context(), "00000000-0000-0000-0000-0000000000ff")

			if tc.wantErr {
				// 「问不到」必须带 err：调用方靠它区分「不存在」和「拿不到答案」，
				// 合并成一个返回值就会让商户服务抖一下拒掉一次合法保存。
				if err == nil {
					t.Fatalf("Resolve = (%q, %v, nil), want an error", state, found)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve = %v, want nil", err)
			}
			if found != tc.wantFound || state != tc.wantState {
				t.Fatalf("Resolve = (%q, %v), want (%q, %v)", state, found, tc.wantState, tc.wantFound)
			}
		})
	}
}

// TestStoreResolverSendsServiceToken 盯的是这条调用拿什么身份去问商户服务。它是基础
// 设施代表用户去问事实，不是某个用户自己的请求——漏了令牌，商户服务会以未授权拒掉，
// 而表现是 503「门店服务暂时不可用」，跟真的连不上长得一模一样。
func TestStoreResolverSendsServiceToken(t *testing.T) {
	conn := &fakeConn{reply: storeWith("active", merchantv1.ResourceStatus_RESOURCE_STATUS_UNSPECIFIED)}
	if _, _, err := newResolver(t, conn).Resolve(t.Context(), "00000000-0000-0000-0000-0000000000ff"); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if len(conn.token) == 0 || conn.token[0] == "" {
		t.Fatalf("这次调用没带 %s", auth.MetadataServiceToken)
	}
}

func storeWith(state string, code merchantv1.ResourceStatus) *merchantv1.GetStoreResponse {
	return &merchantv1.GetStoreResponse{
		Store: &merchantv1.Store{Status: state, StatusCode: code},
	}
}

// TestStoreResolverWithoutToken 拦的是装配错误：没有令牌就不该造出一个「能发但发出去
// 一定被拒」的调用器，而应该在启动时就报出来。
func TestStoreResolverWithoutToken(t *testing.T) {
	if _, err := NewStoreResolver(&fakeConn{}, "", time.Second); err == nil {
		t.Fatal("NewStoreResolver with an empty token = nil, want an error")
	}
}

// TestStoreResolverNilSafe 盯的是零值：main 里装配漏了的时候，调用点拿到的可能是
// (*StoreResolver)(nil)。它不能 panic，也不能答「点位可用」——后者会让校验静默失效。
func TestStoreResolverNilSafe(t *testing.T) {
	var r *StoreResolver
	if _, _, err := r.Resolve(t.Context(), "00000000-0000-0000-0000-0000000000ff"); err == nil {
		t.Fatal("nil resolver reported no error")
	}
}
