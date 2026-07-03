package coordinator

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/xid"
)

const ndsTokenTTL = 10 * time.Minute

type ndsTokenClaims struct {
	Exp int64  `json:"exp"`
	JTI string `json:"jti"`
	P   int    `json:"p"`
	Ref string `json:"ref"`
	RID string `json:"rid"`
}

func (h *Hub) requireNDSAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validNDSAPIKey(r.Header.Get("Authorization")) {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func validNDSAPIKey(header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if got == "" {
		return false
	}
	for _, key := range ndsAPIKeys() {
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

func ndsAPIKeys() []string {
	var keys []string
	for _, raw := range []string{os.Getenv("NDS_API_KEY"), os.Getenv("NDS_API_KEYS")} {
		for _, key := range strings.Split(raw, ",") {
			key = strings.TrimSpace(key)
			if key != "" {
				keys = append(keys, key)
			}
		}
	}
	return keys
}

func makeNDSSeatToken(roomID string, player int, ref string, now time.Time) (string, error) {
	secret := strings.TrimSpace(os.Getenv("NDS_TOKEN_SECRET"))
	if secret == "" {
		return "", fmt.Errorf("NDS_TOKEN_SECRET is not configured")
	}
	claims := ndsTokenClaims{
		RID: roomID,
		P:   player,
		Ref: ref,
		Exp: now.Add(ndsTokenTTL).Unix(),
		JTI: xid.New().String(),
	}
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	sig := hmacSHA256([]byte(secret), []byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func validateNDSSeatToken(token string, now time.Time) (ndsTokenClaims, error) {
	secret := strings.TrimSpace(os.Getenv("NDS_TOKEN_SECRET"))
	if secret == "" {
		return ndsTokenClaims{}, fmt.Errorf("NDS_TOKEN_SECRET is not configured")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ndsTokenClaims{}, fmt.Errorf("malformed token")
	}
	unsigned := parts[0] + "." + parts[1]
	expect := hmacSHA256([]byte(secret), []byte(unsigned))
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ndsTokenClaims{}, fmt.Errorf("malformed token signature")
	}
	if subtle.ConstantTimeCompare(got, expect) != 1 {
		return ndsTokenClaims{}, fmt.Errorf("invalid token signature")
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Alg != "HS256" {
		return ndsTokenClaims{}, fmt.Errorf("invalid token header")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ndsTokenClaims{}, fmt.Errorf("malformed token claims")
	}
	var claims ndsTokenClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		return ndsTokenClaims{}, fmt.Errorf("malformed token claims")
	}
	if claims.RID == "" || claims.P < 1 || claims.P > 4 || claims.Ref == "" || claims.JTI == "" {
		return ndsTokenClaims{}, fmt.Errorf("invalid token claims")
	}
	if now.Unix() > claims.Exp {
		return ndsTokenClaims{}, fmt.Errorf("token expired")
	}
	return claims, nil
}

func hmacSHA256(secret []byte, data []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func base64HMACSHA1(secret string, data string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func firstNonEmptyEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}
