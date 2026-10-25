#!/usr/bin/env python3
"""Summarize probe JSONL without treating unobserved writes as successes."""
import argparse
import hashlib
import json
import statistics
from pathlib import Path


def load(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


def summarize(writes, reads, offset_ns):
    """Return first-observation delays from write completion, in milliseconds."""
    result = {}
    for field, timestamp in (("stat", "stat_end"), ("fstat", "end"), ("read", "end")):
        samples = []
        for write in writes:
            observed = next((r for r in reads if r[field] >= write["size"]), None)
            if observed is None:
                raise ValueError(f"unobserved {field} write {write['seq']}")
            samples.append((observed[timestamp] - write["end"] + offset_ns) / 1e6)
        result[field] = {
            "count": len(samples),
            "median_ms": statistics.median(samples),
            "max_ms": max(samples),
            "min_ms": min(samples),
        }
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--source", required=True, type=Path)
    args = parser.parse_args()
    root = args.directory
    clocks = [min(load(root / f"clock-{part}.jsonl"), key=lambda r: r["rtt_ns"])
              for part in ("before", "after")]
    # Record both bounds; an endpoint calibration cannot exclude an intervening
    # clock step. Do not turn these into an unsupported accuracy guarantee.
    offset = statistics.mean(c["offset_ns"] for c in clocks)
    writers = [load(root / f"writer-{part}.jsonl") for part in ("a", "b")]
    writes = [r for group in writers for r in group if r["kind"] == "write"]
    if [r["seq"] for r in writes] != list(range(560)):
        raise ValueError("expected exactly 560 ordered writes")
    reader = load(root / "reader.jsonl")
    reads = [r for r in reader if r["kind"] == "read"]
    digest = next(r for r in reader if r["kind"] == "digest")
    source = args.source.read_bytes()[:writes[-1]["size"]]
    if digest["size"] != len(source) or digest["sha256"] != hashlib.sha256(source).hexdigest():
        raise ValueError("reader did not consume the exact fixture prefix")
    last_a = next(r for r in reversed(writers[0]) if r["kind"] == "write")
    first_b = next(r for r in writers[1] if r["kind"] == "write")
    changes = [r for r in reads if r["n"] > 0]
    result = {
        "clock_best": clocks,
        "visibility": summarize(writes, reads, offset),
        "handoff": summarize([first_b], reads, offset),
        "last_write_to_next_write_ms": (first_b["end"] - last_a["end"]) / 1e6,
        "close_to_next_write_ms": (first_b["end"] - writers[0][-1]["at"]) / 1e6,
        "max_read_no_growth_ms": max((b["end"] - a["end"]) / 1e6
                                      for a, b in zip(changes, changes[1:])),
        "digest": digest,
    }
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
