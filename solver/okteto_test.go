package solver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// jobValuesVertex is a vertex that reads a value of its jobs while it runs,
// through the job context that the solver gives to every build step.
type jobValuesVertex struct {
	*vertex
	key    string
	values []any
}

func (v *jobValuesVertex) Sys() any {
	return v
}

func (v *jobValuesVertex) Exec(ctx context.Context, jobCtx JobContext, inputs []Result) ([]Result, error) {
	jv, ok := jobCtx.(interface {
		EachValue(ctx context.Context, key string, fn func(any) error) error
	})
	if ok {
		if err := jv.EachValue(ctx, v.key, func(value any) error {
			v.values = append(v.values, value)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return v.vertex.Exec(ctx, jobCtx, inputs)
}

func buildJobValuesVertex(t *testing.T, key string, value any) []any {
	t.Helper()
	s := NewSolver(SolverOpt{
		ResolveOpFunc: testOpResolver,
	})
	defer s.Close()

	j, err := s.NewJob("job0")
	require.NoError(t, err)
	defer j.Discard()
	if value != nil {
		j.SetValue(key, value)
	}

	v := &jobValuesVertex{
		vertex: vtx(vtxOpt{name: "v0", value: "result0"}),
		key:    "key",
	}
	res, err := j.Build(t.Context(), Edge{Vertex: v})
	require.NoError(t, err)
	require.Equal(t, "result0", unwrap(res))
	return v.values
}

func TestStateEachValue(t *testing.T) {
	t.Parallel()
	require.Equal(t, []any{"value"}, buildJobValuesVertex(t, "key", "value"))
}

func TestStateEachValueOfAnotherKey(t *testing.T) {
	t.Parallel()
	require.Empty(t, buildJobValuesVertex(t, "another", "value"))
}

func TestStateEachValueWithoutValue(t *testing.T) {
	t.Parallel()
	require.Empty(t, buildJobValuesVertex(t, "key", nil))
}
