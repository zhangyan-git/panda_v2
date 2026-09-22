package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	khttp "github.com/go-kratos/kratos/v2/transport/http"
	"github.com/jackc/pgx/v5"
	"github.com/panda-dev/panda-v2/backend/platform/auth"
	runtime "github.com/panda-dev/panda-v2/backend/platform/server/runtime"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/model"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/service"
)

type deletionBrandRepo struct {
	repository.BrandRepository
	events                     *[]string
	findErr, hasErr, deleteErr error
	hasStores                  bool
}

func (f *deletionBrandRepo) FindByID(context.Context, string) (*model.Brand, error) {
	*f.events = append(*f.events, "find")
	return &model.Brand{ID: "target"}, f.findErr
}
func (f *deletionBrandRepo) HasStores(context.Context, string) (bool, error) {
	*f.events = append(*f.events, "hasStores")
	return f.hasStores, f.hasErr
}
func (f *deletionBrandRepo) Delete(context.Context, string) error {
	*f.events = append(*f.events, "delete")
	return f.deleteErr
}

type deletionStoreRepo struct {
	repository.StoreRepository
	events             *[]string
	findErr, deleteErr error
}

func (f *deletionStoreRepo) FindByID(context.Context, string) (*model.Store, error) {
	*f.events = append(*f.events, "find")
	return &model.Store{ID: "target"}, f.findErr
}
func (f *deletionStoreRepo) Delete(context.Context, string) error {
	*f.events = append(*f.events, "delete")
	return f.deleteErr
}

type deletionScopeRepo struct {
	repository.MerchantUserRepository
	events *[]string
	err    error
}

func (f *deletionScopeRepo) ResetScopeByTarget(_ context.Context, kind, id string) error {
	*f.events = append(*f.events, "reset:"+kind+":"+id)
	return f.err
}

// TestDeleteFailsClosedOnScopeReset drives the real HTTP delete path. The scope
// reset is a fake here; the gRPC transport failure that produces
// ErrUnavailable is covered where it lives, in internal/client.
func TestDeleteFailsClosedOnScopeReset(t *testing.T) {
	failure := errors.New("scope reset failed")
	for _, kind := range []string{"brand", "store"} {
		for _, tt := range []struct {
			name     string
			resetErr error
			status   int
		}{
			{"reset success", nil, 200},
			{"reset failure", failure, 500},
			{"reset unavailable", repository.ErrUnavailable, 503},
			{"wrapped unavailable", fmt.Errorf("reset: %w", repository.ErrUnavailable), 503},
		} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				var events []string
				var scope repository.MerchantUserRepository = &deletionScopeRepo{events: &events, err: tt.resetErr}
				b := NewAdminBrandHandler(service.NewAdminBrandService(&deletionBrandRepo{events: &events}, nil, nil, scope, nil))
				st := NewAdminStoreHandler(service.NewAdminStoreService(&deletionStoreRepo{events: &events}, nil, nil, nil, scope, nil))
				svc := testJWT(t)
				a, _ := liveAccess(t, []string{}, []string{"admin:" + kind + "s:delete"})
				s := runtime.NewHTTPRouter(khttp.NewServer())
				Register(s, &AdminMerchantHandler{}, b, st, NewAdminUploadHandler(nil, nil), svc, a)
				r := httptest.NewRequest(http.MethodDelete, "/v1/admin/"+kind+"s/target", nil)
				r.Header.Set("Authorization", testToken(t, svc, auth.Grant{Subject: "admin", UserID: "admin"}))
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				if w.Code != tt.status {
					t.Fatalf("status=%d want %d body=%s", w.Code, tt.status, w.Body)
				}
				want := []string{"find"}
				if kind == "brand" {
					want = append(want, "hasStores")
				}
				want = append(want, "reset:"+kind+":target")
				if tt.status == 200 {
					want = append(want, "delete")
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("events=%v want %v", events, want)
				}
			})
		}
	}
}

func TestDeletePrerequisitesAndDeleteError(t *testing.T) {
	failure := errors.New("repository failure")
	for _, tt := range []struct {
		name                       string
		findErr, hasErr, deleteErr error
		hasStores                  bool
		want                       error
		events                     []string
	}{
		{"not found", pgx.ErrNoRows, nil, nil, false, pgx.ErrNoRows, []string{"find"}},
		{"store query failure", nil, failure, nil, false, failure, []string{"find", "hasStores"}},
		{"brand has stores", nil, nil, nil, true, service.ErrBrandHasStores, []string{"find", "hasStores"}},
		{"delete failure", nil, nil, failure, false, failure, []string{"find", "hasStores", "reset:brand:target", "delete"}},
	} {
		t.Run("brand/"+tt.name, func(t *testing.T) {
			var events []string
			svc := service.NewAdminBrandService(&deletionBrandRepo{events: &events, findErr: tt.findErr, hasErr: tt.hasErr, deleteErr: tt.deleteErr, hasStores: tt.hasStores}, nil, nil, &deletionScopeRepo{events: &events}, nil)
			if err := svc.Delete(context.Background(), "target"); !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want %v", err, tt.want)
			}
			if !reflect.DeepEqual(events, tt.events) {
				t.Fatalf("events=%v want %v", events, tt.events)
			}
		})
	}
	for _, missing := range []bool{true, false} {
		var events []string
		repo := &deletionStoreRepo{events: &events}
		want := []string{"find", "reset:store:target", "delete"}
		if missing {
			repo.findErr = failure
			want = []string{"find"}
		} else {
			repo.deleteErr = failure
		}
		svc := service.NewAdminStoreService(repo, nil, nil, nil, &deletionScopeRepo{events: &events}, nil)
		if err := svc.Delete(context.Background(), "target"); !errors.Is(err, failure) {
			t.Fatalf("err=%v", err)
		}
		if !reflect.DeepEqual(events, want) {
			t.Fatalf("events=%v want %v", events, want)
		}
	}
}
