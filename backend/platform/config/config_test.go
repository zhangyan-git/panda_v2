package config

import "testing"

func TestLoadRedisDB(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "empty", value: "", want: 0},
		{name: "valid", value: "7", want: 7},
		{name: "negative", value: "-1", wantErr: true},
		{name: "invalid", value: "redis", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("REDIS_DB", tt.value)
			cfg, err := Load("test")
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected configuration error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RedisDB != tt.want {
				t.Fatalf("RedisDB = %d, want %d", cfg.RedisDB, tt.want)
			}
		})
	}
}
