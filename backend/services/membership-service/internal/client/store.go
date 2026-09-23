package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	merchantv1 "github.com/panda-dev/panda-v2/contracts/proto/merchant/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StoreClient 回答两个关于商户域门店的问题：这家店**存在吗**、这几家店**叫什么**。
//
// 它用的是服务令牌而不是调用方的 access token：问的是商户域的事实，不是「这个人能不能看
// 这家店」。lottery-service 的 StoreClient、coffee-machine-service 的 StoreResolver、
// user-service 的 MerchantGRPCClient 走的都是同一条路。
//
// 与 lottery-service 的同名类型逐字同形（那边是本仓库里最近的先例）。会员库同样只存门店 id
// （见 migrations/membership）：名字是商户域的事实，本服务不留第二份。于是「校验」与
// 「显示」这两件事都落在这个客户端上——一个是开通前问一次存在性，一个是每次读一页名字。
type StoreClient struct {
	merchants merchantv1.MerchantServiceClient
	token     string
	timeout   time.Duration
}

// NewStoreClient 复用调用方那条连接：main 只拨一次，两个调用点共享同一个 *grpc.ClientConn。
func NewStoreClient(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*StoreClient, error) {
	if conn == nil {
		return nil, errors.New("merchant service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("merchant service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("merchant service timeout must be positive")
	}
	return &StoreClient{merchants: merchantv1.NewMerchantServiceClient(conn), token: token, timeout: timeout}, nil
}

func (c *StoreClient) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(auth.WithServiceToken(ctx, c.token), c.timeout)
}

// Exists 回答「商户域里有没有这家门店」。
//
// 「不存在」和「问不到」必须分得开，否则调用方只能二选一，两边都错：把两者都当成不存在，
// 商户服务抖一下就会拒掉一次合法开通；都当成存在，这条校验就形同虚设。所以前者是
// (false, nil)，后者是 (false, err)，不合并成一个错误。
//
// 这一条与名字解析（Names）的处理**故意不一样**：这里问不到必须让开通失败，因为归属门店是
// 一条「事后能对得上账」的留痕，而「这家店到底存不存在」我们当时并没有问出来。
func (c *StoreClient) Exists(ctx context.Context, storeID string) (bool, error) {
	if c == nil || c.merchants == nil {
		return false, errors.New("store client is not configured")
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.GetStore(ctx, &merchantv1.GetStoreRequest{StoreId: storeID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
		return false, err
	}
	if resp.GetStore() == nil {
		// 应答里没有门店又不算 NotFound：这不可能是一次正常回答，当成问不到，别让一个空应答
		// 被读成「存在」——那正好是这条校验唯一不该犯的错。
		return false, errors.New("merchant service returned an empty store")
	}
	return true, nil
}

// Names 一次性解出一页门店的名字。
//
// **问不到的名字返回空串而不是错误**：这个返回值只用来显示。商户服务抖一下就让后台的会员
// 列表整页打不开——连冻结一个会员都做不了——代价比几个空格子大得多；而空格子也不构成谎言，
// 门店的身份是那一列 id，它来自本服务的库，不来自商户域。
//
// 商户域不认识的 id 会从返回的 map 里缺席（那边的契约如此），调用方按空串处理。
func (c *StoreClient) Names(ctx context.Context, storeIDs []string) (map[string]string, error) {
	if c == nil || c.merchants == nil {
		return nil, errors.New("store client is not configured")
	}
	if len(storeIDs) == 0 {
		return map[string]string{}, nil
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.merchants.ResolveScopeNames(ctx, &merchantv1.ResolveScopeNamesRequest{StoreIds: storeIDs})
	if err != nil {
		return nil, err
	}
	return resp.GetStoreNames(), nil
}
