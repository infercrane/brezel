const definitiveFailurePatterns = [
  /^gyp ERR!/,
  /^error: install script from .* exited with [1-9][0-9]*$/,
  /^error: script .* exited with code [1-9][0-9]*$/,
];

// Upstream DAX currently redirects package-manager stderr through a process
// substitution. Some native dependency failures can therefore be printed
// while the enclosing benchmark still exits zero. Keep the timed workload
// byte-for-byte identical, but reject its report when stderr contains an
// unambiguous failed build or install record.
export function definitiveExecutionFailures(stderr) {
  if (typeof stderr !== "string") throw new TypeError("stderr must be a string");
  return stderr
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => definitiveFailurePatterns.some((pattern) => pattern.test(line)));
}
