#!/usr/bin/env python3
"""Expose the existing ALSA fake ONLY in an explicit Go test overlay.

The imported production package never includes _test.go. This overlay gives the
CLI integration tests that same fake without shipping a fake or test hook in the
binary. It does not modify sources or remove either hardware opener guard.
"""
import json
import pathlib
import sys

root = pathlib.Path(__file__).resolve().parents[1]
output = pathlib.Path(sys.argv[1]).resolve()
output.parent.mkdir(parents=True, exist_ok=True)
replace = {}
for source in sorted((root / "internal/alsapcm").glob("*_test.go")):
    if "package alsapcm\n" in source.read_text():
        target = source.with_name("zz_nativeintegration_" + source.stem + "support.go")
        replace[str(target)] = str(source)
source = root / "cmd/bridge/output_dsdtest_integration_test.go"
enabled = output.with_suffix(".go.txt")
enabled.write_text(source.read_text().replace("//go:build nativeintegration\n", "", 1))
replace[str(source)] = str(enabled)
output.write_text(json.dumps({"Replace": replace}, indent=2) + "\n")
