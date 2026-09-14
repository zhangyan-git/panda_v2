package dto

import "time"

// 写入路径的入参。这些结构同时被控制器解码和服务层消费，所以 json tag 只在
// 这一处定义——多一层「请求结构再转一遍入参」的搬运，两份字段清单就会各自漂移。

// ManufacturerInput 是厂商的新增/编辑入参。
//
// 没有 code 以外的标识字段、也没有 status：编码在创建时定下之后不可修改（数据库
// 有触发器拦，服务层也不给出改的入口），启用/停用走专门的状态接口。
type ManufacturerInput struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
}

// DeviceInput 是设备的新增/编辑入参。
//
// 刻意不含三类字段：status（上下架走专门接口）、coffeeBalance（只能走余额调整，
// 那条路才有流水）、以及 vendorOnline / versionNumber / lastFault* 这些厂商同步
// 写入的列。后台改一个版本号，下一轮同步就覆盖掉，留着入口只会让人以为改动生效了。
type DeviceInput struct {
	SerialUnique   string `json:"serialUnique"`
	DeviceName     string `json:"deviceName"`
	ManufacturerID string `json:"manufacturerId"`
	// StoreID 为 null 或空串都表示不挂点位。
	StoreID    *string `json:"storeId"`
	QrcodeType string  `json:"qrcodeType"`
	// RegularQrcodePaymentMethod 只在 QrcodeType = regular 时必填。
	RegularQrcodePaymentMethod *string `json:"regularQrcodePaymentMethod"`
	// PickupPassword 是店员在设备上敲的静态验证码。**编辑时留空表示不改**，不是
	// 清空：这一栏不回填（静态验证码不进列表响应），按空串直接落库的话，改个设备名
	// 就会把店员正在用的码抹掉。创建时留空就是没有码（列是 NOT NULL DEFAULT ''）。
	// 想换码就传新值——「清空」这条路故意没有。
	PickupPassword           *string    `json:"pickupPassword"`
	ShowVip                  bool       `json:"showVip"`
	EnableCouponVerification bool       `json:"enableCouponVerification"`
	WarrantyEndAt            *time.Time `json:"warrantyEndAt"`
}

// DrinkInput 是饮品的新增入参。
//
// ManufacturerID 与 OriginID 是厂商同步的自然键，只在创建时定下：编辑入参是另一个
// 结构（DrinkUpdateInput），它没有这两个字段，从类型上就改不了。
type DrinkInput struct {
	ManufacturerID string `json:"manufacturerId"`
	OriginID       string `json:"originId"`
	ProductNum     string `json:"productNum"`
	ProductName    string `json:"productName"`
	EnName         string `json:"enName"`
	// DrinkType 为 null 或空串都表示不限。
	DrinkType       *string `json:"drinkType"`
	ProductImg      string  `json:"productImg"`
	ProductDesc     string  `json:"productDesc"`
	Price           int64   `json:"price"`
	VipPrice        int64   `json:"vipPrice"`
	PickupCodePrice int64   `json:"pickupCodePrice"`
	Sort            int     `json:"sort"`
}

// DrinkUpdateInput 是饮品的编辑入参，比创建少掉自然键那两个。
type DrinkUpdateInput struct {
	ProductNum      string  `json:"productNum"`
	ProductName     string  `json:"productName"`
	EnName          string  `json:"enName"`
	DrinkType       *string `json:"drinkType"`
	ProductImg      string  `json:"productImg"`
	ProductDesc     string  `json:"productDesc"`
	Price           int64   `json:"price"`
	VipPrice        int64   `json:"vipPrice"`
	PickupCodePrice int64   `json:"pickupCodePrice"`
	Sort            int     `json:"sort"`
}

// BalanceAdjustInput 是一次余额调整。
//
// Amount 有符号，正数增加、负数减少，0 不接受。RequestID 必填：它是幂等键，管理员
// 那边一次点击超时重发时，靠它在流水上分辨出这是同一次调整。
type BalanceAdjustInput struct {
	Amount    int64  `json:"amount"`
	RequestID string `json:"requestId"`
	Remark    string `json:"remark"`
}

// StatusInput 是状态变更入参（厂商启用停用 / 设备上下架 / 饮品上下架共用）。
type StatusInput struct {
	Status string `json:"status"`
}

// BalanceResult 是余额调整的结果，回的是调整之后的余额。
type BalanceResult struct {
	CoffeeBalance int64 `json:"coffeeBalance"`
}
