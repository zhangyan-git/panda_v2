package model

import "time"

// Device 是 devices 表的一行。
//
// 列分成三类，写路径碰哪一类是设计的一部分：
//
//  1. 后台可编辑：SerialUnique、DeviceName、ManufacturerID、StoreID、QrcodeType、
//     RegularQrcodePaymentMethod、PickupPassword、ShowVip、EnableCouponVerification、
//     WarrantyEndAt。
//  2. 厂商同步写入：VendorOnline、LastSyncedAt、VersionNumber、AndroidVersion、
//     MainBoardVersion、LastFault*、LastActiveAt。后台的编辑接口一律不碰它们——
//     管理员手改一个版本号，下一轮同步就覆盖掉，只会让人以为改动生效了。
//  3. 资金：CoffeeBalance。只由余额调整接口经流水改动，**不在设备编辑的字段里**。
//     把余额混进通用更新，等于给每个能编辑设备的人都开了改钱的口子，而且不留流水。
//
// Status 与 VendorOnline 是两件事，不能压成一个字段：Status 是本系统让不让这台
// 设备接单，VendorOnline 是厂商说它此刻通不通。调用方（order-service 下单校验，
// 方案 5.8）两条都要看，所以两个都得出。
type Device struct {
	ID             string `db:"id"`
	SerialUnique   string `db:"serial_unique"`
	DeviceName     string `db:"device_name"`
	ManufacturerID string `db:"manufacturer_id"`
	// StoreID 为 nil 表示这台设备还没挂到任何点位上。它是跨库值引用，本服务不校验
	// 这个点位是否存在——那是 merchant-service 的事。
	StoreID *string `db:"store_id"`
	// Status 只有 active / disabled 两个取值，由数据库 CHECK 约束保证。
	Status string `db:"status"`
	// VendorOnline 为 nil 表示从未同步过厂商状态。不要把它当成 false：一台还没同步
	// 过的设备是「不知道」，不是「离线」，压成 bool 之后调用方再也分不出来。
	VendorOnline  *bool      `db:"vendor_online"`
	LastSyncedAt  *time.Time `db:"last_synced_at"`
	LastFaultCode string     `db:"last_fault_code"`
	// LastFaultMessage 是给人看的描述，LastFaultCode 才是机器判定的依据。
	LastFaultMessage string     `db:"last_fault_message"`
	LastFaultAt      *time.Time `db:"last_fault_at"`
	LastActiveAt     *time.Time `db:"last_active_at"`

	VersionNumber    string `db:"version_number"`
	AndroidVersion   string `db:"android_version"`
	MainBoardVersion string `db:"main_board_version"`

	// PickupPassword 是店员打咖啡用的静态验证码，只在设备详情里给后台看。
	PickupPassword string `db:"pickup_password"`
	// CoffeeBalance 是咖啡余额（分）。改动只能经 device_balance_ledger 流水。
	CoffeeBalance            int64      `db:"coffee_balance"`
	ShowVip                  bool       `db:"show_vip"`
	EnableCouponVerification bool       `db:"enable_coupon_verification"`
	WarrantyEndAt            *time.Time `db:"warranty_end_at"`
	QrcodeType               string     `db:"qrcode_type"`
	// RegularQrcodePaymentMethod 只在 QrcodeType = 'regular' 时有值，两个取值由
	// 数据库 CHECK 约束限定为 fengxuan_wanlian / youlian。
	RegularQrcodePaymentMethod *string `db:"regular_qrcode_payment_method"`

	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}
