package tmux

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/arl/gitstatus"
	"gopkg.in/yaml.v3"
)

// Config is the configuration of the Git status tmux formatter.
type Config struct {
	// Symbols contains the symbols printed before the Git status components.
	Symbols symbols
	// Styles contains the tmux style strings for symbols and Git status
	// components.
	Styles styles
	// Layout sets the output format of the Git status.
	Layout []string `yaml:",flow"`
	// Options contains additional configuration options.
	Options options
}

type symbols struct {
	Branch     string // Branch is the string shown before local branch name.
	HashPrefix string // HasPrefix is the string shown before a SHA1 ref.

	Ahead  string // Ahead is the string shown before the ahead count for the local/upstream branch divergence.
	Behind string // Behind is the string shown before the behind count for the local/upstream branch divergence.

	Staged    string // Staged is the string shown before the count of staged files.
	Conflict  string // Conflict is the string shown before the count of files with conflicts.
	Modified  string // Modified is the string shown before the count of modified files.
	Untracked string // Untracked is the string shown before the count of untracked files.
	Stashed   string // Stashed is the string shown before the count of stash entries.
	Clean     string // Clean is the string shown when the working tree is clean.

	Insertions string // Insertions is the string shown before the count of inserted lines.
	Deletions  string // Deletions is the string shown before the count of deleted lines.

	Unknown string // Unknown is the string shown when the push destination (or its divergence) can't be determined.
}

type styles struct {
	Clear string // Clear is the style string that clears all styles.

	State  string // State is the style string printed before eventual special state.
	Branch string // Branch is the style string printed before the local branch.
	Remote string // Remote is the style string printed before the upstream branch.

	Divergence string // Divergence is the style string printed before divergence count/symbols.

	Staged    string // Staged is the style string printed before the staged files count.
	Conflict  string // Conflict is the style string printed before the conflict count.
	Modified  string // Modified is the style string printed before the modified files count.
	Untracked string // Untracked is the style string printed before the untracked files count.
	Stashed   string // Stashed is the style string printed before the stash entries count.
	Clean     string // Clean is the style string printed before the clean symbols.

	Insertions string // Insertions is the style string printed before the count of inserted lines.
	Deletions  string // Deletions is the style string printed before the count of deleted lines.

	Push           string // Push is the style string printed before the push destination branch.
	PushDivergence string // PushDivergence is the style string printed before the divergence counts against the push destination.
}

// PushStatus describes the default push destination of the local branch and
// the local branch divergence relative to it, as resolved using local
// information only (no network).
type PushStatus struct {
	// Target is the short name of the push destination, "<remote>/<branch>",
	// e.g. "fork/main". Empty unless TargetKnown is true.
	Target string
	// TargetKnown reports whether the push destination could be uniquely
	// determined following the git push rules.
	TargetKnown bool
	// RefKnown reports whether the destination remote-tracking ref exists
	// locally, in which case Ahead and Behind are meaningful. When false, the
	// divergence is unknown (a fetch would be required to determine it).
	RefKnown bool
	// Ahead is the number of local commits not yet pushed to the destination.
	Ahead int
	// Behind is the number of destination commits missing locally.
	Behind int
}

const (
	dirLeft   direction = "left"
	dirRight  direction = "right"
	dirCenter direction = "center"
)

type direction string

func (d *direction) UnmarshalYAML(value *yaml.Node) error {
	s := ""
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("error decoding 'direction': %v", s)
	}
	switch direction(s) {
	case dirLeft:
		*d = dirLeft
	case dirRight:
		*d = dirRight
	case dirCenter:
		*d = dirCenter
	default:
		return fmt.Errorf("'direction': unexpected value %v", s)
	}
	return nil
}

type options struct {
	BranchMaxLen      int       `yaml:"branch_max_len"`
	BranchTrim        direction `yaml:"branch_trim"`
	Ellipsis          string    `yaml:"ellipsis"`
	HideClean         bool      `yaml:"hide_clean"`
	DivergenceSpace   bool      `yaml:"divergence_space"`
	SwapDivergence    bool      `yaml:"swap_divergence"`
	FlagsWithoutCount bool      `yaml:"flags_without_count"`
}

// A Formater formats git status to a tmux style string.
type Formater struct {
	Config
	st *gitstatus.Status

	// Push holds the resolved push destination and divergence information.
	// A nil value means that the push status is not available, in which case
	// the push-related layout components render as unknown.
	Push *PushStatus
}

// LayoutNeedsPush reports whether the layout references one of the push
// components ("push", "push-branch" or "push-divergence"), i.e. whether the
// push destination needs to be resolved.
func (c Config) LayoutNeedsPush() bool {
	for _, item := range c.Layout {
		switch item {
		case "push", "push-branch", "push-divergence":
			return true
		}
	}
	return false
}

// truncate returns s, truncated so that it is no more than max runes long.
// Depending on the provided direction, truncation is performed right, left or
// center. If s is returned truncated, the truncated part is replaced with the
// 'ellipsis' string.
//
// If max is zero, negative or greater than the number of runes in s, truncate
// just returns s.
//
// NOTE: If max is lower than len(ellipsis), in other words if we're not even
// allowed to just return the ellipsis string, then we just return the maximum
// number of runes we can, without inserting ellipsis.
func truncate(s, ellipsis string, max int, dir direction) string {
	slen := utf8.RuneCountInString(s)
	if max <= 0 || slen <= max {
		return s
	}

	runes := []rune(s)
	ell := []rune(ellipsis)

	if max < len(ellipsis) {
		ell = nil // Just truncate s since even ellipsis don't fit.
	}

	switch dir {
	case dirRight:
		runes = runes[:max-len(ell)]
		runes = append(runes, ell...)
	case dirLeft:
		runes = runes[len(runes)+len(ell)-max:]
		runes = append(ell, runes...)
	case dirCenter:
		// We want to keep the same number of runes on both sides of the ellipsis. If the
		// number of runes on each side is odd, we add one more rune to the right side.
		llen := (max - len(ell)) / 2
		rlen := max - len(ell) - llen

		right := runes[len(runes)-rlen:]

		runes = append(runes[:llen], ell...)
		runes = append(runes, right...)
	}

	return string(runes)
}

// Format writes st as json into w.
func (f *Formater) Format(w io.Writer, st *gitstatus.Status) error {
	defer fmt.Fprintf(w, "%s", f.Styles.Clear)

	f.st = st

	// Overall working tree state
	if f.st.IsInitial {
		branch := truncate(f.st.LocalBranch, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
		s := fmt.Sprintf("%s%s%s [no commits yet] %s", f.Styles.Clear, f.Styles.Branch, branch, f.flags())
		_, err := io.WriteString(w, s)
		return err
	}

	_, err := fmt.Fprintf(w, "%s%s", f.Styles.Clear, f.format())
	return err
}

const resetStyles = "#[fg=default,bg=default]"

func (f *Formater) format() string {
	var comps []string

	// Add spacing between non-empty components.
	joinComps := func() string {
		i := 0
		for _, s := range comps {
			if s != "" {
				comps[i] = s
				i++
			}
		}
		return strings.Join(comps[:i], " ")
	}

	sb := strings.Builder{}
	for _, item := range f.Layout {
		switch item {
		case "branch":
			comps = append(comps, f.specialState())
		case "remote":
			comps = append(comps, f.remoteBranch())
			comps = append(comps, f.divergence())
		case "remote-branch":
			comps = append(comps, f.remoteBranch())
		case "divergence":
			comps = append(comps, f.divergence())
		case "push":
			comps = append(comps, f.pushBranch())
			// The push alias already conveys an unknown destination through
			// the branch component, don't duplicate that marker.
			comps = append(comps, f.pushDivergence(f.pushTargetKnown()))
		case "push-branch":
			comps = append(comps, f.pushBranch())
		case "push-divergence":
			comps = append(comps, f.pushDivergence(true))
		case "flags":
			comps = append(comps, f.flags())
		case "stats":
			comps = append(comps, f.stats())
		default:
			sb.WriteString(joinComps())
			sb.WriteString(f.Styles.Clear)
			sb.WriteString(item)
			comps = comps[:0]
		}
	}

	sb.WriteString(joinComps())

	// Reset foreground and background styles to default, since there could be
	// successive elements in user tmux status strings.
	sb.WriteString(resetStyles)
	return sb.String()
}

func (f *Formater) specialState() string {
	s := f.Styles.Clear

	switch f.st.State {
	case gitstatus.Rebasing:
		s += fmt.Sprintf("%s[rebase] ", f.Styles.State)
	case gitstatus.AM:
		s += fmt.Sprintf("%s[am] ", f.Styles.State)
	case gitstatus.AMRebase:
		s += fmt.Sprintf("%s[am-rebase] ", f.Styles.State)
	case gitstatus.Merging:
		s += fmt.Sprintf("%s[merge] ", f.Styles.State)
	case gitstatus.CherryPicking:
		s += fmt.Sprintf("%s[cherry-pick] ", f.Styles.State)
	case gitstatus.Reverting:
		s += fmt.Sprintf("%s[revert] ", f.Styles.State)
	case gitstatus.Bisecting:
		s += fmt.Sprintf("%s[bisect] ", f.Styles.State)
	case gitstatus.Default:
		s += fmt.Sprintf("%s%s", f.Styles.Branch, f.Symbols.Branch)
	}

	s += f.currentRef()
	return s
}

func (f *Formater) remoteBranch() string {
	if f.st.RemoteBranch == "" {
		return ""
	}

	s := f.Styles.Clear

	branch := truncate(f.st.RemoteBranch, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
	s += fmt.Sprintf("%s%s", f.Styles.Remote, branch)
	return s
}

func (f *Formater) divergence() string {
	return f.divergenceString(f.Styles.Divergence, f.st.BehindCount, f.st.AheadCount)
}

// divergenceString renders behind/ahead divergence counts with the given
// style, honoring the swap_divergence and divergence_space options. It
// returns the empty string when both counts are zero.
func (f *Formater) divergenceString(style string, behind, ahead int) string {
	if behind == 0 && ahead == 0 {
		return ""
	}

	behindStr := ""
	aheadStr := ""
	s := f.Styles.Clear + style
	if behind != 0 {
		behindStr = fmt.Sprintf("%s%d", f.Symbols.Behind, behind)
	}

	if ahead != 0 {
		aheadStr = fmt.Sprintf("%s%d", f.Symbols.Ahead, ahead)
	}

	// Handle 'swap divergence'
	var left, right string
	if !f.Options.SwapDivergence {
		left, right = behindStr, aheadStr
	} else {
		left, right = aheadStr, behindStr
	}

	// Handle 'divergence space'
	space := ""
	if f.Options.DivergenceSpace && right != "" && left != "" {
		space = " "
	}
	return s + left + space + right
}

// pushTargetKnown reports whether a uniquely identified push destination is
// available.
func (f *Formater) pushTargetKnown() bool {
	return f.Push != nil && f.Push.TargetKnown
}

// pushBranch renders the push destination branch name (e.g. "fork/main"), or
// the unknown marker if the destination can't be uniquely determined.
func (f *Formater) pushBranch() string {
	if !f.pushTargetKnown() || f.Push.Target == "" {
		return f.unknown(f.Styles.Push)
	}

	branch := truncate(f.Push.Target, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
	return f.Styles.Clear + f.Styles.Push + branch
}

// pushDivergence renders the local branch divergence relative to its push
// destination.
//
// When the destination is known but its remote-tracking ref doesn't exist
// locally, the divergence can't be computed without fetching, so the unknown
// marker is shown.
//
// When the destination itself is unknown, the marker is only shown if
// showUnknownMarker is true. This lets the "push" alias avoid printing it
// twice (the branch component already reports the unknown state).
func (f *Formater) pushDivergence(showUnknownMarker bool) string {
	switch {
	case !f.pushTargetKnown():
		if !showUnknownMarker {
			return ""
		}
		return f.unknown(f.Styles.PushDivergence)
	case !f.Push.RefKnown:
		return f.unknown(f.Styles.PushDivergence)
	default:
		return f.divergenceString(f.Styles.PushDivergence, f.Push.Behind, f.Push.Ahead)
	}
}

// unknown renders the unknown marker using style, or nothing when the unknown
// symbol is empty.
func (f *Formater) unknown(style string) string {
	if f.Symbols.Unknown == "" {
		return ""
	}
	return f.Styles.Clear + style + f.Symbols.Unknown
}

func (f *Formater) currentRef() string {
	if f.st.IsDetached {
		return fmt.Sprintf("%s%s%s%s", f.Styles.Clear, f.Styles.Branch, f.Symbols.HashPrefix, f.st.HEAD)
	}

	branch := truncate(f.st.LocalBranch, f.Options.Ellipsis, f.Options.BranchMaxLen, f.Options.BranchTrim)
	return fmt.Sprintf("%s%s%s", f.Styles.Clear, f.Styles.Branch, branch)
}

// appendFlag appends a flag to the flags slice based on configuration options
func (f *Formater) appendFlag(flags []string, style, symbol string, count int) []string {
	if count == 0 {
		return flags
	}

	if f.Options.FlagsWithoutCount {
		// When flags_without_count is true, show symbol only (empty string if symbol is empty)
		if symbol == "" {
			return flags
		}
		return append(flags, fmt.Sprintf("%s%s", style, symbol))
	}

	// When flags_without_count is false, show symbol + count, or just count if symbol is empty
	return append(flags, fmt.Sprintf("%s%s%d", style, symbol, count))
}

func (f *Formater) flags() string {
	var flags []string
	if f.st.IsClean {
		// For stashed in clean state, handle empty symbols properly
		flags = f.appendFlag(flags, f.Styles.Stashed, f.Symbols.Stashed, f.st.NumStashed)

		// Clean flag only shows if symbol is not empty and hide_clean is false
		if !f.Options.HideClean && f.Symbols.Clean != "" {
			flags = append(flags, fmt.Sprintf("%s%s", f.Styles.Clean, f.Symbols.Clean))
		}

		if len(flags) != 0 {
			return f.Styles.Clear + strings.Join(flags, " ")
		}
	}

	// For all other flags, handle empty symbols properly
	flags = f.appendFlag(flags, f.Styles.Staged, f.Symbols.Staged, f.st.NumStaged)
	flags = f.appendFlag(flags, f.Styles.Conflict, f.Symbols.Conflict, f.st.NumConflicts)
	flags = f.appendFlag(flags, f.Styles.Modified, f.Symbols.Modified, f.st.NumModified)
	flags = f.appendFlag(flags, f.Styles.Stashed, f.Symbols.Stashed, f.st.NumStashed)
	flags = f.appendFlag(flags, f.Styles.Untracked, f.Symbols.Untracked, f.st.NumUntracked)

	if len(flags) > 0 {
		return f.Styles.Clear + strings.Join(flags, " ")
	}

	return ""
}

func (f *Formater) stats() string {
	stats := make([]string, 0, 2)

	if f.st.Insertions != 0 {
		stats = append(stats, fmt.Sprintf("%s%s%d", f.Styles.Insertions, f.Symbols.Insertions, f.st.Insertions))
	}

	if f.st.Deletions != 0 {
		stats = append(stats, fmt.Sprintf("%s%s%d", f.Styles.Deletions, f.Symbols.Deletions, f.st.Deletions))
	}

	if len(stats) == 0 {
		return ""
	}

	return f.Styles.Clear + strings.Join(stats, " ")
}
