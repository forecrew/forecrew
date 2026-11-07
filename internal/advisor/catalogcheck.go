package advisor

import (
	"regexp"
	"slices"
	"strings"

	"github.com/forecrew/forecrew/internal/contextpack"
)

// toolCheck is what a proposal's tools are checked against besides the
// catalog: the tools agents were found lacking, and whether it answers one.
type toolCheck struct {
	lacking        []string // missing tools named by the signals
	answersTooling bool     // it answers a tooling signal
}

// checkFor is the check of a draft citing cited out of signals: every
// missing tool of the brief counts, not only the cited ones.
func checkFor(cited []Signal, signals []Signal) toolCheck {
	var c toolCheck
	for _, s := range signals {
		if s.Detector == DetectorTooling {
			c.lacking = append(c.lacking, s.Subject)
		}
	}
	c.answersTooling = slices.ContainsFunc(cited, func(s Signal) bool { return s.Detector == DetectorTooling })
	return c
}

// checkOfKey is the check of a stored proposal, from its dedupe key: a
// revision has no brief, only the signal it answers.
func checkOfKey(key string) toolCheck {
	if tool, ok := strings.CutPrefix(key, DetectorTooling+":"); ok {
		return toolCheck{lacking: []string{tool}, answersTooling: true}
	}
	return toolCheck{}
}

// tools says what is wrong with the tools a draft uses, "" when nothing is.
// Every name in tools is a catalog entry; a draft answering a missing tool
// names one; and the action text uses no missing tool and installs or runs
// nothing outside the catalog.
func (s *Service) tools(d Draft, c toolCheck) string {
	for _, t := range d.Tools {
		if !s.Catalog.Has(t) {
			return contextpack.OneLine(t) + " is not in the catalog"
		}
	}
	if c.answersTooling && len(d.Tools) == 0 {
		return "it answers a missing tool with no tool of the catalog"
	}
	text := actionText(d.Action)
	for _, tool := range c.lacking {
		if !s.Catalog.knows(tool) && mentions(text, tool) {
			return "its action uses " + tool + ", which is not in the catalog"
		}
	}
	for _, name := range installed(text) {
		if !s.Catalog.knows(name) {
			return "its action installs or runs " + name + ", which is not in the catalog"
		}
	}
	return ""
}

// mentions reports whether text has word as a whole word, ignoring case.
func mentions(text, word string) bool {
	re, err := regexp.Compile(`(?i)(^|[^\w.+-])` + regexp.QuoteMeta(word) + `($|[^\w+-])`)
	return err == nil && re.MatchString(text)
}

// installRules find what a command in an action installs or runs; the
// first group is the package, with its version if any. Flags are skipped.
var installRules = []*regexp.Regexp{
	regexp.MustCompile(`(?im)\b(?:pip3?|pipx|uv pip|uv tool|npm|pnpm|yarn|bun|brew|apt|apt-get|apk|dnf|yum|gem|cargo|go)[ \t]+(?:install|add|i)[ \t]+(?:-{1,2}[\w-]+(?:=\S+)?[ \t]+)*([@\w][\w@./:=+~-]*)`),
	regexp.MustCompile(`(?im)\b(?:npx|pnpx|bunx|uvx|pipx run)[ \t]+(?:-{1,2}[\w-]+[ \t]+)*([@\w][\w@./+~-]*)`),
}

// dockerRule finds the arguments of docker run and docker pull, whose image
// is the first one that is not a flag or the value of one.
var dockerRule = regexp.MustCompile(`(?im)\bdocker[ \t]+(?:run|pull)[ \t]+([^\n]*)`)

// valueFlags are the docker flags that take the next argument as a value.
var valueFlags = strings.Fields("-v --volume -e --env -p --publish -w --workdir --name --entrypoint --network -u --user --platform --mount --env-file -l --label")

// installed returns the packages and images the commands in text install
// or run, without versions, lowercase.
func installed(text string) []string {
	var out []string
	add := func(name string) {
		if name = baseName(name); name != "" && !strings.ContainsAny(name[:1], "./") && !slices.Contains(stopWords, name) && !isFile(name) {
			out = append(out, name)
		}
	}
	for _, rule := range installRules {
		for _, m := range rule.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
	}
	for _, m := range dockerRule.FindAllStringSubmatch(text, -1) {
		args := strings.Fields(m[1])
		for i := 0; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-") {
				if slices.Contains(valueFlags, args[i]) {
					i++
				}
				continue
			}
			add(args[i])
			break
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// baseName is a package or image without its version or tag, lowercase:
// @scope/pkg@1 is @scope/pkg, pkg==1 is pkg, repo/image:tag is repo/image.
func baseName(name string) string {
	name = strings.ToLower(strings.Trim(name, "`'\",;"))
	if i := strings.LastIndex(name, "@"); i > 0 {
		name = name[:i]
	}
	for _, sep := range []string{"==", ">=", "<=", "~="} {
		name, _, _ = strings.Cut(name, sep)
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return name
}

// isFile reports whether a name is a file a command reads, not a package.
func isFile(name string) bool {
	for _, ext := range []string{".txt", ".json", ".toml", ".lock", ".yaml", ".yml", ".cfg", ".sh"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// knows reports whether a tool, package or image is one of the catalog's:
// by its name, the last part of its path, or as the package or image an
// entry installs.
func (c Catalog) knows(name string) bool {
	name = baseName(name)
	if c.Has(name) || c.Has(name[strings.LastIndex(name, "/")+1:]) {
		return true
	}
	for _, t := range c {
		for _, f := range strings.Fields(t.Install) {
			if baseName(f) == name {
				return true
			}
		}
	}
	return false
}
