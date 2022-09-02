package grpcserver

import (
	"context"
	"fmt"
	"strings"

	apikeygenprefix "github.com/grafana/grafana/pkg/components/apikeygenprefixed"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/apikey"
	"github.com/grafana/grafana/pkg/services/entity"
	"github.com/grafana/grafana/pkg/services/org"
	"github.com/grafana/grafana/pkg/services/user"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Authenticator can authenticate GRPC requests.
type Authenticator struct {
	logger        log.Logger
	apiKeyService apikey.Service
	userService   user.Service
}

func NewAuthenticator(apiKeyService apikey.Service, userService user.Service) *Authenticator {
	return &Authenticator{
		logger:        log.New("grpc-server-authenticator"),
		apiKeyService: apiKeyService,
		userService:   userService,
	}
}

// Authenticate checks that a token exists and is valid. It stores the user
// metadata in the returned context and removes the token from the context.
func (a *Authenticator) Authenticate(ctx context.Context) (context.Context, error) {
	return a.tokenAuth(ctx)
}

const tokenPrefix = "Bearer "

func (a *Authenticator) tokenAuth(ctx context.Context) (context.Context, error) {
	auth, err := extractAuthorization(ctx)
	if err != nil {
		return ctx, err
	}

	if !strings.HasPrefix(auth, tokenPrefix) {
		return ctx, status.Error(codes.Unauthenticated, `missing "Bearer " prefix in "authorization" value`)
	}

	token := strings.TrimPrefix(auth, tokenPrefix)
	if token == "" {
		return ctx, status.Error(codes.Unauthenticated, "token required")
	}

	newCtx := purgeHeader(ctx, "authorization")

	newCtx, err = a.validateToken(ctx, token)
	if err != nil {
		a.logger.Warn("request with invalid token", "error", err, "token", token)
		return ctx, status.Error(codes.Unauthenticated, "invalid token")
	}
	return newCtx, nil
}

func (a *Authenticator) validateToken(ctx context.Context, keyString string) (context.Context, error) {
	// prefixed decode key
	decoded, err := apikeygenprefix.Decode(keyString)
	if err != nil {
		return nil, err
	}

	hash, err := decoded.Hash()
	if err != nil {
		return nil, err
	}

	key, err := a.apiKeyService.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		return nil, err
	}

	querySignedInUser := user.GetSignedInUserQuery{UserID: *key.ServiceAccountId, OrgID: key.OrgId}
	res, err := a.userService.GetSignedInUserWithCacheCtx(ctx, &querySignedInUser)
	if err != nil {
		return nil, err
	}

	if !res.HasRole(org.RoleAdmin) {
		return nil, fmt.Errorf("api key does not have admin role")
	}

	// disabled service accounts are not allowed to access the API
	if res.IsDisabled {
		return nil, fmt.Errorf("service account is disabled")
	}

	newCtx := context.WithValue(ctx, entity.TempSignedInUserKey, res)

	return newCtx, nil
}

func extractAuthorization(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "no headers in request")
	}

	authHeaders, ok := md["authorization"]
	if !ok {
		return "", status.Error(codes.Unauthenticated, `no "authorization" header in request`)
	}

	if len(authHeaders) != 1 {
		return "", status.Error(codes.Unauthenticated, `malformed "authorization" header: one value required`)
	}

	return authHeaders[0], nil
}

func purgeHeader(ctx context.Context, header string) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	mdCopy := md.Copy()
	mdCopy[header] = nil
	return metadata.NewIncomingContext(ctx, mdCopy)
}