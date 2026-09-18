"""Small, dependency-free client for the Brezel HTTP API."""

from .client import (
    BrezelClient,
    BrezelError,
    CommandExitError,
    CommandResult,
    Sandbox,
)

__version__ = "0.1.0"

__all__ = [
    "__version__",
    "BrezelClient",
    "BrezelError",
    "CommandExitError",
    "CommandResult",
    "Sandbox",
]
