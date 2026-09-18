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

The SDK intentionally accepts command argument arrays, not shell strings. It
does not retry commands or file writes because their outcome can be
indeterminate after a transport interruption.

Requires Python 3.10 or newer. Until `0.1.0` is present on PyPI, install the
same package from a reviewed Brezel source checkout with
`pip install ./sdk/python`.
