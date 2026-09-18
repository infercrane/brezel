# Brezel TypeScript SDK

Dependency-free Node.js client with first-party TypeScript declarations for
stateful, self-hosted Brezel agent sandboxes.

```bash
npm install @infercrane/brezel
```

```ts
import { BrezelClient } from "@infercrane/brezel";

const client = BrezelClient.fromTokenFile(
  "/var/lib/brezel/secrets/service.token",
  { baseUrl: "https://sandbox.example.com", project: "brezel-default" },
);

await using sandbox = await client.createSandbox({ ttlSeconds: 900 });
const result = await sandbox.run(["python3", "-c", "print(6 * 7)"]);
console.log(result.stdoutText);
```

The SDK accepts command argument arrays, not shell strings. It never retries a
command or file write because an interrupted transport can leave the outcome
indeterminate.

Requires Node.js 20 or newer. Until `0.1.0` is present on npm, install the same
package from a reviewed Brezel source checkout with
`npm install ./sdk/typescript`.
