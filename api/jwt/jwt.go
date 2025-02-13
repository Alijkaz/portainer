package jwt

import (
	"errors"
	"fmt"
	"time"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/apikey"
	"github.com/portainer/portainer/api/dataservices"

	"github.com/golang-jwt/jwt/v4"
	"github.com/rs/zerolog/log"
)

const year = time.Hour * 24 * 365

// scope represents JWT scopes that are supported in JWT claims.
type scope string

// Service represents a service for managing JWT tokens.
type Service struct {
	secrets            map[scope][]byte
	userSessionTimeout time.Duration
	dataStore          dataservices.DataStore
}

type claims struct {
	UserID              int    `json:"id"`
	Username            string `json:"username"`
	Role                int    `json:"role"`
	Scope               scope  `json:"scope"`
	ForceChangePassword bool   `json:"forceChangePassword"`
	jwt.RegisteredClaims
}

var (
	errSecretGeneration = errors.New("Unable to generate secret key")
	errInvalidJWTToken  = errors.New("Invalid JWT token")
)

const (
	defaultScope    = scope("default")
	kubeConfigScope = scope("kubeconfig")
)

// NewService initializes a new service. It will generate a random key that will be used to sign JWT tokens.
func NewService(userSessionDuration string, dataStore dataservices.DataStore) (*Service, error) {
	userSessionTimeout, err := time.ParseDuration(userSessionDuration)
	if err != nil {
		return nil, err
	}

	secret := apikey.GenerateRandomKey(32)
	if secret == nil {
		return nil, errSecretGeneration
	}

	kubeSecret, err := getOrCreateKubeSecret(dataStore)
	if err != nil {
		return nil, err
	}

	service := &Service{
		map[scope][]byte{
			defaultScope:    secret,
			kubeConfigScope: kubeSecret,
		},
		userSessionTimeout,
		dataStore,
	}

	return service, nil
}

func getOrCreateKubeSecret(dataStore dataservices.DataStore) ([]byte, error) {
	settings, err := dataStore.Settings().Settings()
	if err != nil {
		return nil, err
	}

	kubeSecret := settings.OAuthSettings.KubeSecretKey
	if kubeSecret == nil {
		kubeSecret = apikey.GenerateRandomKey(32)
		if kubeSecret == nil {
			return nil, errSecretGeneration
		}

		settings.OAuthSettings.KubeSecretKey = kubeSecret

		if err := dataStore.Settings().UpdateSettings(settings); err != nil {
			return nil, err
		}
	}

	return kubeSecret, nil
}

func (service *Service) defaultExpireAt() time.Time {
	return time.Now().Add(service.userSessionTimeout)
}

// GenerateToken generates a new JWT token.
func (service *Service) GenerateToken(data *portainer.TokenData) (string, time.Time, error) {
	expiryTime := service.defaultExpireAt()
	token, err := service.generateSignedToken(data, expiryTime, defaultScope)
	return token, expiryTime, err
}

// ParseAndVerifyToken parses a JWT token and verify its validity. It returns an error if token is invalid.
func (service *Service) ParseAndVerifyToken(token string) (*portainer.TokenData, error) {
	scope := parseScope(token)
	secret := service.secrets[scope]
	parsedToken, err := jwt.ParseWithClaims(token, &claims{}, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			msg := fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			return nil, msg
		}
		return secret, nil
	})

	if err == nil && parsedToken != nil {
		if cl, ok := parsedToken.Claims.(*claims); ok && parsedToken.Valid {

			user, err := service.dataStore.User().Read(portainer.UserID(cl.UserID))
			if err != nil {
				return nil, errInvalidJWTToken
			}
			if user.TokenIssueAt > cl.RegisteredClaims.IssuedAt.Unix() {
				return nil, errInvalidJWTToken
			}

			return &portainer.TokenData{
				ID:                  portainer.UserID(cl.UserID),
				Username:            cl.Username,
				Role:                portainer.UserRole(cl.Role),
				Token:               token,
				ForceChangePassword: cl.ForceChangePassword,
			}, nil
		}
	}
	return nil, errInvalidJWTToken
}

// parse a JWT token, fallback to defaultScope if no scope is present in the JWT
func parseScope(token string) scope {
	unverifiedToken, _, _ := new(jwt.Parser).ParseUnverified(token, &claims{})
	if unverifiedToken != nil {
		if cl, ok := unverifiedToken.Claims.(*claims); ok {
			if cl.Scope == kubeConfigScope {
				return kubeConfigScope
			}
		}
	}

	return defaultScope
}

// SetUserSessionDuration sets the user session duration
func (service *Service) SetUserSessionDuration(userSessionDuration time.Duration) {
	service.userSessionTimeout = userSessionDuration
}

func (service *Service) generateSignedToken(data *portainer.TokenData, expiresAt time.Time, scope scope) (string, error) {
	secret, found := service.secrets[scope]
	if !found {
		return "", fmt.Errorf("invalid scope: %v", scope)
	}

	settings, err := service.dataStore.Settings().Settings()
	if err != nil {
		return "", fmt.Errorf("failed fetching settings from db: %w", err)
	}

	if settings.IsDockerDesktopExtension {
		// Set expiration to 99 years for docker desktop extension.
		log.Info().Msg("detected docker desktop extension mode")
		expiresAt = time.Now().Add(year * 99)
	}

	cl := claims{
		UserID:              int(data.ID),
		Username:            data.Username,
		Role:                int(data.Role),
		Scope:               scope,
		ForceChangePassword: data.ForceChangePassword,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	// If expiresAt is set to a zero value, the token should never expire
	if expiresAt.IsZero() {
		cl.RegisteredClaims.ExpiresAt = nil
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, cl)
	signedToken, err := token.SignedString(secret)
	if err != nil {
		return "", err
	}

	return signedToken, nil
}
