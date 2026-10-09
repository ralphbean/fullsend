#!/usr/bin/env python3
"""Require a live Fullsend run to fetch a newly rotated assertion."""

import json
import pathlib
import sys

metrics = json.loads(pathlib.Path(sys.argv[1]).read_text())
requests = metrics.get("requests", [])
hashes = {item.get("assertion_sha256") for item in requests if item.get("assertion_sha256")}
if len(requests) < 3 or len(hashes) < 2:
    raise SystemExit("Fullsend OIDC refresh did not observe a rotated assertion")
print("Fullsend fetched an inference assertion before and after projected-token rotation.")
