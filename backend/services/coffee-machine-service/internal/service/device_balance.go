package service

import (
	"context"
	"errors"
	"strings"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// 扣设备余额（取货码那条路）的输入校验错误。
//
// **没有加进 ValidationErrors**：那个切片是给 HTTP 控制器判「该回 400 还是 500」用的，
// 而这条路只有 gRPC 一个入口，rpc 层直接按哨兵错误分档回 InvalidArgument。加进去只会
// 让那份清单里多一条永远不会被 HTTP 走到的项。
var (
	// ErrDeductDeviceRequired 表示没给设备。取货码落在哪台机器上是这条路唯一不能缺的
	// 输入——缺了连查哪一行余额都无从谈起。
	ErrDeductDeviceRequired = errors.New("device_id 不能为空")
	// ErrDeductAmountInvalid 表示金额不是正数。
	//
	// 0 与负数在这里一并挡掉，而不是留给仓储：0 会撞流水表的 amount <> 0 约束，
	// 负数在扣减这条路上等于「给这台机器加钱」，两件都不是调用方想要的。
	ErrDeductAmountInvalid = errors.New("扣减金额必须为正数")
	// ErrDeductRequestIDRequired 表示没给幂等键。
	//
	// 它是这条路唯一防重投的东西（device_balance_ledger_one_per_request 的条件正是
	// `request_id <> ''`，空串会绕过那个索引）。所以空串不能当「没有幂等键但有别的
	// 办法」，只能当输入不合法。
	ErrDeductRequestIDRequired = errors.New("request_id 不能为空")
)

// DeductDeviceBalanceInput 是一次扣减的输入，金额为**正数**（分）。
type DeductDeviceBalanceInput struct {
	Amount    int64
	RequestID string
	Remark    string
	// PickupPassword 是设备上那个静态验证码（顾客敲、厂商转报）。它的比对在仓储里、
	// 行锁之内做（见 repository.DeductDeviceBalanceParams）。
	//
	// 这里**不 trim**：那是顾客敲进去的一串码，前后空格到底算不算数只有比对那一侧知道，
	// 服务层自作主张地 trim 会把一个「码里有空格」的配置悄悄改写成别的意思。
	PickupPassword string
}

// DeductDeviceBalanceResult 是一次扣减的结果。
type DeductDeviceBalanceResult struct {
	// BalanceAfter 是扣完之后的余额（分）；Applied=false 时是**当初那一次**记下的余额。
	BalanceAfter int64
	// Applied=false 表示这个 request_id 已经扣过了，本次一个字段都没写。调用方要把它
	// 当成成功继续往下走（见 repository.DeductDeviceBalanceResult 的说明）。
	Applied bool
	// Amount 是这一次**实际扣掉**的金额（正数，分）；Applied=false 时是当初那一次扣的
	// （从流水上读回来的），与本次请求里带的金额可能不等。
	Amount int64
}

// DeviceBalanceService 是设备余额的扣减路径。
//
// 与 MasterDataService 分家：那个服务从头到尾只读（连它自己的注释都这么写着），
// 而这是本服务唯一对外开的写口（方案 §四的取货码）。混在一起会让那句注释变成假的，
// 也会让「改余额的入口有几个」这个问题没有单一答案——后台调整走 AdminService，
// 这台机器上卖掉一杯走这里，两者都不该顺着读路径进来。
type DeviceBalanceService struct {
	balance repository.DeviceBalanceRepository
}

func NewDeviceBalanceService(balance repository.DeviceBalanceRepository) *DeviceBalanceService {
	return &DeviceBalanceService{balance: balance}
}

// DeductDeviceBalance 从一台设备的咖啡余额里扣一笔并写流水。
//
// 只做形状校验（谁为空、金额正不正），余额够不够是仓储在事务里判的——那需要和设备行
// 一起加锁才准，在这里先查一次只会得到一个会过期的结论。
//
// 三个参数都先 trim 再判空，与写路径（buildDevice / buildDrink）口径一致：厂商报文里
// 多一个空格，不该变成一次「设备不存在」。
func (s *DeviceBalanceService) DeductDeviceBalance(
	ctx context.Context, deviceID string, in DeductDeviceBalanceInput,
) (*DeductDeviceBalanceResult, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil, ErrDeductDeviceRequired
	}
	if in.Amount <= 0 {
		return nil, ErrDeductAmountInvalid
	}
	requestID := strings.TrimSpace(in.RequestID)
	if requestID == "" {
		return nil, ErrDeductRequestIDRequired
	}

	result, err := s.balance.DeductDeviceBalance(ctx, repository.DeductDeviceBalanceParams{
		DeviceID:       deviceID,
		Amount:         in.Amount,
		RequestID:      requestID,
		Remark:         strings.TrimSpace(in.Remark),
		PickupPassword: in.PickupPassword,
	})
	if err != nil {
		return nil, err
	}
	return &DeductDeviceBalanceResult{
		BalanceAfter: result.BalanceAfter,
		Applied:      result.Applied,
		Amount:       result.Amount,
	}, nil
}
