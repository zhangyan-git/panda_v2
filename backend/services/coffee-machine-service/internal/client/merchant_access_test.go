package client

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/authz"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// merchantAccessConn 记下一次调用带出去的 access token，并按配置回一个答案或一个错误。
type merchantAccessConn struct {
	grpc.ClientConnInterface
	reply *userv1.GetMerchantAccessResponse
	err   error
	// authorization 记下这次调用带出去的 Authorization 头。
	authorization []string
}

func (c *merchantAccessConn) Invoke(ctx context.Context, _ string, _, reply any, _ ...grpc.CallOption) error {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		c.authorization = md.Get(auth.MetadataAuthorization)
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
		return status.Errorf(codes.Internal, "merchantAccessConn: unexpected reply type %T", reply)
	}
	proto.Merge(out, c.reply)
	return nil
}

// TestMerchantAccessResolverMapping 钉住「身份服务的每一种应答，到了这边算哪一档」。
//
// 这一层是 401/403/503 的分水岭：中间件只认 ErrUnauthenticated 与 ErrForbidden 两个
// 哨兵，其余一律 503。映射写错的表现是「账号被停用」被显示成「服务不可用」，或者反过来
// 把一次查不到的失败读成「这个账号被拒了」。
func TestMerchantAccessResolverMapping(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"令牌不再被接受", status.Error(codes.Unauthenticated, "no"), authz.ErrUnauthenticated},
		{"账号被停用或不属于这个商户", status.Error(codes.PermissionDenied, "no"), authz.ErrForbidden},
		// 这两条是「问不到」，必须原样传出去，让中间件失败关闭成 503——既不回落成
		// 空集，也不放宽成全量。
		{"身份服务不可用", status.Error(codes.Unavailable, "down"), nil},
		{"身份服务内部错误", status.Error(codes.Internal, "boom"), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := NewMerchantAccessResolver(&merchantAccessConn{err: tt.err})
			_, err := r.Resolve(context.Background(), "tok")
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("err=%v want %v", err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatal("err=nil，一个问不到的边界被当成了答案")
			}
			if errors.Is(err, authz.ErrUnauthenticated) || errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("err=%v 被映射成了「调用方被拒」", err)
			}
		})
	}
}

func TestMerchantAccessResolverReturnsTheExpandedBoundary(t *testing.T) {
	r := NewMerchantAccessResolver(&merchantAccessConn{reply: &userv1.GetMerchantAccessResponse{
		MerchantId: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIds: []string{"b1", "b2"}, StoreIds: []string{"s1", "s2"},
	}})
	grants, err := r.Resolve(context.Background(), "tok")
	if err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	// 目标整组跟着走：过滤只用 StoreIDs，但「边界是怎么来的」不能被展开结果盖掉。
	want := authz.MerchantGrants{MerchantID: "m1", ScopeType: auth.ScopeTypeBrand, ScopeIDs: []string{"b1", "b2"}, StoreIDs: []string{"s1", "s2"}}
	if grants.MerchantID != want.MerchantID || grants.ScopeType != want.ScopeType ||
		!reflect.DeepEqual(grants.ScopeIDs, want.ScopeIDs) || len(grants.StoreIDs) != 2 {
		t.Fatalf("grants=%+v want %+v", grants, want)
	}
}

// 调用方的令牌走 metadata 带过去：user-service 会拿它重新校验身份，请求体里没有任何
// 身份字段可以伪造。
func TestMerchantAccessResolverForwardsTheCallersToken(t *testing.T) {
	conn := &merchantAccessConn{reply: &userv1.GetMerchantAccessResponse{MerchantId: "m1", ScopeType: auth.ScopeTypeMerchant}}
	r := NewMerchantAccessResolver(conn)
	if _, err := r.Resolve(context.Background(), "caller-token"); err != nil {
		t.Fatalf("Resolve = %v", err)
	}
	if len(conn.authorization) != 1 || conn.authorization[0] != "Bearer caller-token" {
		t.Fatalf("Authorization=%v want [Bearer caller-token]", conn.authorization)
	}
}

// 没有解析器是装配错误，不能当成「没有点位」：空边界会让界面显示成「你没有设备」。
func TestMerchantAccessResolverNilSafe(t *testing.T) {
	var r *MerchantAccessResolver
	grants, err := r.Resolve(context.Background(), "tok")
	if err == nil {
		t.Fatal("err=nil，未配置的连接被当成了一个答案")
	}
	if grants.StoreIDs != nil {
		t.Fatalf("StoreIDs=%v want nil", grants.StoreIDs)
	}
}
