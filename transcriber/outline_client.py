"""Minimal Outline API client: create a document and return its absolute URL.

See README §3. The API key lives only in the Authorization header — it is never
logged and never put into an exception message.
"""

import os

import requests

API_PATH = "/api/documents.create"
ENV_VARS = ("OUTLINE_BASE_URL", "OUTLINE_API_KEY", "OUTLINE_COLLECTION_ID")


class OutlineError(Exception):
    """Any failure talking to Outline."""


class OutlineClient:
    def __init__(self, base_url: str, api_key: str, collection_id: str, *, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.collection_id = collection_id
        self.timeout = timeout
        self._api_key = api_key

    @classmethod
    def from_env(cls) -> "OutlineClient":
        values = []
        for name in ENV_VARS:
            value = os.environ.get(name)
            if not value:
                raise OutlineError(f"missing environment variable: {name}")
            values.append(value)
        return cls(*values)

    def create_document(self, title: str, text: str, *, publish: bool = True) -> str:
        """POST /api/documents.create and return the absolute document URL."""
        # ponytail: no retry; add backoff if Outline flakes.
        try:
            response = requests.post(
                self.base_url + API_PATH,
                headers={"Authorization": f"Bearer {self._api_key}"},
                json={
                    "collectionId": self.collection_id,
                    "title": title,
                    "text": text,
                    "publish": publish,
                },
                timeout=self.timeout,
            )
        except requests.RequestException as exc:
            # `from None`: the cause could carry request internals; keep the key out for good.
            raise OutlineError(f"request to Outline failed: {type(exc).__name__}") from None

        if not 200 <= response.status_code < 300:
            raise OutlineError(f"{response.status_code}: {response.text[:200]}")

        try:
            path = response.json()["data"]["url"]
        except (ValueError, TypeError, KeyError):
            path = None
        if not path:
            raise OutlineError("Outline response has no data.url")
        return self.base_url + path
