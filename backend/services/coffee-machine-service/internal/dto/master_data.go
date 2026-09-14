// Package dto 是 coffee-machine-service 的接口出入参形状。
package dto

import "time"

// DeviceQuery 是设备列表的查询条件。空串/空切片表示不过滤。
//
// StoreIDs 对应筛选栏里可以多选的门店。品牌不在这里：品牌不是本库的列，多选品牌
// 由前端换算成它下面的门店 id 一起并进来（见 admin-web 的设备列表页），本服务不认识
// 品牌，也就不必为了一个筛选项去问商户服务。
type DeviceQuery struct {
	Page           int
	PageSize       int
	ManufacturerID string
	StoreIDs       []string
	Status         string
	// Keyword 按设备序列号模糊匹配，对应筛选栏的「设备标识」。
	Keyword string
}

// DrinkQuery 是饮品列表的查询条件。空串表示不过滤。
//
// DeviceID 是给饮品管理页用的：饮品行挂在设备上，所以那一页最常见的一问是
// 「某台设备上有哪些饮品」。必须是合法 UUID，见 controller 里的校验。
type DrinkQuery struct {
	Page           int
	PageSize       int
	DeviceID       string
	ManufacturerID string
	Status         string
}

// DeviceSummary 是设备在后台列表与详情里返回的形状。
//
// VendorOnline 用指针：null 表示从未同步过厂商状态，与 false（厂商说它离线）
// 是两件事。压成 bool 之后前端只能把没见过同步的设备显示成离线。
type DeviceSummary struct {
	ID               string     `json:"id"`
	SerialUnique     string     `json:"serialUnique"`
	DeviceName       string     `json:"deviceName"`
	ManufacturerID   string     `json:"manufacturerId"`
	StoreID          *string    `json:"storeId"`
	Status           string     `json:"status"`
	VendorOnline     *bool      `json:"vendorOnline"`
	LastSyncedAt     *time.Time `json:"lastSyncedAt"`
	LastFaultCode    string     `json:"lastFaultCode"`
	LastFaultMessage string     `json:"lastFaultMessage"`
	LastActiveAt     *time.Time `json:"lastActiveAt"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
}

// DeviceDetail 是设备详情返回的形状，比列表多出编辑页需要的字段。
//
// 与 DeviceSummary 分开而不是合成一个：列表一次几十行，把版本号、二维码配置、
// 余额、静态验证码全带上既没必要，也让静态验证码扩散到每一次列表请求里。
//
// PickupPassword 在这里给明文，是为了运营能查某一台设备的码。它**不用来回填编辑
// 表单**——编辑时留空即不改（见 dto.DeviceInput）。
type DeviceDetail struct {
	ID                         string     `json:"id"`
	SerialUnique               string     `json:"serialUnique"`
	DeviceName                 string     `json:"deviceName"`
	ManufacturerID             string     `json:"manufacturerId"`
	StoreID                    *string    `json:"storeId"`
	Status                     string     `json:"status"`
	VendorOnline               *bool      `json:"vendorOnline"`
	LastSyncedAt               *time.Time `json:"lastSyncedAt"`
	VersionNumber              string     `json:"versionNumber"`
	AndroidVersion             string     `json:"androidVersion"`
	MainBoardVersion           string     `json:"mainBoardVersion"`
	LastFaultCode              string     `json:"lastFaultCode"`
	LastFaultMessage           string     `json:"lastFaultMessage"`
	LastFaultAt                *time.Time `json:"lastFaultAt"`
	LastActiveAt               *time.Time `json:"lastActiveAt"`
	PickupPassword             string     `json:"pickupPassword"`
	CoffeeBalance              int64      `json:"coffeeBalance"`
	ShowVip                    bool       `json:"showVip"`
	EnableCouponVerification   bool       `json:"enableCouponVerification"`
	WarrantyEndAt              *time.Time `json:"warrantyEndAt"`
	QrcodeType                 string     `json:"qrcodeType"`
	RegularQrcodePaymentMethod *string    `json:"regularQrcodePaymentMethod"`
	CreatedAt                  time.Time  `json:"createdAt"`
	UpdatedAt                  time.Time  `json:"updatedAt"`
}

// DeviceBalanceEntrySummary 是一条设备余额流水。金额单位是分。
//
// BalanceBefore 不在表里，是 balance_after - amount 推出来的：这张表只记变动**之后**
// 的余额，而一屏流水要回答的正是「这笔之前是多少」。推它而不是存它，是因为库里的
// balance_after 本身就是权威值——多存一列余额快照就多一个可能与它不一致的地方。
type DeviceBalanceEntrySummary struct {
	ID       string `json:"id"`
	DeviceID string `json:"deviceId"`
	// Type 取值 recharge / deduct / adjust / reverse。
	Type          string `json:"type"`
	Amount        int64  `json:"amount"`
	BalanceBefore int64  `json:"balanceBefore"`
	BalanceAfter  int64  `json:"balanceAfter"`
	// ReversesEntryID 只在 Type = reverse 时有值。
	ReversesEntryID *string `json:"reversesEntryId"`
	// ReferenceType / ReferenceID 是来源单据，后台调整两者都是空。
	ReferenceType string  `json:"referenceType"`
	ReferenceID   *string `json:"referenceId"`
	// RequestID 是后台调整的幂等键，非后台写入的是空串。
	RequestID string `json:"requestId"`
	Remark    string `json:"remark"`
	// OperatorID 是操作人的管理员用户 ID，展示名由前端按 id 解析；OperatorName 是
	// 写入方自己记的名字，后台调整写的是空串。
	OperatorID   *string   `json:"operatorId"`
	OperatorName string    `json:"operatorName"`
	CreatedAt    time.Time `json:"createdAt"`
}

// ManufacturerSummary 是厂商在后台返回的形状，不含接入凭据与令牌。
type ManufacturerSummary struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	Name         string `json:"name"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
	Status       string `json:"status"`
}

// DrinkSummary 是饮品在后台返回的形状。价格单位统一是分。
//
// 这一行是「某台设备上的一杯饮品」，所以三个价格就是这台设备上的售价，没有
// 「覆盖价 / 目录价」的区分。DeviceID 为 null 表示这行还没挂到设备上。
type DrinkSummary struct {
	ID              string  `json:"id"`
	DeviceID        *string `json:"deviceId"`
	ManufacturerID  string  `json:"manufacturerId"`
	OriginID        string  `json:"originId"`
	ProductNum      string  `json:"productNum"`
	ProductName     string  `json:"productName"`
	EnName          string  `json:"enName"`
	DrinkType       *string `json:"drinkType"`
	ProductImg      string  `json:"productImg"`
	ProductDesc     string  `json:"productDesc"`
	Price           int64   `json:"price"`
	VipPrice        int64   `json:"vipPrice"`
	PickupCodePrice int64   `json:"pickupCodePrice"`
	Status          string  `json:"status"`
	Sort            int     `json:"sort"`
}
