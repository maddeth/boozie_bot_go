package auth

import (
	"fmt"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// Claims represents the decoded Supabase JWT payload.
type Claims struct {
	jwt.RegisteredClaims
	Email string `json:"email"`
	Role  string `json:"role"`
}

// JWTVerifier verifies Supabase JWTs using the shared secret.
type JWTVerifier struct {
	secret []byte
}

// NewJWTVerifier creates a verifier from the SUPABASE_JWT_SECRET env var.
func NewJWTVerifier() (*JWTVerifier, error) {
	secret := os.Getenv("SUPABASE_JWT_SECRET")
	if secret == "" {
		return nil, fmt.Errorf("SUPABASE_JWT_SECRET environment variable is required")
	}
	return &JWTVerifier{secret: []byte(secret)}, nil
}

// Verify parses and validates a JWT token string.
// Returns the claims on success, or an error if the token is invalid.
func (v *JWTVerifier) Verify(tokenString string) (*Claims, error) {
	claims := &Claims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		// Ensure the signing method is HMAC (Supabase uses HS256)
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return v.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("parsing token: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}
