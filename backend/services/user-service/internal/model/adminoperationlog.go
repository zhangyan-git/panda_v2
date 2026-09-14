package model

import (
	"encoding/json"
	"time"
)

// 操作结果的取值，与 admin_operation_logs.result 的注释一致。
// 列上没有 CHECK 约束（事件是别的服务发过来的，加约束等于让一条脏事件卡住整个
// 消费队列），所以取值只在这里和查询筛选处把关。
const (
	OperationResultSuccess = "success"
	OperationResultFailure = "failure"
)

// AdminOperationLog 对应 admin_operation_logs 表，平台后台的业务操作日志。
//
// 这张表由 user-service 的审计消费者写入（见 service.OperationLogService），
// 后台页面只读它。
//
// 可空列（admin_user_id / target_id / merchant_id）在这里归一化成空串：它们都可
// 为空，理由各不相同但都是「记录必须活下来」——管理员被删了、目标被删了、操作
// 不针对具体商户。查的时候一并 COALESCE 成空串，前端只需判空。
type AdminOperationLog struct {
	ID string `db:"id"`
	// AdminUserID 操作人；管理员账号被删除后置空，日志本身保留。
	AdminUserID string `db:"admin_user_id"`
	// AdminUsername / AdminName 是操作发生时的快照，不随改名变化。
	AdminUsername string `db:"admin_username"`
	AdminName     string `db:"admin_name"`
	// Module 业务模块，如 miniapp_users、merchants、roles。
	Module string `db:"module"`
	// Action 标准动作，如 create、update_status、delete。
	Action string `db:"action"`
	// Operation 面向后台展示的操作描述，如「修改小程序用户状态」。
	Operation  string `db:"operation"`
	TargetType string `db:"target_type"`
	TargetID   string `db:"target_id"`
	TargetName string `db:"target_name"`
	// MerchantID 关联商户，跨库软指针，用于按商户筛历史日志。
	MerchantID string `db:"merchant_id"`
	// Result 取值见 OperationResult*。
	Result       string `db:"result"`
	ErrorCode    string `db:"error_code"`
	ErrorMessage string `db:"error_message"`
	// BeforeData / AfterData 经过脱敏的业务快照，为空时是 nil。
	BeforeData json.RawMessage `db:"before_data"`
	AfterData  json.RawMessage `db:"after_data"`
	// OccurredAt 落库时间。注意它是消费者写到这张表的时间，不是操作发生的时间：
	// 事件里不带时间戳，relay 积压后补投时这一列会整体晚于实际操作时刻。
	OccurredAt time.Time `db:"occurred_at"`
}
