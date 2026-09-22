package service

import (
	"context"

	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/payment-service/internal/repository"
)

// 本文件是 fakeRepository 上协议那三个方法的实现（字段在 create_test.go 的 struct 里，
// 与别的假实现一样按路分组）。用例见 agreement_create_test.go / agreement_query_test.go。

func (f *fakeRepository) CreateAgreement(_ context.Context, p repository.CreateAgreementParams) (*model.PaymentAgreement, []byte, bool, error) {
	f.createAgreementCalls = append(f.createAgreementCalls, p)
	if f.createAgreementErr != nil {
		return nil, nil, false, f.createAgreementErr
	}
	if f.createAgreementHit {
		return nil, f.createAgreementSnap, true, nil
	}
	agreement := f.agreement
	if agreement == nil {
		agreement = &model.PaymentAgreement{
			ID: "agreement-1", AgreementNo: p.AgreementNo, UserID: p.UserID,
			Provider: p.Provider, PaymentMethod: p.Method, Subject: p.Subject,
			PlanCode: p.PlanCode, MaxChargeAmount: p.MaxChargeAmount,
			Status: model.AgreementStatusPending,
		}
	}
	f.agreement = agreement
	return agreement, nil, false, nil
}

func (f *fakeRepository) FindAgreementByNo(_ context.Context, agreementNo string) (*model.PaymentAgreement, error) {
	if f.agreementByNoErr != nil {
		return nil, f.agreementByNoErr
	}
	if f.agreement == nil || f.agreement.AgreementNo != agreementNo {
		return nil, repository.ErrAgreementNotFound
	}
	return f.agreement, nil
}

// SettleAgreement 就地改「这一份协议」的状态，与真仓储那条锁内判定的**可观察结果**一致
// （改了哪些列、changed 是真是假）。判定本身不在这里重写一遍：那是仓储的单测该管的，
// 这一层要验的是「service 把渠道的答复翻成了哪一个目标状态」。
func (f *fakeRepository) SettleAgreement(_ context.Context, p repository.SettleAgreementParams) (*model.PaymentAgreement, bool, error) {
	f.agreementSettles = append(f.agreementSettles, p)
	if f.agreementSettleErr != nil {
		return nil, false, f.agreementSettleErr
	}
	if f.agreement == nil {
		return nil, false, repository.ErrAgreementNotFound
	}
	if !f.agreementSettleChanged {
		return f.agreement, false, nil
	}
	f.agreement.Status = p.Target
	if p.ContractNo != "" {
		f.agreement.ContractNo = p.ContractNo
	}
	return f.agreement, true, nil
}

// SettleAgreementNotification 与上一条同一个形状：记下实参（service 把报文里的
// change_type 翻成了哪一个目标状态，只有在这里看得见），按开关回答「改了没有」。
//
// 它**不重写**真仓储那条按 contract_no 找协议的判据：那是仓储单测该管的，这一层要验的是
// 「service 把 contract_code 与 contract_id 原样送下去了没有」——送错了那个，真仓储就会
// 去找一份不存在的协议。
func (f *fakeRepository) SettleAgreementNotification(_ context.Context, p repository.AgreementNotificationParams) (*repository.AgreementSettlement, error) {
	f.agreementNotificationSettles = append(f.agreementNotificationSettles, p)
	if f.agreementNotificationErr != nil {
		return nil, f.agreementNotificationErr
	}
	if f.agreement == nil {
		return nil, repository.ErrAgreementNotFound
	}
	if !f.agreementNotificationChanged {
		return &repository.AgreementSettlement{Agreement: f.agreement}, nil
	}
	f.agreement.Status = p.Target
	if p.ContractNo != "" {
		f.agreement.ContractNo = p.ContractNo
	}
	return &repository.AgreementSettlement{Agreement: f.agreement, Changed: true}, nil
}
