package solver

// EachJobValue (okteto) calls fn once for each of the jobs building the
// vertex, with the session of the job and the value that it has for key.
// value is nil for a job without one: unlike EachValue, such a job is not
// left out, so a build step can tell that one of its builds did not set it.
func (s *state) EachJobValue(key string, fn func(sessionID string, value any)) {
	s.mu.RLock()
	jobs := make([]*Job, 0, len(s.jobs))
	for j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.RUnlock()

	for _, j := range jobs {
		value, _ := j.values.Load(key)
		fn(j.SessionID, value)
	}
}
