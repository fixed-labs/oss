package secrets

import (
	"sort"
	"strings"
)

// normalizeRepoID returns the forge-qualified (host/owner/name) and bare
// (owner/name) forms of a repo id. The image pipeline keys repos by bare
// owner/name (GitHub-only today); the secrets layer is forge-qualified now so
// user configs written today survive repo ids gaining a forge component. A bare
// owner/name is assumed to live on github.com until that lands.
func normalizeRepoID(repo string) (qualified, bare string) {
	r := strings.ToLower(strings.Trim(strings.TrimSpace(repo), "/"))
	segs := strings.Split(r, "/")
	switch len(segs) {
	case 2:
		return "github.com/" + r, r
	case 3:
		return r, segs[1] + "/" + segs[2]
	default:
		return r, r
	}
}

// QualifiedRepoID is normalizeRepoID's qualified form, exported for the CLI to
// key user-config entries it writes.
func QualifiedRepoID(repo string) string {
	q, _ := normalizeRepoID(repo)
	return q
}

// matchRepo finds the most-specific repos-config key matching repoID. A pattern
// may be exact or use '*' for one segment; specificity weights the rightmost
// (name) segment highest, so an exact bare owner/name beats a qualified
// owner-glob, and a qualified exact beats a bare exact. Returns "" / false when
// nothing matches. patterns is iterated in sorted order for determinism.
func matchRepo(patterns []string, repoID string) (string, bool) {
	qualified, bare := normalizeRepoID(repoID)
	best := ""
	bestScore := -1
	for _, p := range patterns {
		pl := strings.ToLower(strings.Trim(strings.TrimSpace(p), "/"))
		var target string
		switch strings.Count(pl, "/") {
		case 1:
			target = bare
		case 2:
			target = qualified
		default:
			continue
		}
		score, ok := globScore(pl, target)
		if ok && score > bestScore {
			bestScore = score
			best = p
		}
	}
	return best, bestScore >= 0
}

// globScore matches pattern against target segment-by-segment ('*' = any one
// segment) and returns a specificity score (sum of literal-segment weights,
// rightmost segment weighted highest). Returns false when the segment counts
// differ or a literal segment mismatches.
func globScore(pattern, target string) (int, bool) {
	ps := strings.Split(pattern, "/")
	ts := strings.Split(target, "/")
	if len(ps) != len(ts) {
		return 0, false
	}
	weights := []int{8, 4, 2} // by distance from the right (name=8, owner=4, host=2)
	score := 0
	hasWildcard := false
	for i := range ps {
		if ps[i] == "*" {
			hasWildcard = true
			continue
		}
		if ps[i] != ts[i] {
			return 0, false
		}
		posFromRight := len(ps) - 1 - i
		w := 1
		if posFromRight < len(weights) {
			w = weights[posFromRight]
		}
		score += w
	}
	if !hasWildcard {
		// A fully-literal pattern outranks a same-shape one with a wildcard
		// (e.g. bare-exact `acme/widget` beats wildcard-host `*/acme/widget`).
		score++
	}
	return score, true
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
