package service

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/coffee-machine-service/internal/repository"
)

// recordingBalance 记下仓储真正收到了什么。嵌入 nil 接口：用例要确认的是「这几个参数
// 根本没走到仓储」，真调到别的办法会 panic，而不是安静地回零值让用例变绿。
type recordingBalance struct {
	repository.DeviceBalanceRepository
	calls []repository.DeductDeviceBalanceParams
}

func (r *recordingBalance) DeductDeviceBalance(_ context.Context, in repository.DeductDeviceBalanceParams) (*repository.DeductDeviceBalanceResult, error) {
	r.calls = append(r.calls, in)
	return &repository.DeductDeviceBalanceResult{BalanceAfter: 100, Applied: true}, nil
}

// TestDeductDeviceBalanceRejectsBlanksBeforeTheQuery 是取货码那条路上三件必须在这里
// 停下的事：没给设备、没给金额（或给了负数）、没给幂等键。
//
// 尤其是不给幂等键这一条：device_balance_ledger_one_per_request 只索引 request_id
// 非空的行，空串会**绕过**那个唯一索引——数据库那一层兜不住，只有这里能挡。
// 放它过去的表现是「重投扣两次」，而且两次都成功、都没有报错。
func TestDeductDeviceBalanceRejectsBlanksBeforeTheQuery(t *testing.T) {
	valid := DeductDeviceBalanceInput{Amount: 1500, RequestID: "req-1"}

	cases := []struct {
		name     string
		deviceID string
		in       DeductDeviceBalanceInput
		want     error
	}{
		{"没给设备", "", valid, ErrDeductDeviceRequired},
		{"设备只有空格", "   ", valid, ErrDeductDeviceRequired},
		{"金额为零", "device-1", DeductDeviceBalanceInput{RequestID: "req-1"}, ErrDeductAmountInvalid},
		{"金额为负", "device-1", DeductDeviceBalanceInput{Amount: -1500, RequestID: "req-1"}, ErrDeductAmountInvalid},
		{"没给幂等键", "device-1", DeductDeviceBalanceInput{Amount: 1500}, ErrDeductRequestIDRequired},
		{"幂等键只有空格", "device-1", DeductDeviceBalanceInput{Amount: 1500, RequestID: "   "}, ErrDeductRequestIDRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &recordingBalance{}
			svc := NewDeviceBalanceService(repo)

			_, err := svc.DeductDeviceBalance(context.Background(), tc.deviceID, tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(repo.calls) != 0 {
				t.Fatalf("非法输入走到了仓储：%+v", repo.calls)
			}
		})
	}
}

// TestDeductDeviceBalanceTrimsWhatItPassesOn 盯的是厂商报文里的空格。
//
// 设备号与幂等键都是对方报文里的字符串，前后多一个空格只会在现场变成一次「设备不存在」
// 或者一次重复扣款（幂等键变了）。remark 只是给人看的，同样 trim。
func TestDeductDeviceBalanceTrimsWhatItPassesOn(t *testing.T) {
	repo := &recordingBalance{}
	svc := NewDeviceBalanceService(repo)

	result, err := svc.DeductDeviceBalance(context.Background(), "  device-1  ", DeductDeviceBalanceInput{
		Amount: 1500, RequestID: "  req-1  ", Remark: "  取货码 30 秒  ", PickupPassword: " 1357 ",
	})
	if err != nil {
		t.Fatalf("deduct device balance: %v", err)
	}
	if len(repo.calls) != 1 {
		t.Fatalf("仓储被调了 %d 次，want 1", len(repo.calls))
	}
	got := repo.calls[0]
	if got.DeviceID != "device-1" || got.RequestID != "req-1" || got.Remark != "取货码 30 秒" {
		t.Fatalf("仓储收到 %+v，want 三个字段都 trim 过", got)
	}
	// 验证码是**唯一一个不 trim 的**：它是顾客敲进去的一串码，前后有没有空格只有比对
	// 那一侧知道。服务层在这里自作主张地 trim，等于把一个「码里带空格」的配置悄悄改写成
	// 另一个意思——而那种改写的表现是「后台看着配了，现场就是打不开」。
	if got.PickupPassword != " 1357 " {
		t.Fatalf("pickup_password = %q, want it passed through untrimmed", got.PickupPassword)
	}
	// 金额原样传正数：符号是仓储补的，服务层不改它（改了会让「-1500」在两层之间
	// 各变一次，最后变成加钱）。
	if got.Amount != 1500 {
		t.Fatalf("amount = %d, want the positive amount passed through", got.Amount)
	}
	if !result.Applied || result.BalanceAfter != 100 {
		t.Fatalf("result = %+v, want the repository's answer carried back", result)
	}
}

// TestDeductDeviceBalanceCarriesAppliedFalseThrough 盯的是重投那一条：仓储认出「这个
// request_id 扣过了」时回 applied=false，服务层必须原样传出去。
//
// 服务层若在这里把它当成失败翻掉，调用方就再也分不清「上次已经成功了」和「这次没扣成」
// ——而这两件事在取货码那条路上一个是继续建单、一个是这一单做不成。
func TestDeductDeviceBalanceCarriesAppliedFalseThrough(t *testing.T) {
	repo := &alreadyAppliedBalance{BalanceAfter: 700}
	svc := NewDeviceBalanceService(repo)

	result, err := svc.DeductDeviceBalance(context.Background(), "device-1",
		DeductDeviceBalanceInput{Amount: 1500, RequestID: "req-1"})
	if err != nil {
		t.Fatalf("deduct device balance: %v", err)
	}
	if result.Applied {
		t.Fatal("applied = true, want false for a replayed request")
	}
	if result.BalanceAfter != 700 {
		t.Fatalf("balance_after = %d, want the balance recorded by the first call", result.BalanceAfter)
	}
}

type alreadyAppliedBalance struct {
	repository.DeviceBalanceRepository
	BalanceAfter int64
}

func (r *alreadyAppliedBalance) DeductDeviceBalance(context.Context, repository.DeductDeviceBalanceParams) (*repository.DeductDeviceBalanceResult, error) {
	return &repository.DeductDeviceBalanceResult{BalanceAfter: r.BalanceAfter, Applied: false}, nil
}
