package solver

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/moby/buildkit/okteto"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// jobValuesVertex is a vertex that reads a value of its jobs while it runs,
// through the job context that the solver gives to every build step.
type jobValuesVertex struct {
	*vertex
	key string
	// ready is called before the values are read
	ready func(ctx context.Context) error

	mu sync.Mutex
	// values has the value of each job by its session
	values map[string]any
	// sources has the result of okteto.LocalSource for each session
	sources map[string]string
}

func (v *jobValuesVertex) Sys() any {
	return v
}

func (v *jobValuesVertex) Exec(ctx context.Context, jobCtx JobContext, inputs []Result) ([]Result, error) {
	if v.ready != nil {
		if err := v.ready(ctx); err != nil {
			return nil, err
		}
	}
	values, sources := map[string]any{}, map[string]string{}
	jv, ok := jobCtx.(interface {
		EachJobValue(key string, fn func(sessionID string, value any))
	})
	if ok {
		jv.EachJobValue(v.key, func(sessionID string, value any) {
			values[sessionID] = value
		})
	}
	for sessionID := range values {
		sources[sessionID] = okteto.LocalSource(jobCtx, sessionID, "context")
	}
	v.mu.Lock()
	v.values, v.sources = values, sources
	v.mu.Unlock()
	return v.vertex.Exec(ctx, jobCtx, inputs)
}

func (v *jobValuesVertex) result() (map[string]any, map[string]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return maps.Clone(v.values), maps.Clone(v.sources)
}

func newJobValuesVertex(key string) *jobValuesVertex {
	return &jobValuesVertex{
		vertex: vtx(vtxOpt{name: "v0", value: "result0"}),
		key:    key,
	}
}

// buildJobValuesVertex builds a vertex in a job that has value for key, if it
// is not nil, and returns the values that the vertex read.
func buildJobValuesVertex(t *testing.T, key string, value any) map[string]any {
	t.Helper()
	s := NewSolver(SolverOpt{
		ResolveOpFunc: testOpResolver,
	})
	defer s.Close()

	j, err := s.NewJob("job0")
	require.NoError(t, err)
	defer j.Discard()
	j.SessionID = "session0"
	if value != nil {
		j.SetValue(key, value)
	}

	v := newJobValuesVertex("key")
	res, err := j.Build(t.Context(), Edge{Vertex: v})
	require.NoError(t, err)
	require.Equal(t, "result0", unwrap(res))
	values, _ := v.result()
	return values
}

func TestStateEachJobValue(t *testing.T) {
	t.Parallel()
	require.Equal(t, map[string]any{"session0": "value"}, buildJobValuesVertex(t, "key", "value"))
}

func TestStateEachJobValueOfAnotherKey(t *testing.T) {
	t.Parallel()
	require.Equal(t, map[string]any{"session0": nil}, buildJobValuesVertex(t, "another", "value"))
}

// A job without the value is not left out.
func TestStateEachJobValueWithoutValue(t *testing.T) {
	t.Parallel()
	require.Equal(t, map[string]any{"session0": nil}, buildJobValuesVertex(t, "key", nil))
}

// waitForJobs returns a function that waits until n jobs are building the
// vertex with the digest of v.
func waitForJobs(s *Solver, v Vertex, n int) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		for {
			s.mu.RLock()
			st := s.actives[v.Digest()]
			s.mu.RUnlock()
			jobs := 0
			if st != nil {
				st.mu.RLock()
				jobs = len(st.jobs)
				st.mu.RUnlock()
			}
			if jobs >= n {
				return nil
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(time.Millisecond):
			}
		}
	}
}

// buildSharedJobValuesVertex runs two builds of different clients at the same
// time. Both have the same build step, so the solver runs it once for the two
// of them. sources0 and sources1 are the options of each client, nil for a
// client that sent none. It returns what the step read: the value of each
// build and the source of each session.
func buildSharedJobValuesVertex(t *testing.T, sources0, sources1 map[string]string) (map[string]any, map[string]string) {
	t.Helper()
	ctx := t.Context()
	s := NewSolver(SolverOpt{
		ResolveOpFunc: testOpResolver,
	})
	defer s.Close()

	shared := newJobValuesVertex(okteto.LocalSourcesJobKey)
	shared.ready = waitForJobs(s, shared, 2)

	eg, ctx := errgroup.WithContext(ctx)
	for i, sources := range []map[string]string{sources0, sources1} {
		j, err := s.NewJob("job" + string(rune('0'+i)))
		require.NoError(t, err)
		defer j.Discard()
		j.SessionID = "session" + string(rune('0'+i))
		if sources != nil {
			j.SetValue(okteto.LocalSourcesJobKey, sources)
		}
		eg.Go(func() error {
			// the same vertex in both jobs: one state, one execution
			_, err := j.Build(ctx, Edge{Vertex: shared})
			return err
		})
	}
	require.NoError(t, eg.Wait())
	return shared.result()
}

// A build that sent the option and one that did not, of different clients,
// share a step: the client that did not send it has no source, so it is never
// asked for digests, whichever session the step takes the files from.
func TestStateLocalSourceOfSharedStepWithOrdinaryBuild(t *testing.T) {
	t.Parallel()
	optedIn := map[string]string{"context": "a"}

	values, sources := buildSharedJobValuesVertex(t, optedIn, nil)

	require.Equal(t, map[string]any{"session0": optedIn, "session1": nil}, values)
	require.Equal(t, map[string]string{"session0": "a", "session1": ""}, sources)
}

// Each client gets its own source when both sent the option.
func TestStateLocalSourceOfSharedStepWithTwoSources(t *testing.T) {
	t.Parallel()

	_, sources := buildSharedJobValuesVertex(t, map[string]string{"context": "a"}, map[string]string{"context": "b"})

	require.Equal(t, map[string]string{"session0": "a", "session1": "b"}, sources)
}
