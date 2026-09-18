import {
  BrezelClient,
  CommandExitError,
  VERSION,
  type CommandEvent,
} from "../src/index.js";

const client = new BrezelClient({
  token: "a-service-token-that-is-long-enough",
  baseUrl: "https://sandbox.example.com",
});

async function exercisePublicTypes(): Promise<void> {
  const sandbox = await client.createSandbox({ environmentRevision: "envr_exact", ttlSeconds: 900 });
  const events: CommandEvent[] = [];
  try {
    const result = await sandbox.run(["python3", "-c", "print(42)"], {
      check: true,
      onEvent: (event) => events.push(event),
    });
    result.stdoutText.toUpperCase();
    await sandbox.writeFile("/workspace/value.txt", "42\n");
    await sandbox.readFile("/workspace/value.txt");
    await sandbox.preview(3000);
  } catch (error) {
    if (error instanceof CommandExitError) error.result.exitCode.toFixed();
  } finally {
    await sandbox.delete();
  }
}

void exercisePublicTypes;
VERSION satisfies "0.1.1";
