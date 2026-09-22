package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/client"
	"github.com/panda-dev/panda-v2/backend/services/partner-service/internal/ingress"
)

// 这个文件是**开放接口**那一半：合作方调进来的请求在这里被翻译成一次对别的服务的调用。
//
// # 这一层不碰本库的任何业务数据
//
// 合作方要查的订单、券、会员权益分别属于 order-service / coupon-service /
// membership-service，本服务**一个都不复制**（见 Package service 的说明）。这里做的只有
// 三件事：校验参数形状、把身份（partner_id）取出来、把请求转出去。任何一条被转发的查询都
// 不应该在本库里留下业务行——留下就是第二份真相。
//
// # 今天三条：一条查询，两条写
//
// 一条是「查会员权益」（GET，见 openapi.go 那个入口），另外两条是**设备侧回调**：设备刷卡回执
// （POST /v1/openapi/device/sync-order，见 device_order.go）与**取货码**
// （POST /v1/openapi/device/pickup，见 pickup.go）——它们不属于方案 §3.4 要的那三条查询，
// 而是线下刷卡机与取货码那一节（方案 §四）的两个入口。两条都不是「顺手加的开放接口」：
// 刷卡那条的钱已经在机器上收过了，我们只是让订单域把既成事实记下来；取货码那条**会从我们
// 自己的设备余额里扣钱**。这两条是本服务全部会改变别域状态的路。
//
// # 方案 §3.4 要的三条查询里，仍然只有一条落得下
//
// order-service 与 coupon-service 的查询 RPC 到今天都还没有（order 那边唯一的一条 RPC 是
// CreateDeviceOrder，正是上面那条写路径调的）。硬造一条 RPC 或直接连对方的库都是明确不允许
// 的，而一个假接口比没有接口更坏：它会以「已经支持」的样子出现在对接文档里。缺哪条 RPC 记在
// 回报与 cmd/main.go 的注释里。

// ErrUserIDInvalid：userId 不是合法的 uuid。
//
// 在**调用下游之前**挡下来。理由与 payment 那边 msgInvalidUserID 同一条：不是 uuid 的串会
// 在 PostgreSQL（或对方服务的 uuid.Parse）那里报错，于是合作方输错了一位会收到一句
// 「服务暂时不可用」，而真相是他的参数写错了。
var ErrUserIDInvalid = errors.New("userId must be a UUID")

// ErrCallerMissing：请求上下文里没有合作方身份。
//
// 它**不该出现**：开放接口那棵树整个被 ingress.Guard 包着，而 Guard 只在九道校验全过之后
// 才把 Caller 放进上下文。留着这一条是因为「handler 被单独注册到别处」这种改动不会报错——
// 它会让所有开放接口变成匿名的、任何人都能查任何人的会员权益，更重的是**取货码那一条**：
// 一条没验过签的报文会直接扣掉一台设备的余额。失败关闭比失败开放便宜。
var ErrCallerMissing = errors.New("partner identity is missing from the request context")

// EntitlementReader 读一个用户此刻的会员价资格。
//
// 与其它服务同一条做法：接口定义在消费者这一侧（这里），装配处传真的客户端，测试传桩。
// 返回类型用 client 的那一个而不是在本包再造一个——它是这次调用的**形状**，属于适配器。
type EntitlementReader interface {
	GetMemberPriceEntitlement(ctx context.Context, userID string) (*client.MembershipEntitlement, error)
}

// OpenAPIService 是开放接口的服务。
//
// 两个依赖，方向相反：entitlements 读会员域（只读的转发），deviceOrders 把一笔设备侧订单
// **写进**订单域（本服务全部会改变别域状态的调用都在它身上：刷卡回执与取货码，见
// device_order.go 与 pickup.go）。两者都不碰本库的任何业务数据——这里没有订单、没有券，
// 也没有第二份会员资格。
type OpenAPIService struct {
	entitlements EntitlementReader
	deviceOrders DeviceOrderRecorder
}

func NewOpenAPIService(entitlements EntitlementReader, deviceOrders DeviceOrderRecorder) *OpenAPIService {
	return &OpenAPIService{entitlements: entitlements, deviceOrders: deviceOrders}
}

// MemberPriceEntitlement 查一个用户此刻的会员价资格。
//
// # 为什么返回值里带着发起查询的合作方
//
// 会员服务那条 RPC 只收 user_id（见 client.MembershipEntitlement 的说明），**合作方身份传
// 不过去**——这不是这一层偷懒，是对方的 proto 里没有位置放它，而平台也没有「操作人元数据」
// 这样的约定。所以本服务把 partnerId 放进响应：调用方（与我们的调用日志）至少能对上「这次
// 查询是哪一个合作方发起的」，而下游那边看不到这件事。这个缺口写在回报里，不在这里假装
// 补上了。
func (s *OpenAPIService) MemberPriceEntitlement(ctx context.Context, userID string) (*client.MembershipEntitlement, string, error) {
	caller, ok := ingress.CallerFrom(ctx)
	if !ok {
		return nil, "", ErrCallerMissing
	}
	parsed, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil {
		return nil, "", ErrUserIDInvalid
	}
	entitlement, err := s.entitlements.GetMemberPriceEntitlement(ctx, parsed.String())
	if err != nil {
		return nil, "", err
	}
	return entitlement, caller.PartnerID, nil
}
