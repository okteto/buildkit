package okteto

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc/keepalive"
)

var (
	kcp *keepalive.ClientParameters
	ksp *keepalive.ServerParameters
	kep *keepalive.EnforcementPolicy
)

func loadKeepaliveClientParams() keepalive.ClientParameters {
	if kcp == nil {
		kcp = &keepalive.ClientParameters{}
		if timeEnv := os.Getenv("OKTETO_KEEPALIVE_CLIENT_TIME_MS"); timeEnv != "" {
			timeDuration, err := time.ParseDuration(fmt.Sprintf("%sms", timeEnv))
			if err == nil {
				kcp.Time = timeDuration
			}
		}

		if timeoutEnv := os.Getenv("OKTETO_KEEPALIVE_CLIENT_TIMEOUT_MS"); timeoutEnv != "" {
			timeoutDuration, err := time.ParseDuration(fmt.Sprintf("%sms", timeoutEnv))
			if err == nil {
				kcp.Timeout = timeoutDuration
			}
		}

		if permitWithoutStreamEnv := os.Getenv("OKTETO_KEEPALIVE_CLIENT_PERMIT_WITHOUT_STREAM"); permitWithoutStreamEnv != "" {
			kcp.PermitWithoutStream, _ = strconv.ParseBool(permitWithoutStreamEnv)
		}
	}

	return *kcp

}

func loadKeepaliveServerParams() keepalive.ServerParameters {
	if ksp == nil {
		ksp = &keepalive.ServerParameters{}
		if maxConnEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_MAX_CONN_IDLE_MS"); maxConnEnv != "" {
			maxConnIdleDuration, err := time.ParseDuration(fmt.Sprintf("%sms", maxConnEnv))
			if err == nil {
				ksp.MaxConnectionIdle = maxConnIdleDuration
			}
		}

		if maxConnAgeEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_MAX_CONN_AGE_MS"); maxConnAgeEnv != "" {
			maxConnAgeDuration, err := time.ParseDuration(fmt.Sprintf("%sms", maxConnAgeEnv))
			if err == nil {
				ksp.MaxConnectionAge = maxConnAgeDuration
			}
		}

		if maxConnAgeGraceEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_MAX_CONN_AGE_GRACE_MS"); maxConnAgeGraceEnv != "" {
			maxConnAgeGraceDuration, err := time.ParseDuration(fmt.Sprintf("%sms", maxConnAgeGraceEnv))
			if err == nil {
				ksp.MaxConnectionAgeGrace = maxConnAgeGraceDuration
			}
		}

		if timeEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_TIME_MS"); timeEnv != "" {
			timeDuration, err := time.ParseDuration(fmt.Sprintf("%sms", timeEnv))
			if err == nil {
				ksp.Time = timeDuration
			}
		}

		if timeoutEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_TIMEOUT_MS"); timeoutEnv != "" {
			timeoutDuration, err := time.ParseDuration(fmt.Sprintf("%sms", timeoutEnv))
			if err == nil {
				ksp.Timeout = timeoutDuration
			}
		}
	}

	return *ksp

}

func loadKeepaliveEnforcementPolicy() keepalive.EnforcementPolicy {
	if kep == nil {
		kep = &keepalive.EnforcementPolicy{}
		if mintimeEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_POLICY_MINTIME_MS"); mintimeEnv != "" {
			mintimeDuration, err := time.ParseDuration(fmt.Sprintf("%sms", mintimeEnv))
			if err == nil {
				kep.MinTime = mintimeDuration
			}
		}

		if permitWithoutStreamEnv := os.Getenv("OKTETO_KEEPALIVE_SERVER_POLICY_PERMIT_WITHOUT_STREAM"); permitWithoutStreamEnv != "" {
			kep.PermitWithoutStream, _ = strconv.ParseBool(permitWithoutStreamEnv)
		}
	}

	return *kep

}
