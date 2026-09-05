package app

import (
	"testing"

	"github.com/panda-dev/panda-v2/backend/platform/config"
	"github.com/panda-dev/panda-v2/backend/platform/database"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
)

func TestRepositoryForUsesMemoryOnlyInDevAndTest(t *testing.T) {
	for _, environment := range []string{"dev", "test", "DEV", "Test"} {
		t.Run(environment, func(t *testing.T) {
			repo, err := repositoryFor(config.Config{Environment: environment}, database.Noop{})
			if err != nil {
				t.Fatalf("repositoryFor() error = %v", err)
			}
			if _, ok := repo.(*repository.Memory); !ok {
				t.Fatalf("repositoryFor() type = %T, want *repository.Memory", repo)
			}
		})
	}
}

func TestRepositoryForRejectsNoopOutsideDevAndTest(t *testing.T) {
	for _, environment := range []string{"", "staging", "production", "prod"} {
		t.Run(environment, func(t *testing.T) {
			repo, err := repositoryFor(config.Config{Environment: environment}, database.Noop{})
			if err == nil {
				t.Fatalf("repositoryFor() repo = %T, want error", repo)
			}
		})
	}
}
