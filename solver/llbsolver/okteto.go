package llbsolver

import (
	"github.com/moby/buildkit/okteto"
	"github.com/moby/buildkit/solver"
)

// setLocalSources (okteto) keeps in the job the source of the local
// directories that the client named in the options of the build, see
// okteto.LocalSourceOptPrefix. The local source reads them when it syncs the
// files. Nothing is kept for a build without the option, or when
// OKTETO_LOCAL_CONTENT_DIFF is false.
func setLocalSources(j *solver.Job, opts map[string]string) {
	sources := okteto.LocalSourcesFromOpts(opts)
	if sources == nil || !okteto.LocalContentDiffEnabled() {
		return
	}
	j.SetValue(okteto.LocalSourcesJobKey, sources)
}
