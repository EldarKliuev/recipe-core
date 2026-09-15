package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/recipe-ai/core-api/internal/platform/httpx"
)

// repository is the persistence surface this service needs. Declared here, on
// the consumer side, so the service can be unit tested with a fake.
type repository interface {
	CreateUser(ctx context.Context, u User) error
	UserByEmail(ctx context.Context, email string) (User, error)
	StoreRefreshToken(ctx context.Context, digest []byte, userID uuid.UUID, expiresAt time.Time) error
	ConsumeRefreshToken(ctx context.Context, digest []byte) (uuid.UUID, error)
}

type Service struct {
	repo   repository
	tokens *TokenIssuer
}

func NewService(repo repository, tokens *TokenIssuer) *Service {
	return &Service{repo: repo, tokens: tokens}
}

// Tokens is what the client receives after register, login or refresh.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type"`
}

const minPasswordLen = 10

func (s *Service) Register(ctx context.Context, email, password string) (Tokens, error) {
	email, err := normaliseEmail(email)
	if err != nil {
		return Tokens{}, err
	}
	if len([]rune(password)) < minPasswordLen {
		return Tokens{}, httpx.Invalidf("password must be at least %d characters", minPasswordLen)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return Tokens{}, err
	}
	user := User{ID: uuid.New(), Email: email, PasswordHash: hash}
	if err := s.repo.CreateUser(ctx, user); err != nil {
		return Tokens{}, err
	}
	return s.issue(ctx, user.ID)
}

func (s *Service) Login(ctx context.Context, email, password string) (Tokens, error) {
	email, err := normaliseEmail(email)
	if err != nil {
		// Do not disclose whether the address is merely malformed.
		return Tokens{}, errInvalidCredentials()
	}
	user, err := s.repo.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			// Spend comparable time on unknown emails to blunt user enumeration
			// via response timing.
			_, _ = VerifyPassword(password, dummyHash)
			return Tokens{}, errInvalidCredentials()
		}
		return Tokens{}, err
	}
	ok, err := VerifyPassword(password, user.PasswordHash)
	if err != nil {
		return Tokens{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return Tokens{}, errInvalidCredentials()
	}
	return s.issue(ctx, user.ID)
}

// Refresh rotates the refresh token: the presented one is revoked and a new
// pair is issued.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	if refreshToken == "" {
		return Tokens{}, httpx.Invalidf("refresh_token is required")
	}
	userID, err := s.repo.ConsumeRefreshToken(ctx, HashRefreshToken(refreshToken))
	if err != nil {
		return Tokens{}, err
	}
	return s.issue(ctx, userID)
}

func (s *Service) issue(ctx context.Context, userID uuid.UUID) (Tokens, error) {
	access, expiresAt, err := s.tokens.AccessToken(userID)
	if err != nil {
		return Tokens{}, err
	}
	refresh, digest, refreshExp, err := s.tokens.RefreshToken()
	if err != nil {
		return Tokens{}, err
	}
	if err := s.repo.StoreRefreshToken(ctx, digest, userID, refreshExp); err != nil {
		return Tokens{}, err
	}
	return Tokens{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		TokenType:    "Bearer",
	}, nil
}

func normaliseEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return "", httpx.Invalidf("email is required")
	}
	if len(email) > 320 {
		return "", httpx.Invalidf("email is too long")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return "", httpx.Invalidf("email is not a valid address")
	}
	return strings.ToLower(email), nil
}

func errInvalidCredentials() error {
	return fmt.Errorf("%w: invalid email or password", httpx.ErrUnauthorized)
}

// dummyHash is a valid argon2id hash of a random value, used only to equalise
// login timing for unknown emails.
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=4$c29tZXNhbHRzb21lc2E$RdescudvJCsgt3ub+b+dWRWJTmaaJObG"
