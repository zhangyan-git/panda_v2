package repository

import (
	"context"
	"errors"
)

// ErrUnavailable reports that the remote ownership/identity dependency could not
// be reached. Handlers map it to 503 so the caller retries instead of assuming
// the operation succeeded.
var ErrUnavailable = errors.New("merchant repository is unavailable")

// 客户编码 / DMS 编码撞车。两个编码在 stores 上各有一条「空串不参与」的部分唯一索引
// （merchant/004），所以撞车是一次**正常的用户错误**——别的门店已经用了这个编码——
// 而不是内部故障。不翻成人话的话它会以 23505 一路冒到 handler，用户看到的是
// 「更新失败」500，而他做的事（填了一个别人用过的编码）本来是能说清楚的。
//
// 两个编码各是一个 sentinel 而不是合成一个：填错哪一个，用户要改的是哪一个输入框，
// 合成一句话会把这件事抹掉。
var (
	ErrCustomerCodeTaken = errors.New("这个客户编码已经给别的门店用了")
	ErrDMSCodeTaken      = errors.New("这个 DMS 编码已经给别的门店用了")
)

// 品牌重名。brands 上有 UNIQUE (merchant_id, name)（merchant/001），同一商户下再建一个
// 同名品牌同样是**用户自己就能改好**的事——换个名字就过了——不是内部故障。不翻成人话的话
// 它会以 23505 一路冒到 handler，用户看到的是「创建失败」500。
var ErrBrandNameTaken = errors.New("这个品牌名在该商户下已经存在")

// MerchantUserRepository reclaims account scope on a deleted target. It is
// implemented by the user-service gRPC client, because merchant_users and the
// accounts it points at belong to the identity database.
type MerchantUserRepository interface {
	ResetScopeByTarget(context.Context, string, string) error
}
