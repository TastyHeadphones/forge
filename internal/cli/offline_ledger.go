package cli

// `forge env deploy <env> --offline-ledger` — the break-glass for a hosted
// ledger whose control plane is down.
//
// THE GAP THIS CLOSES. An env that declares forge.ControlPlane keeps its
// release ledger there. With the control plane unreachable, the deploy cannot
// read what the env runs; FORGE_LEDGER=machine is no way out (it hits the
// unimported-ledger refusal and holds none of the live history), and the only
// remaining move was a fresh cut that rebuilds every image, unrehearsed.
//
// WHAT IT DOES. It deploys the env's CURRENT release, by the digests the
// control plane last reported for it (hosted_cache.go keeps that copy fresh on
// every successful ledger read or write), with no build and no contact with
// the control plane. The promotion record is queued locally and replayed on the
// next successful contact.
//
// WHAT IT DELIBERATELY DOES NOT DO.
//   - It never deploys anything but the cached current release. Moving an env
//     to another release is a ledger decision, and the ledger is away.
//   - It refuses an env whose control plane converges part of it (a hosted
//     tier, or a hub-converged cluster): there the control plane IS the
//     applier, and nothing local can stand in for it.
//   - It never runs unless asked. Without the flag the unimported-ledger
//     refusal and every hosted read behave exactly as before, so a stale cache
//     can never change what an ordinary deploy ships.
//
// BECAUSE THE CACHE MAY BE STALE, the deploy prints its age and what it will
// replay, and the replay is a compare-and-set against the promotion the cache
// said was current: if the control plane moved on while this machine was
// offline, the replay is refused (and kept queued) instead of overwriting it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/internal/ledgerfile"
	"github.com/reliant-labs/forge/pkg/release"
)

type offlineLedgerCtxKey struct{}

// withOfflineLedger marks ctx as running under --offline-ledger. A context
// value because the ledger is selected deep inside the deploy (a dozen call
// sites take ctx and none takes a flag), and a flag threaded to each would be
// forgotten by exactly the one that then reaches the dead control plane.
func withOfflineLedger(ctx context.Context) context.Context {
	return context.WithValue(ctx, offlineLedgerCtxKey{}, true)
}

func offlineLedgerRequested(ctx context.Context) bool {
	v, _ := ctx.Value(offlineLedgerCtxKey{}).(bool)
	return v
}

// staleOfflineCache is the age past which the banner stops being a note and
// becomes a warning to check what the env really runs.
const staleOfflineCache = 7 * 24 * time.Hour

// offlineBindings is the bindingStore under --offline-ledger: the cached
// current promotion, and a queue instead of a write.
type offlineBindings struct {
	store    *ledgerfile.Store
	snap     ledgerfile.HostedSnapshot
	endpoint string
	now      func() time.Time

	mu      sync.Mutex
	lastKey string // queue key of the record this process queued
}

// openOfflineLedger builds the offline ledger for env from its cache. The
// caller has already checked that the env's shape is one an offline deploy can
// serve.
func openOfflineLedger(projectDir, env, endpoint string, base envLedger) (envLedger, error) {
	store, err := openMachineLedger(projectDir)
	if err != nil {
		return envLedger{}, err
	}
	snap, err := store.HostedSnapshot(env)
	if err != nil {
		return envLedger{}, fmt.Errorf("read the offline copy of %s's ledger: %w", env, err)
	}
	if snap == nil {
		return envLedger{}, fmt.Errorf("--offline-ledger: this machine has no offline copy of %s's ledger from %s.\n"+
			"  forge writes it whenever it reads or writes that ledger while it is reachable, so it exists only on a machine\n"+
			"  that has run a ledger command for %s before the outage. Once %s answers again, run\n"+
			"  `forge env status %s` (and rehearse this flag) so the next outage has one",
			env, endpoint, env, endpoint, env)
	}
	if snap.Endpoint != endpoint {
		return envLedger{}, fmt.Errorf("--offline-ledger: the offline copy of %s's ledger came from %s, but %s now declares %s; it is not used for a different control plane",
			env, snap.Endpoint, env, endpoint)
	}
	b := &offlineBindings{store: store, snap: *snap, endpoint: endpoint, now: time.Now}
	base.Bindings = b
	base.Releases = offlineReleases{snap: *snap, endpoint: endpoint}
	base.Hosted = false
	base.Offline = b
	return base, nil
}

func (b *offlineBindings) Location() string {
	return fmt.Sprintf("offline copy of %s (refreshed %s)", b.endpoint, b.snap.RefreshedAt.Format(time.RFC3339))
}

// Current is the promotion the control plane last reported, or "never
// promoted" when that is what it reported.
func (b *offlineBindings) Current(_ context.Context, env string) (release.Promotion, bool, error) {
	if env != b.snap.Env {
		return release.Promotion{}, false, nil
	}
	if !b.snap.Bound {
		return release.Promotion{}, false, nil
	}
	return *b.snap.Current, true, nil
}

// Append queues the promotion for replay and returns the cached current one.
//
// Only a re-promotion of the cached release is possible: it is the only
// release offlineReleases can name, so release.Decide always answers "no-op"
// and the binding is unchanged. The queued record is what carries the deploy
// (and, later, its apply verdict) to the control plane.
func (b *offlineBindings) Append(_ context.Context, p release.Promotion, guard appendGuard) (release.Promotion, error) {
	var history []release.Promotion
	var currentID string
	if b.snap.Bound {
		history = []release.Promotion{*b.snap.Current}
		currentID = b.snap.Current.ID
	}
	existing, err := admitPromotion(history, p, guard)
	if err != nil {
		return release.Promotion{}, err
	}
	if existing == nil {
		return release.Promotion{}, fmt.Errorf("--offline-ledger can only redeploy %s's cached current release; moving %s to %s is a ledger decision and the ledger is not being consulted",
			p.Env, p.Env, p.Release)
	}
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	q := ledgerfile.QueuedPromotion{
		Key: hex.EncodeToString(raw[:]), Env: p.Env, Endpoint: b.endpoint, Release: p.Release,
		ExpectedPromotionID: currentID, PromotedBy: p.PromotedBy, Note: p.Note,
		QueuedAt: b.now().UTC().Truncate(time.Second), CacheRefreshedAt: b.snap.RefreshedAt,
	}
	if err := b.store.QueuePromotion(q); err != nil {
		return release.Promotion{}, fmt.Errorf("queue the offline promotion of %s: %w", p.Env, err)
	}
	b.mu.Lock()
	b.lastKey = q.Key
	b.mu.Unlock()
	return *existing, nil
}

// attachGate files the apply's verdict on the queued record, to replay with it.
func (b *offlineBindings) attachGate(env string, g release.Gate) error {
	b.mu.Lock()
	key := b.lastKey
	b.mu.Unlock()
	if key == "" {
		return errors.New("nothing was queued by this deploy")
	}
	return b.store.AttachQueuedGate(env, key, g)
}

// offlineReleases serves the one release the cache holds.
type offlineReleases struct {
	snap     ledgerfile.HostedSnapshot
	endpoint string
}

func (r offlineReleases) Location() string {
	return fmt.Sprintf("offline copy of %s", r.endpoint)
}

func (r offlineReleases) Cut(context.Context, release.Release) (bool, error) {
	return false, errors.New("--offline-ledger cannot cut a release: the ledger is not being consulted")
}

func (r offlineReleases) Get(_ context.Context, version string) (*release.Release, error) {
	if r.snap.Release == nil || r.snap.Release.Version != version {
		return nil, nil
	}
	rel := *r.snap.Release
	return &rel, nil
}

func (r offlineReleases) List(context.Context) ([]release.Release, error) {
	if r.snap.Release == nil {
		return nil, nil
	}
	return []release.Release{*r.snap.Release}, nil
}

// offlineShapeError says why an env cannot be served from the cache.
func offlineShapeError(env, endpoint string, hub bool, tiers []hostedTier) error {
	switch {
	case hub:
		return fmt.Errorf("--offline-ledger cannot deploy %s: its clusters are converged by %s's hub, so the control plane is the applier and nothing on this machine can stand in for it", env, endpoint)
	case len(tiers) > 0:
		return fmt.Errorf("--offline-ledger cannot deploy %s: it binds %s to %s, which the control plane publishes and converges", env, joinTiers(tiers), endpoint)
	default:
		return fmt.Errorf("--offline-ledger has nothing to apply for %s: it declares no workload this machine applies", env)
	}
}

// offlineBanner prints the cache's age and what this deploy will replay.
func offlineBanner(w io.Writer, env string, l envLedger) {
	b := l.Offline
	if b == nil || !b.snap.Bound {
		return
	}
	age := b.now().Sub(b.snap.RefreshedAt).Round(time.Minute)
	fmt.Fprintf(w, "[offline-ledger] %s's ledger on %s is NOT being consulted.\n", env, b.endpoint)
	fmt.Fprintf(w, "[offline-ledger]   cache refreshed %s ago (%s)\n", age, b.snap.RefreshedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "[offline-ledger]   deploying the cached current release %s (promotion %s) by its recorded digests; nothing is built\n",
		b.snap.Current.Release, b.snap.Current.ID)
	if age > staleOfflineCache {
		fmt.Fprintf(w, "[offline-ledger]   WARNING: the cache is older than %s. If %s was promoted since, this deploys an OLDER release than it runs;\n"+
			"[offline-ledger]   the replay will be refused (it compare-and-sets against promotion %s), but the apply cannot be undone by it\n",
			staleOfflineCache, env, b.snap.Current.ID)
	}
	if queued, err := b.store.QueuedPromotions(env); err == nil && len(queued) > 0 {
		fmt.Fprintf(w, "[offline-ledger]   %d earlier offline record(s) are already waiting to replay\n", len(queued))
	}
	fmt.Fprintf(w, "[offline-ledger]   this deploy queues 1 promotion record of %s (with the apply's verdict), replayed idempotently to %s\n"+
		"[offline-ledger]   on the next successful ledger contact (any `forge env status %s` or `forge env deploy %s` once it answers)\n",
		b.snap.Current.Release, b.endpoint, env, env)
}

// recordOfflineApplyOutcome files the apply's verdict on the queued record.
// Best-effort, like its hosted twin: the deploy's outcome is the apply's.
func recordOfflineApplyOutcome(env string, plan promotePlan, ledger envLedger, started time.Time, applyErr error, o promoteFollowOptions) {
	gate := applyOutcomeGate(started, time.Now(), applyErr,
		appliedBundleDigest(env, plan.Recorded.Release, o.clientDeploy), o.planDigest)
	if err := ledger.Offline.attachGate(env, gate); err != nil {
		o.notice("[deploy] Note: the apply's %s verdict could not be queued for replay: %v\n", gate.Status, err)
	}
}

// offlineFlagsError refuses the combinations that would read the ledger.
func offlineFlagsError(f deployCmdFlags) error {
	var bad []string
	if f.explain {
		bad = append(bad, "--explain")
	}
	if f.dryRun {
		bad = append(bad, "--dry-run")
	}
	if f.promote.fromSource().requested() {
		bad = append(bad, "--from")
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("--offline-ledger cannot be combined with %s: it deploys the cached current release and nothing else (preview it with --plan)",
		strings.Join(bad, ", "))
}

// dispatchOfflineDeploy is `forge env deploy <env> --offline-ledger`.
func dispatchOfflineDeploy(ctx context.Context, envName string, f deployCmdFlags) error {
	if err := offlineFlagsError(f); err != nil {
		return err
	}
	ctx = withOfflineLedger(ctx)
	ledger, err := resolveReleaseLedger(ctx, projectDirForKCL(), envName)
	if err != nil {
		return err
	}
	cur, bound, err := ledger.Bindings.Current(ctx, envName)
	if err != nil {
		return err
	}
	if !bound {
		return fmt.Errorf("--offline-ledger: the cached ledger says %s was never promoted, so there is no release to redeploy", envName)
	}
	if ledger.Offline.snap.Release == nil {
		return fmt.Errorf("--offline-ledger: the offline copy of %s's ledger names release %s but not the digests it pins "+
			"(it was written while the release could not be read). Refresh it with `forge env status %s` while the control plane is up",
			envName, cur.Release, envName)
	}
	switch f.promote.version {
	case "":
		f.promote.version = cur.Release
	case cur.Release:
	default:
		return fmt.Errorf("--offline-ledger can only deploy %s's cached current release %s, not %s", envName, cur.Release, f.promote.version)
	}
	offlineBanner(progressWriter(f.jsonOut), envName, ledger)
	return dispatchReleaseDeploy(ctx, envName, f)
}
