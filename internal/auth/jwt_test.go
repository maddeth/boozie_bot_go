package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-key-for-unit-tests"

// makeToken creates a signed JWT for testing.
func makeToken(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return s
}

func TestVerify_ValidToken(t *testing.T) {
	v := &JWTVerifier{secret: []byte(testSecret)}

	tokenString := makeToken(t, testSecret, jwt.MapClaims{
		"sub":   "user-uuid-123",
		"email": "test@example.com",
		"role":  "authenticated",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	claims, err := v.Verify(tokenString)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if claims.Subject != "user-uuid-123" {
		t.Errorf("expected sub=user-uuid-123, got %q", claims.Subject)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("expected email=test@example.com, got %q", claims.Email)
	}
	if claims.Role != "authenticated" {
		t.Errorf("expected role=authenticated, got %q", claims.Role)
	}
}

func TestVerify_ExpiredToken(t *testing.T) {
	v := &JWTVerifier{secret: []byte(testSecret)}

	tokenString := makeToken(t, testSecret, jwt.MapClaims{
		"sub": "user-uuid-123",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	_, err := v.Verify(tokenString)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	v := &JWTVerifier{secret: []byte(testSecret)}

	tokenString := makeToken(t, "wrong-secret", jwt.MapClaims{
		"sub": "user-uuid-123",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	_, err := v.Verify(tokenString)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
}

func TestVerify_MalformedToken(t *testing.T) {
	v := &JWTVerifier{secret: []byte(testSecret)}

	_, err := v.Verify("not-a-jwt")
	if err == nil {
		t.Fatal("expected error for malformed token, got nil")
	}
}

func TestVerify_WrongSigningMethod(t *testing.T) {
	v := &JWTVerifier{secret: []byte(testSecret)}

	// Create a token with RSA signing method but still sign with HMAC bytes
	// This tests the signing method check (should reject non-HMAC)
	token := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "user-uuid-123",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tokenString, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}

	_, err = v.Verify(tokenString)
	if err == nil {
		t.Fatal("expected error for none signing method, got nil")
	}
}

func TestNewJWTVerifier_MissingEnv(t *testing.T) {
	t.Setenv("SUPABASE_JWT_SECRET", "")
	_, err := NewJWTVerifier()
	if err == nil {
		t.Fatal("expected error when SUPABASE_JWT_SECRET is empty")
	}
}

func TestNewJWTVerifier_WithEnv(t *testing.T) {
	t.Setenv("SUPABASE_JWT_SECRET", testSecret)
	v, err := NewJWTVerifier()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if string(v.secret) != testSecret {
		t.Errorf("expected secret=%q, got %q", testSecret, string(v.secret))
	}
}
