package dto

// MaxPageSize 是优惠券各列表接口 pageSize 的上限，取值与 platform/api.MaxPageSize 一致。
//
// 这里曾经是 100（controller 里一个 maxCouponPageSize，服务层与仓储层各有一道
// 「超过就退回 20」的兜底），当时的理由是「优惠券模块没有要全集的下拉」。这个前提
// 后来不成立了：用户券列表要把 batch_id / template_id 翻成批次号与模板名，得先拉
// 一份全集，而前端「要全集」的统一参数（admin-web/src/services/pagination.ts 的
// FULL_PAGE_PARAMS，pageSize=200，注释里写明对齐 api.MaxPageSize）在 100 的接口上
// 直接 400 —— 翻译全部落空，页面上还是一列 uuid。
//
// 三处上限现在共用这个常量：HTTP 层交给 api.ParsePage 校验，服务层与仓储层的兜底
// 也用它，避免再漂移成三个数字。
const MaxPageSize = 200
