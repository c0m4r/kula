package main

import (
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The SARIF 2.1.0 report has one run whose rules are the check catalog.
// FAIL and WARN findings are its results, an ignored one suppressed; PASS and
// SKIP are not results, and an ERROR (a probe that could not complete) is a
// tool execution notification. It goes through the JSON report's
// terminal-safe encoder.

const (
	sarifVersion = "2.1.0"
	sarifSchema  = "https://docs.oasis-open.org/sarif/sarif/v2.1.0/errata01/os/schemas/sarif-schema-2.1.0.json"
	projectURL   = "https://github.com/c0m4r/kula"
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool        sarifTool         `json:"tool"`
	Invocations []sarifInvocation `json:"invocations"`
	Results     []sarifResult     `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	ShortDescription sarifMessage   `json:"shortDescription"`
	FullDescription  sarifMessage   `json:"fullDescription"`
	Properties       sarifRuleProps `json:"properties"`
}

type sarifRuleProps struct {
	Category string   `json:"category"`
	Mode     string   `json:"mode"`
	Tags     []string `json:"tags"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID       string             `json:"ruleId"`
	RuleIndex    int                `json:"ruleIndex"`
	Level        string             `json:"level"`
	Message      sarifMessage       `json:"message"`
	Locations    []sarifLocation    `json:"locations"`
	Suppressions []sarifSuppression `json:"suppressions,omitempty"`
	Properties   sarifResultProps   `json:"properties"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifSuppression struct {
	Kind string `json:"kind"`
}

type sarifResultProps struct {
	Severity         string `json:"severity"`
	Status           string `json:"status"`
	SecuritySeverity string `json:"security-severity"`
	Expectation      string `json:"expectation,omitempty"`
}

type sarifInvocation struct {
	ExecutionSuccessful bool                 `json:"executionSuccessful"`
	ExitCode            int                  `json:"exitCode"`
	ExitCodeDescription string               `json:"exitCodeDescription,omitempty"`
	StartTimeUTC        string               `json:"startTimeUtc,omitempty"`
	EndTimeUTC          string               `json:"endTimeUtc,omitempty"`
	Notifications       []sarifNotification  `json:"toolExecutionNotifications,omitempty"`
	Properties          sarifInvocationProps `json:"properties"`
}

// sarifInvocationProps records what shaped the run, as the JSON report does.
type sarifInvocationProps struct {
	Options     ReportOptions `json:"options"`
	TargetInfo  TargetInfo    `json:"target_info"`
	Interrupted bool          `json:"interrupted"`
	NotRun      int           `json:"not_run,omitempty"`
}

type sarifNotification struct {
	Level          string                  `json:"level"`
	Message        sarifMessage            `json:"message"`
	AssociatedRule sarifRuleReference      `json:"associatedRule"`
	Properties     *sarifNotificationProps `json:"properties,omitempty"`
}

type sarifRuleReference struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}

type sarifNotificationProps struct {
	Ignored bool `json:"ignored"`
}

// writeSARIF renders the report as SARIF 2.1.0.
func (r *Report) writeSARIF(w io.Writer, failOn Severity) error {
	code := r.exitCode(failOn)
	rules, ruleIndex := sarifRules(r.Findings)

	results := []sarifResult{}
	var notifications []sarifNotification
	for _, f := range r.Findings {
		switch f.Status {
		case StatusFail, StatusWarn:
			result := sarifResult{
				RuleID:    f.ID,
				RuleIndex: ruleIndex[f.ID],
				Level:     sarifLevel(f, failOn),
				Message:   sarifMessage{Text: sarifText(f)},
				Locations: []sarifLocation{{PhysicalLocation: sarifPhysicalLocation{
					ArtifactLocation: sarifArtifactLocation{URI: r.Target},
				}}},
				Properties: sarifResultProps{
					Severity:         f.Severity.String(),
					Status:           f.Status.String(),
					SecuritySeverity: securitySeverity(f.Severity),
					Expectation:      f.Expectation,
				},
			}
			if f.Ignored {
				result.Suppressions = []sarifSuppression{{Kind: "external"}}
			}
			results = append(results, result)
		case StatusError:
			note := sarifNotification{
				Level:          "error",
				Message:        sarifMessage{Text: sarifText(f)},
				AssociatedRule: sarifRuleReference{ID: f.ID, Index: ruleIndex[f.ID]},
			}
			if f.Ignored {
				note.Level = "warning"
				note.Properties = &sarifNotificationProps{Ignored: true}
			}
			notifications = append(notifications, note)
		}
	}

	invocation := sarifInvocation{
		ExecutionSuccessful: code != exitUsage && code != exitInconclusive && code != exitInterrupted,
		ExitCode:            code,
		ExitCodeDescription: r.exitReason(failOn),
		Notifications:       notifications,
		Properties: sarifInvocationProps{
			Options:     r.Options,
			TargetInfo:  r.TargetInfo,
			Interrupted: r.Interrupted,
			NotRun:      r.NotRun,
		},
	}
	if !r.Started.IsZero() {
		invocation.StartTimeUTC = r.Started.UTC().Format(time.RFC3339Nano)
		end := r.Started.Add(time.Duration(r.DurationMS) * time.Millisecond)
		invocation.EndTimeUTC = end.UTC().Format(time.RFC3339Nano)
	}

	return writeTerminalSafeJSON(w, sarifLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           "kula-scan",
				Version:        r.Scanner.Version,
				InformationURI: projectURL,
				Rules:          rules,
			}},
			Invocations: []sarifInvocation{invocation},
			Results:     results,
		}},
	})
}

// sarifRules returns a rule per catalog entry, in catalog order, and the
// index of each rule by ID. A finding missing from the catalog gets a
// stand-in rule, so every result names a rule.
func sarifRules(findings []Finding) ([]sarifRule, map[string]int) {
	rules := make([]sarifRule, 0, len(catalog))
	index := make(map[string]int, len(catalog))
	add := func(e catalogEntry) {
		index[e.ID] = len(rules)
		rules = append(rules, sarifRule{
			ID:               e.ID,
			Name:             ruleName(e.Title),
			ShortDescription: sarifMessage{Text: e.Title},
			FullDescription:  sarifMessage{Text: e.Description},
			Properties: sarifRuleProps{
				Category: e.Category,
				Mode:     e.Mode.String(),
				Tags:     []string{"security", e.Category},
			},
		})
	}
	for _, e := range catalog {
		add(e)
	}
	for _, f := range findings {
		if _, ok := index[f.ID]; !ok {
			e := catalogEntryFor(f.ID)
			if e.Description == "" {
				e.Description = e.Title
			}
			add(e)
		}
	}
	return rules, index
}

// sarifLevel is error for a FAIL that gates the exit status, warning for a
// FAIL below -fail-on and note for a WARN. An ignored FAIL keeps the level it
// would have; its suppression says it was accepted.
func sarifLevel(f Finding, failOn Severity) string {
	if f.Status == StatusWarn {
		return "note"
	}
	accepted := f
	accepted.Ignored = false
	if gates(accepted, failOn) {
		return "error"
	}
	return "warning"
}

// sarifText is a finding's detail, or its title when it has none, followed by
// its evidence and remediation.
func sarifText(f Finding) string {
	text := f.Detail
	if text == "" {
		text = f.Title
	}
	if f.Evidence != "" {
		text += "\nEvidence: " + f.Evidence
	}
	if f.Remediation != "" {
		text += "\nRemediation: " + f.Remediation
	}
	return text
}

// securitySeverity maps a severity to the CVSS-like score SARIF consumers
// read from a security-severity property.
func securitySeverity(sev Severity) string {
	switch sev {
	case SevCritical:
		return "9.5"
	case SevHigh:
		return "8.0"
	case SevMedium:
		return "5.5"
	case SevLow:
		return "3.0"
	default:
		return "0.0"
	}
}

// ruleName turns a title into the PascalCase identifier SARIF wants as a
// rule name: "HSTS over TLS" becomes "HSTSOverTLS".
func ruleName(title string) string {
	var b strings.Builder
	words := strings.FieldsFunc(title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		b.WriteRune(unicode.ToUpper(first))
		b.WriteString(word[size:])
	}
	return b.String()
}
