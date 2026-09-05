package dto

import (
	"encoding/json"
	"errors"

	"github.com/panda-dev/panda-v2/backend/services/user-service/internal/model"
)

type RegisterRequest struct {
	Name string `json:"name"`
}

type PatchRequest struct {
	Nickname   *string   `json:"nickname"`
	AvatarURL  *string   `json:"avatar_url"`
	Email      *string   `json:"email"`
	Gender     *string   `json:"gender"`
	Birthday   *string   `json:"birthday"`
	Occupation *string   `json:"occupation"`
	Hobbies    *[]string `json:"hobbies"`
	RegionCode *string   `json:"region_code"`
	RegionName *string   `json:"region_name"`
}

func (p *PatchRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) == 0 {
		return errors.New("patch must contain at least one field")
	}
	for name, raw := range fields {
		if string(raw) == "null" {
			return errors.New("null patch field")
		}
		var target any
		switch name {
		case "nickname":
			target = &p.Nickname
		case "avatar_url":
			target = &p.AvatarURL
		case "email":
			target = &p.Email
		case "gender":
			target = &p.Gender
		case "birthday":
			target = &p.Birthday
		case "occupation":
			target = &p.Occupation
		case "hobbies":
			target = &p.Hobbies
		case "region_code":
			target = &p.RegionCode
		case "region_name":
			target = &p.RegionName
		default:
			return errors.New("unknown patch field")
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return err
		}
	}
	return nil
}

func (p PatchRequest) UserUpdate() model.UserUpdate {
	return model.UserUpdate{Nickname: p.Nickname, AvatarURL: p.AvatarURL, Email: p.Email, Gender: p.Gender, Birthday: p.Birthday, Occupation: p.Occupation, Hobbies: p.Hobbies, RegionCode: p.RegionCode, RegionName: p.RegionName}
}
