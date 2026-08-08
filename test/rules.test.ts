import { describe, expect, test } from "bun:test";
import { parseLabel } from "../src/rules.ts";

describe("parseLabel", () => {
  test("expands a bare port to both protocols", () => {
    expect(parseLabel("25565").rules).toEqual([
      { externalPort: 25565, internalPort: 25565, protocol: "tcp" },
      { externalPort: 25565, internalPort: 25565, protocol: "udp" },
    ]);
  });

  test("maps an external port to a different internal port", () => {
    expect(parseLabel("9999:8000/tcp").rules).toEqual([
      { externalPort: 9999, internalPort: 8000, protocol: "tcp" },
    ]);
  });

  test("honours an explicit protocol", () => {
    expect(parseLabel("19132/udp").rules).toEqual([
      { externalPort: 19132, internalPort: 19132, protocol: "udp" },
    ]);
  });

  test("reads a comma separated list", () => {
    expect(parseLabel("19132/udp,8080/tcp").rules).toEqual([
      { externalPort: 19132, internalPort: 19132, protocol: "udp" },
      { externalPort: 8080, internalPort: 8080, protocol: "tcp" },
    ]);
  });

  test("tolerates whitespace and trailing commas", () => {
    expect(parseLabel(" 8080/tcp , 9090/tcp ,").rules).toEqual([
      { externalPort: 8080, internalPort: 8080, protocol: "tcp" },
      { externalPort: 9090, internalPort: 9090, protocol: "tcp" },
    ]);
  });

  test("accepts an uppercase protocol", () => {
    expect(parseLabel("8080/TCP").rules).toEqual([
      { externalPort: 8080, internalPort: 8080, protocol: "tcp" },
    ]);
  });

  describe("published", () => {
    test("is recognised on its own", () => {
      const terms = parseLabel("published");
      expect(terms.published).toBe(true);
      expect(terms.rules).toEqual([]);
    });

    // v1 only honoured `published` when it was the whole label value, so this
    // combination silently forwarded 9999 and nothing else.
    test("composes with explicit rules", () => {
      const terms = parseLabel("published,9999:80/tcp");
      expect(terms.published).toBe(true);
      expect(terms.rules).toEqual([
        { externalPort: 9999, internalPort: 80, protocol: "tcp" },
      ]);
    });
  });

  describe("rejects rather than guesses", () => {
    // v1's unanchored regex found the `8` in `8O8O` and forwarded port 8.
    test("a port with a letter in it", () => {
      const terms = parseLabel("8O8O");
      expect(terms.rules).toEqual([]);
      expect(terms.errors).toEqual(["ignoring unrecognised rule '8O8O'"]);
    });

    // The README's own typo. v1 matched the number and forwarded both
    // protocols, so a request for UDP quietly opened TCP as well.
    test("a misspelled protocol", () => {
      const terms = parseLabel("19132/up");
      expect(terms.rules).toEqual([]);
      expect(terms.errors).toEqual(["ignoring unrecognised rule '19132/up'"]);
    });

    test("a port above the maximum", () => {
      const terms = parseLabel("70000/tcp");
      expect(terms.rules).toEqual([]);
      expect(terms.errors).toEqual([
        "ignoring rule '70000/tcp': ports must be between 1 and 65535",
      ]);
    });

    test("port zero", () => {
      expect(parseLabel("0/tcp").rules).toEqual([]);
    });

    test("but keeps the rules it could understand", () => {
      const terms = parseLabel("nonsense,8080/tcp");
      expect(terms.rules).toEqual([
        { externalPort: 8080, internalPort: 8080, protocol: "tcp" },
      ]);
      expect(terms.errors).toHaveLength(1);
    });
  });

  // Two rules cannot share one (protocol, external port) slot on the gateway.
  // Left alone they overwrite each other every pass, which is issue #6's
  // deletion churn arriving via the label rather than the router.
  test("collapses rules that collide on the gateway, first term winning", () => {
    expect(parseLabel("8080:80/tcp,8080:90/tcp").rules).toEqual([
      { externalPort: 8080, internalPort: 80, protocol: "tcp" },
    ]);
  });

  test("does not treat different protocols as a collision", () => {
    expect(parseLabel("8080:80/tcp,8080:90/udp").rules).toHaveLength(2);
  });
});
