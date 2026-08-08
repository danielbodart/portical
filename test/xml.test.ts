import { describe, expect, test } from "bun:test";
import { decode, elements, encode, text } from "../src/xml.ts";

describe("text", () => {
  test("reads an element's content", () => {
    expect(text("<a><b>hello</b></a>", "b")).toBe("hello");
  });

  // UPnP responses namespace everything, and routers disagree about prefixes.
  test("ignores the namespace prefix", () => {
    expect(text("<u:NewExternalIPAddress>1.2.3.4</u:NewExternalIPAddress>", "NewExternalIPAddress"))
      .toBe("1.2.3.4");
  });

  test("ignores attributes on the opening tag", () => {
    expect(text(`<b xmlns="urn:x" id="1">hello</b>`, "b")).toBe("hello");
  });

  test("reads an empty element", () => {
    expect(text("<a><b></b></a>", "b")).toBe("");
  });

  test("reads a self-closing element as empty", () => {
    expect(text("<a><b/></a>", "b")).toBe("");
  });

  test("returns undefined when the element is absent", () => {
    expect(text("<a></a>", "b")).toBeUndefined();
  });

  // NewRemoteHost and NewPortMappingDescription both routinely come back
  // empty, and must not be confused with absent.
  test("distinguishes empty from absent", () => {
    expect(text("<b></b>", "b")).toBe("");
    expect(text("<c></c>", "b")).toBeUndefined();
  });

  test("does not match an element whose name merely starts the same", () => {
    expect(text("<NewExternalPortRange>1</NewExternalPortRange>", "NewExternalPort")).toBeUndefined();
  });

  test("decodes entities in the content", () => {
    expect(text("<b>Bob &amp; Alice&apos;s</b>", "b")).toBe("Bob & Alice's");
  });
});

describe("elements", () => {
  test("returns each match in document order", () => {
    expect(elements("<s><t>a</t></s><s><t>b</t></s>", "s")).toEqual(["<t>a</t>", "<t>b</t>"]);
  });

  test("returns nothing when there are no matches", () => {
    expect(elements("<a/>", "service")).toEqual([]);
  });
});

describe("decode", () => {
  test("handles the named entities", () => {
    expect(decode("&amp;&lt;&gt;&quot;&apos;")).toBe(`&<>"'`);
  });

  test("handles decimal and hexadecimal character references", () => {
    expect(decode("&#65;&#x42;")).toBe("AB");
  });

  test("leaves an unknown entity alone rather than dropping it", () => {
    expect(decode("&nbsp;")).toBe("&nbsp;");
  });
});

describe("encode", () => {
  test("escapes everything that would break out of an element", () => {
    expect(encode(`<a & b> "c" 'd'`)).toBe("&lt;a &amp; b&gt; &quot;c&quot; &apos;d&apos;");
  });

  // A container name reaches the router inside a description, and Docker
  // permits characters there that would otherwise close the element early.
  test("round-trips through decode", () => {
    const name = `portical: (80:80/tcp) <weird & "name">`;
    expect(decode(encode(name))).toBe(name);
  });
});
