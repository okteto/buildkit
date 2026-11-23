package solver

import "context"

// EachValue (okteto) calls fn with the value that each of the jobs building
// the vertex has for key. It lets a build step read a value of its build, as
// Builder.EachValue does for a frontend, without changing JobContext.
func (s *state) EachValue(ctx context.Context, key string, fn func(any) error) error {
	s.mu.RLock()
	jobs := make([]*Job, 0, len(s.jobs))
	for j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.RUnlock()

	for _, j := range jobs {
		if err := j.EachValue(ctx, key, fn); err != nil {
			return err
		}
	}
	return nil
}
