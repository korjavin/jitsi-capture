"""Offline tests: the real faster_whisper is shadowed by a fake, so no model is ever loaded."""

import importlib
import sys
import types
from typing import ClassVar

import pytest

from transcriber import transcribe as t

WHISPER_ENV = ("WHISPER_MODEL", "WHISPER_LANGUAGE", "WHISPER_DEVICE", "WHISPER_COMPUTE_TYPE")


class FakeModel:
    instances: ClassVar[list] = []

    def __init__(self, model, device=None, compute_type=None):
        self.init = (model, device, compute_type)
        self.calls = []
        FakeModel.instances.append(self)

    def transcribe(self, audio_path, **kwargs):
        self.calls.append((audio_path, kwargs))
        segments = iter(
            [
                types.SimpleNamespace(start=5.4, end=9.0, text=" hello"),
                types.SimpleNamespace(start=9.0, end=12.0, text="world"),
            ]
        )
        return segments, types.SimpleNamespace(language="en")


@pytest.fixture(autouse=True)
def fake_whisper(monkeypatch):
    module = types.ModuleType("faster_whisper")
    module.WhisperModel = FakeModel
    monkeypatch.setitem(sys.modules, "faster_whisper", module)
    monkeypatch.setattr(t, "_MODELS", {})
    FakeModel.instances.clear()
    for name in WHISPER_ENV:
        monkeypatch.delenv(name, raising=False)


def seg(start, text="x", end=None):
    return t.Segment(start=start, end=start if end is None else end, text=text)


def test_transcribe_maps_segments():
    assert t.transcribe("call.webm") == [
        t.Segment(start=5.4, end=9.0, text=" hello"),
        t.Segment(start=9.0, end=12.0, text="world"),
    ]


def test_defaults_come_from_fallbacks():
    t.transcribe("call.webm")
    model = FakeModel.instances[0]
    assert model.init == ("large-v3", "cpu", "int8")
    assert model.calls == [
        ("call.webm", {"language": None, "vad_filter": True, "beam_size": 5})
    ]


def test_env_overrides_device_and_compute_type(monkeypatch):
    monkeypatch.setenv("WHISPER_MODEL", "medium")
    monkeypatch.setenv("WHISPER_DEVICE", "cuda")
    monkeypatch.setenv("WHISPER_COMPUTE_TYPE", "float16")
    monkeypatch.setenv("WHISPER_LANGUAGE", "ru")

    t.transcribe("call.webm")

    assert FakeModel.instances[0].init == ("medium", "cuda", "float16")
    assert FakeModel.instances[0].calls[0][1]["language"] == "ru"


def test_arguments_beat_env(monkeypatch):
    monkeypatch.setenv("WHISPER_MODEL", "medium")
    monkeypatch.setenv("WHISPER_LANGUAGE", "ru")

    t.transcribe("call.webm", model="small", language="en")

    assert FakeModel.instances[0].init[0] == "small"
    assert FakeModel.instances[0].calls[0][1]["language"] == "en"


def test_model_is_cached_per_configuration():
    t.transcribe("a.webm")
    t.transcribe("b.webm")
    assert len(FakeModel.instances) == 1

    t.transcribe("c.webm", model="small")
    assert len(FakeModel.instances) == 2


def test_to_markdown_timecodes():
    assert t.to_markdown([seg(5, "Hi"), seg(62, "Later")]) == "[00:05] Hi\n[01:02] Later"


def test_to_markdown_switches_to_hours():
    assert t.to_markdown([seg(3723, "Deep")]) == "[1:02:03] Deep"


def test_to_markdown_skips_blank_and_strips():
    assert t.to_markdown([seg(5, " Hi "), seg(7, "   "), seg(9, "")]) == "[00:05] Hi"


def test_to_markdown_empty():
    assert t.to_markdown([]) == ""


def test_import_does_not_need_or_construct_a_model(monkeypatch):
    # sys.modules[name] = None makes `import name` raise ImportError.
    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    monkeypatch.delitem(sys.modules, "transcriber.transcribe")

    module = importlib.import_module("transcriber.transcribe")

    assert module._MODELS == {}


def test_cli_prints_markdown(capsys):
    assert t.main(["call.webm"]) == 0
    assert capsys.readouterr().out == "[00:05] hello\n[00:09] world\n"


def test_cli_rejects_bad_usage(capsys):
    assert t.main([]) == 2
    assert "usage:" in capsys.readouterr().err
