package provenance

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilterArgsLeavesOutLocalSources(t *testing.T) {
	t.Parallel()

	out := FilterArgs(map[string]string{
		"filename":                    "Dockerfile",
		"okteto-local-source:context": "a",
	})

	require.Equal(t, map[string]string{"filename": "Dockerfile"}, out)
}
