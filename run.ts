#!/usr/bin/env bun
import { $ } from "bun";
import packageJson from "./package.json" with { type: "json" };

/**
 * The version, derived from the repository rather than stored in it.
 *
 * Only the major is committed, in package.json, because only the major is a
 * decision: it says this is the rewrite and the shell script was v1, and moving
 * it is something a person does deliberately when compatibility breaks. The
 * rest is a fact about the build, so recording it in the tree would only create
 * a number that can disagree with what was published - a bump commit to forget,
 * or a tag that says something the artifact does not.
 *
 * Minor is the commit count, so it only ever goes up and names exactly one
 * commit. Patch is the CI run number, which separates two builds of the same
 * commit - a re-run, or a manual dispatch - and locally becomes a timestamp
 * instead, so a developer build sorts after CI's and is obviously not one.
 */
export async function version(): Promise<string> {
  const major = packageJson.version.split(".")[0];

  // Counted from HEAD, not from a branch name. On Actions the checkout is
  // detached, so `git rev-parse --abbrev-ref HEAD` answers "HEAD", and
  // GITHUB_REF_NAME on a pull request is "123/merge", which is not a rev at
  // all. HEAD is the commit being built in every one of those cases.
  if ((await $`git rev-parse --is-shallow-repository`.quiet()).text().trim() === "true") {
    throw new Error(
      "this is a shallow clone, so the commit count is wrong and so is the version. " +
        "In Actions, checkout with `fetch-depth: 0`.",
    );
  }

  const revisions = (await $`git rev-list --count HEAD`.quiet()).text().trim();
  const build = process.env.GITHUB_RUN_NUMBER ||
    new Date().toISOString().replace(/[-:T]/g, "").split(".")[0];

  return `${major}.${revisions}.${build}`;
}

export async function check() {
  await $`bunx tsc --noEmit`;
}

export async function test(...args: string[]) {
  await $`bun test ${args}`;
}

/**
 * The version reaches the binary as a build-time define rather than a generated
 * file, so nothing is written into src/ that would then show up as a change.
 * src/version.ts falls back when the define is absent, which is what happens
 * under `bun run src/main.ts`.
 */
export async function build(outfile = "dist/portical") {
  const v = await version();
  await $`bun build --compile --minify --sourcemap --define ${`PORTICAL_VERSION="${v}"`} src/main.ts --outfile ${outfile}`;
  console.log(`built ${outfile} ${v}`);
}

/** The same image CI publishes, for both architectures, built locally. */
export async function image(tag = "danielbodart/portical:dev") {
  const v = await version();
  await $`docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=${v} --tag ${tag} .`;
}

const commands: Record<string, (...args: string[]) => Promise<unknown>> = {
  version: async () => console.log(await version()),
  check,
  test,
  build,
  image,
};

const [name = "build", ...args] = process.argv.slice(2);
const command = commands[name];

if (!command) {
  console.error(`Error: '${name}' is not a command. Try: ${Object.keys(commands).join(", ")}`);
  process.exit(2);
}

try {
  await command(...args);
} catch (error) {
  console.error(`Error: ${(error as Error).message}`);
  process.exit(1);
}
