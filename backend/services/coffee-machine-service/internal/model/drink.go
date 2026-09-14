package model

import "time"

// Drink 是一行「某台设备上的一杯饮品」，设备直接存在这一行上。
//
// 老系统 drinks 集合就是每台设备一行（后台那张表单里 device_id 与 manufacturer_id
// 并存，不是二选一）。V2 起初拆成「厂商级目录 + 供应关系」两张表，拆完却没有任何入口
// 能把目录行挂到设备上，于是并了回来，见 migrations/coffee_machine/003。
//
// 因此 price / vip_price / pickup_code_price 是**这台设备上**的售价，不再有「每机
// 覆盖价 / 目录价」两套说法；status 与 sort 就是这台设备上的上下架与排序。
//
// DeviceID 可空（迁移是加列，库里可能已经有没挂设备的行）：为空表示这行还没挂到设备上，
// 后台列表里显示成「未分配设备」。挂设备由表单和接口强制。
//
// OriginID 是厂商侧 ID，空串表示这行是后台手工新建的，不参与厂商同步判重。判重键是
// drinks_device_origin_unique 这个部分索引，形如 (device_id, manufacturer_id, origin_id)：
// 同一款饮品在 N 台设备上就是 N 行，正是这个模型的应有之义。
type Drink struct {
	ID              string    `db:"id"`
	DeviceID        *string   `db:"device_id"`
	ManufacturerID  string    `db:"manufacturer_id"`
	OriginID        string    `db:"origin_id"`
	ProductNum      string    `db:"product_num"`
	ProductName     string    `db:"product_name"`
	EnName          string    `db:"en_name"`
	DrinkType       *string   `db:"drink_type"`
	ProductImg      string    `db:"product_img"`
	ProductDesc     string    `db:"product_desc"`
	Price           int64     `db:"price"`
	VipPrice        int64     `db:"vip_price"`
	PickupCodePrice int64     `db:"pickup_code_price"`
	Status          string    `db:"status"`
	Sort            int       `db:"sort"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}
