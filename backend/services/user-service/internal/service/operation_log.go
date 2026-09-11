package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/panda-dev/panda-v2/backend/platform/audit"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/repository"
)

// OperationLogService 消费审计事件并落到 admin_operation_logs。
//
// 事件由各服务在业务事务里写进自己的 outbox，经 relay 发到 RabbitMQ；身份库是
// 这张表的归属方，所以只有它消费。落在服务层而不是消费者适配器里，是为了让
// 「怎么落库」和「怎么收消息」分开——前者可以单独测。
type OperationLogService struct {
	logs repository.AdminOperationLogRepository
}

func NewOperationLogService(logs repository.AdminOperationLogRepository) *OperationLogService {
	return &OperationLogService{logs: logs}
}

// Handle 是 messaging.Handler，处理一条投递。
//
// 事件类型不是审计的，就确认掉并跳过：队列的绑定键将来可能放宽到多种事件，
// 那时这里返回错误只会让一条与己无关的消息无限重投。真正该重投的是解析或落库
// 失败，那些仍然返回错误，交给消费者的重试与死信策略处理。
func (s *OperationLogService) Handle(ctx context.Context, event messaging.Envelope) error {
	if event.EventType != audit.EventType {
		slog.DebugContext(ctx, "skipping non-audit event", "event_type", event.EventType, "event_id", event.EventID)
		return nil
	}
	entry, err := audit.Decode(event)
	if err != nil {
		return err
	}
	if entry.Module == "" || entry.Action == "" {
		// 记录方本不该发出这种事件。当作不可重试的错误处理会拖住整个队列，
		// 所以只报出来：日志表里多一条字段不全的记录，好过一条消息反复投递。
		slog.ErrorContext(ctx, "audit event is missing its module or action", "event_id", event.EventID)
	}
	if err := s.logs.Insert(ctx, event.EventID, entry); err != nil {
		return errors.Join(errors.New("record operation log"), err)
	}
	return nil
}
