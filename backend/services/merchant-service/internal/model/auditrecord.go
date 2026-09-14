package model

import "time"

// AuditRecord 对应 brand_audit_records / store_audit_records，一次提交一条
type AuditRecord struct {
	ID           string     `db:"id"`
	TargetID     string     `db:"brand_id/store_id"`
	Type         string     `db:"type"`     // create / update
	Status       string     `db:"status"`   // pending / approved / rejected
	OldData      string     `db:"old_data"` // JSONB 文本，创建时为空
	NewData      string     `db:"new_data"` // JSONB 文本
	SubmitRemark string     `db:"submit_remark"`
	AuditRemark  string     `db:"audit_remark"`
	AuditBy      string     `db:"audit_by"`
	AuditAt      *time.Time `db:"audit_at"`
	SubmittedBy  string     `db:"submitted_by"`
	CreatedAt    time.Time  `db:"created_at"`
}
