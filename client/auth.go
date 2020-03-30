package client

import "google.golang.org/grpc/credentials"

type withRPCCreds struct {
	creds credentials.PerRPCCredentials
}

func WithRPCCreds(c credentials.PerRPCCredentials) ClientOpt {
	return &withRPCCreds{c}
}
