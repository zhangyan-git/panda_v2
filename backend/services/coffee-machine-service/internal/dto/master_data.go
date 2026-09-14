// Package dto 是 coffee-machine-service 的接口出入参形状。
package dto

import "time"

// DeviceQuery 是设备列表的查询条件。空串表示不过滤。
type DeviceQuery struct {
	Page           int
	PageSize       int
	ManufacturerID string
	StoreID        string
	Status         string
}

// DrinkQuery 是饮品列表的查询条件。空串表示不过滤。
type DrinkQuery struct {
	Page           int
	PageSize       int
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
type DrinkSummary struct {
	ID              string  `json:"id"`
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

// DeviceDrinkSummary 是设备与饮品的供应关系。三个价格为 null 表示沿用饮品目录价，
// 与价格为 0 不同：0 是「这台机器上按 0 元卖」。
type DeviceDrinkSummary struct {
	DeviceID        string `json:"deviceId"`
	DrinkID         string `json:"drinkId"`
	Enabled         bool   `json:"enabled"`
	SortOrder       int    `json:"sortOrder"`
	Price           *int64 `json:"price"`
	VipPrice        *int64 `json:"vipPrice"`
	PickupCodePrice *int64 `json:"pickupCodePrice"`
}
