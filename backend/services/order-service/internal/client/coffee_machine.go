// Package client 是 order-service 对其他服务的出网调用，全部走 gRPC。
//
// 两种凭据，别混用：
//
//   - 代表基础设施去问事实（本题的 GetDevice、下单时校验设备状态），带服务令牌
//     （platform/auth.WithServiceToken）。它不代表任何用户。
//   - 代表调用者本人去问「他能做什么」（user.go 的 AdminAccessResolver），带他自己的
//     access token（platform/auth.WithAccessToken）。这里绝不能带服务令牌——那等于用
//     基础设施的身份替一个管理员宣称权限。
package client

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	coffeemachinev1 "github.com/panda-dev/panda-v2/contracts/proto/coffee_machine/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Device 是下单校验用得上的设备事实。
//
// 只带这几个字段，不整个透传 proto 的 Device：「下单时校验设备状态」（方案 5.8）需要的是
// 「这台机器在哪、我们让不让它用、厂商说它通不通」，其余字段（故障描述、上次同步时间）
// 在下单这条路上没有任何判断依据，带进来只会让人以为它们被用上了。
type Device struct {
	ID           string
	StoreID      string
	SerialUnique string
	// active=启用，disabled=停用。这是「我们让不让它用」。
	Status string
	// 厂商上报的在线状态。nil = 从未同步过，与「明确离线」不是一回事。
	VendorOnline *bool
	// 最近一次故障码，空串表示没有故障记录。
	FaultCode string
}

// DeviceReader 按 ID 读设备。
//
// found 为 false 表示咖啡机服务里没有这个 id；err 表示没问到。两者必须分开，理由同
// coffee-machine-service 的 StoreResolver：「不存在」拒单是 404，「问不到」拒单是 503，
// 混成一个错误会让一次下游抖动看起来像用户拿着一个假设备号。
type DeviceReader struct {
	devices coffeemachinev1.CoffeeMachineServiceClient
	token   string
	timeout time.Duration
}

// NewDeviceReader 复用调用方那条连接：main 只拨一次，多个调用点共享同一个 *grpc.ClientConn。
func NewDeviceReader(conn grpc.ClientConnInterface, token string, timeout time.Duration) (*DeviceReader, error) {
	if conn == nil {
		return nil, errors.New("coffee machine service connection is required")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("coffee machine service token is required")
	}
	if timeout <= 0 {
		return nil, errors.New("coffee machine service timeout must be positive")
	}
	return &DeviceReader{devices: coffeemachinev1.NewCoffeeMachineServiceClient(conn), token: token, timeout: timeout}, nil
}

// Get 读一台设备。
func (r *DeviceReader) Get(ctx context.Context, deviceID string) (*Device, bool, error) {
	if r == nil || r.devices == nil {
		return nil, false, errors.New("device reader is not configured")
	}
	ctx, cancel := context.WithTimeout(auth.WithServiceToken(ctx, r.token), r.timeout)
	defer cancel()
	resp, err := r.devices.GetDevice(ctx, &coffeemachinev1.DeviceID{Id: deviceID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	if resp == nil || resp.GetId() == "" {
		// 应答里没有 id 又不算 NotFound：这不可能是一次正常回答，当成问不到，
		// 别让一个空应答被读成「设备存在且状态为空」。
		return nil, false, errors.New("coffee machine service returned an empty device")
	}
	device := &Device{
		ID:           resp.GetId(),
		StoreID:      resp.GetStoreId(),
		SerialUnique: resp.GetSerialUnique(),
		Status:       resp.GetStatus(),
		FaultCode:    resp.GetLastFaultCode(),
	}
	// 用 proto 的 presence 而不是零值：optional bool 的「没给」和「给了 false」是两件事，
	// 读成 bool 就把「从未同步过」压成了「离线」，下单校验会拿一个我们其实不知道的事实
	// 去拒单。
	if resp.VendorOnline != nil {
		online := resp.GetVendorOnline()
		device.VendorOnline = &online
	}
	return device, true, nil
}
