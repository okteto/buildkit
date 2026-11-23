package okteto

import (
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
	EachJobValue(key string, fn func(sessionID string, value any))
}

// LocalSource returns the source that the client of the session sessionID
// named for the local directory name, or "" if there is none.
//
// jobCtx is the job context of the build step and sessionID the session that
// the files are requested from. A step can belong to several builds, of that
// client and of others. Only the builds of that session count, because its
// client is the one that is asked for the digests: a build of another session
// can't opt it in. Every one of them has to name the same source. If one of
// them names none or a different one, or if the session has no build in the
// step, there is no source and the files are compared by modification time as
// usual.
func LocalSource(jobCtx any, sessionID, name string) string {
	jv, ok := jobCtx.(jobValues)
	if !ok || sessionID == "" {
		return ""
	}
	source, first, agreed := "", true, true
	jv.EachJobValue(LocalSourcesJobKey, func(id string, v any) {
		if id != sessionID {
			return
		}
		// a build without the option, or with a value of another type, has
		// no source: it does not agree with one that has it
		sources, _ := v.(map[string]string)
		s := sources[name]
		if !first && s != source {
			agreed = false
		}
		source, first = s, false
	})
	if !agreed {
		return ""
	}
	return source
}
