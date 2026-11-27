package local

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/contenthash"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/okteto"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	srctypes "github.com/moby/buildkit/source/types"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/cachedigest"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/patternmatcher"
	"github.com/moby/sys/user"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/time/rate"
)

type Opt struct {
	CacheAccessor cache.Accessor
}

func NewSource(opt Opt) (source.Source, error) {
	ls := &localSource{
		cm: opt.CacheAccessor,
	}
	return ls, nil
}

type localSource struct {
	cm cache.Accessor
}

func (ls *localSource) Schemes() []string {
	return []string{srctypes.LocalScheme}
}

func (ls *localSource) Identifier(scheme, ref string, attrs map[string]string, platform *pb.Platform) (source.Identifier, error) {
	id, err := NewLocalIdentifier(ref)
	if err != nil {
		return nil, err
	}

	for k, v := range attrs {
		switch k {
		case pb.AttrLocalSessionID:
			id.SessionID = v
			if p := strings.SplitN(v, ":", 2); len(p) == 2 {
				id.Name = p[0] + "-" + id.Name
				id.SessionID = p[1]
			}
		case pb.AttrIncludePatterns:
			var patterns []string
			if err := json.Unmarshal([]byte(v), &patterns); err != nil {
				return nil, err
			}
			id.IncludePatterns = patterns
		case pb.AttrExcludePatterns:
			var patterns []string
			if err := json.Unmarshal([]byte(v), &patterns); err != nil {
				return nil, err
			}
			id.ExcludePatterns = patterns
		case pb.AttrFollowPaths:
			var paths []string
			if err := json.Unmarshal([]byte(v), &paths); err != nil {
				return nil, err
			}
			id.FollowPaths = paths
		case pb.AttrSharedKeyHint:
			id.SharedKeyHint = v
		case pb.AttrLocalDiffer:
			switch v {
			case pb.AttrLocalDifferMetadata, "":
				id.Differ = fsutil.DiffMetadata
			case pb.AttrLocalDifferNone:
				id.Differ = fsutil.DiffNone
			}
		case pb.AttrMetadataTransfer:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid value for local.metadatatransfer %q", v)
			}
			id.MetadataOnly = b
		case pb.AttrMetadataTransferExclude:
			var exceptions []string
			if err := json.Unmarshal([]byte(v), &exceptions); err != nil {
				return nil, err
			}
			id.MetadataExceptions = exceptions
		}
	}

	return id, nil
}

func (ls *localSource) Resolve(ctx context.Context, id source.Identifier, sm *session.Manager, _ solver.Vertex) (source.SourceInstance, error) {
	localIdentifier, ok := id.(*LocalIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid local identifier %v", id)
	}

	return &localSourceHandler{
		src:         *localIdentifier,
		sm:          sm,
		localSource: ls,
	}, nil
}

type localSourceHandler struct {
	src LocalIdentifier
	sm  *session.Manager
	*localSource
}

func (ls *localSourceHandler) CacheKey(ctx context.Context, jobCtx solver.JobContext, index int) (string, string, solver.CacheOpts, bool, error) {
	sessionID := ls.src.SessionID

	if sessionID == "" {
		id := jobCtx.Session().SessionIterator().NextSession()
		if id == "" {
			return "", "", nil, false, errors.New("could not access local files without session")
		}
		sessionID = id
	}
	dt, err := json.Marshal(struct {
		SessionID          string
		IncludePatterns    []string
		ExcludePatterns    []string
		FollowPaths        []string
		MetadataTransfer   bool     `json:",omitempty"`
		MetadataExceptions []string `json:",omitempty"`
	}{
		SessionID:          sessionID,
		IncludePatterns:    ls.src.IncludePatterns,
		ExcludePatterns:    ls.src.ExcludePatterns,
		FollowPaths:        ls.src.FollowPaths,
		MetadataTransfer:   ls.src.MetadataOnly,
		MetadataExceptions: ls.src.MetadataExceptions,
	})
	if err != nil {
		return "", "", nil, false, err
	}
	dgst, err := cachedigest.FromBytes(dt, cachedigest.TypeJSON)
	if err != nil {
		return "", "", nil, false, err
	}
	return "session:" + ls.src.Name + ":" + dgst.String(), dgst.String(), nil, true, nil
}

func (ls *localSourceHandler) Snapshot(ctx context.Context, jobCtx solver.JobContext) (cache.ImmutableRef, error) {
	g := jobCtx.Session()
	sessionID := ls.src.SessionID
	if sessionID == "" {
		return ls.snapshotWithAnySession(ctx, jobCtx, g)
	}

	timeoutCtx, cancel := context.WithCancelCause(ctx)
	timeoutCtx, _ = context.WithTimeoutCause(timeoutCtx, 5*time.Second, errors.WithStack(context.DeadlineExceeded)) //nolint:govet
	defer func() { cancel(errors.WithStack(context.Canceled)) }()

	caller, err := ls.sm.Get(timeoutCtx, sessionID, false)
	if err != nil {
		return ls.snapshotWithAnySession(ctx, jobCtx, g)
	}

	ref, err := ls.snapshot(ctx, caller, ls.syncSource(jobCtx, sessionID))
	if err != nil {
		var serr filesync.InvalidSessionError
		if errors.As(err, &serr) {
			return ls.snapshotWithAnySession(ctx, jobCtx, g)
		}
		return nil, err
	}
	return ref, nil
}

func (ls *localSourceHandler) snapshotWithAnySession(ctx context.Context, jobCtx solver.JobContext, g session.Group) (cache.ImmutableRef, error) {
	var ref cache.ImmutableRef
	err := ls.sm.Any(ctx, g, func(ctx context.Context, sessionID string, c session.Caller) error {
		r, err := ls.snapshot(ctx, c, ls.syncSource(jobCtx, sessionID))
		if err != nil {
			return err
		}
		ref = r
		return nil
	})
	return ref, err
}

// syncSource (okteto) returns the source that the client of the session that
// sends the files named for them, see okteto.LocalSourceOptPrefix. It is
// empty if that client named none: the builds of other sessions that share
// the step don't count.
func (ls *localSourceHandler) syncSource(jobCtx solver.JobContext, sessionID string) string {
	// only the transfers that compare modification times can compare content instead
	if ls.src.Differ != fsutil.DiffMetadata || ls.src.MetadataOnly {
		return ""
	}
	return okteto.LocalSource(jobCtx, sessionID, ls.src.Name)
}

func (ls *localSourceHandler) snapshot(ctx context.Context, caller session.Caller, syncSource string) (out cache.ImmutableRef, retErr error) {
	metaSfx := ""
	if ls.src.MetadataOnly {
		metaSfx = ":metadata"
	}
	sharedKey := ls.src.Name + ":" + ls.src.SharedKeyHint + ":" + caller.SharedKey() + metaSfx // TODO: replace caller.SharedKey() with source based hint from client(absolute-path+nodeid)

	var mutable cache.MutableRef
	sis, err := searchSharedKey(ctx, ls.cm, sharedKey)
	if err != nil {
		return nil, err
	}
	if syncSource != "" {
		// okteto: prefer the copy that has the modification times of this source
		slices.SortStableFunc(sis, func(a, b cacheRefMetadata) int {
			return compareSyncSource(a.getSyncSource(), b.getSyncSource(), syncSource)
		})
	}
	for _, si := range sis {
		if m, err := ls.cm.GetMutable(ctx, si.ID()); err == nil {
			// okteto: only a copy that a sync left complete is reused, see
			// keyCompleteSync
			if !(cacheRefMetadata{m}).hasCompleteSync() {
				bklog.G(ctx).Warnf("not reusing ref %s for local: it is not the result of a sync that finished", m.ID())
				removeLocalRef(ctx, m)
				continue
			}
			bklog.G(ctx).Debugf("reusing ref for local: %s", m.ID())
			mutable = m
			break
		} else {
			bklog.G(ctx).Debugf("not reusing ref %s for local: %v", si.ID(), err)
		}
	}

	// okteto: the modification times of a reused copy are the ones of the source that
	// synced it last. For any other source the files are compared by content.
	contentDiff := mutable != nil && syncSource != "" && cacheRefMetadata{mutable}.getSyncSource() != syncSource

	if mutable == nil {
		m, err := ls.cm.New(ctx, nil, nil, cache.CachePolicyRetain, cache.WithRecordType(client.UsageRecordTypeLocalSource), cache.WithDescription(fmt.Sprintf("local source for %s", ls.src.Name)))
		if err != nil {
			return nil, err
		}
		mutable = m
		bklog.G(ctx).Debugf("new ref for local: %s", mutable.ID())
	}

	defer func() {
		if retErr != nil && mutable != nil {
			// on error remove the record as checksum update is in undefined state
			if err := mutable.SetCachePolicyDefault(); err != nil {
				bklog.G(ctx).Errorf("failed to reset mutable cachepolicy: %v", err)
			}
			contenthash.ClearCacheContext(mutable)
			go mutable.Release(context.WithoutCancel(ctx))
		}
	}()

	mount, err := mutable.Mount(ctx, false, nil)
	if err != nil {
		return nil, err
	}

	lm := snapshot.LocalMounter(mount)

	dest, err := lm.Mount()
	if err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()

	cc, err := contenthash.GetCacheContext(ctx, mutable)
	if err != nil {
		return nil, err
	}

	opt := filesync.FSSendRequestOpt{
		Name:            ls.src.Name,
		IncludePatterns: ls.src.IncludePatterns,
		ExcludePatterns: ls.src.ExcludePatterns,
		FollowPaths:     ls.src.FollowPaths,
		DestDir:         dest,
		CacheUpdater:    &cacheUpdater{cc, mount.IdentityMapping()},
		ProgressCb:      newProgressHandler(ctx, "transferring "+ls.src.Name+":"),
		Differ:          ls.src.Differ,
		MetadataOnly:    ls.src.MetadataOnly,
		ContentDiff:     contentDiff,
	}

	if opt.MetadataOnly && len(ls.src.MetadataExceptions) > 0 {
		matcher, err := patternmatcher.New(ls.src.MetadataExceptions)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		opt.MetadataOnlyFilter = func(p string, _ *fstypes.Stat) bool {
			v, err := matcher.MatchesOrParentMatches(p)
			return err == nil && v
		}
	}

	if idmap := mount.IdentityMapping(); idmap != nil {
		opt.Filter = func(p string, stat *fstypes.Stat) bool {
			uid, gid, err := idmap.ToHost(int(stat.Uid), int(stat.Gid))
			if err != nil {
				return false
			}
			stat.Uid = uint32(uid)
			stat.Gid = uint32(gid)
			return true
		}
	}

	// okteto: the transfer writes the files, and their content hashes are only
	// saved when it has finished. If the daemon dies in between, the saved
	// hashes may not describe the files, and a later sync only corrects the
	// hashes of the files that it writes. So the copy stops being complete
	// before the transfer starts.
	if err := (cacheRefMetadata{mutable}).setCompleteSync(false); err != nil {
		return nil, err
	}

	if err := filesync.FSSync(ctx, caller, opt); err != nil {
		return nil, err
	}

	if err := lm.Unmount(); err != nil {
		return nil, err
	}
	lm = nil

	if err := contenthash.SetCacheContext(ctx, mutable, cc); err != nil {
		return nil, err
	}

	// skip storing snapshot by the shared key if it already exists
	md := cacheRefMetadata{mutable}
	if md.getSharedKey() != sharedKey {
		if err := md.setSharedKey(sharedKey); err != nil {
			return nil, err
		}
		bklog.G(ctx).Debugf("saved %s as %s", mutable.ID(), sharedKey)
	}
	// okteto: remember which source the modification times of the copy belong
	// to. After a sync that named no source they belong to none: the record is
	// removed, so that the source that was recorded does not trust them.
	if md.getSyncSource() != syncSource {
		if err := md.setSyncSource(syncSource); err != nil {
			return nil, err
		}
	}
	// okteto: the content hashes, the shared key and the source of the files
	// that the transfer left are saved
	if err := md.setCompleteSync(true); err != nil {
		return nil, err
	}

	snap, err := mutable.Commit(ctx)
	if err != nil {
		return nil, err
	}

	mutable = nil // avoid deferred cleanup

	return snap, nil
}

func newProgressHandler(ctx context.Context, id string) func(int, bool) {
	limiter := rate.NewLimiter(rate.Every(100*time.Millisecond), 1)
	pw, _, _ := progress.NewFromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
		Action:  "transferring",
	}
	pw.Write(id, st)
	return func(s int, last bool) {
		if last || limiter.Allow() {
			st.Current = s
			if last {
				now := time.Now()
				st.Completed = &now
			}
			pw.Write(id, st)
			if last {
				pw.Close()
			}
		}
	}
}

type cacheUpdater struct {
	contenthash.CacheContext
	idmap *user.IdentityMapping
}

func (cu *cacheUpdater) MarkSupported(bool) {
}

func (cu *cacheUpdater) ContentHasher() fsutil.ContentHasher {
	return contenthash.NewFromStat
}

const (
	keySharedKey   = "local.sharedKey"
	sharedKeyIndex = keySharedKey + ":"

	// keySyncSource (okteto) is the source that synced a local source copy last
	keySyncSource = "okteto.local.syncSource"

	// keyCompleteSync (okteto) is set on a local source copy when a sync has
	// saved its content hashes and its source, and removed before the next
	// sync starts its transfer. A copy without it is not reused: a sync of it
	// did not finish, or it was only synced by daemons that do not record
	// this.
	keyCompleteSync = "okteto.local.completeSync"
)

func searchSharedKey(ctx context.Context, store cache.MetadataStore, k string) ([]cacheRefMetadata, error) {
	var results []cacheRefMetadata
	mds, err := store.Search(ctx, sharedKeyIndex+k, false)
	if err != nil {
		return nil, err
	}
	for _, md := range mds {
		results = append(results, cacheRefMetadata{md})
	}
	return results, nil
}

type cacheRefMetadata struct {
	cache.RefMetadata
}

func (md cacheRefMetadata) getSharedKey() string {
	return md.GetString(keySharedKey)
}

func (md cacheRefMetadata) setSharedKey(key string) error {
	return md.SetString(keySharedKey, key, sharedKeyIndex+key)
}

func (md cacheRefMetadata) getSyncSource() string {
	return md.GetString(keySyncSource)
}

func (md cacheRefMetadata) setSyncSource(source string) error {
	return md.SetString(keySyncSource, source, "")
}

func (md cacheRefMetadata) hasCompleteSync() bool {
	return md.GetString(keyCompleteSync) != ""
}

func (md cacheRefMetadata) setCompleteSync(complete bool) error {
	if md.hasCompleteSync() == complete {
		return nil
	}
	value := ""
	if complete {
		value = "true"
	}
	return md.SetString(keyCompleteSync, value, "")
}

// removeLocalRef (okteto) removes a local source copy that is not reused, as
// the snapshot does with the copy of a sync that fails.
func removeLocalRef(ctx context.Context, mutable cache.MutableRef) {
	if err := mutable.SetCachePolicyDefault(); err != nil {
		bklog.G(ctx).Errorf("failed to reset mutable cachepolicy: %v", err)
	}
	contenthash.ClearCacheContext(mutable)
	go mutable.Release(context.WithoutCancel(ctx))
}

// compareSyncSource orders the copies of a local source so that the ones that
// were last synced by source come first.
func compareSyncSource(a, b, source string) int {
	switch {
	case a == source && b != source:
		return -1
	case a != source && b == source:
		return 1
	default:
		return 0
	}
}
