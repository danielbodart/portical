/**
 * Replaced at build time by run.ts, with `bun build --define`. It is not a real
 * global, and outside a compiled binary it does not exist at all - which is why
 * this is a `typeof` test rather than a plain read.
 */
declare const PORTICAL_VERSION: string;

/**
 * What this build calls itself.
 *
 * The fallback is what you get from `bun run src/main.ts`, where there was no
 * build step to bake anything in. It says so, rather than guessing a number
 * that would then appear in a bug report as if it had been released.
 */
export const version: string = typeof PORTICAL_VERSION === "string" ? PORTICAL_VERSION : "development";
