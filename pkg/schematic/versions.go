package schematic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// versionsCacheTTL is how long a fetched /versions listing is trusted before it is refreshed.
//
// New Talos patches ship rarely relative to the reconcile loop, so a listing that is a few
// minutes stale never picks the wrong upgrade — it only delays noticing a brand new patch by at
// most this long, while keeping the factory off the hot path of every machine reconcile.
const versionsCacheTTL = 10 * time.Minute

// gaVersionRe matches a General Availability Talos version and nothing else. The anchored end
// rejects any pre-release such as "v1.14.0-rc.2", whose "-suffix" carries no comparable patch.
var gaVersionRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// minorRe extracts the major.minor line from a version floor, which may be written as a bare
// minor ("v1.13") or as a full version ("v1.13.9").
var minorRe = regexp.MustCompile(`^v(\d+)\.(\d+)`)

// VersionResolver answers "what is the newest Talos release" against an Image Factory.
//
// It exists so the infrastructure provider can turn an unset or minor-only Talos version into a
// concrete patch release to install and upgrade to. Resolving against the same factory that
// serves the images guarantees the version it returns is actually buildable.
type VersionResolver struct {
	baseURL string
	client  *http.Client
	ttl     time.Duration

	mu        sync.Mutex
	cache     []talosSemver
	fetchedAt time.Time
}

// NewVersionResolver builds a resolver against an Image Factory. An empty factoryURL selects the
// public factory, matching NewRegistrar.
func NewVersionResolver(factoryURL string) *VersionResolver {
	if factoryURL == "" {
		factoryURL = DefaultFactoryURL
	}

	return &VersionResolver{
		baseURL: strings.TrimSuffix(factoryURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
		ttl:     versionsCacheTTL,
	}
}

// LatestPatch returns the newest GA patch release within the minor of floor, e.g. "v1.13" or
// "v1.13.9" resolves to "v1.13.10". It returns "" when the minor has no GA release, which callers
// treat as "leave the version unresolved" rather than an error.
func (r *VersionResolver) LatestPatch(ctx context.Context, floor string) (string, error) {
	major, minor, ok := parseMinor(floor)
	if !ok {
		return "", nil
	}

	versions, err := r.versions(ctx)
	if err != nil {
		return "", err
	}

	var best talosSemver

	found := false

	for _, v := range versions {
		if v.major != major || v.minor != minor {
			continue
		}

		if !found || best.less(v) {
			best = v
			found = true
		}
	}

	if !found {
		return "", nil
	}

	return best.String(), nil
}

// LatestMinor returns the newest GA minor line on the factory, e.g. "v1.14". A minor whose only
// tags are pre-releases is not GA and is skipped. It returns "" when no GA release exists.
func (r *VersionResolver) LatestMinor(ctx context.Context) (string, error) {
	versions, err := r.versions(ctx)
	if err != nil {
		return "", err
	}

	best := talosSemver{}

	found := false

	for _, v := range versions {
		if !found || best.less(v) {
			best = v
			found = true
		}
	}

	if !found {
		return "", nil
	}

	return fmt.Sprintf("v%d.%d", best.major, best.minor), nil
}

// versions returns the GA versions the factory can build, refreshing the cache when the TTL has
// elapsed. Only GA releases are retained; pre-releases are dropped at parse time.
//
// The lock is deliberately held across the fetch: when many machine reconciles hit a cold cache
// at once they coalesce onto a single request rather than stampeding the factory, and the fetch
// is a small, infrequent (TTL-gated) call so the brief serialization is immaterial.
func (r *VersionResolver) versions(ctx context.Context) ([]talosSemver, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cache != nil && time.Since(r.fetchedAt) < r.ttl {
		return r.cache, nil
	}

	fetched, err := r.fetch(ctx)
	if err != nil {
		// Serve a stale listing rather than fail resolution outright: a transient factory outage
		// should not blank out a version that was resolvable a moment ago.
		if r.cache != nil {
			return r.cache, nil
		}

		return nil, err
	}

	r.cache = fetched
	r.fetchedAt = time.Now()

	return r.cache, nil
}

func (r *VersionResolver) fetch(ctx context.Context) ([]talosSemver, error) {
	endpoint := r.baseURL + "/versions"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building versions request: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing versions from %s: %w", endpoint, err)
	}

	defer resp.Body.Close() //nolint:errcheck // read-only response

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading versions response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image factory returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}

	var raw []string
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("decoding versions response: %w", err)
	}

	out := make([]talosSemver, 0, len(raw))

	for _, s := range raw {
		if v, ok := parseGAVersion(s); ok {
			out = append(out, v)
		}
	}

	return out, nil
}

// talosSemver is a parsed GA Talos version. Pre-release identifiers are never represented because
// they are dropped before a version becomes a talosSemver.
type talosSemver struct {
	major, minor, patch int
}

func (v talosSemver) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch)
}

// less reports whether v orders before other.
func (v talosSemver) less(other talosSemver) bool {
	if v.major != other.major {
		return v.major < other.major
	}

	if v.minor != other.minor {
		return v.minor < other.minor
	}

	return v.patch < other.patch
}

// parseGAVersion parses a GA version string, returning ok=false for pre-releases and anything
// unrecognised.
func parseGAVersion(s string) (talosSemver, bool) {
	m := gaVersionRe.FindStringSubmatch(s)
	if m == nil {
		return talosSemver{}, false
	}

	return talosSemver{major: atoi(m[1]), minor: atoi(m[2]), patch: atoi(m[3])}, true
}

// parseMinor extracts the major.minor line from a floor written as either a bare minor or a full
// version.
func parseMinor(floor string) (major, minor int, ok bool) {
	m := minorRe.FindStringSubmatch(floor)
	if m == nil {
		return 0, 0, false
	}

	return atoi(m[1]), atoi(m[2]), true
}

// atoi converts a regex-captured run of digits, which cannot fail to parse.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)

	return n
}
