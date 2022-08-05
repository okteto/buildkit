package okteto

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/keepalive"
)

const (
	initialIntEnvValue   = "5"
	secondaryIntEnvValue = "3"
	initialBoolEnvValue  = true
)

func initialKcp() keepalive.ClientParameters {
	return keepalive.ClientParameters{
		Time:                5000000,
		Timeout:             5000000,
		PermitWithoutStream: true,
	}
}

func initialKsp() keepalive.ServerParameters {
	return keepalive.ServerParameters{
		Time:                  5000000,
		Timeout:               5000000,
		MaxConnectionIdle:     5000000,
		MaxConnectionAge:      5000000,
		MaxConnectionAgeGrace: 5000000,
	}
}

func initialKep() keepalive.EnforcementPolicy {
	return keepalive.EnforcementPolicy{
		MinTime:             5000000,
		PermitWithoutStream: true,
	}
}

func setInitialKeepaliveClientEnvs(t *testing.T, initValues bool) {
	var iEnvValue, bEnvValue string
	if initValues {
		iEnvValue = initialIntEnvValue
		bEnvValue = strconv.FormatBool(initialBoolEnvValue)
	} else {
		iEnvValue = secondaryIntEnvValue
		bEnvValue = strconv.FormatBool(!initialBoolEnvValue)
	}

	t.Setenv("OKTETO_KEEPALIVE_CLIENT_TIME_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_CLIENT_TIMEOUT_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_CLIENT_PERMIT_WITHOUT_STREAM", bEnvValue)
}

func setInitialKeepaliveServerEnvs(t *testing.T, initValues bool) {
	var iEnvValue string
	if initValues {
		iEnvValue = initialIntEnvValue
	} else {
		iEnvValue = secondaryIntEnvValue
	}

	t.Setenv("OKTETO_KEEPALIVE_SERVER_MAX_CONN_IDLE_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_SERVER_MAX_CONN_AGE_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_SERVER_MAX_CONN_AGE_GRACE_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_SERVER_TIME_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_SERVER_TIMEOUT_MS", iEnvValue)
}

func setInitialKeepaliveEnforcementPolicyEnvs(t *testing.T, initValues bool) {
	var iEnvValue, bEnvValue string
	if initValues {
		iEnvValue = initialIntEnvValue
		bEnvValue = strconv.FormatBool(initialBoolEnvValue)
	} else {
		iEnvValue = secondaryIntEnvValue
		bEnvValue = strconv.FormatBool(!initialBoolEnvValue)
	}

	t.Setenv("OKTETO_KEEPALIVE_SERVER_POLICY_MINTIME_MS", iEnvValue)
	t.Setenv("OKTETO_KEEPALIVE_SERVER_POLICY_PERMIT_WITHOUT_STREAM", bEnvValue)
}

func TestLoadKeepaliveParamsDefault(t *testing.T) {
	kcp := LoadKeepaliveClientParams()
	assert.Equal(t, kcp, keepalive.ClientParameters{})

	ksp := LoadKeepaliveServerParams()
	assert.Equal(t, ksp, keepalive.ServerParameters{})

	kep := LoadKeepaliveEnforcementPolicy()
	assert.Equal(t, kep, keepalive.EnforcementPolicy{})
}

func TestLoadKeepaliveParamsFromEnvs(t *testing.T) {
	setInitialKeepaliveClientEnvs(t, true)
	kcp := LoadKeepaliveClientParams()
	assert.Equal(t, kcp, initialKcp())

	setInitialKeepaliveServerEnvs(t, true)
	ksp := LoadKeepaliveServerParams()
	assert.Equal(t, ksp, initialKsp())

	setInitialKeepaliveEnforcementPolicyEnvs(t, true)
	kep := LoadKeepaliveEnforcementPolicy()
	assert.Equal(t, kep, initialKep())
}

func TestLoadKeepaliveParamsSingleton(t *testing.T) {
	setInitialKeepaliveClientEnvs(t, true)
	_ = LoadKeepaliveClientParams()
	setInitialKeepaliveClientEnvs(t, false)
	kcp := LoadKeepaliveClientParams()
	assert.Equal(t, kcp, initialKcp())

	setInitialKeepaliveServerEnvs(t, true)
	_ = LoadKeepaliveServerParams()
	setInitialKeepaliveServerEnvs(t, false)
	ksp := LoadKeepaliveServerParams()
	assert.Equal(t, ksp, initialKsp())

	setInitialKeepaliveEnforcementPolicyEnvs(t, true)
	_ = LoadKeepaliveEnforcementPolicy()
	setInitialKeepaliveEnforcementPolicyEnvs(t, false)
	kep := LoadKeepaliveEnforcementPolicy()
	assert.Equal(t, kep, initialKep())
}
