import assert from "node:assert/strict";
import test from "node:test";

import { definitiveExecutionFailures } from "./output-validation.mjs";

test("rejects native dependency and install failures hidden behind a zero exit", () => {
  const stderr = `Resolving dependencies
gyp ERR! configure error
error: install script from "tree-sitter-powershell" exited with 1
error: script "postinstall" exited with code 2`;
  assert.deepEqual(definitiveExecutionFailures(stderr), [
    "gyp ERR! configure error",
    'error: install script from "tree-sitter-powershell" exited with 1',
    'error: script "postinstall" exited with code 2',
  ]);
});

test("accepts ordinary benchmark progress on stderr", () => {
  const stderr = `debconf: delaying package configuration, since apt-utils is not installed
Resolving dependencies
Resolved, downloaded and extracted [537]
$ bun turbo typecheck`;
  assert.deepEqual(definitiveExecutionFailures(stderr), []);
});

test("requires a string so malformed evidence cannot pass silently", () => {
  assert.throws(() => definitiveExecutionFailures(undefined), /stderr must be a string/);
});
