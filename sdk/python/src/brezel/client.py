"""First-party Python client for the supported Brezel sandbox path."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import stat
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

_MAX_JSON_BYTES = 4 << 20
_MAX_EVENT_BYTES = 2 << 20
_MAX_OUTPUT_BYTES = 64 << 20


class BrezelError(RuntimeError):
    """A transport, protocol, or Brezel API failure."""

    def __init__(self, message: str, *, code: str = "", status: int = 0) -> None:
        super().__init__(message)
        self.code = code
        self.status = status


class CommandExitError(BrezelError):
    """A command reached a confirmed non-zero exit status."""

    def __init__(self, result: CommandResult) -> None:
        super().__init__(f"command exited with status {result.exit_code}", code="command_exit")
        self.result = result


@dataclass(frozen=True)
class CommandResult:
    execution_id: str
    exit_code: int
    stdout: bytes
    stderr: bytes

    @property
    def stdout_text(self) -> str:
        return self.stdout.decode("utf-8", errors="replace")

    @property
    def stderr_text(self) -> str:
        return self.stderr.decode("utf-8", errors="replace")


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req: Any, fp: Any, code: int, msg: str, headers: Any, newurl: str) -> None:
        return None


class BrezelClient:
    """Authenticated client for one Brezel project."""

    def __init__(
        self,
        *,
        token: str,
        base_url: str = "http://127.0.0.1:8080",
        project: str = "brezel-default",
        timeout_seconds: float = 45,
    ) -> None:
        self.base_url = _validate_base_url(base_url)
        self.token = _validate_token(token)
        self.project = _validate_project(project)
        if timeout_seconds <= 0:
            raise ValueError("timeout_seconds must be positive")
        self.timeout_seconds = timeout_seconds
        self._opener = urllib.request.build_opener(_NoRedirect)

    @classmethod
    def from_token_file(
        cls,
        token_file: str | os.PathLike[str],
        *,
        base_url: str = "http://127.0.0.1:8080",
        project: str = "brezel-default",
        timeout_seconds: float = 45,
    ) -> BrezelClient:
        return cls(
            token=_read_private_token(token_file),
            base_url=base_url,
            project=project,
            timeout_seconds=timeout_seconds,
        )

    def create_sandbox(
        self,
        *,
        template: str | None = None,
        environment_revision: str | None = None,
        ttl_seconds: int = 3600,
        standby_after_seconds: int = 0,
        allow_internet: bool = False,
        workspace_mounts: Sequence[Mapping[str, str]] = (),
    ) -> Sandbox:
        if template is not None and environment_revision is not None:
            raise ValueError("template and environment_revision are mutually exclusive")
        revision = (
            self._ensure_environment(template if template is not None else "base")
            if environment_revision is None
            else _validate_environment_revision(environment_revision)
        )
        lifecycle: dict[str, Any] = {"expires_after_seconds": ttl_seconds}
        if standby_after_seconds > 0:
            lifecycle.update(
                {
                    "standby_after_seconds": standby_after_seconds,
                    "standby_checkpoint_kind": "full_state",
                    "auto_resume": True,
                }
            )
        body: dict[str, Any] = {
            "environment_revision": revision,
            "lifecycle": lifecycle,
            "network": {"allow_internet": allow_internet},
        }
        if workspace_mounts:
            body["workspace_mounts"] = [dict(mount) for mount in workspace_mounts]
        payload = self._json(
            "POST",
            "/v1/sandboxes",
            body=body,
            idempotency_key=_random_key("sandbox"),
        )
        resource = _required_mapping(payload, "resource")
        sandbox_id = _required_string(resource, "id")
        return Sandbox(self, sandbox_id, dict(resource))

    def sandbox(self, sandbox_id: str) -> Sandbox:
        resource = self._json("GET", f"/v1/sandboxes/{_segment(sandbox_id)}")
        if not isinstance(resource, dict):
            raise BrezelError("Brezel returned an invalid sandbox response", code="invalid_response")
        return Sandbox(self, sandbox_id, resource)

    def list_sandboxes(self, *, include_terminal: bool = False) -> list[dict[str, Any]]:
        suffix = "?include_terminal=true" if include_terminal else ""
        payload = self._json("GET", "/v1/sandboxes" + suffix)
        sandboxes = payload.get("sandboxes") if isinstance(payload, dict) else None
        if not isinstance(sandboxes, list) or not all(isinstance(item, dict) for item in sandboxes):
            raise BrezelError("Brezel returned an invalid sandbox list", code="invalid_response")
        return sandboxes

    def _ensure_environment(self, template: str) -> str:
        template = template.strip()
        if not template:
            raise ValueError("template cannot be empty")
        if _safe_name(template):
            name = template
        else:
            name = "brezel-" + hashlib.sha256(template.encode()).hexdigest()[:16]
        payload = self._json(
            "POST",
            "/v1/environments",
            body={"name": name, "template": template},
            idempotency_key=_stable_key("environment", template),
        )
        revision = _required_string(_required_mapping(payload, "resource"), "revision_id")
        return revision

    def _json(
        self,
        method: str,
        path: str,
        *,
        body: Mapping[str, Any] | None = None,
        idempotency_key: str = "",
    ) -> Any:
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        response = self._open(
            method,
            path,
            data=data,
            content_type="application/json" if data is not None else "",
            idempotency_key=idempotency_key,
        )
        try:
            raw = response.read(_MAX_JSON_BYTES + 1)
        finally:
            response.close()
        if len(raw) > _MAX_JSON_BYTES:
            raise BrezelError("Brezel JSON response exceeded the SDK limit", code="response_too_large")
        try:
            return json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise BrezelError("Brezel returned invalid JSON", code="invalid_response") from exc

    def _open(
        self,
        method: str,
        path: str,
        *,
        data: bytes | None = None,
        content_type: str = "",
        idempotency_key: str = "",
        timeout_seconds: float | None = None,
    ) -> Any:
        url = urllib.parse.urljoin(self.base_url + "/", path.lstrip("/"))
        headers = {
            "Authorization": "Bearer " + self.token,
            "X-Project-ID": self.project,
            "Accept": "application/json",
        }
        if content_type:
            headers["Content-Type"] = content_type
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        request = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            return self._opener.open(request, timeout=timeout_seconds or self.timeout_seconds)
        except urllib.error.HTTPError as exc:
            raw = exc.read(1 << 20)
            code = "http_error"
            message = f"Brezel API returned HTTP {exc.code}"
            try:
                error = json.loads(raw).get("error", {})
                if isinstance(error, dict):
                    code = str(error.get("code") or code)
                    message = str(error.get("message") or message)
            except (UnicodeDecodeError, json.JSONDecodeError):
                pass
            raise BrezelError(message, code=code, status=exc.code) from exc
        except urllib.error.URLError as exc:
            raise BrezelError(f"Brezel request failed: {exc.reason}", code="transport_error") from exc


class Sandbox:
    """Handle for one project-scoped Brezel sandbox."""

    def __init__(self, client: BrezelClient, sandbox_id: str, resource: Mapping[str, Any] | None = None) -> None:
        self.client = client
        self.id = sandbox_id
        self.resource = dict(resource or {})
        self._deleted = False

    def run(
        self,
        argv: Sequence[str],
        *,
        cwd: str = "",
        env: Mapping[str, str] | None = None,
        timeout_seconds: int = 300,
        check: bool = False,
        on_event: Callable[[Mapping[str, Any]], None] | None = None,
    ) -> CommandResult:
        if isinstance(argv, (str, bytes)) or not argv or not all(isinstance(arg, str) for arg in argv):
            raise ValueError("argv must be a non-empty sequence of strings")
        body = json.dumps(
            {"argv": list(argv), "cwd": cwd, "env": dict(env or {}), "timeout_seconds": timeout_seconds},
            separators=(",", ":"),
        ).encode()
        response = self.client._open(
            "POST",
            f"/v1/sandboxes/{_segment(self.id)}/commands",
            data=body,
            content_type="application/json",
            timeout_seconds=max(self.client.timeout_seconds, timeout_seconds + 5),
        )
        if response.headers.get_content_type() != "application/x-ndjson":
            response.close()
            raise BrezelError("command response used an unexpected media type", code="invalid_response")
        stdout = bytearray()
        stderr = bytearray()
        execution_id = ""
        exit_code: int | None = None
        try:
            while True:
                line = response.readline(_MAX_EVENT_BYTES + 1)
                if not line:
                    break
                if len(line) > _MAX_EVENT_BYTES:
                    raise BrezelError("command event exceeded the SDK limit", code="response_too_large")
                try:
                    event = json.loads(line)
                except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                    raise BrezelError("command stream contained invalid JSON", code="invalid_response") from exc
                if not isinstance(event, dict):
                    raise BrezelError("command stream contained an invalid event", code="invalid_response")
                if on_event is not None:
                    on_event(event)
                if isinstance(event.get("execution_id"), str):
                    execution_id = event["execution_id"]
                event_type = event.get("type")
                if event_type in ("stdout", "stderr"):
                    try:
                        chunk = base64.b64decode(event.get("data", ""), validate=True)
                    except (ValueError, TypeError) as exc:
                        raise BrezelError("command stream contained invalid output encoding", code="invalid_response") from exc
                    destination = stdout if event_type == "stdout" else stderr
                    destination.extend(chunk)
                    if len(stdout) + len(stderr) > _MAX_OUTPUT_BYTES:
                        raise BrezelError("command output exceeded the SDK limit", code="response_too_large")
                elif event_type == "exited":
                    if exit_code is not None or not isinstance(event.get("exit_code"), int):
                        raise BrezelError("command stream contained an invalid terminal event", code="invalid_response")
                    exit_code = event["exit_code"]
                elif event_type == "error":
                    raise BrezelError("command outcome is indeterminate", code="command_indeterminate")
        finally:
            response.close()
        if exit_code is None:
            raise BrezelError("command stream ended without a confirmed exit", code="command_indeterminate")
        result = CommandResult(execution_id, exit_code, bytes(stdout), bytes(stderr))
        if check and exit_code != 0:
            raise CommandExitError(result)
        return result

    def write_file(self, path: str, data: bytes | bytearray | memoryview) -> dict[str, Any]:
        raw = bytes(data)
        response = self.client._open(
            "PUT",
            f"/v1/sandboxes/{_segment(self.id)}/files?path={urllib.parse.quote(path, safe='')}",
            data=raw,
            content_type="application/octet-stream",
        )
        try:
            raw_response = response.read(_MAX_JSON_BYTES + 1)
        finally:
            response.close()
        if len(raw_response) > _MAX_JSON_BYTES:
            raise BrezelError("file metadata exceeded the SDK limit", code="response_too_large")
        try:
            payload = json.loads(raw_response)
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise BrezelError("Brezel returned invalid file metadata", code="invalid_response") from exc
        if not isinstance(payload, dict):
            raise BrezelError("Brezel returned invalid file metadata", code="invalid_response")
        return payload

    def read_file(self, path: str) -> bytes:
        response = self.client._open(
            "GET",
            f"/v1/sandboxes/{_segment(self.id)}/files?path={urllib.parse.quote(path, safe='')}",
        )
        try:
            data = response.read((64 << 20) + 1)
        finally:
            response.close()
        if len(data) > 64 << 20:
            raise BrezelError("file exceeded the SDK download limit", code="response_too_large")
        return data

    def preview(self, port: int, *, ttl_seconds: int = 300) -> str:
        if not 1 <= port <= 65535:
            raise ValueError("port must be between 1 and 65535")
        payload = self.client._json(
            "POST",
            f"/v1/sandboxes/{_segment(self.id)}/ports/{port}/leases",
            body={"ttl_seconds": ttl_seconds},
        )
        path = _required_string(payload, "path")
        return urllib.parse.urljoin(self.client.base_url + "/", path.lstrip("/"))

    def pause(self) -> dict[str, Any]:
        return self._lifecycle("pause")

    def resume(self) -> dict[str, Any]:
        return self._lifecycle("resume")

    def delete(self) -> dict[str, Any]:
        if self._deleted:
            return self.resource
        payload = self.client._json(
            "DELETE",
            f"/v1/sandboxes/{_segment(self.id)}",
            idempotency_key=_random_key("delete"),
        )
        self.resource = dict(_required_mapping(payload, "resource"))
        self._deleted = True
        return self.resource

    def _lifecycle(self, action: str) -> dict[str, Any]:
        payload = self.client._json(
            "POST",
            f"/v1/sandboxes/{_segment(self.id)}:{action}",
            idempotency_key=_random_key(action),
        )
        self.resource = dict(_required_mapping(payload, "resource"))
        return self.resource

    def __enter__(self) -> Sandbox:
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.delete()


def _validate_base_url(value: str) -> str:
    parsed = urllib.parse.urlsplit(value.rstrip("/"))
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
    if parsed.scheme == "http" and parsed.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise ValueError("remote Brezel URLs must use HTTPS")
    return urllib.parse.urlunsplit(parsed)


def _validate_token(value: str) -> str:
    value = value.strip()
    if len(value) < 32 or "\n" in value or "\r" in value:
        raise ValueError("token must contain one line of at least 32 characters")
    return value


def _validate_project(value: str) -> str:
    if not value or "\n" in value or "\r" in value:
        raise ValueError("project is required")
    return value


def _validate_environment_revision(value: str) -> str:
    if not isinstance(value, str) or not value or not _safe_name(value):
        raise ValueError("environment_revision must be one non-empty resource ID")
    return value


def _read_private_token(path_value: str | os.PathLike[str]) -> str:
    path = Path(path_value)
    before = path.lstat()
    _validate_private_file(before)
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    try:
        opened = os.fstat(descriptor)
        _validate_private_file(opened)
        with os.fdopen(descriptor, "r", encoding="utf-8", closefd=False) as handle:
            value = handle.read(4097)
    finally:
        os.close(descriptor)
    after = path.lstat()
    _validate_private_file(after)
    if len(value.encode()) > 4096:
        raise ValueError("token_file exceeds 4096 bytes")
    if (before.st_dev, before.st_ino) != (opened.st_dev, opened.st_ino) or (opened.st_dev, opened.st_ino) != (after.st_dev, after.st_ino):
        raise ValueError("token_file changed while reading")
    return _validate_token(value)


def _validate_private_file(info: os.stat_result) -> None:
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise ValueError("token_file must be a regular file, not a symlink")
    if stat.S_IMODE(info.st_mode) not in (0o400, 0o600) or info.st_nlink != 1:
        raise ValueError("token_file permissions must be 0400 or 0600 with one hard link")
    if hasattr(os, "geteuid") and info.st_uid != os.geteuid():
        raise ValueError("token_file must be owned by the effective user")


def _required_mapping(value: Any, key: str) -> Mapping[str, Any]:
    result = value.get(key) if isinstance(value, dict) else None
    if not isinstance(result, dict):
        raise BrezelError(f"Brezel response is missing {key}", code="invalid_response")
    return result


def _required_string(value: Any, key: str) -> str:
    result = value.get(key) if isinstance(value, Mapping) else None
    if not isinstance(result, str) or not result:
        raise BrezelError(f"Brezel response is missing {key}", code="invalid_response")
    return result


def _segment(value: str) -> str:
    if not value or any(character in value for character in "/\r\n"):
        raise ValueError("resource ID must be one non-empty path segment")
    return urllib.parse.quote(value, safe="")


def _safe_name(value: str) -> bool:
    return len(value) <= 128 and value[0].isalnum() and all(character.isalnum() or character in "._-" for character in value)


def _random_key(prefix: str) -> str:
    return f"{prefix}-{secrets.token_hex(12)}"


def _stable_key(prefix: str, value: str) -> str:
    return f"{prefix}-{hashlib.sha256(value.encode()).hexdigest()[:24]}"
