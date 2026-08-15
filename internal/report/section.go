package report

import (
	"fmt"
	"strings"
)

// Section names one slice of the report.
//
// It is a named selector rather than a set of booleans so that asking for two
// things at once is not representable, and so that --help can list what there is
// without the flag set growing every time the report gains a heading.
type Section string

const (
	// SectionAll is the whole report, which is what an unnarrowed run renders.
	SectionAll Section = "all"
	// SectionMovers is the what-moved write-up, which is the common question an
	// agent asks and by far the cheapest answer to it.
	SectionMovers Section = "movers"
	// SectionRepos is the every-repo table and the package churn under it.
	SectionRepos Section = "repos"
	// SectionProblems is the repos that did not report clean.
	SectionProblems Section = "problems"
)

// sections is the one list. ParseSection validates against it and its error
// message is built from it, so a new section cannot be accepted by the parser
// while going unmentioned in the error, or the other way round.
var sections = []Section{SectionAll, SectionMovers, SectionRepos, SectionProblems}

// ParseSection turns a command-line value into a Section.
//
// An unknown name is an error rather than a silent fallback to the whole
// report. A typo that quietly renders everything is the same shape of lie this
// tool exists to refuse: the caller asked a narrow question and got an answer to
// a different one, with nothing saying so.
func ParseSection(s string) (Section, error) {
	for _, sec := range sections {
		if Section(s) == sec {
			return sec, nil
		}
	}
	return "", fmt.Errorf("report: unknown section %q, valid sections are %s", s, sectionList())
}

// sectionList renders the valid names for an error message or help text.
func sectionList() string {
	names := make([]string, 0, len(sections))
	for _, sec := range sections {
		names = append(names, string(sec))
	}
	return strings.Join(names, ", ")
}

// Sections lists the valid section names, in the order help text should show
// them. It exists so the CLI's help cannot drift from what ParseSection accepts.
func Sections() []string { return strings.Split(sectionList(), ", ") }

// shows reports whether rendering sec should include the named part.
func (s Section) shows(part Section) bool { return s == SectionAll || s == part }

// ShowMovers, ShowRepos and ShowProblems are what the markdown template branches
// on. They are methods on View rather than raw list emptiness because an empty
// list and an unrequested one have to render differently: "nothing cleared the
// reporting threshold" is a finding, and printing it under --section repos would
// be a finding nobody made.
func (v View) ShowMovers() bool { return v.Section.shows(SectionMovers) }

// ShowRepos reports whether the every-repo table belongs in this rendering.
func (v View) ShowRepos() bool { return v.Section.shows(SectionRepos) }

// ShowProblems reports whether the collection-problems list belongs in this
// rendering.
func (v View) ShowProblems() bool { return v.Section.shows(SectionProblems) }
