package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Romero-jace/repo-metrics/internal/store"
)

// minSHAPrefix is how much of a commit sha --against has to carry before it is
// read as one.
//
// Seven because that is what git itself abbreviates to, and because a shorter
// prefix would swallow every snapshot id: ids are small integers, so anything
// under seven digits is far more likely to be one of those. The two forms are
// still checked against each other below rather than being told apart by length
// alone.
const minSHAPrefix = 7

// looksLikeSHA reports whether ref could be an abbreviated commit sha.
func looksLikeSHA(ref string) bool {
	if len(ref) < minSHAPrefix || len(ref) > 40 {
		return false
	}
	return strings.IndexFunc(ref, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdefABCDEF", r)
	}) < 0
}

// looksLikeID reports whether ref could be a snapshot id.
func looksLikeID(ref string) bool {
	if ref == "" {
		return false
	}
	return strings.IndexFunc(ref, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// resolveAgainst turns an --against value into one of this repo's snapshots.
//
// Both forms are tried rather than one being picked by shape, and a value that
// resolves as both is an error instead of a guess. A seven digit run is a valid
// sha prefix and a valid id at once, and while an id that long is not realistic
// today, "not realistic today" is how a silent wrong answer gets built. Picking
// one would compare against a snapshot the caller did not name, which is the
// failure this whole tool is arranged to refuse, arriving through a flag.
//
// Scoped to one repo throughout. A sha belongs to a repo, and an id from another
// repo's series names unrelated code whose numbers are not comparable.
func resolveAgainst(
	ctx context.Context, st *store.Store, repo store.Repo, ref string,
) (*store.Snapshot, error) {
	var bySHA []store.Snapshot
	if looksLikeSHA(ref) {
		var err error
		if bySHA, err = st.SnapshotsByGitSHAPrefix(ctx, repo.ID, ref); err != nil {
			return nil, err
		}
		if len(bySHA) > 1 {
			return nil, fmt.Errorf(
				"--against %q matches %d snapshots of %s: %s. Give more of the sha",
				ref, len(bySHA), repo.Name, describeSnapshots(bySHA))
		}
	}

	var byID *store.Snapshot
	if looksLikeID(ref) {
		id, err := strconv.ParseInt(ref, 10, 64)
		if err != nil {
			// Only reachable for a run of digits too long for an int64, which is
			// not an id anything ever wrote. Treated as no match rather than as a
			// failure, so the sha branch above still gets to answer.
			byID = nil
		} else if byID, err = st.SnapshotByID(ctx, repo.ID, id); err != nil {
			return nil, err
		}
	}

	switch {
	case len(bySHA) == 1 && byID != nil && bySHA[0].ID != byID.ID:
		return nil, fmt.Errorf(
			"--against %q is both snapshot id %d and the commit %s in %s. "+
				"Name the one you mean by giving more of the sha, or by a different snapshot id",
			ref, byID.ID, bySHA[0].GitSHA, repo.Name)
	case len(bySHA) == 1:
		return &bySHA[0], nil
	case byID != nil:
		return byID, nil
	case !looksLikeSHA(ref) && !looksLikeID(ref):
		return nil, fmt.Errorf(
			"--against %q is neither a snapshot id nor a commit sha of at least %d hex characters",
			ref, minSHAPrefix)
	default:
		return nil, fmt.Errorf("no snapshot of %s matches --against %q", repo.Name, ref)
	}
}

// usableAsBaseline rejects the two snapshots that cannot serve as one, with the
// reason rather than with a bare refusal.
//
// A failed snapshot is fetched by resolveAgainst rather than filtered out, so
// that naming one gets this sentence instead of "no snapshot matches". It stored
// no metrics, so comparing against it reports the head's whole coverage as this
// run's gain, which is a cliff nobody climbed.
func usableAsBaseline(base, head *store.Snapshot, repoName, ref string) error {
	if base.Status == store.StatusFailed {
		return fmt.Errorf(
			"--against %q names a snapshot of %s whose collection failed, so it stored no numbers to compare with",
			ref, repoName)
	}
	if head != nil && base.ID == head.ID {
		return fmt.Errorf(
			"--against %q names the newest snapshot of %s, which is the one being reported on. "+
				"Every delta against it is zero by construction",
			ref, repoName)
	}
	return nil
}

// describeSnapshots renders an ambiguous match so the caller can pick.
func describeSnapshots(snaps []store.Snapshot) string {
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, fmt.Sprintf("id %d at %s", s.ID, s.CollectedAt.UTC().Format("2006-01-02 15:04 UTC")))
	}
	return strings.Join(out, ", ")
}
