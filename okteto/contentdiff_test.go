package okteto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocalContentDiffEnabled(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{name: "not set", value: "", expected: true},
		{name: "true", value: "true", expected: true},
		{name: "one", value: "1", expected: true},
		{name: "false", value: "false", expected: false},
		{name: "zero", value: "0", expected: false},
		{name: "not a boolean", value: "disabled", expected: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(LocalContentDiffEnvVar, tt.value)
			assert.Equal(t, tt.expected, LocalContentDiffEnabled())
		})
	}
}

func TestLocalSourcesFromOpts(t *testing.T) {
	tests := []struct {
		name     string
		opts     map[string]string
		expected map[string]string
	}{
		{name: "no options", opts: nil, expected: nil},
		{
			name:     "options of other features",
			opts:     map[string]string{"filename": "Dockerfile", "local-sessionid:context": "id", "build-arg:okteto-local-source:context": "a"},
			expected: nil,
		},
		{
			name:     "source of the context",
			opts:     map[string]string{"filename": "Dockerfile", "okteto-local-source:context": "a"},
			expected: map[string]string{"context": "a"},
		},
		{
			name:     "sources of several local directories",
			opts:     map[string]string{"okteto-local-source:context": "a", "okteto-local-source:other": "b"},
			expected: map[string]string{"context": "a", "other": "b"},
		},
		{name: "without source", opts: map[string]string{"okteto-local-source:context": ""}, expected: nil},
		{name: "without local name", opts: map[string]string{"okteto-local-source:": "a"}, expected: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, LocalSourcesFromOpts(tt.opts))
		})
	}
}

// testJob is one of the builds of a build step: the session of its client and
// its value for LocalSourcesJobKey, nil if the client did not send the option.
type testJob struct {
	sessionID string
	value     any
}

// testJobs is the job context of a build step that belongs to several builds.
type testJobs []testJob

func (j testJobs) EachJobValue(key string, fn func(sessionID string, value any)) {
	for _, job := range j {
		var value any
		if key == LocalSourcesJobKey {
			value = job.value
		}
		fn(job.sessionID, value)
	}
}

func TestLocalSource(t *testing.T) {
	a := map[string]string{"context": "a"}
	b := map[string]string{"context": "b"}
	other := map[string]string{"other": "c"}

	tests := []struct {
		name     string
		jobCtx   any
		expected string
	}{
		{name: "no job context", jobCtx: nil, expected: ""},
		{name: "job context without values", jobCtx: struct{}{}, expected: ""},
		{name: "no builds", jobCtx: testJobs{}, expected: ""},
		{name: "one build", jobCtx: testJobs{{"session", a}}, expected: "a"},
		{name: "build without the option", jobCtx: testJobs{{"session", nil}}, expected: ""},
		{name: "source of another local directory", jobCtx: testJobs{{"session", other}}, expected: ""},
		{name: "value of another type", jobCtx: testJobs{{"session", "a"}}, expected: ""},
		{name: "builds of the session with the same source", jobCtx: testJobs{{"session", a}, {"session", a}}, expected: "a"},
		{name: "builds of the session with different sources", jobCtx: testJobs{{"session", a}, {"session", b}}, expected: ""},
		{name: "a build of the session without the option, first", jobCtx: testJobs{{"session", nil}, {"session", a}}, expected: ""},
		{name: "a build of the session without the option, last", jobCtx: testJobs{{"session", a}, {"session", nil}}, expected: ""},
		{name: "a build of the session with the source of another local directory", jobCtx: testJobs{{"session", other}, {"session", a}}, expected: ""},
		// the builds of other clients don't count, whatever they sent
		{name: "only another session named a source", jobCtx: testJobs{{"session", nil}, {"another", a}}, expected: ""},
		{name: "only another session named a source, first", jobCtx: testJobs{{"another", a}, {"session", nil}}, expected: ""},
		{name: "another session without the option", jobCtx: testJobs{{"session", a}, {"another", nil}}, expected: "a"},
		{name: "another session with another source", jobCtx: testJobs{{"another", b}, {"session", a}}, expected: "a"},
		{name: "the session has no build in the step", jobCtx: testJobs{{"another", a}}, expected: ""},
		{name: "build without session", jobCtx: testJobs{{"", a}}, expected: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, LocalSource(tt.jobCtx, "session", "context"))
		})
	}
}

func TestLocalSourceWithoutSession(t *testing.T) {
	assert.Empty(t, LocalSource(testJobs{{"", map[string]string{"context": "a"}}}, "", "context"))
}
