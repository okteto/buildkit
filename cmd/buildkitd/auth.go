package main

import (
	"context"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"strings"
)

var (
	errMissingMetadata = status.Errorf(codes.InvalidArgument, "missing metadata")
	errInvalidToken    = status.Errorf(codes.Unauthenticated, "invalid okteto token, run `okteto login` and try again")
)

func ensureValidToken(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx, errMissingMetadata
	}
	// The keys within metadata.MD are normalized to lowercase.
	// See: https://godoc.org/google.golang.org/grpc/metadata#New
	if !valid(md["authorization"]) {
		return ctx, errInvalidToken
	}

	return ctx, nil
}

func valid(authorization []string) bool {
	if len(authorization) < 1 {
		logrus.Error("request didn't contain an authorization header")
		return false
	}
	token := strings.TrimPrefix(authorization[0], "Bearer ")

	//TODO: remove token from output
	logrus.Errorf("%s is not a valid token", token)

	// TODO: validate okteto token
	return token == "some-secret-token"
}
