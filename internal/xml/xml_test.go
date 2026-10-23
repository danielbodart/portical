package xml

import (
	"reflect"
	"testing"
)

func assertText(t *testing.T, document, name, want string) {
	t.Helper()
	got, ok := Text(document, name)
	if !ok {
		t.Fatalf("no <%s> in %q", name, document)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func assertAbsent(t *testing.T, document, name string) {
	t.Helper()
	if got, ok := Text(document, name); ok {
		t.Errorf("want no <%s>, got %q", name, got)
	}
}

func TestTextReadsAnElementsContent(t *testing.T) {
	assertText(t, "<a><b>hello</b></a>", "b", "hello")
}

// UPnP responses namespace everything, and routers disagree about prefixes.
func TestTextIgnoresTheNamespacePrefix(t *testing.T) {
	assertText(t, "<u:NewExternalIPAddress>1.2.3.4</u:NewExternalIPAddress>", "NewExternalIPAddress", "1.2.3.4")
}

func TestTextIgnoresAttributesOnTheOpeningTag(t *testing.T) {
	assertText(t, `<b xmlns="urn:x" id="1">hello</b>`, "b", "hello")
}

func TestTextReadsAnEmptyElement(t *testing.T) {
	assertText(t, "<a><b></b></a>", "b", "")
}

func TestTextReadsASelfClosingElementAsEmpty(t *testing.T) {
	assertText(t, "<a><b/></a>", "b", "")
}

func TestTextReportsAnAbsentElement(t *testing.T) {
	assertAbsent(t, "<a></a>", "b")
}

// NewRemoteHost and NewPortMappingDescription both routinely come back empty,
// and must not be confused with absent.
func TestTextDistinguishesEmptyFromAbsent(t *testing.T) {
	assertText(t, "<b></b>", "b", "")
	assertAbsent(t, "<c></c>", "b")
}

func TestTextDoesNotMatchAnElementWhoseNameMerelyStartsTheSame(t *testing.T) {
	assertAbsent(t, "<NewExternalPortRange>1</NewExternalPortRange>", "NewExternalPort")
}

func TestTextDecodesEntitiesInTheContent(t *testing.T) {
	assertText(t, "<b>Bob &amp; Alice&apos;s</b>", "b", "Bob & Alice's")
}

func TestTextTreatsANameWithRegexpCharactersLiterally(t *testing.T) {
	assertAbsent(t, "<ab>x</ab>", "a.")
}

func TestElementsReturnsEachMatchInDocumentOrder(t *testing.T) {
	got := Elements("<s><t>a</t></s><s><t>b</t></s>", "s")
	if want := []string{"<t>a</t>", "<t>b</t>"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestElementsReturnsNothingWhenThereAreNoMatches(t *testing.T) {
	if got := Elements("<a/>", "service"); len(got) != 0 {
		t.Errorf("got %q", got)
	}
}

func TestDecodeHandlesTheNamedEntities(t *testing.T) {
	if got := Decode("&amp;&lt;&gt;&quot;&apos;"); got != `&<>"'` {
		t.Errorf("got %q", got)
	}
}

func TestDecodeHandlesDecimalAndHexadecimalCharacterReferences(t *testing.T) {
	if got := Decode("&#65;&#x42;&#X43;"); got != "ABC" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeLeavesAnUnknownEntityAloneRatherThanDroppingIt(t *testing.T) {
	if got := Decode("&nbsp;"); got != "&nbsp;" {
		t.Errorf("got %q", got)
	}
}

// JavaScript's parseInt read the leading digits of `&#12ab;` and threw on a
// code point past Unicode's end. Neither names a character, so neither is
// decoded.
func TestDecodeLeavesAReferenceThatNamesNoCharacterAlone(t *testing.T) {
	for _, reference := range []string{"&#12ab;", "&#x110000;", "&#xD800;", "&#99999999999;"} {
		if got := Decode(reference); got != reference {
			t.Errorf("%s decoded to %q", reference, got)
		}
	}
}

func TestEncodeEscapesEverythingThatWouldBreakOutOfAnElement(t *testing.T) {
	if got := Encode(`<a & b> "c" 'd'`); got != "&lt;a &amp; b&gt; &quot;c&quot; &apos;d&apos;" {
		t.Errorf("got %q", got)
	}
}

// A container name reaches the router inside a description, and Docker
// permits characters there that would otherwise close the element early.
func TestEncodeRoundTripsThroughDecode(t *testing.T) {
	name := `portical: (80:80/tcp) <weird & "name">`
	if got := Decode(Encode(name)); got != name {
		t.Errorf("got %q", got)
	}
}
