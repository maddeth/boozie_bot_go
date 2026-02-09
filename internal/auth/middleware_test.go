package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// newTestMiddleware creates middleware with a known secret and no DB (for auth-only tests).
func newTestMiddleware() (*Middleware, *JWTVerifier) {
	v := &JWTVerifier{secret: []byte(testSecret)}
	m := &Middleware{verifier: v, db: nil}
	return m, v
}

// okHandler is a simple handler that returns 200.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
})

func TestAuthenticateToken_ValidToken(t *testing.T) {
	m, _ := newTestMiddleware()

	tokenString := makeToken(t, testSecret, jwt.MapClaims{
		"sub":   "user-uuid-123",
		"email": "test@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rr := httptest.NewRecorder()

	// Capture the claims from context in the next handler
	var gotClaims *Claims
	handler := m.AuthenticateToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClaims = GetClaims(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if gotClaims == nil {
		t.Fatal("expected claims in context, got nil")
	}
	if gotClaims.Subject != "user-uuid-123" {
		t.Errorf("expected sub=user-uuid-123, got %q", gotClaims.Subject)
	}
}

func TestAuthenticateToken_MissingHeader(t *testing.T) {
	m, _ := newTestMiddleware()

	req := httptest.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()

	m.AuthenticateToken(okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}

	var body map[string]string
	json.NewDecoder(rr.Body).Decode(&body)
	if body["error"] != "Unauthorized" {
		t.Errorf("expected error=Unauthorized, got %q", body["error"])
	}
}

func TestAuthenticateToken_InvalidBearer(t *testing.T) {
	m, _ := newTestMiddleware()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Basic abc123")
	rr := httptest.NewRecorder()

	m.AuthenticateToken(okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthenticateToken_InvalidToken(t *testing.T) {
	m, _ := newTestMiddleware()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer invalid.token.here")
	rr := httptest.NewRecorder()

	m.AuthenticateToken(okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthenticateToken_ExpiredToken(t *testing.T) {
	m, _ := newTestMiddleware()

	tokenString := makeToken(t, testSecret, jwt.MapClaims{
		"sub": "user-uuid-123",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	rr := httptest.NewRecorder()

	m.AuthenticateToken(okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestRequireModeratorRole_NoClaims(t *testing.T) {
	m, _ := newTestMiddleware()

	// Call RequireModeratorRole without AuthenticateToken first (no claims in context)
	req := httptest.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()

	m.RequireModeratorRole(okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestRequireAdminRole_NoClaims(t *testing.T) {
	m, _ := newTestMiddleware()

	req := httptest.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()

	m.RequireAdminRole(okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestGetClaims_NilContext(t *testing.T) {
	claims := GetClaims(context.Background())
	if claims != nil {
		t.Fatal("expected nil claims from empty context")
	}
}

func TestGetUser_NilContext(t *testing.T) {
	user := GetUser(context.Background())
	if user != nil {
		t.Fatal("expected nil user from empty context")
	}
}
