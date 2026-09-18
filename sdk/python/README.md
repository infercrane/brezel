# Brezel Python SDK

Dependency-free Python client for stateful, self-hosted Brezel agent sandboxes.

```bash
pip install brezel-sdk
```

```python
from brezel import BrezelClient

client = BrezelClient.from_token_file(
    "/var/lib/brezel/secrets/service.token",
    base_url="https://sandbox.example.com",
    project="brezel-default",
)

with client.create_sandbox(template="base", ttl_seconds=900) as sandbox:
    result = sandbox.run(["python3", "-c", "print(6 * 7)"])
    print(result.stdout_text, end="")
```

Use a prequalified immutable environment without creating or resolving a
template alias:

```python
with client.create_sandbox(
    environment_revision="envr_0123456789abcdef01234567",
    ttl_seconds=900,
) as sandbox:
    print(sandbox.run(["node", "--version"]).stdout_text)
```

The SDK intentionally accepts command argument arrays, not shell strings. It
does not retry commands or file writes because their outcome can be
indeterminate after a transport interruption.

Requires Python 3.10 or newer.
