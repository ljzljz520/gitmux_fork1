package push

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// alwaysCount returns a counter reporting the given fixed values.
func alwaysCount(ahead, behind int, ok bool) counter {
	return func(localRef, trackingRef string) (int, int, bool) {
		return ahead, behind, ok
	}
}

func TestResolveRules(t *testing.T) {
	// base is the common triangular-workshop configuration: branch main pulls
	// from origin, two remotes (origin and fork) are configured.
	base := func() configMap {
		return configMap{
			"remote.origin.url":   {"/tmp/origin.git"},
			"remote.origin.fetch": {"+refs/heads/*:refs/remotes/origin/*"},
			"remote.fork.url":     {"/tmp/fork.git"},
			"remote.fork.fetch":   {"+refs/heads/*:refs/remotes/fork/*"},
			"branch.main.remote":  {"origin"},
			"branch.main.merge":   {"refs/heads/main"},
			"push.default":        {"simple"},
		}
	}

	tests := []struct {
		name   string
		branch string
		cfg    func() configMap
		count  counter
		want   Status
	}{
		{
			name:   "empty branch name is unknown",
			branch: "",
			cfg:    base,
			count:  alwaysCount(1, 0, true),
			want:   Status{},
		},
		{
			name: "no remote configured is unknown",
			cfg: func() configMap {
				cfg := base()
				delete(cfg, "remote.origin.url")
				delete(cfg, "remote.origin.fetch")
				delete(cfg, "remote.fork.url")
				delete(cfg, "remote.fork.fetch")
				return cfg
			},
			count: alwaysCount(1, 0, true),
			want:  Status{},
		},
		{
			name: "origin fallback when no branch config",
			cfg: func() configMap {
				cfg := base()
				delete(cfg, "branch.main.remote")
				delete(cfg, "branch.main.merge")
				// leave a single remote
				delete(cfg, "remote.fork.url")
				delete(cfg, "remote.fork.fetch")
				return cfg
			},
			count: alwaysCount(0, 0, true),
			// sole remote => origin, simple triangular check compares against
			// that sole remote as well: same remote, but no merge configured
			// => unknown
			want: Status{},
		},
		{
			name: "simple centralized with aligned names",
			cfg: func() configMap {
				cfg := base()
				delete(cfg, "remote.fork.url")
				delete(cfg, "remote.fork.fetch")
				return cfg
			},
			count: alwaysCount(2, 1, true),
			want: Status{
				Target: "origin/main", TargetKnown: true, RefKnown: true,
				Ahead: 2, Behind: 1,
			},
		},
		{
			name: "simple centralized with mismatching upstream name is unknown",
			cfg: func() configMap {
				cfg := base()
				delete(cfg, "remote.fork.url")
				delete(cfg, "remote.fork.fetch")
				cfg["branch.main.merge"] = []string{"refs/heads/develop"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "triangular simple with remote.pushDefault",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.pushdefault"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(3, 0, true),
			want: Status{
				Target: "fork/main", TargetKnown: true, RefKnown: true,
				Ahead: 3, Behind: 0,
			},
		},
		{
			name: "triangular simple with branch.pushRemote takes precedence",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.pushdefault"] = []string{"origin"}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(1, 2, true),
			want: Status{
				Target: "fork/main", TargetKnown: true, RefKnown: true,
				Ahead: 1, Behind: 2,
			},
		},
		{
			name: "pushDefault pointing to undefined remote is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.pushdefault"] = []string{"ghost"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "push.default current ignores upstream",
			cfg: func() configMap {
				cfg := base()
				cfg["push.default"] = []string{"current"}
				cfg["remote.pushdefault"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(5, 0, true),
			want: Status{
				Target: "fork/main", TargetKnown: true, RefKnown: true,
				Ahead: 5, Behind: 0,
			},
		},
		{
			name: "push.default upstream triangular is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["push.default"] = []string{"upstream"}
				cfg["remote.pushdefault"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "push.default upstream centralized uses merge ref",
			cfg: func() configMap {
				cfg := base()
				delete(cfg, "remote.fork.url")
				delete(cfg, "remote.fork.fetch")
				cfg["push.default"] = []string{"upstream"}
				cfg["branch.main.merge"] = []string{"refs/heads/develop"}
				return cfg
			},
			count: alwaysCount(0, 4, true),
			want: Status{
				Target: "origin/develop", TargetKnown: true, RefKnown: true,
				Ahead: 0, Behind: 4,
			},
		},
		{
			name: "push.default upstream without merge is unknown",
			cfg: func() configMap {
				cfg := base()
				delete(cfg, "remote.fork.url")
				delete(cfg, "remote.fork.fetch")
				cfg["push.default"] = []string{"upstream"}
				delete(cfg, "branch.main.merge")
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "push.default matching is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["push.default"] = []string{"matching"}
				return cfg
			},
			count: alwaysCount(1, 1, true),
			want:  Status{},
		},
		{
			name: "push.default nothing is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["push.default"] = []string{"nothing"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "push.default invalid is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["push.default"] = []string{"bogus"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "explicit push refspec maps the branch",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.push"] = []string{"refs/heads/main:refs/heads/special"}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: func(localRef, trackingRef string) (int, int, bool) {
				if trackingRef != "refs/remotes/fork/special" {
					t.Fatalf("unexpected tracking ref %q", trackingRef)
				}
				return 1, 0, true
			},
			want: Status{
				Target: "fork/special", TargetKnown: true, RefKnown: true,
				Ahead: 1, Behind: 0,
			},
		},
		{
			name: "wildcard push refspec",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.push"] = []string{"+refs/heads/*:refs/heads/topic-*"}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: func(localRef, trackingRef string) (int, int, bool) {
				if trackingRef != "refs/remotes/fork/topic-main" {
					t.Fatalf("unexpected tracking ref %q", trackingRef)
				}
				return 0, 0, true
			},
			want: Status{Target: "fork/topic-main", TargetKnown: true, RefKnown: true},
		},
		{
			name: "push refspec not covering the branch is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.push"] = []string{"refs/heads/other:refs/heads/other"}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "colon push refspec is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.push"] = []string{":"}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "two refspecs mapping to different destinations is unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.push"] = []string{
					"refs/heads/main:refs/heads/a",
					"refs/heads/main:refs/heads/b",
				}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(0, 0, true),
			want:  Status{},
		},
		{
			name: "mirror remote pushes same-named ref",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.mirror"] = []string{"true"}
				cfg["branch.main.pushremote"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(2, 2, true),
			want: Status{
				Target: "fork/main", TargetKnown: true, RefKnown: true,
				Ahead: 2, Behind: 2,
			},
		},
		{
			name: "tracking ref missing locally: target known, ref unknown",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.pushdefault"] = []string{"fork"}
				return cfg
			},
			count: alwaysCount(0, 0, false),
			want:  Status{Target: "fork/main", TargetKnown: true},
		},
		{
			name: "custom fetch refspec maps tracking ref",
			cfg: func() configMap {
				cfg := base()
				cfg["remote.fork.fetch"] = []string{"+refs/heads/*:refs/custom/*"}
				cfg["remote.pushdefault"] = []string{"fork"}
				return cfg
			},
			count: func(localRef, trackingRef string) (int, int, bool) {
				if trackingRef != "refs/custom/main" {
					t.Fatalf("unexpected tracking ref %q", trackingRef)
				}
				return 1, 0, true
			},
			want: Status{Target: "fork/main", TargetKnown: true, RefKnown: true, Ahead: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch := tt.branch
			if branch == "" && tt.name != "empty branch name is unknown" {
				branch = "main"
			}
			got := resolve(branch, tt.cfg(), tt.count)
			if *got != tt.want {
				t.Errorf("got  %+v\nwant %+v", *got, tt.want)
			}
		})
	}
}

func TestApplyRefspec(t *testing.T) {
	tests := []struct {
		spec, src string
		dst       string
		matched   bool
	}{
		{"refs/heads/main:refs/heads/main", "refs/heads/main", "refs/heads/main", true},
		{"+refs/heads/*:refs/remotes/fork/*", "refs/heads/x", "refs/remotes/fork/x", true},
		{"refs/heads/*:refs/heads/topic-*", "refs/heads/main", "refs/heads/topic-main", true},
		{"refs/heads/main:refs/heads/main", "refs/heads/other", "", false},
		{"refs/heads/main", "refs/heads/main", "refs/heads/main", true},
		{":", "refs/heads/main", "", false},
		{":refs/heads/main", "refs/heads/main", "", false},
		{"refs/heads/main:", "refs/heads/main", "", true},
	}
	for _, tt := range tests {
		dst, matched := applyRefspec(tt.spec, tt.src)
		if matched != tt.matched || dst != tt.dst {
			t.Errorf("applyRefspec(%q, %q) = (%q,%v), want (%q,%v)",
				tt.spec, tt.src, dst, matched, tt.dst, tt.matched)
		}
	}
}

func TestCanonicalKey(t *testing.T) {
	tests := map[string]string{
		"core.bare":                 "core.bare",
		"push.default":              "push.default",
		"remote.pushDefault":        "remote.pushdefault",
		"branch.main.pushRemote":    "branch.main.pushremote",
		"branch.Feature/Foo.remote": "branch.Feature/Foo.remote",
		"branch.release-1.2.merge":  "branch.release-1.2.merge",
		"remote.MyFork.pushURL":     "remote.MyFork.pushurl",
	}
	for in, want := range tests {
		if got := canonicalKey(in); got != want {
			t.Errorf("canonicalKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- dual-remote repository integration test --------------------------------

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func TestDualRemoteRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	fork := filepath.Join(root, "fork.git")

	// Bare "upstream" and personal "fork" remotes.
	git(t, root, "init", "-q", "--bare", upstream)
	git(t, root, "init", "-q", "--bare", fork)

	// Clone upstream, create main and push it there (pull upstream).
	work := filepath.Join(root, "work")
	git(t, root, "clone", "-q", upstream, work)
	git(t, work, "config", "user.email", "t@t.com")
	git(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "a.txt")
	git(t, work, "commit", "-qm", "init")
	git(t, work, "push", "-q", "origin", "HEAD:main")
	git(t, work, "branch", "--set-upstream-to=origin/main", "main")

	// Add personal fork as second remote: triangular workflow.
	git(t, work, "remote", "add", "fork", fork)
	git(t, work, "config", "remote.pushDefault", "fork")

	ctx := context.Background()
	resolve := func() *Status { return ResolveIn(ctx, work, "main") }

	t.Run("fork never fetched: target known, divergence unknown", func(t *testing.T) {
		st := resolve()
		want := Status{Target: "fork/main", TargetKnown: true}
		if *st != want {
			t.Fatalf("got %+v, want %+v", *st, want)
		}
	})

	// Push main to the fork and fetch so refs/remotes/fork/main exists.
	git(t, work, "push", "-q", "fork", "main")
	git(t, work, "fetch", "-q", "fork")

	t.Run("aligned with fork", func(t *testing.T) {
		st := resolve()
		want := Status{Target: "fork/main", TargetKnown: true, RefKnown: true}
		if *st != want {
			t.Fatalf("got %+v, want %+v", *st, want)
		}
	})

	// 2 local commits on top.
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "b.txt")
	git(t, work, "commit", "-qm", "local1")
	if err := os.WriteFile(filepath.Join(work, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "c.txt")
	git(t, work, "commit", "-qm", "local2")

	// 1 different commit on the fork.
	forkClone := filepath.Join(root, "forkclone")
	git(t, root, "clone", "-q", fork, forkClone)
	git(t, forkClone, "config", "user.email", "t@t.com")
	git(t, forkClone, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(forkClone, "f.txt"), []byte("f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, forkClone, "add", "f.txt")
	git(t, forkClone, "commit", "-qm", "remote1")
	git(t, forkClone, "push", "-q", "origin", "main")

	// Update local view of the fork (no push from work).
	git(t, work, "fetch", "-q", "fork")

	t.Run("diverged both ways against fork, ahead of upstream only", func(t *testing.T) {
		stPush := resolve()
		if !stPush.TargetKnown || !stPush.RefKnown {
			t.Fatalf("push target not resolved: %+v", stPush)
		}
		if stPush.Target != "fork/main" {
			t.Errorf("target = %q, want fork/main", stPush.Target)
		}
		if stPush.Ahead != 2 || stPush.Behind != 1 {
			t.Errorf("push divergence = ahead %d behind %d, want ahead 2 behind 1",
				stPush.Ahead, stPush.Behind)
		}
	})

	t.Run("explicit branch pushRemote overrides pushDefault", func(t *testing.T) {
		git(t, work, "config", "branch.main.pushRemote", "origin")
		defer git(t, work, "config", "--unset", "branch.main.pushRemote")
		st := resolve()
		if st.Target != "origin/main" || !st.RefKnown || st.Ahead != 2 || st.Behind != 0 {
			t.Fatalf("got %+v, want origin/main ahead 2 behind 0", st)
		}
	})

	t.Run("push.default matching yields unknown", func(t *testing.T) {
		git(t, work, "config", "push.default", "matching")
		defer git(t, work, "config", "--unset", "push.default")
		st := resolve()
		if st.TargetKnown {
			t.Fatalf("got %+v, want unknown", st)
		}
	})

	t.Run("empty branch name is unknown", func(t *testing.T) {
		if st := ResolveIn(ctx, work, ""); st.TargetKnown {
			t.Fatalf("got %+v, want unknown", st)
		}
	})

	t.Run("resolution is deterministic and read-only", func(t *testing.T) {
		st1 := resolve()
		st2 := resolve()
		if fmt.Sprint(st1) != fmt.Sprint(st2) {
			t.Fatalf("non-deterministic: %+v vs %+v", st1, st2)
		}
	})
}
