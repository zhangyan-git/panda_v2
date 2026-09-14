package dto

// MaxPageSize 是订单各列表接口 pageSize 的上限，取值与 platform/api.MaxPageSize 一致。
//
// 与 coupon-service 的 dto.MaxPageSize 是同一个理由下的同一个数字：前端「要全集」的
// 统一参数（admin-web/src/services/pagination.ts 的 FULL_PAGE_PARAMS，pageSize=200）
// 在更小的上限上直接 400，下拉就静默变成一列 uuid。三处上限（HTTP 校验、service 兜底、
// repository 兜底）共用这一个常量，避免漂移成三个数字。
const MaxPageSize = 200
