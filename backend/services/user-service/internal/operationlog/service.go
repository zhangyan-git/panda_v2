package operationlog

import "context"

type Service struct{ repo Repository }

func NewService(repo Repository) *Service { return &Service{repo: repo} }
func (s *Service) Record(ctx context.Context, record *Record) error {
	return s.repo.Create(ctx, record)
}
func (s *Service) List(ctx context.Context, query Query) ([]*Record, int, error) {
	return s.repo.FindAll(ctx, query)
}
