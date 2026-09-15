package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// fakeRepo is an in-memory stand-in for Postgres.
type fakeRepo struct {
	users     map[string]User
	refresh   map[string]uuid.UUID
	consumed  map[string]bool
	createErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users:    map[string]User{},
		refresh:  map[string]uuid.UUID{},
		consumed: map[string]bool{},
	}
}

func (f *fakeRepo) CreateUser(_ context.Context, u User) error {
	if f.createErr != nil {
		return f.createErr
	}
	if _, exists := f.users[u.Email]; exists {
		return httpx.ErrConflict
	}
	f.users[u.Email] = u
	return nil
}

func (f *fakeRepo) UserByEmail(_ context.Context, email string) (User, error) {
	u, ok := f.users[email]
	if !ok {
		return User{}, httpx.ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) StoreRefreshToken(_ context.Context, digest []byte, userID uuid.UUID, _ time.Time) error {
	f.refresh[string(digest)] = userID
	return nil
}

func (f *fakeRepo) ConsumeRefreshToken(_ context.Context, digest []byte) (uuid.UUID, error) {
	key := string(digest)
	userID, ok := f.refresh[key]
	if !ok || f.consumed[key] {
		return uuid.Nil, httpx.ErrUnauthorized
	}
	f.consumed[key] = true
	return userID, nil
}

func testService() (*Service, *fakeRepo) {
	repo := newFakeRepo()
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	return NewService(repo, issuer), repo
}

const goodPassword = "correct horse battery staple"

func TestRegisterIssuesUsableTokens(t *testing.T) {
	svc, repo := testService()

	tokens, err := svc.Register(context.Background(), "Cook@Example.COM", goodPassword)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatal("Register() returned an empty token pair")
	}
	if tokens.TokenType != "Bearer" {
		t.Errorf("TokenType = %q, want Bearer", tokens.TokenType)
	}
	// Email is lowercased on the way in, so lookups are case insensitive.
	if _, ok := repo.users["cook@example.com"]; !ok {
		t.Errorf("stored emails = %v, want the address normalised to lower case", keys(repo.users))
	}
	// The stored hash must not be the password.
	if repo.users["cook@example.com"].PasswordHash == goodPassword {
		t.Error("password was stored in clear text")
	}
}

func TestRegisterRejectsShortPasswordAndBadEmail(t *testing.T) {
	svc, _ := testService()

	if _, err := svc.Register(context.Background(), "cook@example.com", "short"); !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("short password error = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.Register(context.Background(), "not-an-email", goodPassword); !errors.Is(err, httpx.ErrInvalidInput) {
		t.Errorf("bad email error = %v, want ErrInvalidInput", err)
	}
}

func TestLoginAcceptsCorrectPasswordCaseInsensitively(t *testing.T) {
	svc, _ := testService()
	if _, err := svc.Register(context.Background(), "cook@example.com", goodPassword); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := svc.Login(context.Background(), "COOK@example.com", goodPassword); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
}

// Wrong password and unknown email must be indistinguishable to the caller,
// otherwise the endpoint becomes a user enumeration oracle.
func TestLoginDoesNotDistinguishUnknownEmailFromWrongPassword(t *testing.T) {
	svc, _ := testService()
	if _, err := svc.Register(context.Background(), "cook@example.com", goodPassword); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, wrongPassword := svc.Login(context.Background(), "cook@example.com", "not the password")
	_, unknownEmail := svc.Login(context.Background(), "nobody@example.com", goodPassword)

	if !errors.Is(wrongPassword, httpx.ErrUnauthorized) {
		t.Fatalf("wrong password error = %v, want ErrUnauthorized", wrongPassword)
	}
	if !errors.Is(unknownEmail, httpx.ErrUnauthorized) {
		t.Fatalf("unknown email error = %v, want ErrUnauthorized", unknownEmail)
	}
	if wrongPassword.Error() != unknownEmail.Error() {
		t.Errorf("messages differ (%q vs %q); they must be identical",
			wrongPassword.Error(), unknownEmail.Error())
	}
}

func TestRefreshRotatesAndRejectsReplay(t *testing.T) {
	svc, _ := testService()
	first, err := svc.Register(context.Background(), "cook@example.com", goodPassword)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	second, err := svc.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Error("refresh token was not rotated")
	}

	if _, err := svc.Refresh(context.Background(), first.RefreshToken); !errors.Is(err, httpx.ErrUnauthorized) {
		t.Errorf("replayed token error = %v, want ErrUnauthorized", err)
	}
}

func TestAccessTokenRoundTripsThroughTheIssuer(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	userID := uuid.New()

	token, expiresAt, err := issuer.AccessToken(userID)
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Errorf("expiresAt = %v, want a future time", expiresAt)
	}

	got, err := issuer.ParseAccessToken(token)
	if err != nil {
		t.Fatalf("ParseAccessToken() error = %v", err)
	}
	if got != userID {
		t.Errorf("subject = %s, want %s", got, userID)
	}
}

// A token signed with another key must not validate, which is the whole point of
// the signature check.
func TestParseAccessTokenRejectsForeignSignature(t *testing.T) {
	mine := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	theirs := NewTokenIssuer([]byte("a-completely-different-secret-key!"), time.Minute, time.Hour)

	token, _, err := theirs.AccessToken(uuid.New())
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}
	if _, err := mine.ParseAccessToken(token); err == nil {
		t.Fatal("ParseAccessToken() = nil error, want rejection of a foreign signature")
	}
}

func TestParseAccessTokenRejectsExpiredToken(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-that-is-long-enough-32"), time.Minute, time.Hour)
	issuer.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }

	token, _, err := issuer.AccessToken(uuid.New())
	if err != nil {
		t.Fatalf("AccessToken() error = %v", err)
	}

	issuer.now = time.Now
	if _, err := issuer.ParseAccessToken(token); err == nil {
		t.Fatal("ParseAccessToken() = nil error, want rejection of an expired token")
	}
}

func TestPasswordHashIsSaltedAndVerifiable(t *testing.T) {
	first, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical, so the salt is not random")
	}

	ok, err := VerifyPassword(goodPassword, first)
	if err != nil || !ok {
		t.Errorf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}
	ok, err = VerifyPassword("wrong password entirely", first)
	if err != nil || ok {
		t.Errorf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}
}

// The login timing-equalisation path depends on this constant parsing cleanly.
func TestDummyHashIsWellFormed(t *testing.T) {
	if _, err := VerifyPassword("anything", dummyHash); err != nil {
		t.Fatalf("VerifyPassword against dummyHash error = %v; the constant is malformed", err)
	}
}

func keys(m map[string]User) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
