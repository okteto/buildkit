package llbsolver

import (
	"testing"

	"github.com/moby/buildkit/okteto"
	"github.com/moby/buildkit/solver"
	"github.com/stretchr/testify/require"
)

// jobLocalSources returns the values that a job has for the sources of its
// local directories after setLocalSources.
func jobLocalSources(t *testing.T, opts map[string]string) []any {
	t.Helper()
	s := solver.NewSolver(solver.SolverOpt{})
	defer s.Close()
	j, err := s.NewJob("job0")
	require.NoError(t, err)
	defer j.Discard()

	setLocalSources(j, opts)

	var values []any
	require.NoError(t, j.EachValue(t.Context(), okteto.LocalSourcesJobKey, func(v any) error {
		values = append(values, v)
		return nil
	}))
	return values
}

func TestSetLocalSources(t *testing.T) {
	t.Setenv(okteto.LocalContentDiffEnvVar, "")
	values := jobLocalSources(t, map[string]string{
		"filename":                    "Dockerfile",
		"okteto-local-source:context": "a",
	})
	require.Equal(t, []any{map[string]string{"context": "a"}}, values)
}

func TestSetLocalSourcesWithoutTheOption(t *testing.T) {
	t.Setenv(okteto.LocalContentDiffEnvVar, "")
	values := jobLocalSources(t, map[string]string{
		"filename":                "Dockerfile",
		"local-sessionid:context": "a",
	})
	require.Empty(t, values)
}

func TestSetLocalSourcesWhenContentDiffIsDisabled(t *testing.T) {
	t.Setenv(okteto.LocalContentDiffEnvVar, "false")
	values := jobLocalSources(t, map[string]string{
		"okteto-local-source:context": "a",
	})
	require.Empty(t, values)
}
