"""Use the runtime's pinned YAML parser once per immutable publisher manifest."""
import json
import subprocess
from pathlib import Path
from typing import Any


def parse_publisher(data: bytes) -> dict[str, Any]:
    if not data or len(data) > 2 * 1024 * 1024:
        raise ValueError("publisher manifest exceeds byte bound")
    result = subprocess.run(
        ["go", "run", "./cmd/publisher-selection"],
        cwd=Path(__file__).resolve().parent,
        input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
        timeout=120,
    )
    if result.returncode:
        raise ValueError("invalid publisher YAML: " + result.stderr.decode("utf-8", errors="replace")[:2048])
    return json.loads(result.stdout)
