import { PROTOCOLS, type Protocol, type Rule } from "./model.ts";

/**
 * The result of reading a `portical.upnp.forward` label.
 *
 * `published` is kept separate from `rules` because it cannot be resolved
 * here - it means "whatever ports this container publishes on the host", which
 * only the Docker client can answer.
 */
export interface LabelTerms {
  readonly rules: readonly Rule[];
  readonly published: boolean;
  readonly errors: readonly string[];
}

/** The sentinel term meaning "forward every port this container publishes". */
export const PUBLISHED = "published";

/**
 * Anchored, unlike v1's pattern.
 *
 * v1 matched `([0-9]+)(:([0-9]+))?(/(tcp|udp))?` unanchored, so it would find a
 * number anywhere and quietly forward it: `8O8O` (letter O) yielded a rule for
 * port 8, and the README's own `19132/up` typo silently became "both
 * protocols". Anchoring turns those into reported errors rather than a port
 * nobody asked for.
 */
const RULE = /^(\d{1,5})(?::(\d{1,5}))?(?:\/(tcp|udp))?$/i;

const MAX_PORT = 65535;

function port(text: string): number | undefined {
  const value = Number(text);
  return value >= 1 && value <= MAX_PORT ? value : undefined;
}

/**
 * Parse a label value such as `8080:80/tcp,published,25565`.
 *
 * Terms compose freely: `published` is a term like any other, so
 * `published,9999:80/tcp` means both. v1 only honoured `published` when it was
 * the entire label value, and silently dropped it otherwise.
 *
 * A term with no protocol expands to both TCP and UDP, matching v1 and the
 * documented behaviour of `25565:25565`.
 */
export function parseLabel(value: string): LabelTerms {
  const rules: Rule[] = [];
  const errors: string[] = [];
  let published = false;

  for (const raw of value.split(",")) {
    const term = raw.trim();
    if (term === "") continue;

    if (term.toLowerCase() === PUBLISHED) {
      published = true;
      continue;
    }

    const match = RULE.exec(term);
    if (!match) {
      errors.push(`ignoring unrecognised rule '${term}'`);
      continue;
    }

    const [, externalText, internalText, protocolText] = match;
    const externalPort = port(externalText!);
    const internalPort = internalText === undefined ? externalPort : port(internalText);

    if (externalPort === undefined || internalPort === undefined) {
      errors.push(`ignoring rule '${term}': ports must be between 1 and ${MAX_PORT}`);
      continue;
    }

    const protocols = protocolText
      ? [protocolText.toLowerCase() as Protocol]
      : PROTOCOLS;

    for (const protocol of protocols) {
      rules.push({ externalPort, internalPort, protocol });
    }
  }

  return { rules: dedupe(rules), published, errors };
}

/**
 * Collapse rules that would fight over the same slot on the gateway.
 *
 * A label like `8080,8080:80` asks for two different internal ports behind one
 * external port and protocol. The gateway has room for one, so without this the
 * two rules overwrite each other on every pass - the exact deletion churn that
 * issue #6 is about, self-inflicted. First term wins, which keeps the result
 * stable rather than dependent on iteration order.
 */
function dedupe(rules: readonly Rule[]): Rule[] {
  const byIdentity = new Map<string, Rule>();
  for (const rule of rules) {
    const key = `${rule.protocol}/${rule.externalPort}`;
    if (!byIdentity.has(key)) byIdentity.set(key, rule);
  }
  return [...byIdentity.values()];
}
