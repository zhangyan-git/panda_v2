package repository

import (
	"errors"
	"testing"
)

// TestNotificationChannelMatches 钉住回调的渠道一致性判据（见 ErrPaymentChannelMismatch）。
//
// 它挡的是签名挡不住的那一件事：签名证明「这条报文是这条渠道签的」，证明不了「这一单是这条
// 渠道的单」。B 渠道的密钥可以签出一条 payment_no 指向 A 渠道某张支付单的报文，金额甚至可能
// 正好对得上（同一家门店、同一价位）。判据在支付单那一侧（payments.provider），报文碰不到它。
//
// 比的是**渠道名**（provider，今天只有 `ums` 一个取值），不是渠道行的 uuid——那张表已经没了，
// 名字是唯一还指向渠道的事实，回调 URL 里那一段也是它。
//
// 「支付单没有渠道」那两行是**有意的拒绝**，不是漏判：账户出资（咖啡豆）的支付单 provider 为
// 空，它根本不该收到渠道回调——回调地址是公开的，放过去就等于任何人 POST 一份报文就能把一张
// 用豆付的单推成 succeeded。
func TestNotificationChannelMatches(t *testing.T) {
	const channelA = "ums"
	const channelB = "unionpay_link" // 一个今天不存在的渠道名，用例要的是「另一个名字」
	const noChannel = ""

	cases := []struct {
		name             string
		paymentProvider  string
		notifiedProvider string
		wantError        bool
	}{
		{"同一条渠道放行", channelA, channelA, false},
		{"不是同一条渠道拒绝", channelA, channelB, true},
		{"支付单没有渠道（账户出资）拒绝", noChannel, channelA, true},
		{"通知没带渠道拒绝", channelA, noChannel, true},
		{"两边都没有渠道拒绝", noChannel, noChannel, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := notificationChannelMatches(tc.paymentProvider, tc.notifiedProvider)
			if tc.wantError {
				if !errors.Is(err, ErrPaymentChannelMismatch) {
					t.Fatalf("error = %v, want ErrPaymentChannelMismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}
