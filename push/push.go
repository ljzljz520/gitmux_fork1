// Package push resolves the default push destination of the current local
// branch and its divergence relative to it, using local information only (it
// never contacts the remotes).
//
// The destination resolution mirrors what 'git push' does when no repository
// nor refspec are passed on the command line:
//
//  1. branch.<name>.pushRemote
//  2. remote.pushDefault
//  3. branch.<name>.remote
//  4. the unique configured remote, or "origin"
//
// The destination branch is then derived from remote.<name>.push refspecs (if
// any), remote.<name>.mirror, or the push.default option (simple, current,
// upstream, matching, nothing).
package push

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Status is the resolution result of the local branch default push target.
type Status struct {
	// Target is the short name of the push destination, in the form
	// "<remote>/<branch>", e.g. "fork/main". It is empty unless TargetKnown
	// is true.
	Target string

	// TargetKnown reports whether the push destination could be uniquely
	// determined, following the git push rules. When false, Target is empty
	// and the other fields are meaningless.
	TargetKnown bool

	// RefKnown reports whether the remote-tracking ref of the push
	// destination exists locally. When false, Ahead and Behind are not
	// computable without contacting the remote (a fetch is required).
	RefKnown bool

	// Ahead is the number of local commits not yet pushed to the destination.
	Ahead int

	// Behind is the number of destination commits missing locally.
	Behind int
}

// Unknown returns a Status indicating that the push destination can't be
// determined.
func Unknown() *Status { return &Status{} }

// Resolve returns the push status of localBranch in the repository found in
// the current working directory (or one of its parents). It only reads local
// git configuration and local refs, it never fetches nor contacts any remote.
//
// An empty local branch name (detached HEAD, unborn branch) yields an unknown
// status, as do all the situations in which 'git push' would refuse to push or
// wouldn't push to a single, uniquely identifiable destination.
func Resolve(ctx context.Context, localBranch string) *Status {
	return ResolveIn(ctx, "", localBranch)
}

// ResolveIn is like Resolve but runs against the repository in dir. An empty
// dir means the current working directory.
func ResolveIn(ctx context.Context, dir, localBranch string) *Status {
	if localBranch == "" {
		return Unknown()
	}

	cfg, err := readConfig(ctx, dir)
	if err != nil {
		return Unknown()
	}

	return resolve(localBranch, cfg, func(localRef, trackingRef string) (ahead, behind int, ok bool) {
		return divergence(ctx, dir, localRef, trackingRef)
	})
}

// counter reports how many commits localRef is ahead/behind trackingRef. ok is
// false when the tracking ref doesn't exist locally (or can't be read).
type counter func(localRef, trackingRef string) (ahead, behind int, ok bool)

// configMap holds the effective git configuration, with the list of values
// associated to each (lowercased, dot-separated) key. For single-valued keys,
// the last value wins.
type configMap map[string][]string

func (c configMap) last(key string) (string, bool) {
	if vs, ok := c[key]; ok && len(vs) > 0 {
		return vs[len(vs)-1], true
	}
	return "", false
}

// resolve is the pure, testable push-target resolution logic.
func resolve(localBranch string, cfg configMap, count counter) *Status {
	if localBranch == "" {
		return Unknown()
	}

	localRef := "refs/heads/" + localBranch

	remotes := configuredRemotes(cfg)

	// --- resolve the push remote name, like pushremote_for_branch ----------
	remoteName, remoteExplicit := cfg.last("branch." + localBranch + ".pushremote")
	if !remoteExplicit {
		remoteName, remoteExplicit = cfg.last("remote.pushdefault")
	}
	if !remoteExplicit {
		remoteName, remoteExplicit = cfg.last("branch." + localBranch + ".remote")
	}
	if !remoteExplicit {
		// No explicit remote: fall back to the unique configured remote, or
		// the conventional "origin".
		if len(remotes) == 1 {
			for name := range remotes {
				remoteName = name
			}
		} else {
			remoteName = "origin"
		}
	}
	if !remotes[remoteName] {
		// The resolved name doesn't correspond to a usable remote (no URL):
		// 'git push' would die with "No configured push destination".
		return Unknown()
	}

	// Remote used when comparing pull vs push (triangular workflow check).
	upstreamRemote, ok := cfg.last("branch." + localBranch + ".remote")
	if !ok {
		if len(remotes) == 1 {
			for name := range remotes {
				upstreamRemote = name
			}
		} else {
			upstreamRemote = "origin"
		}
	}

	// --- resolve the remote-side destination ref --------------------------
	dst, known := destinationRef(localBranch, localRef, remoteName, upstreamRemote, cfg)
	if !known {
		return Unknown()
	}

	// --- map it to the local remote-tracking ref through the fetch refspecs -
	fetchSpecs := cfg["remote."+remoteName+".fetch"]
	trackingRef, mapped := mapTracking(dst, remoteName, fetchSpecs)

	target := remoteName + "/" + shortenRef(dst)
	st := &Status{Target: target, TargetKnown: true}

	if !mapped {
		return st
	}

	// Display the short tracking name when it lives under the conventional
	// remote-tracking namespace (handles custom fetch refspecs).
	if short, ok := strings.CutPrefix(trackingRef, "refs/remotes/"); ok {
		st.Target = short
	}

	ahead, behind, ok := count(localRef, trackingRef)
	if !ok {
		return st
	}
	st.RefKnown = true
	st.Ahead = ahead
	st.Behind = behind
	return st
}

// destinationRef returns the remote-side ref (e.g. "refs/heads/main") the
// local branch would be pushed to, following setup_default_push_refspecs in
// git's builtin/push.c.
func destinationRef(localBranch, localRef, remoteName, upstreamRemote string, cfg configMap) (string, bool) {
	// A mirror remote pushes refs to refs of the same name.
	if v, present := cfg.last("remote." + remoteName + ".mirror"); present && isBoolTrue(v) {
		return localRef, true
	}

	// Explicit push refspecs take precedence over push.default.
	if specs, ok := cfg["remote."+remoteName+".push"]; ok {
		return applyPushRefspecs(specs, localRef)
	}

	pushDefault, _ := cfg.last("push.default")
	switch pushDefault {
	case "", "simple":
		if remoteName != upstreamRemote {
			// Triangular workflow: push to the same-named branch on the push
			// remote, no upstream required.
			return localRef, true
		}
		// Centralized workflow: the upstream branch must exist and bear the
		// same name as the local branch.
		merge, ok := cfg.last("branch." + localBranch + ".merge")
		if !ok || merge != localRef {
			return "", false
		}
		return merge, true

	case "current":
		return localRef, true

	case "upstream":
		if remoteName != upstreamRemote {
			return "", false
		}
		merge, ok := cfg.last("branch." + localBranch + ".merge")
		if !ok {
			return "", false
		}
		return merge, true

	case "matching", "nothing":
		// "matching" pushes all matching branches (no unique destination),
		// "nothing" pushes nothing.
		return "", false

	default:
		// Unknown/invalid push.default value.
		return "", false
	}
}

// applyPushRefspecs applies a remote's push refspecs to localRef, returning
// the unique destination ref. It reports false if no refspec matches, if a
// refspec has no source (e.g. ":"), or if several distinct destinations
// match.
func applyPushRefspecs(specs []string, localRef string) (string, bool) {
	var dst string
	found := false
	for _, spec := range specs {
		d, matched := applyRefspec(spec, localRef)
		if !matched || d == "" {
			// Sourceless refspec (e.g. ":") or deletion-only mapping: not a
			// unique push destination for the branch.
			continue
		}
		if !found {
			dst = d
			found = true
		} else if dst != d {
			return "", false
		}
	}
	return dst, found
}

// mapTracking maps a remote-side ref to its local remote-tracking ref using
// the remote fetch refspecs, emulating tracking_for_push_dest in git. An empty
// fetch refspec list falls back to git's conventional fetch refspec.
func mapTracking(remoteRef, remoteName string, fetchSpecs []string) (string, bool) {
	if len(fetchSpecs) == 0 {
		fetchSpecs = []string{"+refs/heads/*:refs/remotes/" + remoteName + "/*"}
	}
	for _, spec := range fetchSpecs {
		if dst, matched := applyRefspec(spec, remoteRef); matched && dst != "" {
			return dst, true
		}
	}
	return "", false
}

// applyRefspec applies a single (fetch or push) refspec to src and returns the
// mapped destination. It handles the optional leading "+", the wildcard "*",
// sourceless/destinationless refspecs and refspecs without a colon (the
// destination defaults to the source).
func applyRefspec(spec, src string) (dst string, matched bool) {
	spec = strings.TrimPrefix(spec, "+")

	lhs, rhs, hasColon := strings.Cut(spec, ":")
	if !hasColon {
		rhs = lhs
	}

	if lhs == "" {
		// Sourceless refspec (":" or ":dst"): can't map a single source.
		return "", false
	}

	if strings.Contains(lhs, "*") {
		pre, suf, ok := wildcardParts(lhs)
		if !ok || !strings.HasPrefix(src, pre) || !strings.HasSuffix(src, suf) {
			return "", false
		}
		captured := src[len(pre) : len(src)-len(suf)]
		rpre, rsuf, ok := wildcardParts(rhs)
		if !ok {
			return "", false
		}
		return rpre + captured + rsuf, true
	}

	if lhs != src {
		return "", false
	}
	return rhs, true
}

// wildcardParts splits s at its (single) wildcard, e.g. "refs/heads/*" ->
// ("refs/heads/", "").
func wildcardParts(s string) (prefix, suffix string, ok bool) {
	i := strings.IndexByte(s, '*')
	if i < 0 || strings.IndexByte(s[i+1:], '*') >= 0 {
		return s, "", false
	}
	return s[:i], s[i+1:], true
}

// shortenRef turns a fully qualified branch ref into a short name, e.g.
// "refs/heads/main" -> "main". Other refs are returned unchanged.
func shortenRef(ref string) string {
	if short, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return short
	}
	return ref
}

// configuredRemotes returns the set of remote names that are usable (they have
// a configured URL or push URL).
func configuredRemotes(cfg configMap) map[string]bool {
	remotes := map[string]bool{}
	for key, vals := range cfg {
		if !strings.HasPrefix(key, "remote.") {
			continue
		}
		var name string
		switch {
		case strings.HasSuffix(key, ".pushurl"):
			name = strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".pushurl")
		case strings.HasSuffix(key, ".url"):
			name = strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url")
		default:
			continue
		}
		for _, v := range vals {
			if v != "" {
				remotes[name] = true
			}
		}
	}
	return remotes
}

// isBoolTrue reports whether v is one of git's truthy boolean values,
// including the empty string (bare "key" means true).
func isBoolTrue(v string) bool {
	switch v {
	case "", "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

// --- git plumbing -----------------------------------------------------------

func gitEnv() []string {
	env := []string{
		"LC_ALL=C",             // normalize output
		"GIT_OPTIONAL_LOCKS=0", // never take locks
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	return env
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	buf, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return buf, nil
}

// readConfig returns the effective git configuration ('git config -z --list').
// Records are NUL-separated "key\nvalue" pairs; bare boolean keys have an
// empty value.
func readConfig(ctx context.Context, dir string) (configMap, error) {
	buf, err := runGit(ctx, dir, "config", "-z", "--list")
	if err != nil {
		return nil, err
	}

	cfg := configMap{}
	for _, record := range bytes.Split(buf, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		key, value, ok := bytes.Cut(record, []byte{'\n'})
		if !ok {
			continue
		}
		// Git lowercases the section and variable names but preserves the
		// (case-sensitive) subsection, e.g. branch "feat/Foo" yields
		// "branch.feat/Foo.remote".
		k := canonicalKey(string(key))
		cfg[k] = append(cfg[k], string(value))
	}
	return cfg, nil
}

// canonicalKey lowercases the section and variable of a dotted config key but
// preserves the subsection.
func canonicalKey(key string) string {
	first := strings.IndexByte(key, '.')
	last := strings.LastIndexByte(key, '.')
	if first < 0 || last < first {
		return strings.ToLower(key)
	}
	section := strings.ToLower(key[:first])
	variable := strings.ToLower(key[last+1:])
	if first == last {
		return section + "." + variable
	}
	return section + "." + key[first+1:last] + "." + variable
}

// divergence returns the ahead/behind counts of localRef relative to
// trackingRef, using only local refs. It fails (ok=false) if the tracking ref
// doesn't exist locally.
func divergence(ctx context.Context, dir, localRef, trackingRef string) (ahead, behind int, ok bool) {
	rangeSpec := localRef + "..." + trackingRef
	buf, err := runGit(ctx, dir, "rev-list", "--left-right", "--count", rangeSpec)
	if err != nil {
		return 0, 0, false
	}

	fields := strings.Fields(strings.TrimSpace(string(buf)))
	if len(fields) != 2 {
		return 0, 0, false
	}
	a, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}
