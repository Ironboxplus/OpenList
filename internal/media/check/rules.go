package check

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/syncjob"
	"github.com/rclone/rclone/fs/filter"
	"gopkg.in/yaml.v3"
)

// Rule overrides the defaults for one work, keyed by its path in rules.yaml.
type Rule struct {
	Files   []string `yaml:"files"`   // rclone filter rules, relative to the work
	Episode string   `yaml:"episode"` // regexp with a named group ep, optionally season
	Expect  string   `yaml:"expect"`  // "auto" or "S1: 1-12; S2: 1-13"
	Subs    struct {
		Langs []string `yaml:"langs"`
		Mode  string   `yaml:"mode"` // external | embedded | any | none
	} `yaml:"subs"`
}

// Rules maps a work's path to its Rule.
type Rules map[string]Rule

// LoadRules reads a rules.yaml.
func LoadRules(path string) (Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Rules
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := Rules{}
	for k, v := range r {
		out[strings.TrimRight(k, "/")] = v
	}
	return out, nil
}

// compiled is a Rule with its pieces parsed once.
type compiled struct {
	filter   *filter.Filter
	epRe     *regexp.Regexp
	epIdx    int
	seasonIx int
	expect   map[int]map[int]bool // nil = auto
	langs    []string
	mode     string
}

func compileRule(r Rule) (*compiled, error) {
	c := &compiled{mode: "any", langs: lowerAll(r.Subs.Langs)}
	if m := strings.ToLower(strings.TrimSpace(r.Subs.Mode)); m != "" {
		switch m {
		case "external", "embedded", "any", "none":
			c.mode = m
		default:
			return nil, fmt.Errorf("subs.mode %q: want external, embedded, any or none", r.Subs.Mode)
		}
	}
	if len(r.Files) > 0 {
		rules := append([]string(nil), r.Files...)
		// A "+" rule means "only these": rclone's rule list doesn't close
		// itself with "- **" the way its --include flag does, so do it here.
		for _, x := range rules {
			if strings.HasPrefix(strings.TrimSpace(x), "+") {
				rules = append(rules, "- **")
				break
			}
		}
		f, err := syncjob.NewFilter(rules)
		if err != nil {
			return nil, fmt.Errorf("files: %w", err)
		}
		c.filter = f
	}
	if r.Episode != "" {
		re, err := regexp.Compile(r.Episode)
		if err != nil {
			return nil, fmt.Errorf("episode: %w", err)
		}
		c.epIdx, c.seasonIx = re.SubexpIndex("ep"), re.SubexpIndex("season")
		if c.epIdx < 0 {
			return nil, fmt.Errorf("episode: the pattern needs a named group (?P<ep>...)")
		}
		c.epRe = re
	}
	exp, err := ParseExpect(r.Expect)
	if err != nil {
		return nil, err
	}
	c.expect = exp
	return c, nil
}

func lowerAll(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

var expectPartRe = regexp.MustCompile(`(?i)^(?:S(\d+)\s*:)?\s*(.+)$`)

// ParseExpect reads "S1: 1-12; S2: 1-13, 15" into season -> episode set.
// "" and "auto" give nil: ask the provider.
func ParseExpect(s string) (map[int]map[int]bool, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "auto") {
		return nil, nil
	}
	out := map[int]map[int]bool{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		m := expectPartRe.FindStringSubmatch(part)
		season := 1
		if m[1] != "" {
			season, _ = strconv.Atoi(m[1])
		}
		if out[season] == nil {
			out[season] = map[int]bool{}
		}
		for _, rng := range strings.Split(m[2], ",") {
			lo, hi, err := parseRange(strings.TrimSpace(rng))
			if err != nil {
				return nil, fmt.Errorf("expect %q: %w", s, err)
			}
			for e := lo; e <= hi; e++ {
				out[season][e] = true
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("expect %q: nothing in it", s)
	}
	return out, nil
}

func parseRange(s string) (lo, hi int, err error) {
	a, b, isRange := strings.Cut(s, "-")
	if lo, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
		return 0, 0, fmt.Errorf("bad number %q", s)
	}
	hi = lo
	if isRange {
		if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
			return 0, 0, fmt.Errorf("bad range %q", s)
		}
	}
	if lo < 1 || hi < lo || hi-lo > 2000 {
		return 0, 0, fmt.Errorf("bad range %q", s)
	}
	return lo, hi, nil
}

// compressEps renders sorted numbers as "1-3, 5, 7-8".
func compressEps(ns []int) string {
	sort.Ints(ns)
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] == ns[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(ns[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ns[i], ns[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}
