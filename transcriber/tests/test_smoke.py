"""Smoke test: the package imports from the repo root (pytest path wiring works)."""

import transcriber


def test_package_importable():
    assert transcriber is not None
