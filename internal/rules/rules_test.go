package rules

import (
	"reflect"
	"testing"

	"github.com/danielbodart/portical/internal/model"
)

func rule(external, internal int, protocol model.Protocol) model.Rule {
	return model.Rule{ExternalPort: external, InternalPort: internal, Protocol: protocol}
}

func assertRules(t *testing.T, got []model.Rule, want ...model.Rule) {
	t.Helper()
	if want == nil {
		want = []model.Rule{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandsABarePortToBothProtocols(t *testing.T) {
	assertRules(t, Parse("25565").Rules, rule(25565, 25565, model.TCP), rule(25565, 25565, model.UDP))
}

func TestMapsAnExternalPortToADifferentInternalPort(t *testing.T) {
	assertRules(t, Parse("9999:8000/tcp").Rules, rule(9999, 8000, model.TCP))
}

func TestHonoursAnExplicitProtocol(t *testing.T) {
	assertRules(t, Parse("19132/udp").Rules, rule(19132, 19132, model.UDP))
}

func TestReadsACommaSeparatedList(t *testing.T) {
	assertRules(t, Parse("19132/udp,8080/tcp").Rules, rule(19132, 19132, model.UDP), rule(8080, 8080, model.TCP))
}

func TestToleratesWhitespaceAndTrailingCommas(t *testing.T) {
	assertRules(t, Parse(" 8080/tcp , 9090/tcp ,").Rules, rule(8080, 8080, model.TCP), rule(9090, 9090, model.TCP))
}

func TestAcceptsAnUppercaseProtocol(t *testing.T) {
	assertRules(t, Parse("8080/TCP").Rules, rule(8080, 8080, model.TCP))
}

func TestPublishedIsRecognisedOnItsOwn(t *testing.T) {
	terms := Parse("published")
	if !terms.Published {
		t.Error("want published")
	}
	assertRules(t, terms.Rules)
}

// v1 only honoured `published` when it was the whole label value, so this
// combination silently forwarded 9999 and nothing else.
func TestPublishedComposesWithExplicitRules(t *testing.T) {
	terms := Parse("published,9999:80/tcp")
	if !terms.Published {
		t.Error("want published")
	}
	assertRules(t, terms.Rules, rule(9999, 80, model.TCP))
}

func TestRejectsRatherThanGuesses(t *testing.T) {
	for _, c := range []struct {
		name, value, error string
	}{
		// v1's unanchored regex found the `8` in `8O8O` and forwarded port 8.
		{"a port with a letter in it", "8O8O", "ignoring unrecognised rule '8O8O'"},
		// The README's own typo. v1 matched the number and forwarded both
		// protocols, so a request for UDP quietly opened TCP as well.
		{"a misspelled protocol", "19132/up", "ignoring unrecognised rule '19132/up'"},
		{"a port above the maximum", "70000/tcp", "ignoring rule '70000/tcp': ports must be between 1 and 65535"},
		{"an internal port above the maximum", "80:99999", "ignoring rule '80:99999': ports must be between 1 and 65535"},
		{"port zero", "0/tcp", "ignoring rule '0/tcp': ports must be between 1 and 65535"},
		// JavaScript's \d is ASCII too, but saying so keeps it that way.
		{"digits from another script", "٨٠", "ignoring unrecognised rule '٨٠'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			terms := Parse(c.value)
			assertRules(t, terms.Rules)
			if !reflect.DeepEqual(terms.Errors, []string{c.error}) {
				t.Errorf("got %q", terms.Errors)
			}
		})
	}
}

func TestKeepsTheRulesItCouldUnderstand(t *testing.T) {
	terms := Parse("nonsense,8080/tcp")
	assertRules(t, terms.Rules, rule(8080, 8080, model.TCP))
	if len(terms.Errors) != 1 {
		t.Errorf("got %q", terms.Errors)
	}
}

// Two rules cannot share one (protocol, external port) slot on the gateway.
// Left alone they overwrite each other every pass, which is issue #6's
// deletion churn arriving via the label rather than the router.
func TestCollapsesRulesThatCollideOnTheGatewayFirstTermWinning(t *testing.T) {
	assertRules(t, Parse("8080:80/tcp,8080:90/tcp").Rules, rule(8080, 80, model.TCP))
}

func TestDoesNotTreatDifferentProtocolsAsACollision(t *testing.T) {
	if got := Parse("8080:80/tcp,8080:90/udp").Rules; len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestAnEmptyDeclarationAsksForNothing(t *testing.T) {
	terms := Parse(" , ")
	assertRules(t, terms.Rules)
	if terms.Published || len(terms.Errors) != 0 {
		t.Errorf("got %+v", terms)
	}
}
