#!/usr/bin/env python3
"""Probe the effective context limit of a local OpenAI-compatible model.

The script reads the first active API key from data/api-keys.json, sends
progressively larger prompts to /v1/chat/completions, and narrows the boundary
with binary search. A JSON report records every request and the observed limit.
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

DEFAULT_BASE_URL = "http://127.0.0.1:4141"
DEFAULT_MODEL = "gpt-5.6-sol"
PROJECT_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_KEY_FILE = PROJECT_ROOT / "data" / "api-keys.json"
DEFAULT_OUTPUT = PROJECT_ROOT / "context-length-probe.json"


@dataclass
class ProbeResult:
    requested_tokens: int
    estimated_tokens: int
    characters: int
    status: int
    accepted: bool
    elapsed_seconds: float
    request_id: str | None
    response_id: str | None
    finish_reason: str | None
    usage: dict[str, Any] | None
    error: str | None


def load_api_key(path: Path) -> str:
    try:
        payload = json.loads(path.read_text(encoding="utf-8-sig"))
    except FileNotFoundError as exc:
        raise RuntimeError(f"API key file not found: {path}") from exc
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"Invalid JSON in API key file {path}: {exc}") from exc

    for item in payload.get("keys", []):
        if isinstance(item, dict) and not item.get("revoked") and item.get("raw"):
            return str(item["raw"])
    raise RuntimeError(f"No active raw API key found in {path}")


def make_prompt(estimated_tokens: int) -> str:
    """Create deterministic ASCII text averaging four characters per token."""
    target_chars = max(1, estimated_tokens * 4)
    marker = "test "
    repeats = (target_chars + len(marker) - 1) // len(marker)
    return (marker * repeats)[:target_chars]


def decode_json(raw: bytes) -> tuple[Any | None, str]:
    text = raw.decode("utf-8", errors="replace")
    try:
        return json.loads(text), text
    except json.JSONDecodeError:
        return None, text


def extract_error(payload: Any | None, raw_text: str) -> str | None:
    if isinstance(payload, dict):
        error = payload.get("error")
        if isinstance(error, dict):
            message = error.get("message")
            if message:
                return str(message)
            return json.dumps(error, ensure_ascii=False)
        if error:
            return str(error)
        message = payload.get("message")
        if message:
            return str(message)
    compact = raw_text.strip()
    return compact[:2000] if compact else None


def probe_once(
    endpoint: str,
    api_key: str,
    model: str,
    requested_tokens: int,
    timeout: float,
    max_output_tokens: int,
    reasoning_effort: str | None,
) -> ProbeResult:
    prompt = make_prompt(requested_tokens)
    estimated_tokens = max(1, len(prompt) // 4)
    body: dict[str, Any] = {
        "model": model,
        "messages": [
            {
                "role": "system",
                "content": "Reply with exactly OK and nothing else.",
            },
            {"role": "user", "content": prompt},
        ],
        "stream": False,
        "max_tokens": max_output_tokens,
    }
    if reasoning_effort:
        body["reasoning_effort"] = reasoning_effort

    request = urllib.request.Request(
        endpoint,
        data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
        method="POST",
    )

    started = time.perf_counter()
    status = 0
    headers: Any = {}
    raw = b""
    transport_error: str | None = None

    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            status = response.status
            headers = response.headers
            raw = response.read()
    except urllib.error.HTTPError as exc:
        status = exc.code
        headers = exc.headers
        raw = exc.read()
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        transport_error = f"{type(exc).__name__}: {exc}"

    elapsed = time.perf_counter() - started
    payload, raw_text = decode_json(raw)
    accepted = 200 <= status < 300 and isinstance(payload, dict)

    response_id = payload.get("id") if isinstance(payload, dict) else None
    usage = payload.get("usage") if isinstance(payload, dict) else None
    if not isinstance(usage, dict):
        usage = None

    finish_reason = None
    if isinstance(payload, dict):
        choices = payload.get("choices")
        if isinstance(choices, list) and choices and isinstance(choices[0], dict):
            value = choices[0].get("finish_reason")
            finish_reason = str(value) if value is not None else None

    request_id = None
    if headers:
        request_id = (
            headers.get("x-request-id")
            or headers.get("request-id")
            or headers.get("x-ms-request-id")
        )

    error = transport_error
    if not accepted and error is None:
        error = extract_error(payload, raw_text)

    return ProbeResult(
        requested_tokens=requested_tokens,
        estimated_tokens=estimated_tokens,
        characters=len(prompt),
        status=status,
        accepted=accepted,
        elapsed_seconds=round(elapsed, 3),
        request_id=request_id,
        response_id=str(response_id) if response_id is not None else None,
        finish_reason=finish_reason,
        usage=usage,
        error=error,
    )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Measure the effective and upstream context boundary of gpt-5.6-sol."
    )
    parser.add_argument("--base-url", default=DEFAULT_BASE_URL)
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--api-key", help="API key override; otherwise read the key file")
    parser.add_argument("--key-file", type=Path, default=DEFAULT_KEY_FILE)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--start", type=int, default=4096)
    parser.add_argument("--ceiling", type=int, default=1048576)
    parser.add_argument("--precision", type=int, default=1024)
    parser.add_argument("--timeout", type=float, default=300.0)
    parser.add_argument("--max-output-tokens", type=int, default=8)
    parser.add_argument(
        "--reasoning-effort",
        choices=["none", "minimal", "low", "medium", "high", "xhigh"],
        default="low",
    )
    args = parser.parse_args()
    if args.start <= 0:
        parser.error("--start must be positive")
    if args.ceiling < args.start:
        parser.error("--ceiling must be greater than or equal to --start")
    if args.precision <= 0:
        parser.error("--precision must be positive")
    if args.timeout <= 0:
        parser.error("--timeout must be positive")
    if args.max_output_tokens <= 0:
        parser.error("--max-output-tokens must be positive")
    return args


def main() -> int:
    args = parse_args()
    api_key = args.api_key or load_api_key(args.key_file)
    endpoint = args.base_url.rstrip("/") + "/v1/chat/completions"
    results: list[ProbeResult] = []
    cache: dict[int, ProbeResult] = {}

    def probe(size: int) -> ProbeResult:
        if size in cache:
            return cache[size]
        result = probe_once(
            endpoint=endpoint,
            api_key=api_key,
            model=args.model,
            requested_tokens=size,
            timeout=args.timeout,
            max_output_tokens=args.max_output_tokens,
            reasoning_effort=args.reasoning_effort,
        )
        cache[size] = result
        results.append(result)
        state = "PASS" if result.accepted else "FAIL"
        detail = ""
        if result.error:
            detail = " | " + result.error.replace("\n", " ")[:240]
        print(
            f"[{state}] requested={size:,} estimated={result.estimated_tokens:,} "
            f"status={result.status} elapsed={result.elapsed_seconds:.3f}s{detail}",
            flush=True,
        )
        return result

    print(f"Endpoint: {endpoint}")
    print(f"Model: {args.model}")
    print("Phase 1: exponential search")

    last_pass = 0
    first_fail: int | None = None
    size = args.start

    while size <= args.ceiling:
        result = probe(size)
        if result.accepted:
            last_pass = size
            if size == args.ceiling:
                break
            size = min(size * 2, args.ceiling)
        else:
            first_fail = size
            break

    if first_fail is None and last_pass < args.ceiling:
        result = probe(args.ceiling)
        if result.accepted:
            last_pass = args.ceiling
        else:
            first_fail = args.ceiling

    print("Phase 2: binary search")
    if first_fail is not None and last_pass > 0:
        low = last_pass
        high = first_fail
        while high - low > args.precision:
            midpoint = (low + high) // 2
            result = probe(midpoint)
            if result.accepted:
                low = midpoint
            else:
                high = midpoint
        last_pass = low
        first_fail = high
    elif first_fail is None:
        print("No rejection observed before the configured ceiling.")
    else:
        print("The initial request failed, so no accepted lower bound was found.")

    successful = [item for item in results if item.accepted]
    failed = [item for item in results if not item.accepted]
    reported_inputs: list[int] = []
    for item in successful:
        if not item.usage:
            continue
        value = item.usage.get("prompt_tokens", item.usage.get("input_tokens"))
        if isinstance(value, int):
            reported_inputs.append(value)

    report = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "endpoint": endpoint,
        "model": args.model,
        "method": {
            "description": "Exponential search followed by binary search",
            "estimated_characters_per_token": 4,
            "start_tokens": args.start,
            "precision_tokens": args.precision,
            "configured_ceiling_tokens": args.ceiling,
            "max_output_tokens": args.max_output_tokens,
            "reasoning_effort": args.reasoning_effort,
        },
        "result": {
            "largest_accepted_requested_tokens": last_pass or None,
            "smallest_rejected_requested_tokens": first_fail,
            "boundary_width_tokens": (
                first_fail - last_pass
                if first_fail is not None and last_pass > 0
                else None
            ),
            "largest_server_reported_input_tokens": (
                max(reported_inputs) if reported_inputs else None
            ),
            "ceiling_reached_without_rejection": (
                first_fail is None and last_pass == args.ceiling
            ),
            "successful_attempts": len(successful),
            "failed_attempts": len(failed),
        },
        "interpretation": {
            "effective_limit": (
                "The largest accepted request is the end-to-end effective limit "
                "observed through the local API within the configured precision."
            ),
            "upstream_limit": (
                "A context-length error returned by the first rejected request is "
                "evidence of the upstream boundary. If the proxy truncates before "
                "forwarding, compare usage values and server logs to distinguish "
                "proxy truncation from a true upstream rejection."
            ),
        },
        "attempts": [asdict(item) for item in results],
    }

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8"
    )

    print("\nSummary")
    if last_pass:
        print(f"Largest accepted requested size: {last_pass:,} tokens")
    else:
        print("No request succeeded")
    if first_fail is not None:
        print(f"Smallest rejected requested size: {first_fail:,} tokens")
        if last_pass:
            print(f"Boundary width: {first_fail - last_pass:,} tokens")
    else:
        print(f"No failure detected up to {args.ceiling:,} tokens")
    if reported_inputs:
        print(f"Largest server-reported input: {max(reported_inputs):,} tokens")
    print(f"Detailed JSON report: {args.output}")
    return 0 if successful else 2


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("Interrupted", file=sys.stderr)
        sys.exit(130)
    except Exception as exc:
        print(f"Fatal error: {exc}", file=sys.stderr)
        sys.exit(1)
