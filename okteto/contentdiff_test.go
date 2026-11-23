package okteto

import (
	"context"
	"errors"
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

// testJobValues has the values that each job of a build step has for
// LocalSourcesJobKey.
type testJobValues struct {
	values []any
	err    error
}

func (j testJobValues) EachValue(_ context.Context, key string, fn func(any) error) error {
	if key != LocalSourcesJobKey {
		return nil
	}
	for _, v := range j.values {
		if err := fn(v); err != nil {
			return err
		}
	}
	return j.err
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
		{name: "job context without values", jobCtx: struct{}{}, expected: ""},
		{name: "no job named a source", jobCtx: testJobValues{}, expected: ""},
		{name: "one job", jobCtx: testJobValues{values: []any{a}}, expected: "a"},
		{name: "source of another local directory", jobCtx: testJobValues{values: []any{other}}, expected: ""},
		{name: "jobs with the same source", jobCtx: testJobValues{values: []any{a, a}}, expected: "a"},
		{name: "one of the jobs named no source", jobCtx: testJobValues{values: []any{other, a}}, expected: "a"},
		{name: "jobs with different sources", jobCtx: testJobValues{values: []any{a, b}}, expected: ""},
		{name: "value of another type", jobCtx: testJobValues{values: []any{"a"}}, expected: ""},
		{name: "values can't be read", jobCtx: testJobValues{values: []any{a}, err: errors.New("failed")}, expected: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, LocalSource(t.Context(), tt.jobCtx, "context"))
		})
	}
}
