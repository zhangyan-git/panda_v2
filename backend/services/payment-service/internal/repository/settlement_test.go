package repository

import (
	"errors"
	"testing"
)

// 这一组测的是金额恒等式的**比较**那一步（checkSettlementIdentity），不碰数据库。
//
// 判据与取数分开正是因为这条恒等式曾经写成一个永远为真的同义反复：平台金额是
// base − Σreceivers 倒挤出来的，拿算计划时手上那几个变量再算一遍，两边是逐字相同的表达式。
// 现在比较的是三个**读回来的**数（settlementBalance），于是它有了一个能被构造出来的失败态，
// 也就是下面第一条用例。
func TestCheckSettlementIdentity(t *testing.T) {
	cases := []struct {
		name      string
		balance   settlementBalance
		wantError bool
	}{
		{
			// 库里的三行事实对不上：任务说基数 1980、平台自留 200，而接收方只写进去 1700。
			// 少的那 80 分没有任何一处会再发现它，直到对账。
			name:      "接收方之和加平台自留少于基数",
			balance:   settlementBalance{TaskNo: "SET1", Base: 1980, Platform: 200, Receivers: 1700, ReceiverRows: 2},
			wantError: true,
		},
		{
			name:      "接收方之和加平台自留多于基数",
			balance:   settlementBalance{TaskNo: "SET1", Base: 1980, Platform: 300, Receivers: 1700, ReceiverRows: 2},
			wantError: true,
		},
		{
			// 整单归平台：一条接收方都没有（计划里 Receivers 为空），平台自留等于基数。
			name:    "整单归平台",
			balance: settlementBalance{TaskNo: "SET1", Base: 1980, Platform: 1980},
		},
		{
			name:    "全部拆出去",
			balance: settlementBalance{TaskNo: "SET1", Base: 1980, Platform: 0, Receivers: 1980, ReceiverRows: 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSettlementIdentity(tc.balance)
			if tc.wantError {
				if !errors.Is(err, ErrSettlementAmountsDoNotBalance) {
					t.Fatalf("error = %v, want ErrSettlementAmountsDoNotBalance", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}
