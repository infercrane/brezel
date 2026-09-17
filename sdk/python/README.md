# Brezel Python SDK

Dependency-free Python client for the supported Brezel developer-preview API.

```bash
python -m pip install ./sdk/python
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
