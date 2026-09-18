#!/usr/bin/env python3
"""Fail when first-party SDK release versions disagree."""

from __future__ import annotations

import argparse
import json
import re
import sys
import tomllib
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tag", help="expected release tag in sdk-vX.Y.Z form")
    arguments = parser.parse_args()

    python_project = tomllib.loads((ROOT / "sdk/python/pyproject.toml").read_text())
    python_version = python_project["project"]["version"]
    node_project = json.loads((ROOT / "sdk/typescript/package.json").read_text())
    node_version = node_project["version"]

    source_path = ROOT / "sdk/python/src"
    sys.path.insert(0, str(source_path))
    try:
        import brezel
    finally:
        sys.path.pop(0)

    javascript = (ROOT / "sdk/typescript/src/index.js").read_text()
    declaration = (ROOT / "sdk/typescript/src/index.d.ts").read_text()
    javascript_match = re.search(r'^export const VERSION = "([0-9]+\.[0-9]+\.[0-9]+)";$', javascript, re.MULTILINE)
    declaration_match = re.search(r'^export const VERSION: "([0-9]+\.[0-9]+\.[0-9]+)";$', declaration, re.MULTILINE)
    versions = {
        "Python project": python_version,
        "Python import": getattr(brezel, "__version__", None),
        "npm project": node_version,
        "JavaScript export": javascript_match.group(1) if javascript_match else None,
        "TypeScript declaration": declaration_match.group(1) if declaration_match else None,
    }
    expected = python_version
    failures = [f"{name}={value!r}" for name, value in versions.items() if value != expected]
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", expected):
        failures.append(f"Python project version is not a stable X.Y.Z version: {expected!r}")
    if arguments.tag and arguments.tag != f"sdk-v{expected}":
        failures.append(f"tag={arguments.tag!r}; expected 'sdk-v{expected}'")
    if failures:
        print("SDK release version mismatch:", file=sys.stderr)
        for failure in failures:
            print(f"  - {failure}", file=sys.stderr)
        return 1
    print(f"SDK versions agree: {expected}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
