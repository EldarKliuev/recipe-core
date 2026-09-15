"""Optional long-tail fallback: ask llm-service to normalise leftover lines.

Used only for ingredient lines the rules and the CRF both failed on. It routes
through llm-service rather than calling LM Studio directly, because llm-service is
the only process allowed to talk to a model.
"""

from __future__ import annotations

import os

import httpx


def normalise_batch(lines: list[str], batch_size: int = 40, timeout: float = 300.0) -> dict[str, str]:
    """Map raw line -> canonical ingredient name for as many lines as possible."""
    base_url = os.environ.get("LLM_SERVICE_URL", "http://llm-service:9000").rstrip("/")
    out: dict[str, str] = {}

    with httpx.Client(timeout=timeout) as client:
        for start in range(0, len(lines), batch_size):
            batch = lines[start:start + batch_size]
            try:
                response = client.post(f"{base_url}/v1/parse-ingredients", json={"lines": batch})
                response.raise_for_status()
            except httpx.HTTPError as exc:
                # The corpus load must not depend on the model being up.
                print(f"  llm-service unavailable, skipping {len(batch)} lines: {exc}")
                continue
            for item in response.json().get("items", []):
                raw, name = item.get("raw"), item.get("name")
                if raw and name:
                    out[raw] = name.strip().lower()
            print(f"  normalised {len(out)}/{len(lines)} lines")
    return out
