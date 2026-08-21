package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/Romero-jace/repo-metrics/internal/config"
	"github.com/Romero-jace/repo-metrics/internal/store"
)

// nameList collects a flag that may be given more than once.
//
// The stdlib has no slice flag and this package has no CLI framework to borrow
// one from, so it is the eight lines below. Repeating a flag rather than
// accepting a comma separated value because a repo name or a step name is
// operator text: a comma inside one would silently split it into two names that
// match nothing, and this tool does not do silently.
type nameList []string

func (n *nameList) String() string { return strings.Join(*n, ", ") }

// Set appends. An empty value is refused rather than recorded, since a name that
// matches nothing would otherwise narrow a run to nothing and exit 0.
func (n *nameList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("needs a name")
	}
	*n = append(*n, v)
	return nil
}

// has reports membership, which is what narrowing needs once a flag can repeat.
func (n nameList) has(name string) bool {
	for _, v := range n {
		if v == name {
			return true
		}
	}
	return false
}

// collectPool runs each repo's collection, up to jobs at a time, and returns the
// names of the ones that failed.
//
// Concurrency is over repos rather than over signals inside a repo. What a
// collection actually spends is subprocess time, minutes of it per repo on a
// large test suite, and the store handle is capped at one connection so writes
// serialize whatever this does. Fanning out inside a repo would also mean two
// steps of one repo racing for the same working tree.
//
// Output is the part that needs care. The streaming contract, one line as a repo
// starts and one as it lands, cannot survive several repos writing at once: the
// lines interleave and the table shreds. So above one job each repo's output is
// buffered and flushed as a block when it finishes, in completion order, which
// is the honest order for a parallel run. At one job nothing is buffered and the
// output is byte for byte what it has always been, which is why the default is
// one and not the machine's core count.
func collectPool(
	ctx context.Context,
	st *store.Store,
	repos []config.Repo,
	jobs int,
	stdout, stderr io.Writer,
) ([]string, error) {
	if jobs < 1 {
		jobs = 1
	}
	if jobs > len(repos) {
		jobs = len(repos)
	}

	var (
		mu      sync.Mutex
		failed  []string
		skipped int
	)

	work := make(chan int)
	var wg sync.WaitGroup
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				repo := repos[i]

				// Checked per repo rather than only before the pool starts, so a
				// signal part way through a long run stops the repos that have
				// not begun instead of queueing them behind it. Whatever is
				// already collected has been written.
				if ctx.Err() != nil {
					mu.Lock()
					skipped++
					mu.Unlock()
					continue
				}

				out, errOut := stdout, stderr
				var outBuf, errBuf bytes.Buffer
				if jobs > 1 {
					out, errOut = &outBuf, &errBuf
				}

				printStarting(out, repo.Name, i+1, len(repos))
				err := collectOne(ctx, st, repo, out, errOut)

				mu.Lock()
				if jobs > 1 {
					// One lock for both streams and for the whole of each, so a
					// repo's diagnostics cannot land between another repo's two
					// lines. The error is discarded for the same reason every
					// other write to these writers discards it: a failed write to
					// the caller's own writer is not actionable.
					_, _ = io.Copy(stdout, &outBuf)
					_, _ = io.Copy(stderr, &errBuf)
				}
				if err != nil {
					failed = append(failed, repo.Name)
				}
				mu.Unlock()
			}
		}()
	}

	for i := range repos {
		work <- i
	}
	close(work)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintf(stderr,
			"stopping early, collection was canceled with %d of %d repos not started\n",
			skipped, len(repos))
		return failed, err
	}
	return failed, nil
}

// narrowSignals keeps only the steps named, and says what that costs.
//
// A repo whose steps are all filtered out is dropped rather than collected with
// nothing, because a repo with no steps has nothing to measure and would store a
// snapshot that failed for a reason the operator created.
//
// A name matching no step in any repo is an error. Quietly collecting nothing
// for a typo is the silent wrong answer this tool exists to refuse, and it is
// worse here than elsewhere: the run exits 0 having written narrower snapshots
// than anyone intended, and snapshots cannot be re-collected.
func narrowSignals(repos []config.Repo, only nameList, stderr io.Writer) ([]config.Repo, error) {
	if len(only) == 0 {
		return repos, nil
	}

	available := map[string]bool{}
	for _, repo := range repos {
		for _, sig := range repo.Signals {
			available[sig.Name] = true
		}
	}
	var unknown []string
	for _, name := range only {
		if !available[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no step named %s in the selected repos, which have %s",
			strings.Join(unknown, ", "), strings.Join(sortedSet(available), ", "))
	}

	out := make([]config.Repo, 0, len(repos))
	for _, repo := range repos {
		kept := make([]config.Signal, 0, len(repo.Signals))
		for _, sig := range repo.Signals {
			if only.has(sig.Name) {
				kept = append(kept, sig)
			}
		}
		if len(kept) == 0 {
			continue
		}
		repo.Signals = kept
		out = append(out, repo)
	}

	// Said out loud on every narrowed run, because the cost is not obvious and
	// is not recoverable. A snapshot written by this run records the steps that
	// ran and nothing about the ones that did not, and it comes back ok rather
	// than partial, since nothing failed. It is then a legitimate baseline
	// forever, and next week's report says "no baseline yet" for every signal
	// skipped here. Re-collecting is not an option: these numbers measured a
	// working tree at a commit that has since moved on.
	_, _ = fmt.Fprintf(stderr,
		"collecting only %s, so the snapshots this run writes are narrower than the config "+
			"and will read as unmeasured for every other signal from now on\n",
		strings.Join(only, ", "))
	return out, nil
}

// sortedSet renders a set for an error message, in a stable order so the same
// mistake reads the same way twice.
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// oneName adapts a single-valued --repo flag to the list selectRepos takes.
//
// report and history keep a single-valued flag on purpose. report publishes the
// narrowed-to name as one string in its scope object, and history charts one
// repo by construction, so neither has a reading for two.
func oneName(only string) nameList {
	if only == "" {
		return nil
	}
	return nameList{only}
}
