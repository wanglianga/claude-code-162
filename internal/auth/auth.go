package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type Claims struct {
	Sub  int64  `json:"sub"`
	Role string `json:"role"`
	Name string `json:"name"`
	Exp  int64  `json:"exp"`
}

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func sign(data []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Issue 生成简化版 JWT（HS256 风格，避免额外依赖）。
func Issue(secret string, c Claims) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	if c.Exp == 0 {
		c.Exp = time.Now().Add(12 * time.Hour).Unix()
	}
	body, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := sign([]byte(header+"."+payload), secret)
	return header + "." + payload + "." + sig
}

// Parse 校验并解析令牌。
func Parse(secret, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("invalid token")
	}
	expected := sign([]byte(parts[0]+"."+parts[1]), secret)
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return Claims{}, errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil {
		return Claims{}, err
	}
	if time.Now().Unix() > c.Exp {
		return Claims{}, errors.New("token expired")
	}
	return c, nil
}

type ctxKey string

const ClaimsKey ctxKey = "claims"

// Middleware 校验 Bearer Token；require 为空表示任意已登录角色。
func Middleware(secret string, require ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
				return
			}
			c, err := Parse(secret, strings.TrimPrefix(h, "Bearer "))
			if err != nil {
				http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
			if len(require) > 0 {
				ok := false
				for _, role := range require {
					if c.Role == role {
						ok = true
						break
					}
				}
				if !ok {
					http.Error(w, `{"error":"forbidden: requires role `+require[0]+`"}`, http.StatusForbidden)
					return
				}
			}
			ctx := contextWithClaims(r.Context(), c)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
