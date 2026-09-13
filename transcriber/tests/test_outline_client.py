"""Offline tests for the Outline client: requests.post is monkeypatched, nothing leaves the box."""

import json

import pytest
import requests

from transcriber.outline_client import OutlineClient, OutlineError

BASE_URL = "https://outline.example.com"
API_KEY = "ol_api_fake_key_for_tests"
COLLECTION_ID = "00000000-0000-0000-0000-000000000001"


class FakeResponse:
    def __init__(self, status_code=200, payload=None, text=None):
        self.status_code = status_code
        self._payload = payload
        self.text = text if text is not None else json.dumps(payload)

    def json(self):
        if self._payload is None:
            raise ValueError("not json")
        return self._payload


@pytest.fixture
def client():
    return OutlineClient(BASE_URL, API_KEY, COLLECTION_ID)


def fake_post(monkeypatch, response=None, exc=None):
    """Patch requests.post; return a dict that captures the call kwargs."""
    calls = {}

    def _post(url, **kwargs):
        calls["url"] = url
        calls.update(kwargs)
        if exc is not None:
            raise exc
        return response

    monkeypatch.setattr(requests, "post", _post)
    return calls


def test_create_document_sends_url_headers_payload(monkeypatch, client):
    calls = fake_post(monkeypatch, FakeResponse(payload={"data": {"url": "/doc/meeting-abc123"}}))

    url = client.create_document("Meeting", "# Transcript")

    assert url == "https://outline.example.com/doc/meeting-abc123"
    assert calls["url"] == "https://outline.example.com/api/documents.create"
    assert calls["headers"] == {"Authorization": f"Bearer {API_KEY}"}
    assert calls["json"] == {
        "collectionId": COLLECTION_ID,
        "title": "Meeting",
        "text": "# Transcript",
        "publish": True,
    }
    assert calls["timeout"] == 30.0


def test_publish_false_and_custom_timeout(monkeypatch):
    calls = fake_post(monkeypatch, FakeResponse(payload={"data": {"url": "/doc/x"}}))

    OutlineClient(BASE_URL, API_KEY, COLLECTION_ID, timeout=5.0).create_document(
        "t", "x", publish=False
    )

    assert calls["json"]["publish"] is False
    assert calls["timeout"] == 5.0


def test_trailing_slash_in_base_url_does_not_double(monkeypatch):
    calls = fake_post(monkeypatch, FakeResponse(payload={"data": {"url": "/doc/x"}}))

    url = OutlineClient(BASE_URL + "/", API_KEY, COLLECTION_ID).create_document("t", "x")

    assert url == "https://outline.example.com/doc/x"
    assert calls["url"] == "https://outline.example.com/api/documents.create"


def test_non_2xx_raises_with_status_and_body(monkeypatch, client):
    fake_post(monkeypatch, FakeResponse(status_code=401, text='{"error":"authentication_required"}'))

    with pytest.raises(OutlineError) as err:
        client.create_document("t", "x")

    assert str(err.value).startswith("401: ")
    assert "authentication_required" in str(err.value)


def test_non_2xx_truncates_body_to_200_chars(monkeypatch, client):
    fake_post(monkeypatch, FakeResponse(status_code=500, text="e" * 500))

    with pytest.raises(OutlineError) as err:
        client.create_document("t", "x")

    assert str(err.value) == "500: " + "e" * 200


def test_network_error_raises_outline_error(monkeypatch, client):
    fake_post(monkeypatch, exc=requests.ConnectionError("connection refused"))

    with pytest.raises(OutlineError) as err:
        client.create_document("t", "x")

    assert "ConnectionError" in str(err.value)


@pytest.mark.parametrize(
    "response",
    [
        FakeResponse(payload={"data": {}}),
        FakeResponse(payload={"data": {"url": ""}}),
        FakeResponse(payload={"data": None}),
        FakeResponse(payload={}),
        FakeResponse(text="<html>not json</html>"),
    ],
)
def test_missing_data_url_raises(monkeypatch, client, response):
    fake_post(monkeypatch, response)

    with pytest.raises(OutlineError, match="no data.url"):
        client.create_document("t", "x")


@pytest.mark.parametrize(
    "response,exc",
    [
        (FakeResponse(status_code=403, text='{"error":"forbidden"}'), None),
        (FakeResponse(payload={"data": {}}), None),
        (None, requests.ConnectionError(f"Max retries exceeded with url {BASE_URL}")),
    ],
)
def test_api_key_never_appears_in_error(monkeypatch, client, response, exc):
    fake_post(monkeypatch, response, exc)

    with pytest.raises(OutlineError) as err:
        client.create_document("t", "x")

    assert API_KEY not in str(err.value)
    assert API_KEY not in repr(err.value)


def test_from_env(monkeypatch):
    monkeypatch.setenv("OUTLINE_BASE_URL", BASE_URL)
    monkeypatch.setenv("OUTLINE_API_KEY", API_KEY)
    monkeypatch.setenv("OUTLINE_COLLECTION_ID", COLLECTION_ID)

    client = OutlineClient.from_env()

    assert client.base_url == BASE_URL
    assert client.collection_id == COLLECTION_ID


@pytest.mark.parametrize("missing", ["OUTLINE_BASE_URL", "OUTLINE_API_KEY", "OUTLINE_COLLECTION_ID"])
def test_from_env_missing_var_raises(monkeypatch, missing):
    monkeypatch.setenv("OUTLINE_BASE_URL", BASE_URL)
    monkeypatch.setenv("OUTLINE_API_KEY", API_KEY)
    monkeypatch.setenv("OUTLINE_COLLECTION_ID", COLLECTION_ID)
    monkeypatch.delenv(missing)

    with pytest.raises(OutlineError, match=missing):
        OutlineClient.from_env()
