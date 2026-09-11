package client

import (
	"context"
	"errors"
	"testing"

	"github.com/panda-dev/panda-v2/backend/services/merchant-service/internal/repository"
	userv1 "github.com/panda-dev/panda-v2/contracts/proto/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testServiceToken = "0123456789abcdef0123456789abcdef"

// fakeUserService records the credential and payload of the last call. The
// embedded interface supplies the RPCs this test does not exercise.
type fakeUserService struct {
	userv1.UserServiceClient

	scopeErr     error
	hasUsers     bool
	hasUsersErr  error
	serviceToken string
	scopeRequest *userv1.ResetAccountScopeRequest
	hasUsersID   string
}

func (f *fakeUserService) ResetAccountScope(ctx context.Context, in *userv1.ResetAccountScopeRequest, _ ...grpc.CallOption) (*userv1.ResetAccountScopeResponse, error) {
	f.serviceToken = outgoingServiceToken(ctx)
	f.scopeRequest = in
	return &userv1.ResetAccountScopeResponse{}, f.scopeErr
}

func (f *fakeUserService) HasUsers(ctx context.Context, in *userv1.HasUsersRequest, _ ...grpc.CallOption) (*userv1.HasUsersResponse, error) {
	f.serviceToken = outgoingServiceToken(ctx)
	f.hasUsersID = in.GetMerchantId()
	return &userv1.HasUsersResponse{HasUsers: f.hasUsers}, f.hasUsersErr
}

func outgoingServiceToken(ctx context.Context) string {
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return ""
	}
	values := md.Get("x-service-token")
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func TestResetScopeByTargetUsesServiceToken(t *testing.T) {
	users := &fakeUserService{}
	client := &UserServiceClient{users: users, serviceToken: testServiceToken}
	if err := client.ResetScopeByTarget(context.Background(), "brand", "b1"); err != nil {
		t.Fatal(err)
	}
	if users.serviceToken != testServiceToken {
		t.Fatalf("service token=%q", users.serviceToken)
	}
	if got := users.scopeRequest; got.GetScopeType() != "brand" || got.GetScopeId() != "b1" {
		t.Fatalf("request=%v", got)
	}
}

// TestResetScopeByTargetFailsClosed covers both halves of the contract: no RPC
// error is swallowed, and an unreachable user-service keeps the historical
// ErrUnavailable signal the HTTP layer turns into 503.
func TestResetScopeByTargetFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name            string
		err             error
		wantUnavailable bool
	}{
		{"transport unavailable", status.Error(codes.Unavailable, "dial tcp: connection refused"), true},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, "context deadline exceeded"), true},
		{"rejected by user-service", status.Error(codes.InvalidArgument, "bad scope"), false},
		{"internal failure", status.Error(codes.Internal, "boom"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &UserServiceClient{users: &fakeUserService{scopeErr: tt.err}, serviceToken: testServiceToken}
			err := client.ResetScopeByTarget(context.Background(), "store", "s1")
			if err == nil {
				t.Fatal("swallowed the scope reset error")
			}
			if got := errors.Is(err, repository.ErrUnavailable); got != tt.wantUnavailable {
				t.Fatalf("ErrUnavailable=%v want %v (err=%v)", got, tt.wantUnavailable, err)
			}
		})
	}
}

func TestHasUsers(t *testing.T) {
	users := &fakeUserService{hasUsers: true}
	client := &UserServiceClient{users: users, serviceToken: testServiceToken}
	hasUsers, err := client.HasUsers(context.Background(), "m1")
	if err != nil || !hasUsers {
		t.Fatalf("hasUsers=%v err=%v", hasUsers, err)
	}
	if users.serviceToken != testServiceToken || users.hasUsersID != "m1" {
		t.Fatalf("token=%q merchantID=%q", users.serviceToken, users.hasUsersID)
	}
	failure := errors.New("user-service unavailable")
	if _, err := (&UserServiceClient{users: &fakeUserService{hasUsersErr: failure}, serviceToken: testServiceToken}).HasUsers(context.Background(), "m1"); !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
}
