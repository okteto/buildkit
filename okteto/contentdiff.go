package okteto

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/buildkit/util/bklog"
)

const (
	// LocalContentDiffEnvVar disables comparing the files of a local source by
	// content. The comparison is available unless the variable is false, and
	// it is only used by the builds that name the source of their files.
	LocalContentDiffEnvVar = "OKTETO_LOCAL_CONTENT_DIFF"

	// LocalSourceOptPrefix is the prefix of the build option that names the
	// source of the files of a local directory:
	//
	//	okteto-local-source:<local name>=<source>
	//
	// source identifies the directory that the files are sent from, for
	// example a hash of the machine and the absolute path. The daemon records
	// the source that synced a saved copy last. Modification times are only
	// comparable with that source, so when a different one reuses the copy the
	// files are compared by content instead.
	//
	// A build without the option behaves as upstream buildkit.
	LocalSourceOptPrefix = "okteto-local-source:"

	// LocalSourcesJobKey is the key of the job value that keeps the sources
	// named by the client of a build, as a map from local name to source.
	LocalSourcesJobKey = "okteto.localsources"
)

var localContentDiffWarning sync.Once

// LocalContentDiffEnabled reports whether the daemon compares local sources
// by content for the builds that ask for it. It is enabled unless
// OKTETO_LOCAL_CONTENT_DIFF is false.
func LocalContentDiffEnabled() bool {
	v := os.Getenv(LocalContentDiffEnvVar)
	if v == "" {
		return true
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil {
		localContentDiffWarning.Do(func() {
			bklog.L.Infof("couldn't parse %s value: %s", LocalContentDiffEnvVar, err.Error())
		})
		return true
	}
	return enabled
}

// LocalSourcesFromOpts returns the source of every local directory named in
// the options of a build, see LocalSourceOptPrefix. It returns nil when the
// options name none.
func LocalSourcesFromOpts(opts map[string]string) map[string]string {
	var sources map[string]string
	for k, v := range opts {
		name, ok := strings.CutPrefix(k, LocalSourceOptPrefix)
		if !ok || name == "" || v == "" {
			continue
		}
		if sources == nil {
			sources = map[string]string{}
		}
		sources[name] = v
	}
	return sources
}

// jobValues is implemented by the job context that a build step runs with.
type jobValues interface {
	EachValue(ctx context.Context, key string, fn func(any) error) error
}

// LocalSource returns the source that the client of the build named for the
// local directory name, or "" if it named none.
//
// jobCtx is the job context of the build step. A step can belong to several
// builds: if they don't agree on the source there is none, and the files are
// compared by modification time as usual.
func LocalSource(ctx context.Context, jobCtx any, name string) string {
	jv, ok := jobCtx.(jobValues)
	if !ok {
		return ""
	}
	source, conflict := "", false
	err := jv.EachValue(ctx, LocalSourcesJobKey, func(v any) error {
		sources, ok := v.(map[string]string)
		if !ok {
			return nil
		}
		s := sources[name]
		switch {
		case s == "" || s == source:
		case source == "":
			source = s
		default:
			conflict = true
		}
		return nil
	})
	if err != nil || conflict {
		return ""
	}
	return source
}
