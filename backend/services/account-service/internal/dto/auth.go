package dto

import "github.com/panda-dev/panda-v2/backend/services/account-service/internal/model"

type LoginRequest struct {
	Username string            `json:"username"`
	Password string            `json:"password"`
	Type     model.AccountType `json:"type"`
}

type LoginResponse struct {
	AccessToken  string        `json:"access_token"`
	RefreshToken string        `json:"refresh_token"`
	TokenType    string        `json:"token_type"`
	ExpiresIn    int64         `json:"expires_in"`
	Account      model.Account `json:"account,omitempty"`
}

type TokenRequest struct {
	RefreshToken string `json:"refresh_token"`
}
