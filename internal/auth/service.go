package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	qolv1 "github.com/aleksclark/qol/gen/qol/v1"
	"github.com/aleksclark/qol/internal/store"
	"golang.org/x/crypto/argon2"
	"google.golang.org/protobuf/proto"
)

const (
	UsersBucket    = "QOL_USERS"
	SessionsBucket = "QOL_SESSIONS"
	TicketsBucket  = "QOL_UPLOAD_TICKETS"
)

var ErrCredentials = errors.New("invalid credentials")

type Service struct {
	store      store.Store
	now        func() time.Time
	sessionTTL time.Duration
	ticketTTL  time.Duration
}

func New(storage store.Store, sessionTTL, ticketTTL time.Duration) *Service {
	return &Service{store: storage, now: time.Now, sessionTTL: sessionTTL, ticketTTL: ticketTTL}
}

func (s *Service) Bootstrap(ctx context.Context, username, password string) error {
	username = normalize(username)
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}
	salt, err := randomBytes(16)
	if err != nil {
		return err
	}
	record := &qolv1.UserRecord{Username: username, Admin: true, PasswordSalt: salt, PasswordHash: passwordHash(password, salt), CreatedAtUnix: s.now().Unix()}
	payload, err := proto.Marshal(record)
	if err != nil {
		return err
	}
	return s.store.Create(ctx, UsersBucket, username, payload)
}

func (s *Service) Login(ctx context.Context, username, password string) (string, *qolv1.User, error) {
	value, err := s.store.Get(ctx, UsersBucket, normalize(username))
	if err != nil {
		return "", nil, ErrCredentials
	}
	record := new(qolv1.UserRecord)
	if err := proto.Unmarshal(value.Data, record); err != nil {
		return "", nil, err
	}
	actual := passwordHash(password, record.PasswordSalt)
	if subtle.ConstantTimeCompare(actual, record.PasswordHash) != 1 {
		return "", nil, ErrCredentials
	}
	token, hash, err := secret()
	if err != nil {
		return "", nil, err
	}
	session := &qolv1.SessionRecord{Username: record.Username, ExpiresAtUnix: s.now().Add(s.sessionTTL).Unix()}
	payload, err := proto.Marshal(session)
	if err != nil {
		return "", nil, err
	}
	if err := s.store.Create(ctx, SessionsBucket, hash, payload); err != nil {
		return "", nil, err
	}
	return token, &qolv1.User{Username: record.Username, Admin: record.Admin}, nil
}

func (s *Service) Current(ctx context.Context, token string) (*qolv1.User, error) {
	value, err := s.store.Get(ctx, SessionsBucket, hashSecret(token))
	if err != nil {
		return nil, ErrCredentials
	}
	session := new(qolv1.SessionRecord)
	if err := proto.Unmarshal(value.Data, session); err != nil {
		return nil, err
	}
	if session.ExpiresAtUnix <= s.now().Unix() {
		_ = s.store.Delete(ctx, SessionsBucket, hashSecret(token), value.Revision)
		return nil, ErrCredentials
	}
	userValue, err := s.store.Get(ctx, UsersBucket, session.Username)
	if err != nil {
		return nil, ErrCredentials
	}
	user := new(qolv1.UserRecord)
	if err := proto.Unmarshal(userValue.Data, user); err != nil {
		return nil, err
	}
	return &qolv1.User{Username: user.Username, Admin: user.Admin}, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	value, err := s.store.Get(ctx, SessionsBucket, hashSecret(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.store.Delete(ctx, SessionsBucket, hashSecret(token), value.Revision)
}

func (s *Service) CreateTicket(ctx context.Context, username string) (string, time.Time, error) {
	token, hash, err := secret()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().Add(s.ticketTTL)
	payload, err := proto.Marshal(&qolv1.UploadTicketRecord{Username: username, ExpiresAtUnix: expires.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	if err := s.store.Create(ctx, TicketsBucket, hash, payload); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

func (s *Service) ConsumeTicket(ctx context.Context, token string) (*qolv1.User, error) {
	key := hashSecret(token)
	value, err := s.store.Get(ctx, TicketsBucket, key)
	if err != nil {
		return nil, ErrCredentials
	}
	record := new(qolv1.UploadTicketRecord)
	if err := proto.Unmarshal(value.Data, record); err != nil {
		return nil, err
	}
	if record.ExpiresAtUnix <= s.now().Unix() {
		return nil, ErrCredentials
	}
	if err := s.store.Delete(ctx, TicketsBucket, key, value.Revision); err != nil {
		return nil, ErrCredentials
	}
	userValue, err := s.store.Get(ctx, UsersBucket, record.Username)
	if err != nil {
		return nil, ErrCredentials
	}
	user := new(qolv1.UserRecord)
	if err := proto.Unmarshal(userValue.Data, user); err != nil {
		return nil, err
	}
	return &qolv1.User{Username: user.Username, Admin: user.Admin}, nil
}

func normalize(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

func passwordHash(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 1, 64*1024, 4, 32)
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := rand.Read(value)
	return value, err
}

func secret() (string, string, error) {
	value, err := randomBytes(32)
	if err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(value)
	return token, hashSecret(token), nil
}

func hashSecret(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
