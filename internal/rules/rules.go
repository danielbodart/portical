// Package rules reads the forward grammar shared by the container label and
// the unit file key: `8080:80/tcp,published,25565`.
package rules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/danielbodart/portical/internal/model"
)

// Terms is the result of reading a forward declaration.
//
// Published is kept separate from Rules because it cannot be resolved here -
// it means "whatever ports this container publishes on the host", or for a
// unit, whatever it is listening on, which only the source can answer.
type Terms struct {
	Rules     []model.Rule
	Published bool
	Errors    []string
}

// Published is the sentinel term meaning "forward every port this publishes".
const Published = "published"

// Anchored, unlike v1's pattern.
//
// v1 matched `([0-9]+)(:([0-9]+))?(/(tcp|udp))?` unanchored, so it would find a
// number anywhere and quietly forward it: `8O8O` (letter O) yielded a rule for
// port 8, and the README's own `19132/up` typo silently became "both
// protocols". Anchoring turns those into reported errors rather than a port
// nobody asked for.
var grammar = regexp.MustCompile(`(?i)^(\d{1,5})(?::(\d{1,5}))?(?:/(tcp|udp))?$`)

// MaxPort is the highest port there is.
const MaxPort = 65535

// port reads a port the pattern has already limited to five digits.
func port(text string) (int, bool) {
	value, err := strconv.Atoi(text)
	return value, err == nil && value >= 1 && value <= MaxPort
}

// Parse reads a declaration such as `8080:80/tcp,published,25565`.
//
// Terms compose freely: `published` is a term like any other, so
// `published,9999:80/tcp` means both. v1 only honoured `published` when it was
// the entire label value, and silently dropped it otherwise.
//
// A term with no protocol expands to both TCP and UDP, matching v1 and the
// documented behaviour of `25565:25565`.
func Parse(value string) Terms {
	var terms Terms
	var found []model.Rule

	for _, raw := range strings.Split(value, ",") {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}

		if strings.EqualFold(term, Published) {
			terms.Published = true
			continue
		}

		match := grammar.FindStringSubmatch(term)
		if match == nil {
			terms.Errors = append(terms.Errors, fmt.Sprintf("ignoring unrecognised rule '%s'", term))
			continue
		}

		external, externalOK := port(match[1])
		internal, internalOK := external, externalOK
		if match[2] != "" {
			internal, internalOK = port(match[2])
		}
		if !externalOK || !internalOK {
			terms.Errors = append(terms.Errors, fmt.Sprintf("ignoring rule '%s': ports must be between 1 and %d", term, MaxPort))
			continue
		}

		protocols := model.Protocols
		if match[3] != "" {
			protocols = []model.Protocol{model.Protocol(strings.ToLower(match[3]))}
		}
		for _, protocol := range protocols {
			found = append(found, model.Rule{ExternalPort: external, InternalPort: internal, Protocol: protocol})
		}
	}

	terms.Rules = Collapse(found)
	return terms
}

// Collapse drops later rules that would fight an earlier one for the same
// slot on the gateway.
//
// A label like `8080,8080:80` asks for two different internal ports behind one
// external port and protocol. The gateway has room for one, so without this the
// two rules overwrite each other on every pass - the exact deletion churn that
// issue #6 is about, self-inflicted. First wins, which keeps the result stable
// rather than dependent on iteration order.
//
// Sources use it again once `published` has been expanded, explicit rules
// first, so that what was written down beats what was inferred.
func Collapse(rules []model.Rule) []model.Rule {
	seen := map[string]bool{}
	kept := []model.Rule{}
	for _, rule := range rules {
		if seen[rule.Identity()] {
			continue
		}
		seen[rule.Identity()] = true
		kept = append(kept, rule)
	}
	return kept
}
