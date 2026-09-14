package model

import "time"

// Drink 是饮品目录的一行。饮品按厂商维护，price / vip_price / pickup_code_price
// 是三级目录价，单位都是分。
//
// OriginID 是厂商侧 ID，空串表示这行是后台手工新建的，不参与厂商同步判重
// （唯一性用 drinks_manufacturer_origin_unique 这个部分索引，只对非空生效）。
type Drink struct {
	ID              string    `db:"id"`
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

// DeviceDrink 是设备与饮品的供应关系，并可覆盖该设备上的售价。
//
// 三个价格指针为 nil 表示沿用 Drink 的目录价，与「覆盖成 0」是两回事——0 是
// 「这台机器上按 0 元卖」，数据库那一列也没 NOT NULL，就是给这个区别留的。
type DeviceDrink struct {
	ID              string    `db:"id"`
	DeviceID        string    `db:"device_id"`
	DrinkID         string    `db:"drink_id"`
	Enabled         bool      `db:"enabled"`
	SortOrder       int       `db:"sort_order"`
	Price           *int64    `db:"price"`
	VipPrice        *int64    `db:"vip_price"`
	PickupCodePrice *int64    `db:"pickup_code_price"`
	CreatedAt       time.Time `db:"created_at"`
	UpdatedAt       time.Time `db:"updated_at"`
}
