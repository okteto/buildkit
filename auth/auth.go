package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var (
	errMissingMetadata = status.Errorf(codes.InvalidArgument, "missing metadata")
	errInvalidToken    = status.Errorf(codes.Unauthenticated, "invalid okteto token, run `okteto context` and try again")
)

// Service is the service buildkit can use to authenticate
type Service struct {
	endpoint string
	client   *http.Client
}

// NewService returns an AuthService configured to use endpoint
func NewService(endpoint string) *Service {
	return &Service{
		endpoint: endpoint,
		client:   &http.Client{},
	}
}

// EnsureValidToken validates that the context includes authentication metadata
// and that it's valild
func (s *Service) EnsureValidToken(ctx context.Context) (context.Context, error) {
	// Skip authentication for Unix socket connections (internal pod traffic)
	if isUnixSocketConnection(ctx) {
		logrus.Debug("skipping authentication for Unix socket connection")
		return ctx, nil
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, errMissingMetadata
	}

	if !s.valid(md["authorization"]) {
		return ctx, errInvalidToken
	}

	return ctx, nil
}

// isUnixSocketConnection checks if the gRPC connection is over a Unix socket
func isUnixSocketConnection(ctx context.Context) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}

	// Check if the address type indicates a Unix socket
	// Unix socket addresses typically start with "unix:" or contain no port
	addr := p.Addr.String()
	network := p.Addr.Network()

	// Check if it's explicitly a Unix socket network type
	if network == "unix" || strings.HasPrefix(network, "unix") {
		logrus.Debugf("detected Unix socket connection: network=%s, addr=%s", network, addr)
		return true
	}

	return false
}

func (s *Service) valid(authorization []string) bool {
	if len(authorization) < 1 {
		logrus.Error("request didn't contain an authorization header")
		return false
	}

	req, err := http.NewRequest("POST", s.endpoint, nil)
	if err != nil {
		logrus.Errorf("couldn't create request: %s", err)
		return false
	}

	// TODO: the OKTETO CLI should not be sending a bearer token, but just the plain token
	a := strings.TrimPrefix(authorization[0], "Bearer")
	a = "Bearer " + a

	req.Header.Add("Authorization", a)
	resp, err := s.client.Do(req)
	if err != nil {
		logrus.Errorf("authentication request failed: %s", err)
		return false
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		logrus.Errorf("authentication request failed due to a bad token: %d", resp.StatusCode)
		return false
	}

	return true
}
